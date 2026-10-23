package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/spectate/agent/v2/internal/update"
)

// buildAgent builds this command with the given version and update key.
func buildAgent(t *testing.T, dir, ver, pubKey string) string {
	t.Helper()
	out := filepath.Join(dir, "spectate-agent-"+ver)
	ldflags := "-X github.com/spectate/agent/v2/internal/version.Version=" + ver
	if pubKey != "" {
		ldflags += " -X github.com/spectate/agent/v2/internal/version.UpdatePublicKey=" + pubKey
	}
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", ver, err, b)
	}
	return out
}

// fakeReleases serves a GitHub-like releases API and asset downloads.
type fakeReleases struct {
	mu    sync.Mutex
	srv   *httptest.Server
	rels  []update.Release
	files map[string][]byte
}

func (f *fakeReleases) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/repos/spectate/agent/releases":
		_ = json.NewEncoder(w).Encode(f.rels)
	case strings.HasPrefix(r.URL.Path, "/download/"):
		http.Redirect(w, r, "/cdn/"+strings.TrimPrefix(r.URL.Path, "/download/"), http.StatusFound)
	case strings.HasPrefix(r.URL.Path, "/cdn/"):
		if data, ok := f.files[strings.TrimPrefix(r.URL.Path, "/cdn/")]; ok {
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeReleases) publish(tag string, bin []byte, signer ed25519.PrivateKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := update.BinaryName(runtime.GOOS, runtime.GOARCH)
	sums := []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(bin), name))
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(signer, sums)) + "\n")
	rel := update.Release{TagName: tag}
	for file, data := range map[string][]byte{name: bin, update.ChecksumsName: sums, update.SignatureName: sig} {
		f.files[tag+"/"+file] = data
		rel.Assets = append(rel.Assets, update.Asset{Name: file, URL: f.srv.URL + "/download/" + tag + "/" + file})
	}
	f.rels = []update.Release{rel}
}

func (f *fakeReleases) tamper(path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = data
}

// TestSelfUpdateEndToEnd builds an "installed" 2.0.0-dev binary and a 2.1.0
// release with a throwaway signing key, then runs `update` against a fake
// GitHub: it must reject tampered releases and atomically install a good one.
func TestSelfUpdateEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("update supports linux and darwin")
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.StdEncoding.EncodeToString(pub)

	build := t.TempDir()
	oldBin := buildAgent(t, build, "2.0.0-dev", key)
	newBin := buildAgent(t, build, "2.1.0", key)
	newData, err := os.ReadFile(newBin)
	if err != nil {
		t.Fatal(err)
	}

	gh := &fakeReleases{files: map[string][]byte{}}
	gh.srv = httptest.NewServer(gh)
	defer gh.srv.Close()

	installDir := t.TempDir()
	installed := filepath.Join(installDir, "spectate-agent")
	reset := func() {
		data, _ := os.ReadFile(oldBin)
		if err := os.WriteFile(installed, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command(installed, append([]string{"update", "--no-restart"}, args...)...)
		cmd.Env = append(os.Environ(), "SPECTATE_UPDATE_API_URL="+gh.srv.URL)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	installedVersion := func() string {
		out, _ := exec.Command(installed, "version").Output()
		return string(out)
	}
	reset()

	// --check only reports.
	gh.publish("v2.1.0", newData, priv)
	out, err := run("--check")
	if err != nil || !strings.Contains(out, "current: 2.0.0-dev") || !strings.Contains(out, "latest:  2.1.0") {
		t.Fatalf("--check: %v\n%s", err, out)
	}
	if !strings.HasPrefix(installedVersion(), "spectate-agent 2.0.0-dev ") {
		t.Fatal("--check must not install")
	}

	// Signed by another key.
	gh.publish("v2.1.0", newData, otherPriv)
	if out, err := run(); err == nil || !strings.Contains(out, "signature verification failed") {
		t.Fatalf("foreign signature accepted: %v\n%s", err, out)
	}
	// Tampered checksum: the binary no longer matches the signed checksums.txt.
	gh.publish("v2.1.0", newData, priv)
	gh.tamper("v2.1.0/"+update.BinaryName(runtime.GOOS, runtime.GOARCH), append([]byte(nil), oldBinData(t, oldBin)...))
	if out, err := run(); err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("tampered binary accepted: %v\n%s", err, out)
	}
	// Tampered checksums.txt (consistent with the swapped binary, but unsigned by the key).
	gh.tamper("v2.1.0/"+update.ChecksumsName, []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(oldBinData(t, oldBin)), update.BinaryName(runtime.GOOS, runtime.GOARCH))))
	if out, err := run(); err == nil || !strings.Contains(out, "signature verification failed") {
		t.Fatalf("tampered checksums accepted: %v\n%s", err, out)
	}
	if !strings.HasPrefix(installedVersion(), "spectate-agent 2.0.0-dev ") {
		t.Fatal("a rejected update modified the installed binary")
	}

	// A good release is installed atomically.
	gh.publish("v2.1.0", newData, priv)
	out, err = run()
	if err != nil || !strings.Contains(out, "signature and checksum verified") {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if !strings.HasPrefix(installedVersion(), "spectate-agent 2.1.0 ") {
		t.Fatalf("installed binary reports %q", installedVersion())
	}
	if entries, _ := os.ReadDir(installDir); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}

	// A newer build with --if-newer is quiet and leaves the binary alone.
	newer := buildAgent(t, build, "2.2.0-dev", key)
	cmd := exec.Command(newer, "update", "--if-newer", "--no-restart")
	cmd.Env = append(os.Environ(), "SPECTATE_UPDATE_API_URL="+gh.srv.URL)
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("--if-newer when current: %v %q", err, out)
	}

	// Release builds ignore the API override; keyless builds refuse to update.
	cmd = exec.Command(installed, "update", "--no-restart")
	cmd.Env = append(os.Environ(), "SPECTATE_UPDATE_API_URL="+gh.srv.URL)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "only honoured by development builds") {
		t.Fatalf("release build accepted the API override: %v\n%s", err, out)
	}
	keyless := buildAgent(t, build, "2.0.0-dev.nokey", "")
	cmd = exec.Command(keyless, "update", "--no-restart")
	cmd.Env = append(os.Environ(), "SPECTATE_UPDATE_API_URL="+gh.srv.URL)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "no release signing key") {
		t.Fatalf("keyless build updated without --insecure-skip-signature: %v\n%s", err, out)
	}
}

func oldBinData(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
