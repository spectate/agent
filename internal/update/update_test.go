package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fakeGitHub struct {
	srv      *httptest.Server
	releases []Release
	files    map[string][]byte
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/spectate/agent/releases":
			_ = json.NewEncoder(w).Encode(f.releases)
		case strings.HasPrefix(r.URL.Path, "/repos/spectate/agent/releases/tags/"):
			tag := strings.TrimPrefix(r.URL.Path, "/repos/spectate/agent/releases/tags/")
			for _, rel := range f.releases {
				if rel.TagName == tag {
					_ = json.NewEncoder(w).Encode(rel)
					return
				}
			}
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/download/"):
			// Like github.com: release downloads redirect to a CDN.
			http.Redirect(w, r, "/cdn/"+strings.TrimPrefix(r.URL.Path, "/download/"), http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/cdn/"):
			data, ok := f.files[strings.TrimPrefix(r.URL.Path, "/cdn/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// publish adds a release with a binary, checksums.txt and (when priv is set)
// checksums.txt.sig.
func (f *fakeGitHub) publish(tag string, bin []byte, priv ed25519.PrivateKey) {
	name := BinaryName(runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(bin)
	sums := []byte(fmt.Sprintf("%x  %s\n%x  install.sh\n", sum, name, sha256.Sum256([]byte("x"))))
	rel := Release{TagName: tag}
	add := func(file string, data []byte) {
		f.files[tag+"/"+file] = data
		rel.Assets = append(rel.Assets, Asset{Name: file, URL: f.srv.URL + "/download/" + tag + "/" + file})
	}
	add(name, bin)
	add(ChecksumsName, sums)
	if priv != nil {
		add(SignatureName, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums))+"\n"))
	}
	f.releases = append(f.releases, rel)
}

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestLatestPicksHighestStableV2(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.releases = []Release{
		{TagName: "v2.9.0"},
		{TagName: "v2.10.0-rc.1", Prerelease: true},
		{TagName: "v2.11.0", Draft: true},
		{TagName: "v2.10.0"},
		{TagName: "v2.10.1", Prerelease: true}, // flagged as pre-release on GitHub
		{TagName: "v1.5.0"},
		{TagName: "v3.0.0"},
	}
	c, _ := New(gh.srv.URL, nil)
	rel, err := c.Latest(context.Background())
	if err != nil || rel.TagName != "v2.10.0" {
		t.Fatalf("latest = %+v, %v", rel, err)
	}

	rel, err = c.ByVersion(context.Background(), "2.10.0-rc.1")
	if err != nil || rel.TagName != "v2.10.0-rc.1" {
		t.Fatalf("by version = %+v, %v", rel, err)
	}
	if _, err := c.ByVersion(context.Background(), "1.5.0"); err == nil {
		t.Fatal("v1 releases must not be installable")
	}
	if _, err := c.ByVersion(context.Background(), "2.11.0"); err == nil {
		t.Fatal("drafts must not be installable")
	}

	gh.releases = []Release{{TagName: "v1.5.0"}}
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("no v2 release must be an error")
	}
}

func TestDownloadVerifiesSignatureAndChecksum(t *testing.T) {
	pub, priv := keypair(t)
	_, otherPriv := keypair(t)
	gh := newFakeGitHub(t)
	bin := []byte("new agent binary")
	gh.publish("v2.1.0", bin, priv)
	gh.publish("v2.2.0", bin, otherPriv) // signed by a different key
	gh.publish("v2.3.0", bin, nil)       // unsigned
	ctx := context.Background()
	c, _ := New(gh.srv.URL, pub)

	rel, _ := c.ByVersion(ctx, "2.1.0")
	got, err := c.Download(ctx, rel, runtime.GOOS, runtime.GOARCH)
	if err != nil || string(got) != string(bin) {
		t.Fatalf("download = %q, %v", got, err)
	}

	rel, _ = c.ByVersion(ctx, "2.2.0")
	if _, err := c.Download(ctx, rel, runtime.GOOS, runtime.GOARCH); err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("foreign signature: %v", err)
	}
	rel, _ = c.ByVersion(ctx, "2.3.0")
	if _, err := c.Download(ctx, rel, runtime.GOOS, runtime.GOARCH); err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("unsigned release: %v", err)
	}

	// Tampered binary (checksums.txt still validly signed).
	gh.files["v2.1.0/"+BinaryName(runtime.GOOS, runtime.GOARCH)] = []byte("evil binary")
	rel, _ = c.ByVersion(ctx, "2.1.0")
	if _, err := c.Download(ctx, rel, runtime.GOOS, runtime.GOARCH); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered binary: %v", err)
	}
	// Tampered checksums.txt (matches the evil binary, signature no longer does).
	sum := sha256.Sum256([]byte("evil binary"))
	gh.files["v2.1.0/"+ChecksumsName] = []byte(fmt.Sprintf("%x  %s\n", sum, BinaryName(runtime.GOOS, runtime.GOARCH)))
	if _, err := c.Download(ctx, rel, runtime.GOOS, runtime.GOARCH); err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("tampered checksums: %v", err)
	}

	// Without a key (explicit --insecure-skip-signature) only the checksum protects.
	insecure, _ := New(gh.srv.URL, nil)
	if _, err := insecure.Download(ctx, rel, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatalf("insecure download of a consistent release: %v", err)
	}
	if _, err := insecure.Download(ctx, rel, "plan9", "mips"); err == nil {
		t.Fatal("missing platform binary must be an error")
	}
}

func TestOnlyGitHubHostsAreContacted(t *testing.T) {
	c, _ := New(DefaultAPIURL, nil)
	for raw, ok := range map[string]bool{
		"https://api.github.com/repos/spectate/agent/releases":                         true,
		"https://github.com/spectate/agent/releases/download/v2.1.0/checksums.txt":     true,
		"https://objects.githubusercontent.com/github-production-release-asset/x":      true,
		"https://release-assets.githubusercontent.com/github-production-release-asset": true,
		"http://github.com/spectate/agent/releases/download/v2.1.0/checksums.txt":      false,
		"https://github.com:8443/x":            false,
		"https://evil.example.com/x":           false,
		"https://github.com.evil.example.com/": false,
		"https://user@githubusercontent.com/x": false,
	} {
		u, _ := url.Parse(raw)
		if err := c.allowed(u); (err == nil) != ok {
			t.Errorf("allowed(%s) = %v, want ok=%v", raw, err, ok)
		}
	}

	// A redirect off the allowlist is not followed.
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/binary", http.StatusFound)
	}))
	defer evil.Close()
	tc, _ := New(evil.URL, nil)
	if _, err := tc.fetch(context.Background(), evil.URL+"/download/x", 10); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("redirect to a foreign host: %v", err)
	}
}

func TestChecksumParsing(t *testing.T) {
	sums := []byte("" +
		"0000000000000000000000000000000000000000000000000000000000000001  spectate-agent_linux_amd64.tar.gz\n" +
		"00000000000000000000000000000000000000000000000000000000000000ff  spectate-agent_linux_amd64\n")
	got, err := Checksum(sums, "spectate-agent_linux_amd64")
	if err != nil || got[31] != 0xff {
		t.Fatalf("checksum = %x, %v", got, err)
	}
	if _, err := Checksum(sums, "spectate-agent_linux_arm64"); err == nil {
		t.Fatal("missing entry must be an error")
	}
	if _, err := Checksum([]byte("zz  spectate-agent_linux_amd64\n"), "spectate-agent_linux_amd64"); err == nil {
		t.Fatal("malformed entry must be an error")
	}
}

func TestInstallReplacesAtomicallyAfterSelfTest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "spectate-agent")
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho 'spectate-agent 2.0.0 (commit a, built b)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	good := []byte("#!/bin/sh\necho 'spectate-agent 2.1.0 (commit c, built d)'\n")

	// Wrong version reported: target untouched, temp file removed.
	if err := Install(good, target, "2.2.0"); err == nil || !strings.Contains(err.Error(), "want version 2.2.0") {
		t.Fatalf("version mismatch: %v", err)
	}
	// Not runnable at all.
	if err := Install([]byte("\x7fELF garbage"), target, "2.1.0"); err == nil {
		t.Fatal("a binary that does not run must not be installed")
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), "2.0.0") {
		t.Fatal("target replaced by a failed install")
	}

	if err := Install(good, target, "2.1.0"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != string(good) {
		t.Fatal("target not replaced")
	}
	if info, _ := os.Stat(target); info.Mode().Perm()&0o111 == 0 {
		t.Fatal("installed binary is not executable")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestParsePublicKey(t *testing.T) {
	pub, _ := keypair(t)
	got, err := ParsePublicKey(" " + base64.StdEncoding.EncodeToString(pub) + "\n")
	if err != nil || !got.Equal(pub) {
		t.Fatalf("parse = %v, %v", got, err)
	}
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("ParsePublicKey(%q) should fail", bad)
		}
	}
}
