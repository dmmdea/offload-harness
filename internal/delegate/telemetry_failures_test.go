package delegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// The records this train added are the only way a hang, a ghost or an orphaned job
// shows up while it matters, so a record that cannot be written must not vanish
// without a trace: telemetry still never fails the work, but it says so, once.

// TestRecordStartedWarnsOncePerRunWhenTheMarkerCannotBeWritten: the dispatch marker
// exists so a hang or a ghost is visible while it happens, which makes its loss the
// invisible case. It is not counted in the ledger-loss tally (that reports the rows
// the run OWES, and a marker is an addition), so the warning is its only trace.
func TestRecordStartedWarnsOncePerRunWhenTheMarkerCannotBeWritten(t *testing.T) {
	logs := captureLog(t)
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Close(); err != nil { // every Record on a closed handle fails
		t.Fatal(err)
	}
	r := &runner{cfg: config.Config{LedgerPath: path}, led: led, route: "remote"}

	for i := 0; i < 3; i++ {
		r.recordStarted(withdrawContract(), "agd-marker", "agd-marker", "node-a", "seat", "")
	}

	if got := strings.Count(logs.String(), "dispatch marker"); got != 1 {
		t.Fatalf("the marker failure was logged %d times for 3 markers, want exactly once per run\nlog:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), path) {
		t.Fatalf("the warning does not name the ledger it could not write: %s", logs.String())
	}
	if tried, lost := r.ledgerTried.Load(), r.ledgerLost.Load(); tried != 0 || lost != 0 {
		t.Fatalf("ledger tally = %d tried, %d lost: a marker is not a row the run owes, and must stay out of the loss tally", tried, lost)
	}
}

// TestIntentLedgerWarnsOnceWhenItCannotWrite: an intent that was never written is a
// job the recovery pass can never find, and ADR 0064 now leans on an OPEN intent as
// the safe state for every unconfirmed give-up and every 401. append() swallowed
// every marshal, open, write and close error, so a full disk or a bad path looked
// exactly like a working ledger. Once per ledger handle, not once per event.
func TestIntentLedgerWarnsOnceWhenItCannotWrite(t *testing.T) {
	logs := captureLog(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "in-the-way")
	if err := os.WriteFile(blocker, []byte("a file where a directory must be"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &intentLedger{path: filepath.Join(blocker, "delegate-intent.jsonl")} // OpenFile fails: the parent is a file

	l.dispatched("agd-a", "http://192.0.2.1:18811", "goal")
	l.dispatched("agd-b", "http://192.0.2.1:18811", "goal")
	l.done("agd-a", intentNoteTerminal)

	out := logs.String()
	if got := strings.Count(out, "intent ledger"); got != 1 {
		t.Fatalf("the failure was logged %d times for 3 events, want exactly once per ledger\nlog:\n%s", got, out)
	}
	if !strings.Contains(out, "agd-a") || !strings.Contains(out, "recovery") {
		t.Fatalf("the warning must name the first job it could not record and what that costs (recovery cannot find it): %s", out)
	}
}

// TestIntentLedgerIsSilentWhenItWrites: the warning is for failures only.
func TestIntentLedgerIsSilentWhenItWrites(t *testing.T) {
	logs := captureLog(t)
	l := &intentLedger{path: filepath.Join(t.TempDir(), "delegate-intent.jsonl")}
	l.dispatched("agd-ok", "http://192.0.2.1:18811", "goal")
	l.done("agd-ok", intentNoteTerminal)
	if logs.Len() != 0 {
		t.Fatalf("a working intent ledger logged: %s", logs.String())
	}
}

// TestRecoverOrphansSaysWhatItLeftUnexamined: the pass runs on a 2-minute clock and
// stops when it runs out; intents behind a slow or unreachable node were never
// looked at and nothing said so. They stay open (the next pass continues), and the
// pass now says how many it did not reach.
func TestRecoverOrphansSaysWhatItLeftUnexamined(t *testing.T) {
	cfg, _ := intentCfg(t)
	l := openIntentLedger(cfg)
	for _, id := range []string{"agd-x1", "agd-x2", "agd-x3"} {
		l.dispatched(id, "http://192.0.2.1:18811", "work")
	}
	logs := captureLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the clock has already run out

	n, err := RecoverOrphans(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recovered = %d, want 0", n)
	}
	if !strings.Contains(logs.String(), "3 of 3 open intent(s) not examined") {
		t.Fatalf("the pass did not say how many intents it left unexamined\nlog:\n%s", logs.String())
	}
}

// TestRecoverOrphansIsSilentWhenItReachesEverything: no cap hit, no line.
func TestRecoverOrphansIsSilentWhenItReachesEverything(t *testing.T) {
	cfg, _ := intentCfg(t)
	logs := captureLog(t)
	if _, err := RecoverOrphans(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "not examined") {
		t.Fatalf("an empty pass claims intents were left unexamined: %s", logs.String())
	}
}
