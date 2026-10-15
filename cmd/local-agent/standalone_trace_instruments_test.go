package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// The per-goal trace is the ONLY durable record of a standalone run, and until now it
// kept the final transcript and nothing the run's own instruments had measured. The pager gate
// and the prefill report were printed to stderr and lost, so the trace corpus could not answer
// "how often did the agent come back for what compaction took out" from anything on disk.
// These tests run the real drainer over a scripted seat and read the files it writes.

// doneClient answers every call with a final answer, reporting the server's prefill accounting
// so the run's Prefill report is MEASURED.
type doneClient struct{}

func (doneClient) Chat(context.Context, []agent.Msg, []agent.ToolSpec, int) (agent.Completion, error) {
	return agent.Completion{
		Msg:          agent.Msg{Role: "assistant", Content: "done"},
		FinishReason: "stop",
		Serve:        &agent.ServeStats{CacheN: 30, PromptN: 70},
	}, nil
}

// drainOneGoal runs a one-goal queue through runStandalone and returns the decoded trace file.
func drainOneGoal(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	return drainGoals(t, 1)["g1"]
}

// drainGoals runs n goals (g1..gn) through ONE loop, the way the queue drainer does, and returns
// every decoded trace file by goal id.
func drainGoals(t *testing.T, n int) map[string]map[string]json.RawMessage {
	t.Helper()
	dir := t.TempDir()
	queue := filepath.Join(dir, "queue.jsonl")
	var q []byte
	for i := 1; i <= n; i++ {
		q = append(q, []byte(fmt.Sprintf("{\"id\":\"g%d\",\"goal\":\"say done\"}\n", i))...)
	}
	if err := os.WriteFile(queue, q, 0o644); err != nil {
		t.Fatal(err)
	}
	traces := filepath.Join(dir, "traces")
	loop := agent.NewLoop(doneClient{}, nil, 3).WithoutExemplars()
	err := runStandalone(context.Background(), loop, standaloneOpts{
		queuePath: queue, tracesDir: traces, askQueuePath: filepath.Join(dir, "asks.jsonl"),
		checkpointPath: filepath.Join(traces, "_checkpoint.jsonl"), goalTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("runStandalone: %v", err)
	}
	out := map[string]map[string]json.RawMessage{}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("g%d", i)
		raw, err := os.ReadFile(filepath.Join(traces, id+".json"))
		if err != nil {
			t.Fatalf("trace file %s not written: %v", id, err)
		}
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("trace file %s is not JSON: %v", id, err)
		}
		out[id] = rec
	}
	return out
}

// TestTraceFileCarriesThePagerReport: a run that never compacted is a measured zero passes with
// basis insufficient_data, written as the object itself. Its absence would read as "an older
// trace", the one state that must never stand for a quiet run.
func TestTraceFileCarriesThePagerReport(t *testing.T) {
	rec := drainOneGoal(t)
	raw, ok := rec["pager"]
	if !ok {
		t.Fatalf("trace has no pager object; keys: %v", keysOfRaw(rec))
	}
	var pg struct {
		Basis            string `json:"basis"`
		CompactionPasses *int   `json:"compaction_passes"`
	}
	if err := json.Unmarshal(raw, &pg); err != nil {
		t.Fatal(err)
	}
	if pg.Basis != "insufficient_data" {
		t.Fatalf("pager basis = %q on a run that evicted nothing, want insufficient_data", pg.Basis)
	}
	if pg.CompactionPasses == nil {
		t.Fatalf("compaction_passes is absent from the pager object, want a present 0")
	}
	if *pg.CompactionPasses != 0 {
		t.Fatalf("compaction_passes = %d, want 0 on a run that never compacted", *pg.CompactionPasses)
	}
}

// TestTraceFileCarriesThePrefillReport: the server's own prefill accounting survives the run.
func TestTraceFileCarriesThePrefillReport(t *testing.T) {
	rec := drainOneGoal(t)
	raw, ok := rec["prefill"]
	if !ok {
		t.Fatalf("trace has no prefill object; keys: %v", keysOfRaw(rec))
	}
	var pf struct {
		Basis         string `json:"basis"`
		ObservedSteps int    `json:"observed_steps"`
		PrefillTokens int64  `json:"prefill_tokens"`
		CacheTokens   int64  `json:"cache_tokens"`
	}
	if err := json.Unmarshal(raw, &pf); err != nil {
		t.Fatal(err)
	}
	if pf.Basis != "measured" || pf.ObservedSteps != 1 || pf.PrefillTokens != 70 || pf.CacheTokens != 30 {
		t.Fatalf("prefill = %+v, want measured over 1 step with 70 prefilled and 30 cached tokens", pf)
	}
}

// TestEachGoalsTraceCoversThatGoalAlone: one loop drains the whole queue, and its instruments
// accumulate for the loop's life. A trace that carried the running total would make the second
// goal's prefill the sum of both goals', and a corpus that pools traces would count the first
// goal twice. Every goal's file must report its own run.
func TestEachGoalsTraceCoversThatGoalAlone(t *testing.T) {
	traces := drainGoals(t, 3)
	for _, id := range []string{"g1", "g2", "g3"} {
		var pf struct {
			ObservedSteps int   `json:"observed_steps"`
			PrefillTokens int64 `json:"prefill_tokens"`
			CacheTokens   int64 `json:"cache_tokens"`
		}
		if err := json.Unmarshal(traces[id]["prefill"], &pf); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if pf.ObservedSteps != 1 || pf.PrefillTokens != 70 || pf.CacheTokens != 30 {
			t.Errorf("%s: prefill = %+v, want this goal's one step (70 prefilled, 30 cached), not a running total", id, pf)
		}
	}
}

// TestTraceRecordKeepsItsExistingKeys: the additive fields must not move what compaction-eval
// harvest and the supervisors already read.
func TestTraceRecordKeepsItsExistingKeys(t *testing.T) {
	rec := drainOneGoal(t)
	for _, k := range []string{"id", "goal", "stop_reason", "steps", "output", "transcript"} {
		if _, ok := rec[k]; !ok {
			t.Fatalf("trace lost existing key %q; keys: %v", k, keysOfRaw(rec))
		}
	}
}

// TestAnOlderTraceShapeStillOmitsTheInstruments: a record built without the instruments (the
// pre-field shape, or a future caller that has none) must not serialize them as empty objects.
func TestAnOlderTraceShapeStillOmitsTheInstruments(t *testing.T) {
	b, err := json.Marshal(traceRecord{ID: "g", Goal: "x", Transcript: []agent.Msg{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pager", "prefill"} {
		if _, ok := rec[k]; ok {
			t.Fatalf("a record with no instruments serialized %q: %s", k, b)
		}
	}
}

func keysOfRaw(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
