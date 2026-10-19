package nodeswap

import (
	"context"
	"testing"
)

// 2026-10-19, a Windows node: the fleet node had been restarted over `ssh localhost`, so it ran
// elevated, and an unelevated swap read its CIM row with no path and no command line. The swap saw
// no node to stop and no new node to verify, and rolled back against the running node. A swap that
// stops and restarts the node now refuses on such a holder, on a dry run as on a real one, before
// anything is touched.
func TestSwapRefusesANodeItCannotRead(t *testing.T) {
	for _, dry := range []bool{true, false} {
		s := stagedState()
		s.procs = []ProcessInfo{{PID: 58040, Unreadable: true}}
		p := Plan{Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH", BackupSuffix: "t",
			RestartCommand: "restart-node", DryRun: dry}
		out := Run(context.Background(), p, s.deps(), NewLogger(nil))
		if out.OK || len(out.Steps) == 0 || out.Steps[len(out.Steps)-1].Name != "process-access" {
			t.Fatalf("dry=%v: want a refusal at process-access, got ok=%v steps=%+v", dry, out.OK, out.Steps)
		}
		if len(s.files) != 2 || s.files["target.exe"] != "OLDHASH" || s.restartCalled != 0 || !procAlive(s, 58040) {
			t.Fatalf("dry=%v: the refusal touched something: files=%v restarts=%d procs=%v", dry, s.files, s.restartCalled, s.procs)
		}
	}
}
