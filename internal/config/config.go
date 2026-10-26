// Package config holds the agent's local settings (environment), its
// persisted state (host token, last server config) and the server-driven
// remote config.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spectate/agent/v2/internal/version"
)

// Remote is the server-driven config returned on register and every ingest.
// Defaults mirror laravel/config/host_agent.php so an agent that never reached
// the API behaves like one that did.
type Remote struct {
	CollectionInterval int      `json:"collection_interval"`
	IngestInterval     int      `json:"ingest_interval"`
	Collectors         []string `json:"collectors"`
	Disk               struct {
		IncludeMountpoints []string `json:"include_mountpoints"`
		ExcludeMountpoints []string `json:"exclude_mountpoints"`
		ExcludeDevices     []string `json:"exclude_devices"`
		ExcludeFilesystems []string `json:"exclude_filesystems"`
	} `json:"disk"`
	Net struct {
		IncludeInterfaces []string `json:"include_interfaces"`
		ExcludeInterfaces []string `json:"exclude_interfaces"`
	} `json:"net"`
	DiskIO struct {
		ExcludeDevices []string `json:"exclude_devices"`
	} `json:"diskio"`
}

// DefaultRemote returns the built-in remote config.
func DefaultRemote() Remote {
	var r Remote
	r.CollectionInterval = 15
	r.IngestInterval = 60
	r.Collectors = []string{
		"cpu", "memory", "load", "disk", "net", "uptime", "agent",
		"diskio", "pressure", "services", "updates", "sockets", "clock",
	}
	// Network and automount filesystems: statfs on a dead server blocks
	// (uninterruptibly), and their usage belongs to the server that exports
	// them. On Linux gopsutil already skips "nodev" filesystems, which covers
	// these; macOS lists every mount, so the list is what keeps them out there.
	r.Disk.ExcludeFilesystems = []string{
		"squashfs", "vfat",
		"nfs", "nfs4", "cifs", "smbfs", "smb3", "afpfs", "webdav", "autofs",
		"fuse.sshfs", "ceph", "glusterfs", "fuse.glusterfs", "9p",
	}
	r.Net.IncludeInterfaces = []string{"en0", "eth0", "lo0"}
	r.Net.ExcludeInterfaces = []string{"lo", "br*", "bridge*", "docker*", "veth*", "virbr*", "cni*", "flannel*", "cali*", "utun*", "llw*", "ap*", "anpi*", "awdl*", "gif*", "stf*"}
	// Physical disks only: virtual devices (loop, RAM disks, optical drives)
	// and stacked ones (device mapper/LVM, software RAID), whose I/O is
	// already counted on the disks underneath.
	r.DiskIO.ExcludeDevices = []string{"loop*", "ram*", "zram*", "sr*", "fd*", "dm-*", "md*", "nbd*"}
	return r
}

// Normalize clamps intervals to sane bounds; a zero-value Remote becomes the default.
func (r Remote) Normalize() Remote {
	d := DefaultRemote()
	if r.IngestInterval == 0 && r.CollectionInterval == 0 && r.Collectors == nil {
		return d
	}
	if r.IngestInterval <= 0 {
		r.IngestInterval = d.IngestInterval
	}
	r.IngestInterval = min(max(r.IngestInterval, 30), 600)
	if r.CollectionInterval <= 0 {
		r.CollectionInterval = d.CollectionInterval
	}
	r.CollectionInterval = min(max(r.CollectionInterval, 10), r.IngestInterval)
	return r
}

// Enabled reports whether a collector is enabled.
func (r Remote) Enabled(name string) bool { return slices.Contains(r.Collectors, name) }

// Equal compares two configs.
func (r Remote) Equal(o Remote) bool {
	a, _ := json.Marshal(r)
	b, _ := json.Marshal(o)
	return string(a) == string(b)
}

// Settings are local, operator-controlled settings read from the environment
// (systemd: EnvironmentFile=/etc/spectate-agent/agent.env).
type Settings struct {
	APIURL        string
	StateDir      string
	SpoolMaxBytes int64
	SpoolMaxAge   time.Duration
	LogLevel      string
}

// LoadSettings reads SPECTATE_* environment variables.
func LoadSettings() Settings {
	s := Settings{
		APIURL:        env("SPECTATE_API_URL", ""),
		StateDir:      env("SPECTATE_STATE_DIR", env("STATE_DIRECTORY", defaultStateDir())),
		SpoolMaxBytes: 50 << 20,
		SpoolMaxAge:   24 * time.Hour,
		LogLevel:      env("SPECTATE_LOG_LEVEL", "info"),
	}
	if v, err := strconv.ParseInt(env("SPECTATE_SPOOL_MAX_MB", ""), 10, 64); err == nil && v > 0 {
		s.SpoolMaxBytes = v << 20
	}
	if v, err := time.ParseDuration(env("SPECTATE_SPOOL_MAX_AGE", "")); err == nil && v > 0 {
		s.SpoolMaxAge = v
	}
	// systemd may pass several colon-separated state directories.
	if i := strings.IndexByte(s.StateDir, ':'); i > 0 {
		s.StateDir = s.StateDir[:i]
	}
	return s
}

func defaultStateDir() string {
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "spectate-agent")
		}
	}
	return "/var/lib/spectate-agent"
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// State is persisted in <state dir>/state.json (mode 0600).
type State struct {
	APIURL    string `json:"api_url"`
	HostToken string `json:"host_token"`
	HostUUID  string `json:"host_uuid,omitempty"`
	Remote    Remote `json:"config"`
	// HostDeleted is set when the API answered 410 HOST_DELETED: the agent
	// refuses to run until `register --force` replaces the state.
	HostDeleted bool `json:"host_deleted,omitempty"`
	// AvailableUpdate is the newer release the API last announced.
	AvailableUpdate *AvailableUpdate `json:"available_update,omitempty"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// AvailableUpdate is a newer agent release announced by the API.
type AvailableUpdate struct {
	Version    string `json:"version"`
	ReleaseURL string `json:"release_url,omitempty"`
}

// ErrNotRegistered is returned when no host token is stored.
var ErrNotRegistered = errors.New("agent is not registered: run `spectate-agent register --token <install token>`")

// StatePath returns the state file path in dir.
func StatePath(dir string) string { return filepath.Join(dir, "state.json") }

// LoadState reads the persisted state.
func LoadState(dir string) (State, error) {
	var st State
	data, err := os.ReadFile(StatePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return st, ErrNotRegistered
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("parse %s: %w", StatePath(dir), err)
	}
	if st.HostToken == "" {
		return st, ErrNotRegistered
	}
	st.Remote = st.Remote.Normalize()
	if st.APIURL == "" {
		st.APIURL = version.DefaultAPIURL
	}
	return st, nil
}

// SaveState writes the state atomically.
func SaveState(dir string, st State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := StatePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, StatePath(dir))
}

// NewInstanceID returns a random id identifying this agent process's batches.
func NewInstanceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LegacyToken reads the host token of the v1 agent (spectated) so a host can
// switch agents without registering again. It understands the YAML the v1
// agent writes (`host:\n  token: hst_...`).
func LegacyToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	inHost := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indented := line[0] == ' ' || line[0] == '\t'
		if !indented {
			inHost = trimmed == "host:"
			continue
		}
		if inHost && strings.HasPrefix(trimmed, "token:") {
			token := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "token:")), `"'`)
			if token != "" {
				return token, nil
			}
		}
	}
	return "", fmt.Errorf("no host.token in %s", path)
}

// LegacyConfigPath is where the v1 agent keeps its config.
func LegacyConfigPath() string {
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".spectated", "config.yaml")
		}
	}
	return "/etc/spectated/config.yaml"
}
