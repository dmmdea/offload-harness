package main

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestRefuseListen table-tests the bind-safety decision fleet-ui makes before
// it ever opens a socket. The all-interfaces check must win regardless of
// --listen-trusted-network — that flag exists to permit ONE tailnet address,
// never a wildcard bind — which is why every 0.0.0.0/[::]/bare-port case
// below is refused with trusted true AND false.
func TestRefuseListen(t *testing.T) {
	cases := []struct {
		name    string
		listen  string
		trusted bool
		wantErr bool
	}{
		{"loopback v4, untrusted", "127.0.0.1:18813", false, false},
		{"loopback v4, trusted (irrelevant)", "127.0.0.1:18813", true, false},
		{"localhost by name, untrusted", "localhost:18813", false, false},
		{"loopback v6, untrusted", "[::1]:18813", false, false},

		{"all-interfaces v4, untrusted", "0.0.0.0:18813", false, true},
		{"all-interfaces v4, trusted", "0.0.0.0:18813", true, true},
		{"all-interfaces v6, untrusted", "[::]:18813", false, true},
		{"all-interfaces v6, trusted", "[::]:18813", true, true},
		{"bare port, untrusted", ":18813", false, true},
		{"bare port, trusted", ":18813", true, true},

		{"non-loopback address, untrusted", "192.0.2.1:18813", false, true},
		{"non-loopback address, trusted", "192.0.2.1:18813", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := refuseListen(c.listen, c.trusted)
			if c.wantErr && err == nil {
				t.Fatalf("refuseListen(%q, %v) = nil, want a refusal", c.listen, c.trusted)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("refuseListen(%q, %v) = %v, want nil", c.listen, c.trusted, err)
			}
		})
	}
}

// TestFleetUIRemotes pins the roster-resolution rule: an explicit --remote
// list wins outright (no config fallback merged in), and otherwise the
// config's delegate_remotes are appended with this box's own fleet_listen
// ONLY when that listener is bound beyond loopback — a loopback fleet-serve
// is not reachable from fleet-ui's own poller as an http:// base anyway, and
// a delegator that never enabled fleet-serve (FleetListen == "") gets no
// self-entry at all.
func TestFleetUIRemotes(t *testing.T) {
	t.Run("explicit remotes win outright", func(t *testing.T) {
		cfg := config.Config{DelegateRemotes: []string{"http://cfg-a:1"}, FleetListen: "192.0.2.5:18811"}
		got := fleetUIRemotes(cfg, []string{"http://explicit:1"})
		if len(got) != 1 || got[0] != "http://explicit:1" {
			t.Fatalf("fleetUIRemotes = %v, want only the explicit list", got)
		}
	})

	t.Run("config remotes alone, no fleet_listen", func(t *testing.T) {
		cfg := config.Config{DelegateRemotes: []string{"http://cfg-a:1", "http://cfg-b:1"}}
		got := fleetUIRemotes(cfg, nil)
		want := []string{"http://cfg-a:1", "http://cfg-b:1"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("fleetUIRemotes = %v, want %v", got, want)
		}
	})

	t.Run("non-loopback fleet_listen is appended", func(t *testing.T) {
		cfg := config.Config{DelegateRemotes: []string{"http://cfg-a:1"}, FleetListen: "192.0.2.5:18811"}
		got := fleetUIRemotes(cfg, nil)
		want := []string{"http://cfg-a:1", "http://192.0.2.5:18811"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("fleetUIRemotes = %v, want %v", got, want)
		}
	})

	t.Run("loopback fleet_listen is NOT appended", func(t *testing.T) {
		cfg := config.Config{DelegateRemotes: []string{"http://cfg-a:1"}, FleetListen: "127.0.0.1:18811"}
		got := fleetUIRemotes(cfg, nil)
		want := []string{"http://cfg-a:1"}
		if len(got) != len(want) || got[0] != want[0] {
			t.Fatalf("fleetUIRemotes = %v, want %v (loopback fleet_listen excluded)", got, want)
		}
	})
}
