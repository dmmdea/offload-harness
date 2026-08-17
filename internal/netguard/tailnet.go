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
//     under the HOUSE tailnet suffix pass.
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
)

// tailnetCGNAT is the Tailscale-assigned CGNAT block (100.64.0.0/10) every
// tailnet peer lives in. Mirrors internal/fleetnode's ingress allowlist —
// duplicated rather than shared because fleetnode already imports netguard;
// an import back the other way would cycle.
var tailnetCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// houseTailnetSuffix is the operator's OWN tailnet DNS zone. Only hostnames
// under this suffix pass validation: a generic ".ts.net" rule would accept
// ANY tailnet's Funnel-published hostname — a public-internet endpoint
// wearing a tailnet-looking name.
const houseTailnetSuffix = "tail38a707.ts.net"

// TailnetURL vets a remote seat endpoint's base URL at CONFIG time, no DNS
// touched. Allowed: http(s) with a loopback or 100.64.0.0/10 IP-literal host,
// a dotless hostname (a MagicDNS short name — its resolution is enforced at
// dial time by SafeDialContext, which this check alone cannot do), or a
// hostname under houseTailnetSuffix. Everything else — public FQDNs, public
// or LAN IP literals, generic .ts.net hosts, non-http schemes — fails with an
// error the config loader can attribute to its key.
func TailnetURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	// Scheme first, like ingress.go's fetchOne: a tailnet host on a bizarre
	// scheme should be named as a scheme problem, not sneak past a host check.
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed in %q (want http or https)", u.Scheme, raw)
	}
	host := u.Hostname() // strips any port and IPv6 brackets
	if host == "" {
		return fmt.Errorf("URL %q has no host", raw)
	}
	if addr, perr := netip.ParseAddr(host); perr == nil {
		if tailnetAddrAllowed(addr) {
			return nil
		}
		return fmt.Errorf("IP literal %s in %q is outside the tailnet (need loopback or 100.64.0.0/10)", host, raw)
	}
	// DNS names are case-insensitive (RFC 4343) and may carry a trailing
	// root dot; normalize both before the suffix/dot checks.
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if strings.HasSuffix(name, "."+houseTailnetSuffix) {
		return nil
	}
	if !strings.Contains(name, ".") {
		// Dotless = a search-domain-resolved short name. Shape alone cannot
		// prove where it resolves — SafeDialContext closes that at dial time.
		return nil
	}
	return fmt.Errorf("hostname %q in %q not allowed (need loopback, a 100.64.0.0/10 literal, a dotless MagicDNS name, or a host under %s)",
		host, raw, houseTailnetSuffix)
}

// tailnetAddrAllowed reports whether one literal/resolved address is inside
// the boundary delegation may reach: loopback or the tailnet CGNAT range.
// Unmap() first so an IPv4-mapped IPv6 form (::ffff:100.77.1.9) is judged as
// its IPv4 self — netip.Prefix.Contains deliberately never matches 4-in-6
// against a v4 prefix, which would refuse legitimate mapped answers some
// resolvers return.
func tailnetAddrAllowed(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || tailnetCGNAT.Contains(a)
}

// lookupNetIP resolves a hostname at DIAL time. A package var so the
// rebinding tests can pin DNS answers; production always rides the default
// resolver.
var lookupNetIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
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
		addrs, err := lookupNetIP(ctx, host)
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
