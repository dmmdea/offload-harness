package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// The ledger stores a delegated job's reason whole (ADR 0064, up to 4096 bytes), and
// a queue-deadline reason carries the number of polls that answered `accepted`, so
// no two of them are byte-equal. Every reader that GROUPS by reason has to group by
// its short form (ledger.ShortReason) or one class fragments into a group per
// job-specific number, and a report meant to say "how many jobs died queued" says
// "1, 1, 1, 1, ...".

// queueDeadlineReason is one stored queue-deadline reason: 231 bytes of class, then
// the part that differs per job.
func queueDeadlineReason(polls int) string {
	head := strings.Repeat("queue deadline after 5m0s: the node accepted the job but never started it — ", 3)
	return head + fmt.Sprintf("(%d poll(s) answered `accepted`)", polls)
}

// TestAtlasGroupsByTheShortFormOfAStoredReason: two defers whose reasons differ only
// past the 120-byte short form are ONE failure class of 2, keyed by the short form.
func TestAtlasGroupsByTheShortFormOfAStoredReason(t *testing.T) {
	rows := append(rowsOf(1, "agent_delegate", "n:s", true, queueDeadlineReason(31)),
		rowsOf(1, "agent_delegate", "n:s", true, queueDeadlineReason(29))...)
	rep := buildAtlas(rows, 30)
	if rep.TotalDefers != 2 || len(rep.Classes) != 1 || rep.Classes[0].Count != 2 {
		t.Fatalf("atlas = %d defers in %d class(es) %+v, want one class of 2: the stored reason was grouped whole", rep.TotalDefers, len(rep.Classes), rep.Classes)
	}
	if got := rep.Classes[0].Reason; got != ledger.ShortReason(queueDeadlineReason(31)) || len(got) > 120 {
		t.Fatalf("class key = %q (%d bytes), want the short form, at most 120 bytes", got, len(got))
	}
}

// TestLoupeStatsGroupsDefersByTheShortForm drives the verb itself: `loupe --json`
// over a real ledger file, whose top_defer_reasons must be one class of 2.
func TestLoupeStatsGroupsDefersByTheShortForm(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, polls := range []int{31, 29} {
		if err := led.Record(ledger.Entry{Task: "agent_delegate", ModelTier: "n:s", Deferred: true, Reason: queueDeadlineReason(polls)}); err != nil {
			t.Fatal(err)
		}
	}
	led.Close()
	cfgPath := filepath.Join(dir, "config.json")
	cfg, _ := json.Marshal(map[string]any{"home": dir, "ledger_path": ledgerPath})
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}

	var runErr error
	out := captureStdout(t, func() { runErr = runLoupe([]string{"--config", cfgPath, "--json"}) })
	if runErr != nil {
		t.Fatalf("runLoupe: %v", runErr)
	}
	var rep struct {
		Deferred int       `json:"deferred"`
		Top      []statRow `json:"top_defer_reasons"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("loupe --json is not JSON: %v\n%s", err, out)
	}
	if rep.Deferred != 2 || len(rep.Top) != 1 || rep.Top[0].Count != 2 {
		t.Fatalf("deferred = %d, top_defer_reasons = %+v, want one class of 2", rep.Deferred, rep.Top)
	}
	if len(rep.Top[0].Key) > 120 {
		t.Fatalf("class key is %d bytes, want the short form (at most 120)", len(rep.Top[0].Key))
	}
}
