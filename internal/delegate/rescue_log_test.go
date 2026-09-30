package delegate

import (
	"errors"
	"strings"
	"testing"
)

// A rescue that could not re-pack a finished answer says so in the log, and the
// line has to name the JOB: finish() stamped the job id after the rescue had run,
// so the line read "the finished answer of job on <node>" and could not be matched
// to the ledger row or the job on the node.
func TestRescueFailureLogNamesTheJob(t *testing.T) {
	logged := captureLog(t)
	rs := &rescuer{err: errors.New("the local seat is not serving")}
	results, _ := runRescued(t, rescueNode(t, legacyRepackStall()), rescueContract(), rs.fn())
	id := results[0].JobID
	if id == "" {
		t.Fatal("the result carries no job id")
	}
	if !strings.Contains(logged.String(), "could not be re-packed") || !strings.Contains(logged.String(), id) {
		t.Fatalf("log = %q, want the failed rescue named with its job %s", logged.String(), id)
	}
}
