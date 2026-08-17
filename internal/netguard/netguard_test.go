package netguard

import "testing"

// TestValidate locks the loopback guard: the unauthenticated endpoints must
// bind loopback only, unless the operator explicitly opts into exposing them.
// Empty-host forms (":18800") bind ALL interfaces under Go's net.Listen, so
// they are treated as non-loopback and refused. Ported verbatim from
// cmd/local-agent's TestValidateListenAddr at extraction time (2026-07-17).
func TestValidate(t *testing.T) {
	loopback := []string{
		"127.0.0.1:18800",
		"[::1]:18800",
		"localhost:18800",
	}
	nonLocal := []string{
		"0.0.0.0:18800",
		"192.168.1.5:18800",
		":18800", // empty host = bind all interfaces
	}

	for _, addr := range loopback {
		if err := Validate(addr, false); err != nil {
			t.Errorf("Validate(%q, false) = %v, want nil (loopback is allowed)", addr, err)
		}
	}
	for _, addr := range nonLocal {
		if err := Validate(addr, false); err == nil {
			t.Errorf("Validate(%q, false) = nil, want refusal (non-loopback)", addr)
		}
		// The override must let every refused address through (the caller,
		// not the validator, is responsible for emitting the loud warning).
		if err := Validate(addr, true); err != nil {
			t.Errorf("Validate(%q, true) = %v, want nil (override allows non-loopback)", addr, err)
		}
	}
}

// TestValidateMalformed locks the refuse-on-unparseable rule: an address we
// cannot prove loopback (missing port) is refused rather than allowed.
func TestValidateMalformed(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "localhost", ""} {
		if err := Validate(addr, false); err == nil {
			t.Errorf("Validate(%q, false) = nil, want parse refusal", addr)
		}
		if err := Validate(addr, true); err != nil {
			t.Errorf("Validate(%q, true) = %v, want nil (override skips parsing)", addr, err)
		}
	}
}

// TestLoopbackAddr locks the exported loopback FACT (fleet-serve's agent-lane
// auth gate keys on it): same notion as Validate's guard, but reported rather
// than enforced. Malformed/unprovable addresses must report FALSE — a listener
// we cannot prove loopback must be treated as exposed, so the agent lane fails
// closed on it.
func TestLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:18811", true},
		{"127.9.9.9:18811", true}, // any 127/8, not just .0.0.1
		{"[::1]:18811", true},
		{"localhost:18811", true},
		{"100.64.0.10:18811", false}, // tailnet is NOT loopback (token required there)
		{"0.0.0.0:18811", false},
		{":18811", false},     // empty host = all interfaces
		{"127.0.0.1", false},  // missing port: unprovable → treated as exposed
		{"", false},
	}
	for _, c := range cases {
		if got := LoopbackAddr(c.addr); got != c.want {
			t.Errorf("LoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
