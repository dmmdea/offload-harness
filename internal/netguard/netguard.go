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
// allowNonLocal (from --listen-trusted-network) overrides the refusal; the loud
// warning is the caller's responsibility, not the validator's — this stays a
// pure, side-effect-free function so it is trivial to test.
func Validate(addr string, allowNonLocal bool) error {
	if allowNonLocal {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Missing port / malformed — we cannot prove it is loopback, so refuse.
		return fmt.Errorf("cannot parse --listen %q: %w", addr, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("refusing to bind --listen %q: this endpoint is UNAUTHENTICATED and "+
		"each request drives the agent's write/GitHub tools; binding beyond loopback exposes "+
		"an RCE-class surface to your network. Use a loopback address (127.0.0.1, [::1], localhost) "+
		"or pass --listen-trusted-network to override (only on a network you fully trust)", addr)
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
