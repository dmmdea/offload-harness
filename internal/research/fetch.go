// Package research is the harness's one-call research lane: fetch public web
// pages DELEGATOR-side, strip them to text, and fan the digests out to the
// free local seats through the ordinary delegation path.
//
// Why it exists (2026-08-30, operator directive): every "web research" leg was
// being handed to a cloud subagent because the seats have no network, while the
// digest work — the expensive part — is exactly what the seats do all day. The
// fetch here is the same act as the operator's own WebFetch: the CALLER names
// the URLs, the harness reads them, and the seats only ever see inline text.
// The seats never gain network access; the agent loop's egress cage is untouched.
package research

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// Limits. MaxFetchBytes bounds what is read off the wire (a page past it is
// truncated and flagged, never refused); TextCap bounds what a seat is handed
// (well under the 128 KiB per-doc contract cap so the goal and schema fit).
const (
	MaxFetchBytes  = 2 << 20
	TextCap        = 96 << 10
	DefaultTimeout = 30 * time.Second
	MaxRedirects   = 5
	defaultUA      = "offload-harness-research/1.0 (+https://github.com/dmmdea/offload-harness)"
	maxURLsPerCall = 12
	MaxURLsPerCall = maxURLsPerCall
)

// Fetched is one URL's outcome. Err is set (and Text empty) when the page could
// not be used; the caller reports it as a skipped source, never as a digest.
type Fetched struct {
	URL         string        `json:"url"`
	FinalURL    string        `json:"final_url,omitempty"`
	Title       string        `json:"title,omitempty"`
	Status      int           `json:"status,omitempty"`
	ContentType string        `json:"content_type,omitempty"`
	Bytes       int           `json:"bytes"`
	TextBytes   int           `json:"text_bytes"`
	Truncated   bool          `json:"truncated,omitempty"`
	Err         string        `json:"error,omitempty"`
	Elapsed     time.Duration `json:"-"`
	Text        string        `json:"-"`
}

// Options tunes one Fetch. Zero values take the package defaults.
type Options struct {
	Timeout   time.Duration
	MaxBytes  int
	UserAgent string
	// Client overrides the HTTP client (tests). Its Transport is used as-is;
	// the redirect policy is always ours.
	Client *http.Client
}

// ValidateURL is the SSRF guard: only http(s), only public hosts. Loopback,
// private (RFC 1918 / ULA), link-local, `.local`, `localhost`, and the
// operator's tailnet zone are refused — the research lane must never become a
// way to read a fleet node's admin endpoints or a box's local services through
// the harness. DNS is resolved here and every returned address is checked, so a
// public name that resolves to a private address is refused too.
func ValidateURL(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("bad url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme %q not allowed (http/https only)", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, errors.New("url has no host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, fmt.Errorf("host %q is not public", host)
	}
	if suffix := netguard.TailnetSuffix(); suffix != "" && (host == suffix || strings.HasSuffix(host, "."+suffix)) {
		return nil, fmt.Errorf("host %q is on the tailnet — the research lane reads the public web only", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !publicIP(ip) {
			return nil, fmt.Errorf("address %s is not public", ip)
		}
		return u, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolving %s: no addresses", host)
	}
	for _, a := range addrs {
		if !publicIP(a.IP) {
			return nil, fmt.Errorf("host %s resolves to non-public address %s", host, a.IP)
		}
	}
	return u, nil
}

func publicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	// Tailscale CGNAT range (100.64.0.0/10) — MagicDNS peers and fleet nodes.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	return true
}

// Fetch reads one page under the guard and returns it stripped to text. It
// never panics and never returns an error value — failures live in Err so a
// batch keeps its shape (one row per requested URL).
func Fetch(ctx context.Context, raw string, opt Options) Fetched {
	out := Fetched{URL: raw}
	start := time.Now()
	defer func() { out.Elapsed = time.Since(start) }()

	if _, err := ValidateURL(ctx, raw); err != nil {
		out.Err = err.Error()
		return out
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxBytes := opt.MaxBytes
	if maxBytes <= 0 {
		maxBytes = MaxFetchBytes
	}
	ua := opt.UserAgent
	if ua == "" {
		ua = defaultUA
	}
	client := opt.Client
	if client == nil {
		client = &http.Client{}
	}
	// Copy so the caller's client is never mutated; redirects re-run the guard
	// (an open redirect to a private address is the classic bypass).
	c := *client
	c.Timeout = timeout
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= MaxRedirects {
			return fmt.Errorf("stopped after %d redirects", MaxRedirects)
		}
		if _, err := ValidateURL(req.Context(), req.URL.String()); err != nil {
			return fmt.Errorf("redirect refused: %w", err)
		}
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, raw, nil)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,text/markdown;q=0.9,application/json;q=0.8,*/*;q=0.5")
	resp, err := c.Do(req)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	defer resp.Body.Close()
	out.Status = resp.StatusCode
	out.FinalURL = resp.Request.URL.String()
	out.ContentType = resp.Header.Get("Content-Type")
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		out.Err = fmt.Sprintf("http %d", resp.StatusCode)
		return out
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		out.Err = "reading body: " + err.Error()
		return out
	}
	if len(body) > maxBytes {
		body = body[:maxBytes]
		out.Truncated = true
	}
	out.Bytes = len(body)
	ct := strings.ToLower(out.ContentType)
	var text string
	switch {
	case strings.Contains(ct, "html") || (ct == "" && looksLikeHTML(body)):
		out.Title, text = HTMLToText(string(body))
	case strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml") || ct == "":
		text = normalizeWhitespace(string(body))
	default:
		out.Err = fmt.Sprintf("unsupported content type %q (text/html, text/*, json only)", out.ContentType)
		return out
	}
	if len(text) > TextCap {
		text = text[:TextCap]
		out.Truncated = true
	}
	text = strings.TrimSpace(text)
	if text == "" {
		out.Err = "page stripped to empty text"
		return out
	}
	out.Text = text
	out.TextBytes = len(text)
	return out
}

func looksLikeHTML(b []byte) bool {
	head := strings.ToLower(string(b[:min(len(b), 512)]))
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html") || strings.Contains(head, "<body")
}

var (
	reDropBlocks = regexp.MustCompile(`(?is)<(script|style|noscript|svg|template|iframe)\b[^>]*>.*?</\s*(script|style|noscript|svg|template|iframe)\s*>`)
	reDropChrome = regexp.MustCompile(`(?is)<(nav|footer|header|aside)\b[^>]*>.*?</\s*(nav|footer|header|aside)\s*>`)
	reComments   = regexp.MustCompile(`(?s)<!--.*?-->`)
	reTitle      = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reBlockTag   = regexp.MustCompile(`(?i)</?(p|div|br|li|ul|ol|h[1-6]|pre|tr|td|th|table|section|article|blockquote|dd|dt|dl|hr|figure|figcaption|main)\b[^>]*>`)
	reAnyTag     = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpaces     = regexp.MustCompile(`[ \t\r\f\v\x{00a0}]+`)
	reBlankLines = regexp.MustCompile(`\n\s*\n+`)
	// Inline tags become a space so words never glue together; that leaves a
	// stray space before punctuation ("/dev/shm ."), which this closes.
	rePunctGap = regexp.MustCompile(` +([.,;:!?)\]])`)
)

// HTMLToText is a deterministic, dependency-free stripper: drops scripts,
// styles and page chrome, turns block boundaries into newlines, removes the
// remaining tags, unescapes entities, and collapses whitespace. It is meant to
// hand a seat readable prose and code blocks, not to render a page.
func HTMLToText(src string) (title, text string) {
	if m := reTitle.FindStringSubmatch(src); m != nil {
		title = strings.TrimSpace(html.UnescapeString(reAnyTag.ReplaceAllString(m[1], "")))
	}
	s := reComments.ReplaceAllString(src, "")
	s = reDropBlocks.ReplaceAllString(s, "")
	s = reDropChrome.ReplaceAllString(s, "")
	s = reBlockTag.ReplaceAllString(s, "\n")
	s = reAnyTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return title, normalizeWhitespace(s)
}

func normalizeWhitespace(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = reSpaces.ReplaceAllString(s, " ")
	s = rePunctGap.ReplaceAllString(s, "$1")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
