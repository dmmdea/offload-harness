package gpuactivity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// End must not leave the record behind when a reader has it open at that moment.
// On Windows a delete fails with a sharing violation while another handle holds the
// file (the rename in write() already retries for the same reason). End ignored that
// error, so the run stayed registered with its heartbeat stopped until HeartbeatTTL
// (120 s) aged it out: a drain waited on a run that had ended, and the local run cap
// counted it (register C-84; seen as TestDrainWaitsForARegisteredRunAcrossTheStepGap
// timing out at 2 s with "1 run(s) registered" on a run ended at 60 ms).
func TestEndRemovesTheRecordEvenWhileAReaderHoldsItOpen(t *testing.T) {
	reg := OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	h, err := reg.Begin(Run{Seat: "seat", Kind: "agent_run", MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(h.path) // a drain or a status call reading the record
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = f.Close()
		close(released)
	}()
	h.End()
	<-released
	if _, err := os.Stat(h.path); !os.IsNotExist(err) {
		t.Fatalf("the record of an ended run is still on disk after End (stat err %v): the run stays registered until HeartbeatTTL", err)
	}
	if runs := reg.OnSeat(time.Now(), "seat"); len(runs) != 0 {
		t.Fatalf("OnSeat still lists %d run(s) after End", len(runs))
	}
}
