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
	"regexp"
	"strings"
	"time"
)

// tailnetCGNAT is the Tailscale-assigned CGNAT block (100.64.0.0/10) every
// fleet node and the dispatcher live in — allowlisted by default, no
// extraAllow entry required.
var tailnetCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// ingressTimeout bounds one ref's whole fetch (dial + headers + body).
const ingressTimeout = 60 * time.Second

// refKeyPattern is the allowlist for ref map keys. fetchOne writes to
// destDir/<key><ext>, so an unvalidated key is a path-traversal primitive —
// a key of "../../evil" would write outside destDir entirely. Real ref keys
// are the fixed short names the dispatch payload actually uses
// (product/logo/background); this pattern matches how they're used, nothing
// more permissive.
var refKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// newIngressClient builds the HTTP client shared by every ref fetch in one
// FetchRefs call: a fixed timeout, every redirect refused, and — critically —
// no environment proxy. Left to its zero value, http.Client falls back to
// http.DefaultTransport, whose Proxy field is http.ProxyFromEnvironment: with
// HTTP_PROXY/HTTPS_PROXY/ALL_PROXY set, a request whose host just passed the
// allowlist check would be handed to a completely unchecked proxy, which
// could then dial anywhere on our behalf — defeating the "checked BEFORE any
// network dial" invariant this whole file exists to enforce. Cloning
// DefaultTransport keeps its other sane defaults (connection pooling, HTTP/2,
// dial/TLS timeouts) and nils only the one field that matters here.
func newIngressClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Timeout:       ingressTimeout,
		CheckRedirect: refuseRedirect,
		Transport:     transport,
	}
}

// FetchRefs downloads each ref into destDir (created if needed) and returns
// ref key -> absolute local path. Any failure aborts the whole set (the
// returned cleanup removes destDir). Fetches are refused unless the URL host
// is a tailnet CGNAT address (100.64.0.0/10), loopback, or in extraAllow
// ("host:port" entries). Each body is capped at capMB MiB and must start
// with PNG/JPEG/WebP magic bytes.
func FetchRefs(ctx context.Context, refs map[string]string, destDir string,
	extraAllow []string, capMB int) (paths map[string]string, cleanup func(), err error) {
	noop := func() {}

	// Validate every ref key BEFORE touching the filesystem or the network:
	// a bad key must never create destDir, let alone contact any server. Uses
	// refErr (not a bespoke format) so this failure reads exactly like every
	// other per-ref error below — "ingress: ref %q (%s): ...".
	for key, rawURL := range refs {
		if !refKeyPattern.MatchString(key) {
			return nil, noop, refErr(key, rawURL, fmt.Errorf("invalid ref key (must match %s)", refKeyPattern.String()))
		}
	}

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
	// following it would reopen the hole this file exists to close), and its
	// Transport ignores env proxies (see newIngressClient).
	client := newIngressClient()

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
	// Explicit scheme check BEFORE the allowlist: without it, a URL like
	// "gopher://100.64.1.2/x" sails through checkAllowed (the host IS a valid
	// tailnet address) and only fails deep inside the HTTP transport with a
	// confusing "unsupported protocol scheme" error — a slow, indirect way to
	// reject something we can name outright, fast, with zero dial attempted.
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", refErr(key, rawURL, fmt.Errorf("scheme %q not allowed (want http or https)", u.Scheme))
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
		drainBody(resp) // let the transport reuse this connection instead of tearing it down
		return "", refErr(key, rawURL, fmt.Errorf("unexpected status %s", resp.Status))
	}

	capLimit := int64(capMB) << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, capLimit+1))
	if err != nil {
		return "", refErr(key, rawURL, fmt.Errorf("reading body: %w", err))
	}
	if int64(len(body)) > capLimit {
		drainBody(resp) // the LimitReader above only consumed capLimit+1 bytes; the rest is still on the wire
		return "", refErr(key, rawURL, fmt.Errorf("body exceeds the %d MiB cap", capMB))
	}

	ext, err := sniffImageExt(body)
	if err != nil {
		drainBody(resp) // body is already fully read here (ReadAll succeeded under the cap); a no-op, kept for consistency
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

// drainBody discards whatever is left of resp.Body, bounded, so the
// underlying connection can be returned to the client's pool instead of
// being torn down on Close (an unread/partially-read body forces Go's
// transport to close the connection rather than reuse it). The drain itself
// is capped — not unbounded — so an oversized or malicious body can't turn an
// error path into unbounded work.
func drainBody(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
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
	// extraAllow entries are DNS names (the CIDR/IP-literal path above is
	// already handled structurally and untouched here): DNS names are
	// case-insensitive (RFC 4343), so an operator-authored entry like
	// "MyHost:8080" must match a URL host that arrives as "myhost:8080" (or
	// vice versa). Lowercase both sides of the comparison; the port digits
	// are unaffected by case, so this keeps port matching exact.
	normalizedHostport := strings.ToLower(hostport)
	for _, a := range extraAllow {
		if strings.ToLower(a) == normalizedHostport {
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
