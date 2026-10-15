package config

import "testing"

// SF-16: fleet_ingress_guard defaults to warn, accepts its three modes in any case, and a
// mistyped mode fails the load by name instead of falling back to off or enforce.
func TestFleetIngressGuardModes(t *testing.T) {
	for in, want := range map[string]string{"": "warn", "warn": "warn", " WARN ": "warn", "off": "off", "Enforce": "enforce"} {
		if err := validateFleetIngressGuard(in); err != nil {
			t.Errorf("validate(%q) = %v, want ok", in, err)
		}
		if got := (Config{FleetIngressGuard: in}).FleetIngressGuardMode(); got != want {
			t.Errorf("mode(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"enforced", "on", "true", "strict"} {
		if err := validateFleetIngressGuard(bad); err == nil {
			t.Errorf("validate(%q) accepted a mistyped mode", bad)
		}
		// A one-shot verb keeps going after a load error: the mode must then fail closed.
		if got := (Config{FleetIngressGuard: bad}).FleetIngressGuardMode(); got != "enforce" {
			t.Errorf("mode(%q) = %q, want enforce (fail closed)", bad, got)
		}
	}
}
