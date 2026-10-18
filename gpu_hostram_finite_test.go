package main

import (
	"strings"
	"testing"
)

// A host-RAM figure is a finite number of GiB. NaN passes every ordered comparison (neither "< 0" nor "<= 0" is true of it) and
// max(NaN, 0) is NaN in Go, so a --ram-commit of NaN or Inf got through the flag checks and into the lease, where no comparison
// against it ever refuses.
func TestReserveRefusesAHostRAMFigureThatIsNotFinite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"commit NaN", []string{"--ram", "10", "--ram-commit", "NaN"}, "--ram-commit"},
		{"commit Inf", []string{"--ram", "10", "--ram-commit", "Inf"}, "--ram-commit"},
		{"commit +Inf", []string{"--ram", "10", "--ram-commit", "+Inf"}, "--ram-commit"},
		{"ram NaN", []string{"--ram", "NaN"}, "--ram"},
		{"ram Inf", []string{"--ram", "Inf"}, "--ram"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, m := scopedLeaseFixture(t)
			useCardTable(t, "")
			hostMemAs(t, roomyForCommit)
			flags := append([]string{"--class", "media", "--devices", "2", "--wait", "0"}, tc.flags...)
			err := runGPUReserve(reserveWithCmd(cfg, flags))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "finite") {
				t.Fatalf("want a refusal naming %s and saying the figure must be finite, got %v", tc.want, err)
			}
			if l := m.Leases(); len(l) != 0 {
				t.Fatalf("a refused declaration holds nothing, got %+v", l)
			}
		})
	}
}

// The hidden holder takes the same four figures from its parent, which validated them. It is also a verb of its own, so it checks
// them itself: max(NaN, 0) is NaN, and a lease that declares one is neither refused nor admitted by any comparison.
func TestHoldRefusesAHostRAMFigureThatIsNotFinite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"ram NaN", []string{"--ram", "NaN"}, "--ram"},
		{"ram Inf", []string{"--ram", "Inf"}, "--ram"},
		{"commit NaN", []string{"--ram", "10", "--ram-commit", "NaN"}, "--ram-commit"},
		{"commit -Inf", []string{"--ram", "10", "--ram-commit", "-Inf"}, "--ram-commit"},
		{"wait NaN", []string{"--ram-wait", "NaN"}, "--ram-wait"},
		{"wait commit NaN", []string{"--ram-wait", "10", "--ram-wait-commit", "NaN"}, "--ram-wait-commit"},
		{"wait commit +Inf", []string{"--ram-wait", "10", "--ram-wait-commit", "+Inf"}, "--ram-wait-commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, m := scopedLeaseFixture(t)
			useCardTable(t, "")
			hostMemAs(t, roomyForCommit)
			// A holder that is not refused takes the lease and holds it to its own deadline, one second on.
			args := append([]string{"--config", cfg, "--class", "media", "--devices", "gpu-aaaa0000-x", "--for", "1s", "--release-at-expiry"}, tc.flags...)
			err := runGPUHold(args)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "finite") {
				t.Fatalf("want a refusal naming %s and saying the figure must be finite, got %v", tc.want, err)
			}
			if l := m.Leases(); len(l) != 0 {
				t.Fatalf("a refused declaration holds nothing, got %+v", l)
			}
		})
	}
}
