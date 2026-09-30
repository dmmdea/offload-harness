package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFleetPollLeaseResolution pins fleet_poll_lease_sec's resolver (ADR 0064):
// unset takes the 60 s default, a negative value turns the abandoned-job rules
// off, and a value under the floor is raised to it — a lease shorter than the
// delegator's own gap between polls would reap jobs that ARE being polled.
func TestFleetPollLeaseResolution(t *testing.T) {
	cases := []struct {
		name string
		sec  int
		want time.Duration
	}{
		{"unset takes the default", 0, 60 * time.Second},
		{"explicit value", 90, 90 * time.Second},
		{"exactly the floor", FleetPollLeaseSecFloor, FleetPollLeaseSecFloor * time.Second},
		{"under the floor is raised to it", 3, FleetPollLeaseSecFloor * time.Second},
		{"negative turns the rule off", -1, 0},
	}
	for _, c := range cases {
		if got := (Config{FleetPollLeaseSec: c.sec}).FleetPollLease(); got != c.want {
			t.Errorf("%s: FleetPollLease() = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestFleetPollLeaseLoadsFromJSON: the key round-trips through Load.
func TestFleetPollLeaseLoadsFromJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte(`{"fleet_poll_lease_sec":45}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.FleetPollLeaseSec != 45 || got.FleetPollLease() != 45*time.Second {
		t.Fatalf("fleet_poll_lease_sec = %d (%v), want 45 (45s)", got.FleetPollLeaseSec, got.FleetPollLease())
	}
}
