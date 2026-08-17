package main

// Decision-Path Golden Tests (memory-frontier R2-11).
//
// # Why this reopens a killed idea, and in what narrowed form
//
// Round 1 killed "Golden-State Fixtures" for a good reason: CUDA decode is non-deterministic
// on this box, so any fixture asserting a MODEL OUTPUT is flaky by construction, and a flaky
// suite trains everyone to ignore it.
//
// R2-11 is the narrowed form that survives that objection: assert the DECISIONS, never the
// decoded text. Cache-key construction, template/exemplar binding, tier keyspace separation,
// grounding verdicts and the proof validators are all pure functions of their inputs. They
// are exactly as deterministic as the GPU is not.
//
// The event-sourcing / historical-replay half stays dead, and the reason is worth keeping
// written down: replay needs recorded OUTPUTS, and the only place outputs live here is the
// exact-result cache — so "re-live the ledger" really means "re-live the ~18 rows that
// happen to be cached". That is not a regression suite.
//
// # What makes these golden rather than merely present
//
// Each case asserts a RELATION that must hold ("changing X must change the key", "this
// classification must not sweep in that neighbour"), not a hard-coded hash. A hash fixture
// would have to be regenerated on every legitimate change, and regenerating a fixture is
// indistinguishable from silencing it.

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/grounding"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// ---------------------------------------------------------------------------
// Decision surface 1 — the failure-class atlas (R2-16)
// ---------------------------------------------------------------------------

func TestGolden_AtlasClassification(t *testing.T) {
	cases := []struct {
		name         string
		reason       string
		wantObsolete bool
		why          string
	}{
		{
			name:         "truncated context overflow",
			reason:       `reasoning model call failed: llama-server 400: {"error":{"code":400,"message":"request (10532 tokens) exceeds the availa`,
			wantObsolete: true,
			why:          "the ledger truncates reasons; matching the full sentence misses every real row",
		},
		{
			name:         "http timeout mentioning context",
			reason:       `reasoning model call failed: Post "http://127.0.0.1:11436/v1/chat/completions": context deadline exceeded (Client.Timeou`,
			wantObsolete: false,
			why:          "a Go HTTP timeout is a LIVE failure class; sweeping it in hides a real problem",
		},
		{
			name:         "http cancel mentioning context",
			reason:       `vision model call failed: Post "http://127.0.0.1:11436/v1/chat/completions": context canceled`,
			wantObsolete: false,
			why:          "cancellation is not a context-window overflow",
		},
		{
			name:         "gpu contention",
			reason:       "gpu busy: another generation job in this process still holds the card after 1m30s",
			wantObsolete: false,
			why:          "an ops failure that can recur today",
		},
		{
			name:         "missing render script",
			reason:       `script not found at D:\Dev\dmmdea\local-offload\bin\render\tts.mjs`,
			wantObsolete: false,
			why:          "a config failure, live until the path is fixed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isObsoleteDefer(c.reason); got != c.wantObsolete {
				t.Fatalf("isObsoleteDefer = %v, want %v — %s", got, c.wantObsolete, c.why)
			}
		})
	}
}

// The gate must be reachable in BOTH directions from realistic inputs, or it is decoration.
func TestGolden_AtlasGateIsReachableBothWays(t *testing.T) {
	mk := func(n int, reason string) []ledger.Entry {
		out := make([]ledger.Entry, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, ledger.Entry{Task: "summarize", ModelTier: "gemma-4-e4b", Deferred: true, Reason: reason})
		}
		return out
	}
	if a := buildAtlas(mk(2, "output truncated (hit token limit)"), 30); a.QualifyingCount != 0 {
		t.Fatal("2/month cleared a >=5/month gate")
	}
	if a := buildAtlas(mk(20, "output truncated (hit token limit)"), 30); a.QualifyingCount != 1 {
		t.Fatal("20/month failed to clear a >=5/month gate")
	}
}

// ---------------------------------------------------------------------------
// Decision surface 2 — reliability suppression (R2-14)
// ---------------------------------------------------------------------------

func TestGolden_ReliabilitySuppressionBoundary(t *testing.T) {
	mk := func(n int) []ledger.Entry {
		out := make([]ledger.Entry, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, ledger.Entry{Task: "classify", ModelTier: "gemma-4-e2b"})
		}
		return out
	}
	// One below the floor must NOT publish a rate; exactly at the floor must.
	if c := buildReliability(mk(minReliableSamples - 1)).Cells[0]; c.SuccessRate != nil {
		t.Fatalf("a cell one sample below the floor published a rate (%v)", *c.SuccessRate)
	}
	if c := buildReliability(mk(minReliableSamples)).Cells[0]; c.SuccessRate == nil {
		t.Fatal("a cell exactly at the floor suppressed its rate")
	}
}

// ---------------------------------------------------------------------------
// Decision surface 3 — grounding + the CPU proof validators (R2-10)
// ---------------------------------------------------------------------------

func TestGolden_GroundingVerdicts(t *testing.T) {
	cases := []struct {
		name                 string
		task                 core.TaskType
		input, data          string
		wantGrounded, wantOK bool
	}{
		{
			name:         "extract with a verbatim value is grounded",
			task:         core.TaskExtract,
			input:        "Invoice 88213 was issued to Acme Corp for 4200 USD.",
			data:         `{"invoice":"88213","customer":"Acme Corp"}`,
			wantGrounded: true, wantOK: true,
		},
		{
			name:         "extract with an invented value is not grounded",
			task:         core.TaskExtract,
			input:        "Invoice 88213 was issued to Acme Corp for 4200 USD.",
			data:         `{"invoice":"88213","customer":"Globex Industries"}`,
			wantGrounded: false, wantOK: true,
		},
		{
			name:         "classify is label-pinned so grounding does not apply",
			task:         core.TaskClassify,
			input:        "anything",
			data:         `{"label":"billing"}`,
			wantGrounded: false, wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, ok := grounding.Check(c.task, c.input, []byte(c.data))
			if g != c.wantGrounded || ok != c.wantOK {
				t.Fatalf("Check = (grounded=%v, ok=%v), want (%v, %v)", g, ok, c.wantGrounded, c.wantOK)
			}
		})
	}
}

// The proof validators exist to catch what Check() structurally cannot. This pins that
// division of labour so a future "simplification" cannot quietly collapse them into it.
func TestGolden_ProofsCatchWhatGroundingCannot(t *testing.T) {
	input := "The report notes revenue rose. Separately, costs fell in the same quarter."
	data := []byte(`{"finding":"The report notes \"revenue rose and costs fell\" this quarter."}`)

	// Every WORD of the fabricated quote is present in the source, so per-value grounding
	// has nothing to object to.
	if _, ok := grounding.Check(core.TaskSummarize, input, data); ok {
		if g, _ := grounding.Check(core.TaskSummarize, input, data); !g {
			t.Fatal("precondition changed: grounding now rejects this, so the proof no longer adds anything")
		}
	}
	// The citation proof does object, because the quote never appears contiguously.
	res := grounding.ProveCitedSpans(input, data)
	if !res.Applicable || len(res.Failures) != 1 {
		t.Fatalf("cited-span proof failed to catch a fabricated contiguous quote: %+v", res)
	}
}

func TestGolden_ProofInapplicableIsNotAPass(t *testing.T) {
	res := grounding.ProvePathsExist([]byte(`{"summary":"no paths"}`), func(string) error { return nil })
	if res.OK() {
		t.Fatal("an inapplicable validator reported OK — 'nothing to check' is a non-answer, not a pass")
	}
}

// ---------------------------------------------------------------------------
// Decision surface 4 — the pager gate (R2-13)
// ---------------------------------------------------------------------------

// Pinned because this gate's job is to CLOSE an item, and the way that goes wrong is a run
// that never exercised the question reporting a confident 0%.
func TestGolden_PagerUnexercisedRunIsNotAVerdict(t *testing.T) {
	var p agent.PagerStats
	r := p.Report()
	if r.RefetchRate != nil || r.Basis != "insufficient_data" {
		t.Fatalf("an unexercised pager run produced a rate: %+v", r)
	}
	if strings.Contains(r.Verdict, "BELOW GATE") {
		t.Fatal("an unexercised run produced a gate verdict that would close the item")
	}
}
