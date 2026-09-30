package nodeswap

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The Linux shape of verifyRunning: the holder finder (FindProcessesByExe) is
// empty because a Linux swap stops nothing, but the verification finder
// (FindRunningByExe, a /proc scan) sees the restarted node. The tests around
// deps_other.go prove the /proc scan and the platformDeps wiring; none drives
// verifyRunning through the two finders, so `find := deps.FindProcessesByExe`
// (the pre-0.143.0 line) stays green on Windows AND on Linux. Portable: fake Deps.
func TestRunVerifiesThroughTheVerificationFinderWhenOneIsSet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setFinder bool
		wantPID   int
	}{
		{"finder set: the restarted node is found and the swap stands", true, 777},
		{"finder nil: falls back to the (empty) holder finder, cannot verify, rolls back", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeState()
			s.files["staged"], s.files["target"] = "NEWHASH", "OLDHASH"
			s.health = func() (HealthInfo, error) { return HealthInfo{OK: true}, nil }
			d := s.deps()
			var live []ProcessInfo
			d.FindProcessesByExe = func(string) ([]ProcessInfo, error) { return nil, nil }
			if tc.setFinder {
				d.FindRunningByExe = func(string) ([]ProcessInfo, error) { return live, nil }
			}
			d.RunCommand = func(ctx context.Context, timeout time.Duration, command string) (string, error) {
				if strings.Contains(command, "systemctl restart") {
					live = []ProcessInfo{{PID: 777, CommandLine: "target fleet-serve --listen 192.0.2.10:1", ExecutablePath: "target"}}
				}
				return "", nil
			}
			plan := Plan{Staged: "staged", Target: "target", ExpectedSHA256: "NEWHASH", RestartCommand: "systemctl restart offload-fleet-node",
				HealthURL: "http://node/fleet/health", BackupSuffix: "t", VerifyTimeout: 5 * time.Second, VerifyPollInterval: time.Millisecond}
			out := Run(context.Background(), plan, d, NewLogger(nil))
			if out.FinalPID != tc.wantPID || out.OK != tc.setFinder {
				t.Fatalf("OK=%v FinalPID=%d, want OK=%v FinalPID=%d (%s)", out.OK, out.FinalPID, tc.setFinder, tc.wantPID, out.Error)
			}
		})
	}
}
