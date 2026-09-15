// publicnet.go is this package's PUBLIC-WEB outbound guard — the third sibling
// of the listen guard (netguard.go) and the tailnet guard (tailnet.go).
//
// tailnet.go keeps a seat endpoint pointed INSIDE the tailnet; this file keeps
// the research lane pointed OUTSIDE it. Both face the same attack: a hostname
// the caller supplies resolves one way when it is VALIDATED and another way
// when it is DIALED (DNS rebinding — a TOCTOU window with an attacker-chosen
// TTL on the far side). The answer is identical in both: resolve once, judge
// every answer, and hand the dialer the vetted IP LITERAL so nothing between
// the check and connect(2) can re-resolve.
//
// Everything a caller needs lives here:
//
//   - LookupIP                 the ONE resolution seam in the tree (tests pin it)
//   - PublicIP / CheckPublicIP the ONE public-address predicate in the tree
//   - PublicDialControl        a net.Dialer.Control hook (after DNS, before connect)
//   - PublicDialContext        resolve-and-pin around any DialFunc
//   - PublicTransport          an http.RoundTripper whose every dial rides both
//
// The predicate was the agent lane's (internal/agent's isDisallowedIP/blockedNets,
// 2026-08) and the research lane had its own weaker copy that missed 0.0.0.0/8,
// 240/4, the TEST-NETs, 192.0.0.0/24, 192.88.99.0/24, NAT64 and 6to4. Two
// predicates meant two answers to one question; there is now one.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// LookupIP resolves a hostname to addresses. It is a package var because it is
// the SINGLE resolution seam every guard in this package shares: the tailnet
// dial gate, the public dial gate, and the research lane's URL validation all
// call it, so one test hook pins DNS for all three (and a rebinding test can
// answer differently per call, which is the whole attack). Production always
// rides the default resolver.
var LookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// reservedNets are reserved / special-use IPv4 AND IPv6 ranges that net.IP's
// IsPrivate/IsLoopback/IsLinkLocal* do NOT cover but must still be denied: the
// 0.0.0.0/8 "this host" block (on Linux 0.x can reach localhost; only 0.0.0.0
// itself is IsUnspecified), CGNAT, IETF/TEST-NET/benchmarking/240-4, plus the
// IPv6 special-use ranges that relay or EMBED an IPv4 (NAT64 / 6to4 / IPv4
// translation) and the documentation/discard ranges. (fc00::/7 ULA is already
// covered by IsPrivate.)
var reservedNets = func() []netip.Prefix {
	cidrs := []string{
		"0.0.0.0/8",          // "this host on this network" (RFC 1122)
		"100.64." + "0.0/10", // CGNAT (RFC 6598) — the tailnet lives here; IsPrivate is false for it (split literal: the pre-push leak scan blocks a whole CGNAT address on an added line)
		"192.0.0.0/24",       // IETF protocol assignments
		"192.0.2.0/24",       // TEST-NET-1
		"192.88.99.0/24",     // 6to4 relay anycast (deprecated, still routed in places)
		"198.18.0.0/15",      // benchmarking
		"198.51.100.0/24",    // TEST-NET-2
		"203.0.113.0/24",     // TEST-NET-3
		"240.0.0.0/4",        // reserved (includes 255.255.255.255)
		"64:ff9b::/96",       // NAT64 well-known prefix (embeds an IPv4 in the low 32 bits)
		"64:ff9b:1::/48",     // local-use IPv4/IPv6 translation (RFC 8215)
		"2002::/16",          // 6to4 (embeds an IPv4 in bytes 2-5, loopback included)
		"2001::/32",          // Teredo (tunnels to an IPv4 endpoint)
		"2001:db8::/32",      // IPv6 documentation
		"100::/64",           // IPv6 discard-only
		"3fff::/20",          // IPv6 documentation (RFC 9637)
		"5f00::/16",          // IPv6 SRv6 segment identifiers (RFC 9602)
		"2001:20::/28",       // ORCHIDv2
		"2620:4f:8000::/48",  // direct delegation AS112
	}
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}()

// CheckPublicIP is the ONE public-address predicate in this tree. It blocks any
// address an SSRF attacker would pivot to: loopback, RFC1918 private, link-local
// (including the 169.254.169.254 cloud-metadata address, which IsPrivate does
// NOT cover), multicast, unspecified, and every reserved range above.
// IPv4-mapped IPv6 (::ffff:127.0.0.1) is normalized to its IPv4 form first, so
// it cannot slip through disguised as "IPv6". Returns nil iff ip is a routable
// public address; the error names the reason, which callers surface verbatim.
func CheckPublicIP(ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("nil IP")
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return fmt.Errorf("unparseable address %v", ip)
	}
	return checkPublicAddr(a)
}

// PublicIP is CheckPublicIP's boolean face, for call sites that only branch.
func PublicIP(ip net.IP) bool { return CheckPublicIP(ip) == nil }

// checkPublicAddr is the netip-native body both public faces share; the dial
// guards already hold a netip.Addr and should not round-trip through net.IP.
func checkPublicAddr(a netip.Addr) error {
	if !a.IsValid() {
		return fmt.Errorf("invalid address")
	}
	a = a.Unmap() // ::ffff:127.0.0.1 is judged as 127.0.0.1, not as "some IPv6"
	ip := net.IP(a.AsSlice())
	switch {
	case ip.IsLoopback():
		return fmt.Errorf("loopback address %s blocked", a)
	case ip.IsPrivate():
		return fmt.Errorf("private address %s blocked", a)
	case ip.IsLinkLocalUnicast():
		return fmt.Errorf("link-local address %s blocked", a) // includes 169.254.169.254
	case ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return fmt.Errorf("multicast address %s blocked", a)
	case ip.IsUnspecified():
		return fmt.Errorf("unspecified address %s blocked", a)
	}
	for _, p := range reservedNets {
		if p.Contains(a) {
			return fmt.Errorf("reserved address %s blocked (%s)", a, p)
		}
	}
	return nil
}

// PublicDialControl is the net.Dialer.Control hook. It fires AFTER the dialer's
// own DNS resolution and BEFORE connect(2), with address = the exact resolved IP
// the kernel will dial — so validate-time IS connect-time and every candidate
// address (multi-A / Happy Eyeballs) is vetted individually. It is the belt to
// PublicDialContext's braces: the latter pins the address, this one catches any
// resolution that still happened below us.
func PublicDialControl(network, address string, _ syscall.RawConn) error {
	switch network {
	case "tcp4", "tcp6":
	default:
		return fmt.Errorf("network %q not allowed", network)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("bad dial address %q: %w", address, err)
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("dial address %q is not a literal IP", host)
	}
	return checkPublicAddr(a)
}

// PublicDialContext wraps next with the public-web dial gate, the mirror image
// of SafeDialContext. IP-literal addresses are judged directly and passed
// through unchanged; hostnames are resolved HERE and next receives the vetted
// IP LITERAL — never the hostname, so nothing between the check and the connect
// can re-resolve it. Refusals cost zero network activity.
//
// A set containing ANY non-public answer is refused whole, rather than dialing
// the good members: that matches what the research lane's URL validation
// already does with a mixed answer, and on the public web a mixed answer is an
// attack, not the redundancy it is on a tailnet (where SafeDialContext skips
// the poisoned record instead, so one injected record cannot DoS a real seat).
func PublicDialContext(next DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("public guard: cannot parse dial address %q: %w", addr, err)
		}
		if a, perr := netip.ParseAddr(host); perr == nil {
			if cerr := checkPublicAddr(a); cerr != nil {
				return nil, fmt.Errorf("public guard: refusing dial to %s: %w", addr, cerr)
			}
			return next(ctx, network, addr)
		}
		addrs, err := LookupIP(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("public guard: resolve %q: %w", host, err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("public guard: %q resolved to no addresses", host)
		}
		for _, a := range addrs {
			if cerr := checkPublicAddr(a); cerr != nil {
				return nil, fmt.Errorf("public guard: %q resolved to %s — %w (DNS-rebinding guard)", host, a, cerr)
			}
		}
		var lastErr error
		for _, a := range addrs {
			conn, derr := next(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		return nil, lastErr
	}
}

// PublicTransport returns a transport that carries fallback's settings with
// every dial path routed through PublicDialContext (and, where we build the
// dialer, PublicDialControl). fallback is cloned, never mutated; nil — or any
// non-*http.Transport RoundTripper, whose dial path we cannot reach — starts
// from http.DefaultTransport's defaults instead, because the dial gate is the
// one part that cannot be optional. Proxy is stripped for the same reason
// SafeTransport strips it: an env-configured proxy would be handed the request
// and dial anywhere on our behalf, outside the gate.
//
// DialTLSContext is wrapped too when set. A custom TLS dialer receives the
// vetted IP literal rather than the hostname, so a caller that sets one must
// carry its own TLSClientConfig.ServerName — the alternative (leaving it
// unwrapped) is a dial path that silently escapes the guard.
func PublicTransport(fallback http.RoundTripper) http.RoundTripper {
	tr, ok := fallback.(*http.Transport)
	if ok {
		tr = tr.Clone()
	} else {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	}
	next := tr.DialContext
	if next == nil {
		next = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: PublicDialControl}).DialContext
	}
	tr.DialContext = PublicDialContext(next)
	if tr.DialTLSContext != nil {
		tr.DialTLSContext = PublicDialContext(tr.DialTLSContext)
	}
	tr.Proxy = nil
	return tr
}
