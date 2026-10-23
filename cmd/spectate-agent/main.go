// Command spectate-agent is the Spectate host monitoring agent (protocol v2).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/spectate/agent/v2/internal/agent"
	"github.com/spectate/agent/v2/internal/collector"
	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/encode"
	"github.com/spectate/agent/v2/internal/service"
	"github.com/spectate/agent/v2/internal/spool"
	"github.com/spectate/agent/v2/internal/transport"
	"github.com/spectate/agent/v2/internal/version"
)

const usage = `spectate-agent — Spectate host monitoring agent

Usage:
  spectate-agent register --token <install token> [--api-url URL] [--force]
  spectate-agent import-legacy [--config /etc/spectated/config.yaml] [--api-url URL]
  spectate-agent run
  spectate-agent install [--user spectate-agent]   (root; systemd / launchd)
  spectate-agent uninstall
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
	case "register":
		err = register(settings, args)
	case "import-legacy":
		err = importLegacy(settings, args)
	case "install":
		err = install(args)
	case "uninstall":
		err = service.Uninstall()
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
	_ = fs.Parse(args)

	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return err
	}
	if err := service.Install(service.Options{Binary: bin, User: *user, EnvFile: *envFile}); err != nil {
		return err
	}
	fmt.Println("spectate-agent service installed and started.")
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
	return nil
}
