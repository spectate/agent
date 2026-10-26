// Package update finds, verifies and installs agent releases from GitHub.
//
// A release is trusted when checksums.txt carries a valid ed25519 signature
// (checksums.txt.sig, made by cmd/release-sign with the release key) and the
// binary matches its sha256 in checksums.txt. The binary is then swapped in
// atomically: written next to the running one, fsynced, test-run, renamed.
package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spectate/agent/v2/internal/version"
)

const (
	// Repo is the GitHub repository releases come from.
	Repo = "spectate/agent"
	// DefaultAPIURL is the GitHub API.
	DefaultAPIURL = "https://api.github.com"
	// ChecksumsName and SignatureName are the release's checksum assets.
	ChecksumsName = "checksums.txt"
	SignatureName = ChecksumsName + ".sig"

	maxBinary = 256 << 20
	maxSmall  = 1 << 20
)

// allowedHosts are the only hosts the updater talks to: the API, release
// download URLs and the CDN hosts GitHub redirects asset downloads to.
var allowedHosts = []string{
	"api.github.com",
	"github.com",
	"objects.githubusercontent.com",
	"release-assets.githubusercontent.com",
}

// stableTag matches v2 release tags; pre-releases (v2.1.0-rc.1) do not match.
var stableTag = regexp.MustCompile(`^v2\.[0-9]+\.[0-9]+$`)

// Release is a GitHub release.
type Release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	HTMLURL    string  `json:"html_url"`
	Assets     []Asset `json:"assets"`
}

// Version is the tag without its "v".
func (r *Release) Version() string { return strings.TrimPrefix(r.TagName, "v") }

func (r *Release) asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Asset is one release file.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Client fetches releases.
type Client struct {
	apiURL string
	// PublicKey verifies checksums.txt.sig; nil skips the signature check
	// (the caller must have been told to, explicitly).
	PublicKey ed25519.PublicKey
	http      *http.Client
	// testHost is allowed in addition to allowedHosts (any scheme) when the
	// API URL is overridden, which only development builds permit.
	testHost string
}

// New returns a client for apiURL (DefaultAPIURL in production). A different
// URL also becomes an allowed download host; callers only pass one for
// development builds.
func New(apiURL string, pub ed25519.PublicKey) (*Client, error) {
	apiURL = strings.TrimRight(apiURL, "/")
	c := &Client{apiURL: apiURL, PublicKey: pub}
	if apiURL != DefaultAPIURL {
		u, err := url.Parse(apiURL)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("invalid update API URL %q", apiURL)
		}
		c.testHost = u.Host
	}
	c.http = &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return c.allowed(req.URL)
		},
	}
	return c, nil
}

func (c *Client) allowed(u *url.URL) error {
	if c.testHost != "" && u.Host == c.testHost {
		return nil
	}
	if u.Scheme != "https" || !slices.Contains(allowedHosts, u.Hostname()) || u.Port() != "" {
		return fmt.Errorf("refusing to download from %s://%s (not a GitHub release host)", u.Scheme, u.Host)
	}
	return nil
}

// ParsePublicKey decodes a base64 ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid release public key (want base64 of a 32-byte ed25519 key)")
	}
	return ed25519.PublicKey(b), nil
}

// Latest returns the newest stable v2 release. Releases are listed rather than
// asking for releases/latest, so only stable v2.x.y tags are ever installed,
// whatever is marked latest.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	var releases []Release
	if err := c.getJSON(ctx, c.apiURL+"/repos/"+Repo+"/releases?per_page=30", &releases); err != nil {
		return nil, err
	}
	var best *Release
	for i := range releases {
		r := &releases[i]
		if r.Draft || r.Prerelease || !stableTag.MatchString(r.TagName) {
			continue
		}
		if best == nil || version.Compare(r.Version(), best.Version()) > 0 {
			best = r
		}
	}
	if best == nil {
		return nil, errors.New("no v2 release found")
	}
	return best, nil
}

// ByVersion returns the release tagged v<ver> (pre-releases included).
func (c *Client) ByVersion(ctx context.Context, ver string) (*Release, error) {
	ver = strings.TrimPrefix(strings.TrimSpace(ver), "v")
	if v, ok := version.Parse(ver); !ok || v.Major != 2 {
		return nil, fmt.Errorf("invalid version %q (want 2.x.y)", ver)
	}
	var r Release
	if err := c.getJSON(ctx, c.apiURL+"/repos/"+Repo+"/releases/tags/v"+url.PathEscape(ver), &r); err != nil {
		return nil, err
	}
	if r.Draft || r.TagName != "v"+ver {
		return nil, fmt.Errorf("release v%s not found", ver)
	}
	return &r, nil
}

// BinaryName is the release asset for a platform.
func BinaryName(goos, goarch string) string { return "spectate-agent_" + goos + "_" + goarch }

// Download fetches and verifies the release binary for goos/goarch:
// checksums.txt must be signed by c.PublicKey (unless nil) and the binary
// must match its sha256 there.
func (c *Client) Download(ctx context.Context, r *Release, goos, goarch string) ([]byte, error) {
	name := BinaryName(goos, goarch)
	binAsset, ok := r.asset(name)
	if !ok {
		return nil, fmt.Errorf("release %s has no %s", r.TagName, name)
	}
	sumsAsset, ok := r.asset(ChecksumsName)
	if !ok {
		return nil, fmt.Errorf("release %s has no %s", r.TagName, ChecksumsName)
	}

	sums, err := c.fetch(ctx, sumsAsset.URL, maxSmall)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", ChecksumsName, err)
	}
	if c.PublicKey != nil {
		sigAsset, ok := r.asset(SignatureName)
		if !ok {
			return nil, fmt.Errorf("release %s has no %s: refusing an unsigned release", r.TagName, SignatureName)
		}
		sig, err := c.fetch(ctx, sigAsset.URL, maxSmall)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", SignatureName, err)
		}
		if err := VerifySignature(c.PublicKey, sums, sig); err != nil {
			return nil, err
		}
	}
	want, err := Checksum(sums, name)
	if err != nil {
		return nil, err
	}

	bin, err := c.fetch(ctx, binAsset.URL, maxBinary)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	got := sha256.Sum256(bin)
	if !bytes.Equal(got[:], want) {
		return nil, fmt.Errorf("checksum mismatch for %s: got %x, checksums.txt says %x", name, got, want)
	}
	return bin, nil
}

// VerifySignature checks a base64 ed25519 signature (checksums.txt.sig).
func VerifySignature(pub ed25519.PublicKey, data, sigFile []byte) error {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigFile)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("malformed %s", SignatureName)
	}
	if !ed25519.Verify(pub, data, sig) {
		return fmt.Errorf("%s: signature verification failed (not signed by the release key)", ChecksumsName)
	}
	return nil
}

// Checksum returns the sha256 listed for name in a checksums.txt
// ("<hex>  <name>" lines, as written by sha256sum and goreleaser).
func Checksum(sums []byte, name string) ([]byte, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum, err := hex.DecodeString(fields[0])
		if err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("malformed checksum for %s", name)
		}
		return sum, nil
	}
	return nil, fmt.Errorf("%s has no checksum for %s", ChecksumsName, name)
}

// Install atomically replaces target with bin: it writes a temporary file in
// target's directory, fsyncs it, checks that it runs and reports
// wantVersion, then renames it over target.
func Install(bin []byte, target, wantVersion string) (err error) {
	dir := filepath.Dir(target)
	mode := fs.FileMode(0o755)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm() | 0o111
	}
	tmp, err := os.CreateTemp(dir, ".spectate-agent-update-*")
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("cannot write to %s: run as root (sudo spectate-agent update)", dir)
		}
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(bin); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := selfTest(tmp.Name(), wantVersion); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // persist the rename
		_ = d.Close()
	}
	return nil
}

// selfTest runs `<bin> version` and expects "spectate-agent <wantVersion> ".
func selfTest(bin, wantVersion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return fmt.Errorf("new binary does not run: %w", err)
	}
	if !strings.HasPrefix(string(out), "spectate-agent "+wantVersion+" ") {
		return fmt.Errorf("new binary reports %q, want version %s", strings.TrimSpace(string(out)), wantVersion)
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, rawURL string, out any) error {
	data, err := c.get(ctx, rawURL, maxSmall*4, "application/vnd.github+json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s: %w", rawURL, err)
	}
	return nil
}

func (c *Client) fetch(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	return c.get(ctx, rawURL, limit, "application/octet-stream")
}

func (c *Client) get(ctx context.Context, rawURL string, limit int64, accept string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if err := c.allowed(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", version.UserAgent())
	if strings.HasPrefix(accept, "application/vnd.github") {
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("GET %s: not found", rawURL)
		}
		return nil, fmt.Errorf("GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", rawURL, limit)
	}
	return data, nil
}
