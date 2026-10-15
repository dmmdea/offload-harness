package main

// The self-improvement chapter (docs/systems/self-improvement.md) makes claims about what each command writes and about
// which values read null. Both are claims a reader acts on (does running this touch my ledger? does a 0 here mean none?),
// and an adversarial review found the page wider than the code in both places. These tests hold the page to the code:
// each has a behaviour half that drives the code and a page half that reads the page.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

const selfImprovementPage = "docs/systems/self-improvement.md"

// selfImprovementText is the whole page with whitespace folded, so a phrase that wraps across lines is still found.
func selfImprovementText(t *testing.T) string {
	t.Helper()
	text, err := docText(".", selfImprovementPage)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(text), " ")
}

// selfImprovementRetired fails for each retired sentence the page still carries.
func selfImprovementRetired(t *testing.T, text string, retired map[string]string) {
	t.Helper()
	for phrase, why := range retired {
		if strings.Contains(text, phrase) {
			t.Errorf("%s still says %q: %s", selfImprovementPage, phrase, why)
		}
	}
}

// The page used to say that every command except `ledger --failures` writes only its --out file (plus the review trace),
// and that each reader writes only what it is told to. `gate selftest` with the real scorer runs it through the
// production pipeline, which opens the ledger and the result cache; a review runs an agent contract, whose one exit
// records the run's own ledger row. The page names those writes.
func TestSelfImprovementPageSaysWhatEachCommandWrites(t *testing.T) {
	text := selfImprovementText(t)

	selfImprovementRetired(t, text, map[string]string{
		"the others write only the file they are told to":  "`gate selftest` also writes a ledger row and the result cache, and a review also writes the run's ledger row",
		"Each reader writes only what it is told to write": "the same overclaim, in the invariants",
	})

	scheduling := docSpan(t, selfImprovementPage, "- Scheduling.", "\n## Key concepts")
	docNeeds(t, "the page's non-scope bullet on writes", strings.Join(strings.Fields(scheduling), " "),
		"`gate selftest`", "production pipeline", "ledger row", "result cache", "`review-evidence.jsonl`")
}

// The behaviour half: `gate selftest` with the production scorer is not a file-only command. Every path is redirected into
// a temp dir and the seat is a stub, so nothing outside the test is touched. The ledger row and the cache are what the page
// says the command writes; if the scorer stops going through the production pipeline this fails, and the page is then too
// narrow instead of too wide.
func TestGateSelftestWithTheRealScorerWritesALedgerRowAndTheCache(t *testing.T) {
	srv := stubCascadeServer(t)
	defer srv.Close()

	scratch := t.TempDir()
	t.Setenv("USERPROFILE", scratch)
	t.Setenv("HOME", scratch)

	cfg := redirectWrites(config.Default(), scratch)
	cfg.Endpoint = srv.URL
	cfg.MaxRetries = 0
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(scratch, "config.json")
	if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	pairs := filepath.Join(scratch, "pairs.json")
	if err := os.WriteFile(pairs, []byte(`[{"name":"one","good":"Alpha report from Bravo42 about Charlie","degraded":"nothing"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	prevSHA := gateBuildSHA
	gateBuildSHA = func() string { return strings.Repeat("a", 64) }
	defer func() { gateBuildSHA = prevSHA }()

	if _, err := os.Stat(cfg.LedgerPath); err == nil {
		t.Fatalf("precondition: the ledger %s exists before the run", cfg.LedgerPath)
	}
	var out, errb bytes.Buffer
	runErr := gateMain([]string{"selftest", "--pairs", pairs, "--scorer", gateCompactionScorerID, "--config", cfgPath}, &out, &errb)
	t.Logf("selftest err=%v stdout=%s stderr=%s", runErr, out.String(), errb.String())

	if rows, err := os.ReadFile(cfg.LedgerPath); err != nil || len(bytes.TrimSpace(rows)) == 0 {
		t.Errorf("selftest wrote no ledger row at %s (read err=%v, %d bytes); the page says it does", cfg.LedgerPath, err, len(rows))
	}
	if _, err := os.Stat(cfg.CachePath); err != nil {
		t.Errorf("selftest left no result cache at %s (%v); the page says it does", cfg.CachePath, err)
	}
}
