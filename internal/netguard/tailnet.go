// tailnet.go is this package's OUTBOUND guard — the listen guard's mirror
// image. netguard.Validate keeps unauthenticated servers from binding beyond
// loopback; TailnetURL + SafeTransport keep a remote model seat (config
// seat_endpoints, the delegation lanes) from ever POINTING beyond loopback or
// the operator's tailnet. The threat is a config edit — fat-fingered or
// hostile — that aims a "local" seat at the public internet, which would break
// the never-cloud rule (ADR 0001) silently: the completion still succeeds,
// just against a server the operator never chose.
//
// The guard is TWO layers, because URL-shape checks and DNS answers fail
// independently:
//
//  1. TailnetURL (config-load time): pure shape check, no DNS. Only loopback,
//     100.64.0.0/10 literals, dotless MagicDNS short names, and hostnames
//     under a configured tailnet zone (the operator's own, and the zone of any
//     other tailnet that shared a node in) pass.
//  2. SafeTransport / SafeDialContext (dial time, EVERY dial): a hostname
//     that passed the shape check is resolved here, any answer outside
//     loopback/CGNAT is refused, and the connection goes to the vetted IP
//     LITERAL — never back through the name. ingress.go bans hostnames
//     outright to avoid resolve-then-redial rebinding; seat endpoints exist
//     to name MagicDNS hosts, so this guard resolves-and-PINS instead of
//     banning. The check runs per dial, so a DNS answer that drifts to a
//     public address after validation (classic rebinding) dies at the socket,
//     not in a comment.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
)

// tailnetCGNAT is the Tailscale-assigned CGNAT block (100.64.0.0/10) every
// tailnet peer lives in. Mirrors internal/fleetnode's ingress allowlist —
// duplicated rather than shared because fleetnode already imports netguard;
// an import back the other way would cycle.
var tailnetCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// houseTailnetZones are the operator's tailnet DNS zones, e.g. "tailnnnnnn.ts.net":
// the operator's OWN zone first, then any zone of ANOTHER tailnet that shared a node
// in (Tailscale names a shared node under the SHARER's zone, and only by that name).
// Only hostnames under a listed zone pass validation: a generic ".ts.net" rule would
// accept ANY tailnet's Funnel-published hostname — a public-internet endpoint wearing a
// tailnet-looking name. An explicit second zone is not that rule: the operator names it
// (ADR 0074), and the dial gate below still refuses any name that resolves outside
// loopback and the tailnet CGNAT range.
//
// It is CONFIGURED, never compiled in: this is a public repo and one operator's
// tailnet zone is both private to them and wrong for everybody else. The zero
// value is the empty list and that is FAIL-CLOSED — with no zone set the
// suffix branch is skipped entirely, so the admitted set is strictly NARROWER
// than a hardcoded default (loopback, 100.64.0.0/10 literals and dotless
// MagicDNS names still pass; a dotted tailnet FQDN does not). Set it from the
// config keys `tailnet_suffix` and `tailnet_suffixes` via SetTailnetSuffixes.
//
// Atomic because every config load installs it and two loads can run at once in one
// process (a server re-reading its config while a handler loads it; in-process CLI
// tests); a plain string was a data race the Linux race gate caught on 0.161.0. The list
// is replaced whole and never mutated, so a reader holds a consistent snapshot.
var houseTailnetZones atomic.Pointer[[]string]

// ParseZone normalizes one tailnet DNS zone the way TailnetURL normalizes a host —
// lowercased, trailing root dot and a leading dot removed — so "…ts.net.", ".…ts.net"
// and "…TS.NET" all behave identically instead of silently never matching. "" (or only
// whitespace and dots) is an unset slot and returns "" with no error. A value that is
// not a plausible DNS zone is REFUSED, naming label (the config key and index it came
// from), because a typo here fails open in the reader's mind ("I set it, so my host is
// allowed") while failing closed in the gate.
//
// A zone is dot-separated labels of [a-z0-9-] only: a wildcard ("*.x.ts.net"), a stray
// query or backslash ("x.ts.net?", "x.ts.net\") or an empty label is not a zone, and a
// value that is merely not matched by anything is exactly the typo this refuses. The bare
// "ts.net" is refused by name: as a zone it is every tailnet's, Funnel-published public
// hostnames included, which is the generic rule the comment on houseTailnetZones rules out.
func ParseZone(label, s string) (string, error) {
	n := strings.ToLower(strings.Trim(strings.TrimSpace(s), "."))
	if n == "" {
		return "", nil
	}
	if n == genericTailnetDomain {
		return "", fmt.Errorf("%s %q is the generic tailnet domain, not a zone: it would admit every tailnet's hostnames, Funnel-published public ones included; name your tailnet's own zone (something like tailnnnnnn.ts.net)", label, s)
	}
	if !strings.Contains(n, ".") || !zoneLabelsOK(n) {
		return "", fmt.Errorf("%s %q is not a DNS zone (want something like tailnnnnnn.ts.net)", label, s)
	}
	return n, nil
}

// genericTailnetDomain is the parent of every tailnet zone. It is never a valid zone itself.
const genericTailnetDomain = "ts.net"

// zoneLabelsOK reports whether every dot-separated label of an already lowercased zone is
// non-empty and made only of [a-z0-9-].
func zoneLabelsOK(zone string) bool {
	for _, label := range strings.Split(zone, ".") {
		if label == "" {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// SetTailnetSuffix installs ONE tailnet DNS zone, replacing whatever was installed.
// Empty clears it (back to fail-closed). It is SetTailnetSuffixes for the original
// single key, kept so every caller of the one-zone form is unchanged.
func SetTailnetSuffix(s string) error {
	n, err := ParseZone("tailnet_suffix", s)
	if err != nil {
		return err
	}
	if n == "" {
		return SetTailnetSuffixes(nil)
	}
	return SetTailnetSuffixes([]string{n})
}

// SetTailnetSuffixes installs the operator's tailnet zones, in the order given (the
// first is the primary one TailnetSuffix reports). Each entry is normalized by
// ParseZone; blank entries are unset slots and duplicates collapse. The list is
// installed all-or-nothing: one malformed entry refuses the call and leaves the
// previous list in force. nil or an empty list clears it (fail-closed).
func SetTailnetSuffixes(zones []string) error {
	out := make([]string, 0, len(zones))
	seen := map[string]bool{}
	for i, z := range zones {
		n, err := ParseZone(fmt.Sprintf("tailnet_suffixes[%d]", i), z)
		if err != nil {
			return err
		}
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	houseTailnetZones.Store(&out)
	return nil
}

// TailnetSuffix reports the primary configured zone (normalized, "" when unset): the
// first one installed. A caller that must consider EVERY zone uses TailnetSuffixes,
// InTailnetZone or TailnetURL; this exists for the places that name one.
func TailnetSuffix() string {
	if zs := TailnetSuffixes(); len(zs) > 0 {
		return zs[0]
	}
	return ""
}

// TailnetSuffixes reports every configured zone (normalized, nil when unset). The slice
// is the caller's own copy.
func TailnetSuffixes() []string {
	p := houseTailnetZones.Load()
	if p == nil || len(*p) == 0 {
		return nil
	}
	return append([]string(nil), (*p)...)
}

// InTailnetZone reports whether host is a configured zone or sits under one of them.
// It is the NAME test for the callers that must treat the tailnet as a class — the
// research lane refuses to read it as web — and judges every installed zone, not only
// the primary one. host is matched the way TailnetURL matches: case-insensitive, with
// any trailing root dot ignored.
func InTailnetZone(host string) bool {
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, z := range TailnetSuffixes() {
		if name == z || strings.HasSuffix(name, "."+z) {
			return true
		}
	}
	return false
}

// TailnetURL vets a remote seat endpoint's base URL at CONFIG time, no DNS
// touched. Allowed: http(s) with a loopback or 100.64.0.0/10 IP-literal host,
// a dotless hostname (a MagicDNS short name — its resolution is enforced at
// dial time by SafeDialContext, which this check alone cannot do), or a
// hostname under one of the installed tailnet zones. Everything else — public
// FQDNs, public or LAN IP literals, generic .ts.net hosts, non-http schemes —
// fails with an error the config loader can attribute to its key.
func TailnetURL(raw string) error {
	return TailnetURLIn(TailnetSuffixes(), raw)
}

// TailnetURLIn is TailnetURL judged against the zones it is GIVEN rather than the
// installed ones. It exists for a caller that must reach the same verdict a loaded
// config will reach without depending on which config this process loaded last
// (the doctor's roster finding), and it is the one body TailnetURL runs, so the two
// can never disagree about a shape. zones are expected normalized (ParseZone).
func TailnetURLIn(zones []string, raw string) error {
	// Every message below names the value through shown, never raw: a roster entry is a base URL,
	// but nothing stops one being pasted with a token in its query string or user:password in
	// front of its host, and a refusal is printed by doctor, the lanes' "probed ..." lines and the
	// config loader, all of which get shared. The verdict is judged on raw; only the TEXT is redacted.
	shown := RedactBase(raw)
	u, err := url.Parse(raw)
	if err != nil {
		// parseFailure, not %w: a url.Error quotes the whole input, credentials and all.
		return fmt.Errorf("invalid URL %q: %s", shown, parseFailure(err))
	}
	// Scheme first, like ingress.go's fetchOne: a tailnet host on a bizarre
	// scheme should be named as a scheme problem, not sneak past a host check.
	if u.Scheme != "http" && u.Scheme != "https" {
		scheme := u.Scheme
		if strings.Contains(u.Opaque, "@") {
			scheme = "?" // "user:pw@node" reads as scheme "user": that is a username, not a scheme
		}
		return fmt.Errorf("scheme %q not allowed in %q (want http or https)", scheme, shown)
	}
	host := u.Hostname() // strips any port and IPv6 brackets
	if host == "" {
		return fmt.Errorf("URL %q has no host", shown)
	}
	if addr, perr := netip.ParseAddr(host); perr == nil {
		if tailnetAddrAllowed(addr) {
			return nil
		}
		return fmt.Errorf("IP literal %s in %q is outside the tailnet (need loopback or the tailnet CGNAT range)", host, shown)
	}
	// DNS names are case-insensitive (RFC 4343) and may carry a trailing
	// root dot; normalize both before the suffix/dot checks.
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	// zones is read once by the caller: the checks and the message judge one list.
	for _, zone := range zones {
		if zone != "" && strings.HasSuffix(name, "."+zone) {
			return nil
		}
	}
	if !strings.Contains(name, ".") {
		// Dotless = a search-domain-resolved short name. Shape alone cannot
		// prove where it resolves — SafeDialContext closes that at dial time.
		return nil
	}
	switch len(zones) {
	case 0:
		return fmt.Errorf("hostname %q in %q not allowed (need loopback, a tailnet CGNAT-range address literal, or a dotless MagicDNS name; set config `tailnet_suffix` to admit your own tailnet zone, and `tailnet_suffixes` for a node shared in from another tailnet)", host, shown)
	case 1:
		return fmt.Errorf("hostname %q in %q not allowed (need loopback, a tailnet CGNAT-range address literal, a dotless MagicDNS name, or a host under %s)",
			host, shown, zones[0])
	}
	return fmt.Errorf("hostname %q in %q not allowed (need loopback, a tailnet CGNAT-range address literal, a dotless MagicDNS name, or a host under one of %s)",
		host, shown, strings.Join(zones, ", "))
}

// tailnetAddrAllowed reports whether one literal/resolved address is inside
// the boundary delegation may reach: loopback or the tailnet CGNAT range.
// Unmap() first so an IPv4-mapped IPv6 form (::ffff: followed by the tailnet address) is judged as
// its IPv4 self — netip.Prefix.Contains deliberately never matches 4-in-6
// against a v4 prefix, which would refuse legitimate mapped answers some
// resolvers return.
func tailnetAddrAllowed(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || tailnetCGNAT.Contains(a)
}

// DialFunc matches net.Dialer.DialContext's shape.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// SafeDialContext wraps next with the tailnet dial gate. IP-literal addresses
// are judged directly and passed through unchanged; hostnames are resolved
// HERE, every off-tailnet answer is discarded, and next receives the vetted
// IP literal — never the hostname, so nothing between check and connect can
// re-resolve to somewhere else. Refusals cost zero network activity.
func SafeDialContext(next DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("tailnet guard: cannot parse dial address %q: %w", addr, err)
		}
		if a, perr := netip.ParseAddr(host); perr == nil {
			if !tailnetAddrAllowed(a) {
				return nil, fmt.Errorf("tailnet guard: refusing dial to %s (not loopback or 100.64.0.0/10)", addr)
			}
			return next(ctx, network, addr)
		}
		addrs, err := LookupIP(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("tailnet guard: resolve %q: %w", host, err)
		}
		var lastErr error
		dialedAny := false
		for _, a := range addrs {
			if !tailnetAddrAllowed(a) {
				// A poisoned answer beside a genuine one must simply never be
				// dialed — refusing the whole set would let an attacker DoS a
				// valid seat by injecting one bad record.
				continue
			}
			dialedAny = true
			conn, derr := next(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if !dialedAny {
			return nil, fmt.Errorf("tailnet guard: %q resolved to %v — no loopback or 100.64.0.0/10 address, refusing (DNS-rebinding guard)", host, addrs)
		}
		return nil, lastErr
	}
}

// SafeTransport returns a transport that carries fallback's settings with its
// DialContext wrapped by SafeDialContext. fallback is cloned, never mutated;
// nil (or any non-*http.Transport RoundTripper, whose dial path we cannot
// reach) starts from http.DefaultTransport's defaults instead — the dial gate
// is the one part that cannot be optional. Proxy is stripped for the same
// reason ingress.go's client strips it: an env-configured proxy would be
// handed the request and dial anywhere on our behalf, outside the gate.
func SafeTransport(fallback http.RoundTripper) http.RoundTripper {
	tr, ok := fallback.(*http.Transport)
	if ok {
		tr = tr.Clone()
	} else {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	}
	next := tr.DialContext
	if next == nil {
		next = (&net.Dialer{}).DialContext
	}
	tr.DialContext = SafeDialContext(next)
	tr.Proxy = nil
	return tr
}
