package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spectate/agent/v2/internal/collector"
	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/pkgupdates"
	"github.com/spectate/agent/v2/internal/service"
	"github.com/spectate/agent/v2/internal/spool"
	"github.com/spectate/agent/v2/internal/transport"
	"github.com/spectate/agent/v2/internal/version"
)

// statusReport is `status --json`.
type statusReport struct {
	Version            string                  `json:"version"`
	APIURL             string                  `json:"api_url"`
	HostUUID           string                  `json:"host_uuid"`
	StateDir           string                  `json:"state_dir"`
	CollectionInterval int                     `json:"collection_interval"`
	IngestInterval     int                     `json:"ingest_interval"`
	Collectors         []string                `json:"collectors"`
	SpoolBatches       int                     `json:"spool_batches"`
	SpoolBytes         int64                   `json:"spool_bytes"`
	AvailableUpdate    *config.AvailableUpdate `json:"available_update,omitempty"`
	HostDeleted        bool                    `json:"host_deleted"`
}

func status(settings config.Settings, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)

	st, err := config.LoadState(settings.StateDir)
	if err != nil {
		return err
	}
	sp, err := spool.Open(filepath.Join(settings.StateDir, "spool"), settings.SpoolMaxBytes, settings.SpoolMaxAge)
	if err != nil {
		return err
	}
	var update *config.AvailableUpdate
	if st.AvailableUpdate != nil && version.Compare(st.AvailableUpdate.Version, version.Version) > 0 {
		update = st.AvailableUpdate
	}
	r := statusReport{
		Version:            version.Version,
		APIURL:             apiURL(settings, "", st.APIURL),
		HostUUID:           st.HostUUID,
		StateDir:           settings.StateDir,
		CollectionInterval: st.Remote.CollectionInterval,
		IngestInterval:     st.Remote.IngestInterval,
		Collectors:         st.Remote.Collectors,
		SpoolBatches:       sp.Len(),
		SpoolBytes:         sp.Bytes(),
		AvailableUpdate:    update,
		HostDeleted:        st.HostDeleted,
	}
	if *asJSON {
		return printJSON(r)
	}
	fmt.Printf("version:      %s\n", r.Version)
	fmt.Printf("api:          %s\n", r.APIURL)
	fmt.Printf("host uuid:    %s\n", r.HostUUID)
	fmt.Printf("state dir:    %s\n", r.StateDir)
	fmt.Printf("collection:   every %ds, shipped every %ds\n", r.CollectionInterval, r.IngestInterval)
	fmt.Printf("collectors:   %s\n", strings.Join(r.Collectors, ", "))
	fmt.Printf("spool:        %d batches, %d bytes\n", r.SpoolBatches, r.SpoolBytes)
	if update != nil {
		fmt.Printf("update:       %s available: sudo spectate-agent update\n", update.Version)
	}
	if st.HostDeleted {
		fmt.Println("host:         removed from Spectate; the agent no longer runs. Remove it: sudo spectate-agent uninstall")
	}
	return nil
}

// configCmd: `config show` prints the server-driven config the agent runs with.
func configCmd(settings config.Settings, args []string) error {
	if len(args) == 0 || args[0] != "show" {
		return errors.New("usage: spectate-agent config show")
	}
	st, err := config.LoadState(settings.StateDir)
	if err != nil {
		return err
	}
	fmt.Println("# Effective config, sent by the Spectate API (change it in the dashboard: host → Settings & alerts)")
	return printJSON(st.Remote)
}

// collectCmd runs every collector once (two rounds, for rates) and prints what
// would be shipped. Nothing is sent.
func collectCmd(settings config.Settings, args []string) error {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	_ = fs.Bool("once", true, "collect one round (the only mode)")
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)

	remote := config.DefaultRemote()
	if st, err := config.LoadState(settings.StateDir); err == nil {
		remote = st.Remote
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	results := collector.Probe(ctx, collector.System(settings.StateDir), remote, time.Second)
	if *asJSON {
		return printJSON(results)
	}
	for _, r := range results {
		state := "on"
		if !r.Enabled {
			state = "off (not collected; shown for reference)"
		}
		fmt.Printf("%s · %s · %s\n", r.Collector, state, r.Duration)
		if r.Error != "" {
			fmt.Printf("  error: %s\n", r.Error)
		}
		if len(r.Samples) == 0 && r.Error == "" {
			fmt.Println("  nothing to report on this host")
		}
		for _, s := range r.Samples {
			labels := make([]string, 0, len(s.Labels))
			for _, l := range s.Labels {
				labels = append(labels, l.Key+"="+l.Value)
			}
			label := ""
			if len(labels) > 0 {
				label = " {" + strings.Join(labels, ", ") + "}"
			}
			fmt.Printf("  %-36s %14s %s%s\n", s.Metric, formatValue(s.Value), s.Unit, label)
		}
		fmt.Println()
	}
	return nil
}

func formatValue(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.3f", v)
}

// checkUpdates is run hourly by spectate-agent-packages.timer.
func checkUpdates(settings config.Settings) error {
	cache := os.Getenv("CACHE_DIRECTORY") // set by systemd's CacheDirectory=
	if i := strings.IndexByte(cache, ':'); i > 0 {
		cache = cache[:i]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()
	r := pkgupdates.Checker{CacheDir: cache}.Check(ctx)
	if err := os.MkdirAll(settings.StateDir, 0o700); err != nil {
		return err
	}
	if err := pkgupdates.Write(settings.StateDir, r); err != nil {
		return fmt.Errorf("write %s: %w", pkgupdates.Path(settings.StateDir), err)
	}
	if r.Error != "" {
		if r.Error == pkgupdates.ErrNoPackageManager.Error() {
			fmt.Println(r.Error)
			return nil
		}
		return errors.New(r.Error)
	}
	security := "unknown"
	if r.Security != nil {
		security = fmt.Sprint(*r.Security)
	}
	fmt.Printf("%s: %d pending, %s security\n", r.Manager, r.Pending, security)
	return nil
}

// doctor checks everything the agent depends on and says what to fix.
func doctor(settings config.Settings) error {
	failed := false
	line := func(mark, what, detail string) {
		if mark == "✗" {
			failed = true
		}
		fmt.Printf("%s %-22s %s\n", mark, what, detail)
	}

	st, err := config.LoadState(settings.StateDir)
	switch {
	case errors.Is(err, config.ErrNotRegistered):
		line("✗", "registration", "not registered: sudo spectate-agent register --token <install token>")
	case errors.Is(err, os.ErrPermission):
		line("✗", "registration", "cannot read "+config.StatePath(settings.StateDir)+": run doctor with sudo")
	case err != nil:
		line("✗", "registration", err.Error())
	case st.HostDeleted:
		line("✗", "registration", "this host was removed from Spectate; remove the agent (sudo spectate-agent uninstall) or register again with --force")
	default:
		line("✓", "registration", "host "+st.HostUUID)
	}

	if err == nil {
		base := apiURL(settings, "", st.APIURL)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		resp, cerr := transport.New(base, st.HostToken).Check(ctx)
		cancel()
		var apiErr *transport.Error
		switch {
		case cerr == nil:
			line("✓", "api", base+" accepts the host token ("+resp.Label+")")
			off := time.Duration(resp.ClockOffsetMs) * time.Millisecond
			if off.Abs() > 30*time.Second {
				line("!", "clock", fmt.Sprintf("off by %s; samples are corrected, but fix NTP (timedatectl set-ntp true)", off.Abs().Round(time.Second)))
			} else {
				line("✓", "clock", fmt.Sprintf("within %s of the server", off.Abs().Round(time.Millisecond)))
			}
		case errors.As(cerr, &apiErr) && apiErr.Status == http.StatusNotFound:
			line("!", "api", base+" is reachable, but too old to check the token")
		case errors.As(cerr, &apiErr) && apiErr.Status == http.StatusUnauthorized:
			line("✗", "api", "the host token was rejected (host deleted or token revoked): register again with --force")
		case errors.As(cerr, &apiErr) && apiErr.Status == http.StatusGone:
			line("✗", "api", "this host was removed from Spectate")
		case errors.As(cerr, &apiErr):
			line("✗", "api", fmt.Sprintf("%s answered %v", base, cerr))
		default:
			line("✗", "api", fmt.Sprintf("cannot reach %s: %v (DNS, firewall or proxy?)", base, cerr))
		}

		if sp, err := spool.Open(filepath.Join(settings.StateDir, "spool"), settings.SpoolMaxBytes, settings.SpoolMaxAge); err == nil {
			switch {
			case sp.Bytes() > settings.SpoolMaxBytes/2:
				line("!", "spool", fmt.Sprintf("%d batches (%d bytes) waiting: the agent cannot ship (see api above)", sp.Len(), sp.Bytes()))
			case sp.Len() > 2:
				line("!", "spool", fmt.Sprintf("%d batches waiting to be sent", sp.Len()))
			default:
				line("✓", "spool", fmt.Sprintf("%d batches waiting", sp.Len()))
			}
		}
		if st.AvailableUpdate != nil && version.Compare(st.AvailableUpdate.Version, version.Version) > 0 {
			line("!", "version", fmt.Sprintf("%s, %s is available: sudo spectate-agent update", version.Version, st.AvailableUpdate.Version))
		} else {
			line("✓", "version", version.Version)
		}
	}

	switch {
	case runtime.GOOS == "linux" && service.Installed():
		out, _ := exec.Command("systemctl", "is-active", service.Name).Output()
		if s := strings.TrimSpace(string(out)); s == "active" {
			line("✓", "service", service.Name+" is running")
		} else {
			line("✗", "service", fmt.Sprintf("%s is %s: sudo systemctl start %s; logs: journalctl -u %s", service.Name, s, service.Name, service.Name))
		}
	case service.Installed():
		line("✓", "service", "installed")
	default:
		line("!", "service", "not installed as a service: sudo spectate-agent install")
	}

	remote := config.DefaultRemote()
	if err == nil {
		remote = st.Remote
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, r := range collector.Probe(ctx, collector.System(settings.StateDir), remote, time.Second) {
		name := "collector " + r.Collector
		switch {
		case !r.Enabled:
			line("·", name, "off in Spectate")
		case r.Error != "":
			line("✗", name, r.Error)
		case len(r.Samples) == 0:
			line("·", name, "nothing to report on this host")
		default:
			line("✓", name, plural(len(r.Samples), "value"))
		}
	}

	if runtime.GOOS == "linux" && service.HasPackageManager() && remote.Enabled("updates") {
		r, err := pkgupdates.Read(settings.StateDir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			line("!", "package check", "has not run yet: sudo systemctl enable --now "+service.PackagesName+".timer (or sudo spectate-agent install)")
		case err != nil:
			line("✗", "package check", err.Error())
		case r.Error != "":
			line("✗", "package check", r.Error+": journalctl -u "+service.PackagesName)
		case time.Since(r.CheckedAt) > 3*time.Hour:
			line("!", "package check", "last ran "+time.Since(r.CheckedAt).Round(time.Minute).String()+" ago: systemctl status "+service.PackagesName+".timer")
		default:
			line("✓", "package check", fmt.Sprintf("%s, %d pending, %s ago", r.Manager, r.Pending, time.Since(r.CheckedAt).Round(time.Minute)))
		}
	}

	if failed {
		return errors.New("some checks failed")
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
