package nodeswap

import (
	"context"
	"strings"
	"testing"
)

// A node that listens beyond loopback runs under the new binary only if the config its fleet-serve reads holds a
// fleet_auth_token: every lane requires the fleet bearer, and fleet-serve will not start without the key. A swap that
// replaced the binary of such a node and then restarted it would take the node down, so the swap refuses first,
// before it hashes, waits, stops or renames anything, on a dry run as on a real one. The caller reads the config and
// reports only whether the key is there (never its value).

const (
	tailnetHealth = "http://192.0.2.7:18811/fleet/health"
	nodeConfig    = "/srv/offload/etc/config.json"
)

func fleetKeyWorld() *fakeState {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	s.health = func() (HealthInfo, error) { return HealthInfo{OK: true}, nil }
	return s
}

func TestRun_RefusesToSwapANodeBeyondLoopbackWhoseConfigHoldsNoFleetKey(t *testing.T) {
	for _, dry := range []bool{true, false} {
		s := fleetKeyWorld()
		plan := Plan{
			Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH",
			HealthURL: tailnetHealth, DryRun: dry, RestartTaskName: "offload-fleet-node",
			FleetKey: &FleetKeyCheck{Config: nodeConfig, Present: false},
		}
		out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
		if out.OK {
			t.Fatalf("dry=%v: the swap went ahead on a node whose config holds no fleet_auth_token", dry)
		}
		for _, want := range []string{"fleet_auth_token", nodeConfig} {
			if !strings.Contains(out.Error, want) {
				t.Errorf("dry=%v: the refusal does not name %q: %q", dry, want, out.Error)
			}
		}
		if n := len(out.Steps); n == 0 || out.Steps[n-1].Name != "fleet-key" || out.Steps[n-1].OK {
			t.Errorf("dry=%v: the failing step is %+v, want fleet-key", dry, out.Steps)
		}
		// Before anything: not even the staged binary was hashed (no hash step ran), and nothing moved.
		for _, st := range out.Steps {
			if st.Name == "verify-hash" || st.Name == "wait-idle" {
				t.Errorf("dry=%v: step %s ran before the fleet key was checked", dry, st.Name)
			}
		}
		if s.files["target.exe"] != "OLDHASH" || s.restartCalled != 0 {
			t.Errorf("dry=%v: the refused swap touched the node (target %q, restarts %d)", dry, s.files["target.exe"], s.restartCalled)
		}
	}
}

func TestRun_TheFleetKeyCheckOnlyRefusesWhatItShould(t *testing.T) {
	for name, plan := range map[string]Plan{
		"the key is there":                  {HealthURL: tailnetHealth, FleetKey: &FleetKeyCheck{Config: nodeConfig, Present: true}},
		"a loopback node needs no key":      {HealthURL: "http://127.0.0.1:18811/fleet/health", FleetKey: &FleetKeyCheck{Config: nodeConfig}},
		"a loopback name needs no key":      {HealthURL: "http://localhost:18811/fleet/health", FleetKey: &FleetKeyCheck{Config: nodeConfig}},
		"a standalone node serves no lanes": {FleetKey: &FleetKeyCheck{Config: nodeConfig}},
		"no check was made":                 {HealthURL: tailnetHealth},
	} {
		s := fleetKeyWorld()
		plan.Staged, plan.Target, plan.ExpectedSHA256, plan.DryRun = "staged.exe", "target.exe", "NEWHASH", true
		out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
		if !out.OK {
			t.Errorf("%s: the swap was refused: %q", name, out.Error)
		}
	}
}
