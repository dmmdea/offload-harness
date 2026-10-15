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

// The page also said that a value nobody measured reads null, never 0, and that the register and the trace-corpus readout
// keep this "for every total and rate". The readout keeps it for every rate only: its totals (tcPagerStats.Evictions,
// tcCompaction.Passes, tcPrefillStats.PrefillTokens and the rest) are plain int64 sums over the recorded runs, so they read
// 0 when nothing was recorded, with the recorded and unrecorded run counts beside them. The register keeps null for its card
// work (cards_tokens). A reader who took a 0 pass count for "measured, none fired" would be wrong, and so would one who
// waited for a null total.
func TestSelfImprovementPageKeepsNullForRatesAndCardWorkOnly(t *testing.T) {
	text := selfImprovementText(t)

	selfImprovementRetired(t, text, map[string]string{
		"keep this for every total and rate":                     "the readout's totals read 0, not null; only its rates and the register's cards_tokens read null",
		"In the register, the readout and the gate's efficiency": "the same overclaim, in the invariants",
		"A register total, a trace-corpus rate":                  "the register's nullable figure is cards_tokens, and a trace-corpus total that reads 0 is not proof of a measurement",
	})

	unrecorded := strings.Join(strings.Fields(docSpan(t, selfImprovementPage, "- **Unrecorded.**", "- **Failure class and kind.**")), " ")
	docNeeds(t, "the page's Unrecorded concept", unrecorded,
		"every rate", "`cards_tokens`", "totals", "read 0", "`recorded_runs`", "`unrecorded_runs`", "`unmeasured_runs`")

	invariants := strings.Join(strings.Fields(docSpan(t, selfImprovementPage, "## Invariants and assumptions", "## Security and privacy notes")), " ")
	docNeeds(t, "the page's invariants", invariants, "`cards_tokens`", "a rate", "totals", "read 0", "`recorded_runs`")

	pitfall := strings.Join(strings.Fields(docSpan(t, selfImprovementPage, "- **`null` and `NO_DATA` are not zero.**", "- **A review PASS")), " ")
	docNeeds(t, "the page's null pitfall", pitfall, "`cards_tokens`", "total", "`recorded_runs`")
}

// The behaviour half: one run that recorded no pager object and no prefill accounting. The rates are null, the totals are 0,
// and the counts beside them say nothing was recorded.
func TestTraceCorpusTotalsReadZeroAndRatesReadNullWhenNothingWasRecorded(t *testing.T) {
	runs := []tcRun{{ID: "dl-2026-10-14-000001", Source: tcSourceDelegation, Seat: "seat-a", TraceBearing: true, Steps: 3}}
	b, err := tcMarshalReport(tcBuildReport(runs, "h"))
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		All struct {
			Pager struct {
				RecordedRuns    int `json:"recorded_runs"`
				UnrecordedRuns  int `json:"unrecorded_runs"`
				Evictions       any `json:"evictions"`
				DistinctEvicted any `json:"distinct_evicted"`
				Refetched       any `json:"refetched_distinct"`
				RefetchRate     any `json:"refetch_rate"`
			} `json:"pager"`
			Compaction struct {
				RecordedRuns   int `json:"recorded_runs"`
				UnrecordedRuns int `json:"unrecorded_runs"`
				Passes         any `json:"passes"`
				FireRate       any `json:"fire_rate"`
			} `json:"compaction"`
			Prefill struct {
				UnmeasuredRuns int `json:"unmeasured_runs"`
				PrefillTokens  any `json:"prefill_tokens"`
				CacheTokens    any `json:"cache_tokens"`
				ObservedSteps  any `json:"observed_steps"`
				KVReusePct     any `json:"kv_reuse_pct"`
			} `json:"prefill"`
		} `json:"all"`
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	p, c, f := rep.All.Pager, rep.All.Compaction, rep.All.Prefill
	for name, v := range map[string]any{
		"pager.evictions": p.Evictions, "pager.distinct_evicted": p.DistinctEvicted, "pager.refetched_distinct": p.Refetched,
		"compaction.passes": c.Passes, "prefill.prefill_tokens": f.PrefillTokens, "prefill.cache_tokens": f.CacheTokens,
		"prefill.observed_steps": f.ObservedSteps,
	} {
		if n, ok := v.(float64); !ok || n != 0 {
			t.Errorf("%s = %v for a run that recorded nothing; the page says a trace-corpus total reads 0", name, v)
		}
	}
	for name, v := range map[string]any{"pager.refetch_rate": p.RefetchRate, "compaction.fire_rate": c.FireRate, "prefill.kv_reuse_pct": f.KVReusePct} {
		if v != nil {
			t.Errorf("%s = %v for a run that recorded nothing; the page says a rate reads null", name, v)
		}
	}
	if p.RecordedRuns != 0 || p.UnrecordedRuns != 1 || c.RecordedRuns != 0 || c.UnrecordedRuns != 1 || f.UnmeasuredRuns != 1 {
		t.Errorf("the counts beside the totals must say nothing was recorded: pager %d/%d, compaction %d/%d, prefill unmeasured %d",
			p.RecordedRuns, p.UnrecordedRuns, c.RecordedRuns, c.UnrecordedRuns, f.UnmeasuredRuns)
	}
}

// The gate's own page carried the same sentence before the chapter did: "The verb reads only the files it is named and
// writes only `freeze --out` and `selftest --out`". With its real scorer `gate selftest` also reads the config and writes
// what a pipeline call writes (TestGateSelftestWithTheRealScorerWritesALedgerRowAndTheCache), so the chapter and the page
// it links as the owner must not disagree.
func TestTheGatePageDoesNotClaimTheVerbWritesOnlyItsOutFiles(t *testing.T) {
	const page = "docs/systems/measurement-gate.md"
	text, err := docText(".", page)
	if err != nil {
		t.Fatal(err)
	}
	text = strings.Join(strings.Fields(text), " ")
	if phrase := "writes only `freeze --out` and `selftest --out`"; strings.Contains(text, phrase) {
		t.Errorf("%s still says %q: gate selftest with its real scorer also writes ledger rows and the result cache", page, phrase)
	}
	docNeeds(t, page, text, "production pipeline", "result cache")
}
