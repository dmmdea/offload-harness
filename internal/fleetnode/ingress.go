// ingress.go is the object-storage seam AND the SSRF boundary for pipeline
// jobs (Task 6's buildPipelineJob calls FetchRefs with a job's image_refs):
// it turns the dispatch payload's bare URLs into local files a pipeline CLI
// can read. Every ref is checked against the allowlist BEFORE any network
// dial — the threat is a compromised/malicious dispatcher, or a crafted
// job_spec, using this node as an open proxy into its private network or the
// wider internet. No hostname is ever DNS-resolved to make that check: only a
// literal IP address can pass via the tailnet CGNAT range or loopback: a
// hostname passes ONLY by an exact "host:port" match against extraAllow.
// Resolving a hostname to inspect its IP and then dialing that same hostname
// again is the classic DNS-rebinding bypass this design avoids by construction.
package fleetnode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tailnetCGNAT is the Tailscale-assigned CGNAT block (100.64.0.0/10) every
// fleet node and the dispatcher live in — allowlisted by default, no
// extraAllow entry required.
var tailnetCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// ingressTimeout bounds one ref's whole fetch (dial + headers + body).
const ingressTimeout = 60 * time.Second

// FetchRefs downloads each ref into destDir (created if needed) and returns
// ref key -> absolute local path. Any failure aborts the whole set (the
// returned cleanup removes destDir). Fetches are refused unless the URL host
// is a tailnet CGNAT address (100.64.0.0/10), loopback, or in extraAllow
// ("host:port" entries). Each body is capped at capMB MiB and must start
// with PNG/JPEG/WebP magic bytes.
func FetchRefs(ctx context.Context, refs map[string]string, destDir string,
	extraAllow []string, capMB int) (paths map[string]string, cleanup func(), err error) {
	noop := func() {}

	absDestDir, absErr := filepath.Abs(destDir)
	if absErr != nil {
		return nil, noop, fmt.Errorf("ingress: resolve destDir %q: %w", destDir, absErr)
	}
	if mkErr := os.MkdirAll(absDestDir, 0o755); mkErr != nil {
		return nil, noop, fmt.Errorf("ingress: create %s: %w", absDestDir, mkErr)
	}
	remove := func() { os.RemoveAll(absDestDir) }

	// One client for the whole set: CheckRedirect refuses every redirect (a
	// redirect target has not itself passed the allowlist check, so
	// following it would reopen the hole this file exists to close).
	client := &http.Client{
		Timeout:       ingressTimeout,
		CheckRedirect: refuseRedirect,
	}

	out := make(map[string]string, len(refs))
	for key, rawURL := range refs {
		p, fetchErr := fetchOne(ctx, client, absDestDir, key, rawURL, extraAllow, capMB)
		if fetchErr != nil {
			remove()
			return nil, noop, fetchErr
		}
		out[key] = p
	}
	return out, remove, nil
}

// refuseRedirect makes the client treat any redirect as fatal.
func refuseRedirect(req *http.Request, via []*http.Request) error {
	return fmt.Errorf("redirect to %s refused", req.URL)
}

// fetchOne fetches a single ref: allowlist check, GET, status/cap/magic
// checks, then write destDir/<key><ext-by-magic>. Every error names key and
// rawURL — dispatch carries several refs (product/logo/background), and a
// bare error would leave the caller guessing which one failed.
func fetchOne(ctx context.Context, client *http.Client, absDestDir, key, rawURL string, extraAllow []string, capMB int) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", refErr(key, rawURL, fmt.Errorf("invalid URL: %w", err))
	}
	host, port := hostPort(u)
	if err := checkAllowed(host, port, extraAllow); err != nil {
		return "", refErr(key, rawURL, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", refErr(key, rawURL, fmt.Errorf("build request: %w", err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", refErr(key, rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", refErr(key, rawURL, fmt.Errorf("unexpected status %s", resp.Status))
	}

	capLimit := int64(capMB) << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, capLimit+1))
	if err != nil {
		return "", refErr(key, rawURL, fmt.Errorf("reading body: %w", err))
	}
	if int64(len(body)) > capLimit {
		return "", refErr(key, rawURL, fmt.Errorf("body exceeds the %d MiB cap", capMB))
	}

	ext, err := sniffImageExt(body)
	if err != nil {
		return "", refErr(key, rawURL, err)
	}

	dest := filepath.Join(absDestDir, key+ext)
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		return "", refErr(key, rawURL, fmt.Errorf("write %s: %w", dest, err))
	}
	return dest, nil
}

// refErr names the ref key and URL on every failure.
func refErr(key, rawURL string, err error) error {
	return fmt.Errorf("ingress: ref %q (%s): %w", key, rawURL, err)
}

// hostPort splits a URL's authority into host and port, defaulting the port
// by scheme (http -> 80, https -> 443) when the URL carries none — a ref URL
// without an explicit port must still resolve to a concrete "host:port" for
// both the CIDR/loopback check and extraAllow's exact match.
func hostPort(u *url.URL) (host, port string) {
	if h, p, err := net.SplitHostPort(u.Host); err == nil {
		return h, p
	}
	// No explicit port: net.SplitHostPort failed on the "missing port" form,
	// so u.Host is the bare host (possibly IPv6 bracketed, no port). Strip
	// any brackets before the CIDR/loopback check.
	host = strings.TrimSuffix(strings.TrimPrefix(u.Host, "["), "]")
	if u.Scheme == "https" {
		return host, "443"
	}
	return host, "80"
}

// checkAllowed refuses hostport unless it is loopback, inside the tailnet
// CGNAT range, or an exact extraAllow entry. host is never DNS-resolved: only
// a literal IP address can satisfy the CIDR/loopback check (netip.ParseAddr
// does no lookup); a hostname can only pass via an exact "host:port" listing
// in extraAllow.
func checkAllowed(host, port string, extraAllow []string) error {
	hostport := net.JoinHostPort(host, port)
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.IsLoopback() || tailnetCGNAT.Contains(addr) {
			return nil
		}
	}
	for _, a := range extraAllow {
		if a == hostport {
			return nil
		}
	}
	return fmt.Errorf("host %s not allowlisted (need tailnet 100.64.0.0/10, loopback, or an explicit ingress_allow entry)", hostport)
}

// sniffImageExt identifies body's image type by magic bytes and returns the
// extension to store it under. Only PNG/JPEG/WebP pass — everything else,
// including SVG (XML text, and in some renderers executable), is refused
// outright rather than guessed at.
func sniffImageExt(body []byte) (string, error) {
	switch {
	case bytes.HasPrefix(body, []byte("\x89PNG")):
		return ".png", nil
	case bytes.HasPrefix(body, []byte("\xFF\xD8\xFF")):
		return ".jpg", nil
	case len(body) >= 12 && bytes.HasPrefix(body, []byte("RIFF")) && bytes.Equal(body[8:12], []byte("WEBP")):
		return ".webp", nil
	default:
		return "", fmt.Errorf("not an allowed image type (want PNG/JPEG/WebP magic bytes)")
	}
}
