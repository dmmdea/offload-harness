package main

import (
	"os"
	"path/filepath"
	"strings"
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

// TestFleetUIRefusesTheHailoSidecarPort (register E-08): fleet-ui's documented default listen is
// 127.0.0.1:18813, which is also the port the Hailo sidecar answers on. On a box that lists
// hailo-8l the two cannot share it — the page would squat the sidecar's port, or the sidecar's
// spawn would find it taken — so fleet-ui refuses to bind there and names the way out. The
// documented default is NOT moved: every box without the Hailo binds it exactly as before, and the
// check is per device (the Coral's and the RKNPU's boxes are untouched).
func TestFleetUIRefusesTheHailoSidecarPort(t *testing.T) {
	boxWith := func(endpoint string, ids ...string) config.Config {
		cfg := config.Default()
		cfg.Accelerators = ids
		if endpoint != "" {
			cfg.HailoEndpoint = endpoint
		}
		return cfg
	}
	cases := []struct {
		name    string
		cfg     config.Config
		listen  string
		wantErr bool
	}{
		{"hailo box on the documented default", boxWith("", "hailo-8l"), "127.0.0.1:18813", true},
		{"hailo box on a tailnet-shaped address with the sidecar's port", boxWith("", "hailo-8l"), "192.0.2.1:18813", true},
		{"hailo box listed beside another device", boxWith("", "coral-edgetpu", "hailo-8l"), "127.0.0.1:18813", true},
		{"hailo box on another port", boxWith("", "hailo-8l"), "127.0.0.1:18899", false},
		{"hailo box on the Coral sidecar's port is not this check's concern", boxWith("", "hailo-8l"), "127.0.0.1:18814", false},
		{"a box with no accelerator keeps the documented default", config.Default(), "127.0.0.1:18813", false},
		{"a Coral box keeps the documented default", boxWith("", "coral-edgetpu"), "127.0.0.1:18813", false},
		{"an RKNPU box keeps the documented default", boxWith("", "rknpu"), "127.0.0.1:18813", false},
		{"a moved sidecar port is the one refused", boxWith("http://127.0.0.1:18900", "hailo-8l"), "127.0.0.1:18900", true},
		{"a moved sidecar frees the documented default", boxWith("http://127.0.0.1:18900", "hailo-8l"), "127.0.0.1:18813", false},
		{"an endpoint with no port reads as its scheme's own", boxWith("http://127.0.0.1", "hailo-8l"), "127.0.0.1:80", true},
		{"an unset endpoint falls back to the documented sidecar port", func() config.Config {
			cfg := boxWith("", "hailo-8l")
			cfg.HailoEndpoint = ""
			return cfg
		}(), "127.0.0.1:18813", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := refuseHailoPort(c.cfg, c.listen)
			if c.wantErr && err == nil {
				t.Fatalf("refuseHailoPort(%q) = nil, want a refusal", c.listen)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("refuseHailoPort(%q) = %v, want nil", c.listen, err)
			}
			if err != nil {
				for _, want := range []string{"fleet-ui", "hailo-8l", "--listen"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal must name %q so the operator knows the way out: %v", want, err)
					}
				}
			}
		})
	}
}

// The refusal is wired into the verb ahead of anything that binds or polls: a hailo box on the
// default listen fails with the sidecar-port reason, and a box with no Hailo gets past it (and
// stops at the empty roster — no node to poll — without ever opening a socket).
func TestFleetUIRunRefusesTheHailoSidecarPortBeforeBinding(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	err := runFleetUI([]string{"--config", write(`{"accelerators":["hailo-8l"]}`)})
	if err == nil || !strings.Contains(err.Error(), "hailo-8l") || !strings.Contains(err.Error(), "18813") {
		t.Fatalf("a hailo box on the default listen must be refused naming the device and the port, got %v", err)
	}
	err = runFleetUI([]string{"--config", write(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "no nodes to poll") {
		t.Fatalf("a box with no Hailo must pass the check and stop at the empty roster, got %v", err)
	}
	err = runFleetUI([]string{"--config", write(`{"accelerators":["coral-edgetpu"]}`)})
	if err == nil || !strings.Contains(err.Error(), "no nodes to poll") {
		t.Fatalf("a Coral box must pass the check and stop at the empty roster, got %v", err)
	}
}
