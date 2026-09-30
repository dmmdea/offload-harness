// Package netguard holds the shared loopback listen-address guard.
//
// Extracted 2026-07-17 from cmd/local-agent (validateListenAddr) for
// fleet-serve; behavior identical. Both unauthenticated HTTP servers
// (local-agent's OpenAI shim and fleet-serve) refuse to bind beyond loopback
// unless the operator explicitly opts into a trusted network.
package netguard

import (
	"fmt"
	"net"
	"strings"
)

// Validate refuses any listen host that is not loopback. The servers using it
// are UNAUTHENTICATED by design, so binding beyond loopback exposes an
// RCE-class surface to the local network — a footgun for anyone publishing
// this repo.
//
// Empty-host forms ("", ":18800") are treated as non-loopback: Go's net.Listen
// binds ALL interfaces when the host is empty, so they are exactly as exposed as
// "0.0.0.0" and must be refused too. Bracketed IPv6 ("[::1]") is handled by
// net.SplitHostPort, which strips the brackets; we also accept a bare "[::1]"
// host defensively in case brackets survive.
//
// allowNonLocal (from --listen-trusted-network) permits ONE specific
// non-loopback address — the tailnet address — and nothing wider: an
// all-interfaces address (an empty host such as ":18811", 0.0.0.0, ::) is
// refused even with the override, and so is an address that does not parse.
// Until 0.144.1 the override returned before any check, so a unit whose
// `--listen "$(tailscale ip -4)":18811` ran before tailscaled had an address
// expanded to ":18811" and served the unauthenticated endpoints on every
// interface instead of failing — and the unit's Restart=on-failure, written
// for exactly that boot race, never fired because nothing failed. The loud
// warning is the caller's responsibility, not the validator's — this stays a
// pure, side-effect-free function so it is trivial to test.
func Validate(addr string, allowNonLocal bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Missing port / malformed — we cannot prove what it binds, so refuse.
		return fmt.Errorf("cannot parse --listen %q: %w", addr, err)
	}
	if AllInterfaces(addr) {
		return fmt.Errorf("refusing to bind --listen %q: that address binds EVERY interface, and "+
			"--listen-trusted-network permits one specific address (the tailnet address), never all of them. "+
			"An empty host usually means `tailscale ip -4` printed nothing because tailscaled had no address "+
			"yet: fail now and let the service manager retry", addr)
	}
	if allowNonLocal || isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("refusing to bind --listen %q: this endpoint is UNAUTHENTICATED and "+
		"each request drives the agent's write/GitHub tools; binding beyond loopback exposes "+
		"an RCE-class surface to your network. Use a loopback address (127.0.0.1, [::1], localhost) "+
		"or pass --listen-trusted-network to override (only on a network you fully trust)", addr)
}

// LoopbackAddr reports whether a "host:port" listen address is bound to
// loopback — the same notion Validate enforces, exported as a FACT for
// callers that need the answer rather than the refusal (fleet-serve's
// agent-lane auth gate: a tokenless agent lane is acceptable only on a
// loopback listener). Malformed/unprovable addresses report false — a
// listener we cannot prove loopback must be treated as exposed, so the
// caller fails closed.
func LoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

// AllInterfaces reports whether a "host:port" listen address binds every
// interface: an empty host (":18811" — Go's net.Listen binds all interfaces
// for it) or an unspecified IP (0.0.0.0, ::, and their spellings). A malformed
// address reports false; Validate refuses it on its own.
func AllInterfaces(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsUnspecified()
	}
	return false
}

// isLoopbackHost reports whether host is a loopback address the guard permits.
// "localhost" is accepted by name (it resolves to loopback); an empty host is
// NOT loopback (it binds all interfaces). Numeric hosts are checked via
// net.IP.IsLoopback so any 127.0.0.0/8 address and ::1 count.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	host = strings.Trim(host, "[]") // tolerate a surviving bracketed IPv6 literal
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
