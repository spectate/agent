// Command spectate-agent is the Spectate host monitoring agent (protocol v2).
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	// Bundled Mozilla CA roots, used only when the host has no CA bundle
	// (minimal images, appliances): otherwise TLS to the API would fail.
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/spectate/agent/v2/internal/agent"
	"github.com/spectate/agent/v2/internal/collector"
	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/encode"
	"github.com/spectate/agent/v2/internal/service"
	"github.com/spectate/agent/v2/internal/spool"
	"github.com/spectate/agent/v2/internal/transport"
	"github.com/spectate/agent/v2/internal/update"
	"github.com/spectate/agent/v2/internal/version"
)

const usage = `spectate-agent — Spectate host monitoring agent

Usage:
  spectate-agent register --token <install token> [--api-url URL] [--force]
  spectate-agent import-legacy [--config /etc/spectated/config.yaml] [--api-url URL]
  spectate-agent run
  spectate-agent install [--user spectate-agent] [--auto-update]   (root; systemd / launchd)
  spectate-agent uninstall
  spectate-agent update [--version X] [--check] [--if-newer]      (root)
  spectate-agent status
  spectate-agent version

Environment:
  SPECTATE_API_URL        API base URL (default from registration, else ` + "https://api.spectate.net" + `)
  SPECTATE_STATE_DIR      state + spool directory (default /var/lib/spectate-agent)
  SPECTATE_SPOOL_MAX_MB   spool size bound (default 50)
  SPECTATE_SPOOL_MAX_AGE  spool age bound (default 24h)
  SPECTATE_LOG_LEVEL      debug | info | warn | error (default info)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	settings := config.LoadSettings()
	logger := newLogger(settings.LogLevel)
	slog.SetDefault(logger)

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = runAgent(settings, logger)
		if errors.Is(err, agent.ErrHostDeleted) {
			os.Exit(service.ExitHostDeleted) // already logged; the unit does not restart on it
		}
	case "register":
		err = register(settings, args)
	case "import-legacy":
		err = importLegacy(settings, args)
	case "install":
		err = install(args)
	case "uninstall":
		err = service.Uninstall()
	case "update":
		err = selfUpdate(args)
	case "status":
		err = status(settings)
	case "version", "--version", "-v":
		fmt.Printf("spectate-agent %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildDate)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func apiURL(settings config.Settings, flagValue, stored string) string {
	for _, v := range []string{flagValue, settings.APIURL, stored} {
		if v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return version.DefaultAPIURL
}

func runAgent(settings config.Settings, logger *slog.Logger) error {
	// The agent's working set is a few MB; keep the GC from letting it grow.
	debug.SetMemoryLimit(48 << 20)
	debug.SetGCPercent(50)

	st, err := config.LoadState(settings.StateDir)
	if err != nil {
		return err
	}
	sp, err := spool.Open(filepath.Join(settings.StateDir, "spool"), settings.SpoolMaxBytes, settings.SpoolMaxAge)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return agent.New(agent.Options{
		Client:   transport.New(apiURL(settings, "", st.APIURL), st.HostToken),
		Spool:    sp,
		State:    st,
		StateDir: settings.StateDir,
		Logger:   logger,
	}).Run(ctx)
}

func register(settings config.Settings, args []string) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	token := fs.String("token", os.Getenv("SPECTATE_INSTALL_TOKEN"), "team install token")
	url := fs.String("api-url", "", "API base URL")
	force := fs.Bool("force", false, "replace an existing registration")
	_ = fs.Parse(args)

	if *token == "" {
		return errors.New("--token is required")
	}
	if existing, err := config.LoadState(settings.StateDir); err == nil && existing.HostToken != "" && !*force {
		if existing.HostDeleted {
			return fmt.Errorf("host %s was removed from Spectate; use --force to register this machine again", existing.HostUUID)
		}
		return fmt.Errorf("already registered as host %s (use --force to register again)", existing.HostUUID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, err := collector.HostInfo(ctx)
	if err != nil {
		return fmt.Errorf("gather host info: %w", err)
	}
	hostInfo, cpus := collector.HostInfoMaps(info)
	base := apiURL(settings, *url, "")

	resp, err := transport.New(base, "").Register(ctx, transport.RegisterRequest{
		Token:        *token,
		Hostname:     collector.Hostname(info),
		HostInfo:     hostInfo,
		CPUInfo:      cpus,
		AgentVersion: version.Version,
		HostInfoHash: encode.HostInfoHash(info),
	})
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	if off := time.Duration(resp.ClockOffsetMs) * time.Millisecond; off.Abs() > 30*time.Second {
		fmt.Fprintf(os.Stderr, "warning: host clock is off by %ds; samples are corrected by the server, but fix NTP\n", int64(off.Abs().Seconds()))
	}

	// A fresh state: also clears the host_deleted marker of a previous registration.
	if err := config.SaveState(settings.StateDir, config.State{
		APIURL:    base,
		HostToken: resp.HostToken,
		HostUUID:  resp.HostUUID,
		Remote:    resp.Config.Normalize(),
	}); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	fmt.Printf("Registered host %s (%s). Start the agent with `systemctl start %s` or `spectate-agent run`.\n",
		collector.Hostname(info), resp.HostUUID, service.Name)
	return nil
}

func importLegacy(settings config.Settings, args []string) error {
	fs := flag.NewFlagSet("import-legacy", flag.ExitOnError)
	path := fs.String("config", config.LegacyConfigPath(), "v1 agent (spectated) config file")
	url := fs.String("api-url", "", "API base URL")
	_ = fs.Parse(args)

	token, err := config.LegacyToken(*path)
	if err != nil {
		return err
	}
	if err := config.SaveState(settings.StateDir, config.State{
		APIURL:    apiURL(settings, *url, ""),
		HostToken: token,
		Remote:    config.DefaultRemote(),
	}); err != nil {
		return err
	}
	fmt.Println("Imported the host token of the v1 agent; this host keeps its identity and history.")
	fmt.Println("Stop and remove the v1 agent (`spectated uninstall`) before starting spectate-agent.")
	return nil
}

func install(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	user := fs.String("user", service.Name, "service user (Linux)")
	envFile := fs.String("env-file", "/etc/spectate-agent/agent.env", "EnvironmentFile for the unit (Linux)")
	autoUpdate := fs.Bool("auto-update", false, "also install a daily timer that runs `update --if-newer` (Linux)")
	_ = fs.Parse(args)

	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return err
	}
	if *autoUpdate && runtime.GOOS != "linux" {
		return errors.New("--auto-update is only supported on Linux (systemd)")
	}
	if err := service.Install(service.Options{Binary: bin, User: *user, EnvFile: *envFile, AutoUpdate: *autoUpdate}); err != nil {
		return err
	}
	fmt.Println("spectate-agent service installed and started.")
	if *autoUpdate {
		fmt.Printf("Automatic updates enabled (%s.timer, daily).\n", service.UpdateName)
	}
	return nil
}

func status(settings config.Settings) error {
	st, err := config.LoadState(settings.StateDir)
	if err != nil {
		return err
	}
	sp, err := spool.Open(filepath.Join(settings.StateDir, "spool"), settings.SpoolMaxBytes, settings.SpoolMaxAge)
	if err != nil {
		return err
	}
	fmt.Printf("version:      %s\n", version.Version)
	fmt.Printf("api:          %s\n", apiURL(settings, "", st.APIURL))
	fmt.Printf("host uuid:    %s\n", st.HostUUID)
	fmt.Printf("state dir:    %s\n", settings.StateDir)
	fmt.Printf("collection:   every %ds, shipped every %ds\n", st.Remote.CollectionInterval, st.Remote.IngestInterval)
	fmt.Printf("collectors:   %s\n", strings.Join(st.Remote.Collectors, ", "))
	fmt.Printf("spool:        %d batches, %d bytes\n", sp.Len(), sp.Bytes())
	if st.AvailableUpdate != nil && version.Compare(st.AvailableUpdate.Version, version.Version) > 0 {
		fmt.Printf("update:       %s available: sudo spectate-agent update\n", st.AvailableUpdate.Version)
	}
	if st.HostDeleted {
		fmt.Println("host:         removed from Spectate; the agent no longer runs. Remove it: sudo spectate-agent uninstall")
	}
	return nil
}

// selfUpdate replaces this binary with a verified release and restarts the
// service. The running agent cannot do this itself (it is unprivileged), so
// this runs as root, by hand or from the auto-update timer.
func selfUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	want := fs.String("version", "", "install this release (e.g. 2.1.0; may be older or a pre-release)")
	check := fs.Bool("check", false, "only print the current and latest version")
	ifNewer := fs.Bool("if-newer", false, "do nothing, quietly, unless a newer release exists (for the timer)")
	insecure := fs.Bool("insecure-skip-signature", false, "skip the release signature check (sha256 is still verified)")
	noRestart := fs.Bool("no-restart", false, "do not restart the service")
	_ = fs.Parse(args)

	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("update is not supported on %s", runtime.GOOS)
	}
	apiURL := update.DefaultAPIURL
	if v := os.Getenv("SPECTATE_UPDATE_API_URL"); v != "" {
		if !version.IsDev() {
			return errors.New("SPECTATE_UPDATE_API_URL is only honoured by development builds")
		}
		apiURL = v
	}

	var pub ed25519.PublicKey
	switch {
	case *insecure:
		if !*check {
			fmt.Fprintln(os.Stderr, "warning: --insecure-skip-signature: the release signature is not verified")
		}
	case version.UpdatePublicKey != "":
		var err error
		if pub, err = update.ParsePublicKey(version.UpdatePublicKey); err != nil {
			return err
		}
	case !*check:
		return errors.New("this build has no release signing key (development build): refusing to update " +
			"without signature verification; pass --insecure-skip-signature to update anyway")
	}

	client, err := update.New(apiURL, pub)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var rel *update.Release
	if *want != "" {
		rel, err = client.ByVersion(ctx, *want)
	} else {
		rel, err = client.Latest(ctx)
	}
	if err != nil {
		return fmt.Errorf("find release: %w", err)
	}
	target := rel.Version()
	cmp := version.Compare(target, version.Version)

	if *check {
		fmt.Printf("current: %s\nlatest:  %s\n", version.Version, target)
		if cmp > 0 {
			fmt.Println("An update is available: sudo spectate-agent update")
		}
		return nil
	}
	if cmp <= 0 && (*ifNewer || *want == "") {
		if !*ifNewer {
			fmt.Printf("spectate-agent %s is up to date (newest release: %s).\n", version.Version, target)
		}
		return nil
	}

	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return err
	}
	fmt.Printf("Updating spectate-agent %s -> %s (%s)\n", version.Version, target, update.BinaryName(runtime.GOOS, runtime.GOARCH))
	data, err := client.Download(ctx, rel, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	if err := update.Install(data, bin, target); err != nil {
		return fmt.Errorf("install %s: %w", bin, err)
	}
	if pub != nil {
		fmt.Printf("Installed spectate-agent %s at %s (signature and checksum verified).\n", target, bin)
	} else {
		fmt.Printf("Installed spectate-agent %s at %s (checksum verified, signature NOT verified).\n", target, bin)
	}

	switch {
	case *noRestart || !service.Installed():
		fmt.Printf("Restart the agent to run the new version: %s\n", service.RestartHint())
	case runtime.GOOS == "linux":
		// From the timer, leave a stopped (or failed) agent stopped.
		if err := service.Restart(*ifNewer); err != nil {
			return fmt.Errorf("installed, but restarting failed: %w", err)
		}
		fmt.Println("Restarted spectate-agent.")
	default:
		fmt.Printf("Restart the agent to run the new version: %s\n", service.RestartHint())
	}
	return nil
}
