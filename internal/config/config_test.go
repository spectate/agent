package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStateRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadState(dir); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("missing state should be ErrNotRegistered, got %v", err)
	}
	if err := SaveState(dir, State{APIURL: "https://x", HostToken: "hst_a", HostUUID: "u"}); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(dir)
	if err != nil || st.HostToken != "hst_a" || st.Remote.IngestInterval != 60 {
		t.Fatalf("state = %+v, %v", st, err)
	}
	info, _ := os.Stat(StatePath(dir))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, want 0600 (it holds the host token)", info.Mode().Perm())
	}
}

func TestNormalizeClamps(t *testing.T) {
	r := Remote{CollectionInterval: 1, IngestInterval: 5000, Collectors: []string{"cpu"}}.Normalize()
	if r.CollectionInterval != 10 || r.IngestInterval != 600 {
		t.Fatalf("clamped = %+v", r)
	}
	r = Remote{CollectionInterval: 120, IngestInterval: 60, Collectors: []string{}}.Normalize()
	if r.CollectionInterval != 60 {
		t.Fatal("collection interval must not exceed ingest interval")
	}
	if !(Remote{}).Normalize().Equal(DefaultRemote()) {
		t.Fatal("zero config should become the default")
	}
}

func TestLegacyToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "checks:\n  net:\n    token: not-this\nhost:\n  token: hst_legacy123\ntelemetry:\n  error_reporting: true\nversion: 1\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := LegacyToken(path)
	if err != nil || token != "hst_legacy123" {
		t.Fatalf("token = %q, %v", token, err)
	}
	_ = os.WriteFile(path, []byte("host:\n  token: \"\"\n"), 0o600)
	if _, err := LegacyToken(path); err == nil {
		t.Fatal("empty token must be an error")
	}
}
