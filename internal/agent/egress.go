package agent

import (
	"fmt"
	"strings"
)

// Allowlist is the P3 egress host allowlist: a set of canonical hosts plus
// opt-in, boundary-anchored wildcard suffixes. It is built once and never
// mutated, so the policy broker that reads it stays deterministic. The zero
// value permits NOTHING (default-deny) — exactly how P0/P1 grant no network tool
// at all. The host-matching rule is the security boundary; see canonHost.
type Allowlist struct {
	exact  map[string]struct{} // canonical hosts, e.g. "example.com"
	suffix []string            // ".example.com" forms, from "*.example.com" entries
}

// canonHost lower-cases, strips one trailing FQDN-root dot, and enforces a strict
// ASCII letter/digit/hyphen/dot rule. Any host containing a byte outside that set
// — non-ASCII (homographs such as the Cyrillic "а" U+0430), zero-width/format
// characters, underscores, spaces, "@", ":" (so IPv6 literals and embedded ports),
// "/" — is REJECTED (fail-closed) rather than normalized. This is a
// dependency-free homograph / hidden-character defense: a deceptive host can never
// canonicalize ONTO a legitimate ASCII allowlist entry, it can only be rejected.
// (Trade-off vs. an IDN library: a genuine internationalized host must be entered
// in its punycode "xn--" form. Chosen over golang.org/x/net/idna to avoid a new
// module dependency and the Unicode-table drift that comes with it.)
func canonHost(h string) (string, error) {
	// NOTE: no strings.TrimSpace here — it strips Unicode whitespace (U+2028,
	// U+00A0, …), which would normalize a deceptive host like "allowed.com "
	// ONTO a legit entry while the dialer used the untrimmed form. Operator entries
	// are ASCII-trimmed in NewAllowlist; the checked host is matched as-is, so any
	// whitespace/control byte falls through to the non-LDH reject below.
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if h == "" {
		return "", fmt.Errorf("empty host")
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-' {
			continue
		}
		return "", fmt.Errorf("host %q contains a non-LDH/non-ASCII character", h)
	}
	return h, nil
}

// NewAllowlist parses bare-hostname entries and "*.host" wildcards into an
// Allowlist, rejecting any entry that carries a scheme, port, path, userinfo, or
// backslash (bare hostnames only — the Managed-Agents rule). Empty input yields
// the deny-all zero value. A "*." prefix becomes a boundary-anchored suffix; any
// other "*" (mid-label, bare) is rejected by canonHost.
func NewAllowlist(entries []string) (Allowlist, error) {
	a := Allowlist{exact: map[string]struct{}{}}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.ContainsAny(e, "/:@\\") {
			return Allowlist{}, fmt.Errorf("egress host %q must be a bare hostname (no scheme, port, path, or userinfo)", e)
		}
		if rest, ok := strings.CutPrefix(e, "*."); ok {
			c, err := canonHost(rest)
			if err != nil {
				return Allowlist{}, fmt.Errorf("bad wildcard %q: %w", e, err)
			}
			// Reject a wildcard on a single-label base (e.g. "*.com"): it would open
			// an entire TLD. Require at least one dot in the base. NOTE: not a full
			// public-suffix check — "*.co.uk" still passes (a PSL would need a dep);
			// this just blocks the obvious whole-TLD footgun.
			if !strings.Contains(c, ".") {
				return Allowlist{}, fmt.Errorf("wildcard %q is too broad (a single-label base would match an entire TLD); use *.yourdomain.tld", e)
			}
			a.suffix = append(a.suffix, "."+c)
			continue
		}
		c, err := canonHost(e)
		if err != nil {
			return Allowlist{}, fmt.Errorf("bad host %q: %w", e, err)
		}
		a.exact[c] = struct{}{}
	}
	return a, nil
}

// permits is the deterministic membership test the broker relies on. Exact
// canonical match, OR a boundary-anchored wildcard match — a "*.example.com"
// entry matches "a.example.com" but NEVER the apex "example.com" nor
// "evil-example.com" (the len(c) > len(s) guard requires a real label before the
// suffix). A host that fails canonicalization permits nothing. A bare entry is
// exact-only (no implicit subdomain inclusion): adding a host opens exactly that
// host, deliberately the opposite of web_fetch's auto-subdomain behavior.
func (a Allowlist) permits(host string) bool {
	c, err := canonHost(host)
	if err != nil {
		return false
	}
	if _, ok := a.exact[c]; ok {
		return true
	}
	for _, s := range a.suffix {
		if len(c) > len(s) && strings.HasSuffix(c, s) {
			return true
		}
	}
	return false
}
