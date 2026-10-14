package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/compeval"
)

func withFakeContextBenchSeat(t *testing.T, seat compeval.CBSeat) {
	t.Helper()
	old := contextBenchSeat
	contextBenchSeat = func(endpoint, model string, timeout time.Duration) compeval.CBSeat { return seat }
	t.Cleanup(func() { contextBenchSeat = old })
}

func contextBenchArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	// A config path that does not exist keeps the run off the operator's own config.
	return append([]string{"--config", filepath.Join(t.TempDir(), "absent.json")}, extra...)
}

// It is a subcommand of compaction-eval and needs no corpus.
func TestCompactionEvalRoutesContextBenchWithoutACorpus(t *testing.T) {
	withFakeContextBenchSeat(t, func(context.Context, []agent.Msg, int) (string, error) {
		return compeval.CBAdvanceMarker, nil
	})
	out := filepath.Join(t.TempDir(), "res.json")
	args := append([]string{"contextbench"}, contextBenchArgs(t, "--family", "kv_store", "--level", "R130", "--tasks", "2",
		"--seed", "7", "--endpoint", "http://fake.invalid", "--model", "seat-x", "--out", out)...)
	if err := runCompactionEval(args); err != nil {
		t.Fatalf("a run that graded its tasks must not error: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var rep compeval.CBReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 2 || rep.Results[0].ID != "kv_store-R130-seed7" || rep.Results[1].ID != "kv_store-R130-seed8" {
		t.Fatalf("results = %+v", rep.Results)
	}
	// A seat that only ever says READY answers nothing: graded, task_fail, reward 0.
	for _, r := range rep.Results {
		if r.Outcome != compeval.CBOutcomeTaskFail || r.Reward == nil || *r.Reward != 0 {
			t.Errorf("%s: %+v", r.ID, r)
		}
	}
	if rep.Seat != "seat-x" || rep.Delivery != compeval.CBDeliveryToolResult || rep.Counts[compeval.CBOutcomeTaskFail] != 2 {
		t.Fatalf("report header = %+v", rep)
	}
}

func TestContextBenchSeatAliasAndFlagRefusals(t *testing.T) {
	var got string
	old := contextBenchSeat
	contextBenchSeat = func(endpoint, model string, timeout time.Duration) compeval.CBSeat {
		got = model
		return func(context.Context, []agent.Msg, int) (string, error) { return compeval.CBAdvanceMarker, nil }
	}
	t.Cleanup(func() { contextBenchSeat = old })
	out := filepath.Join(t.TempDir(), "r.json")
	if err := runCompactionContextBench(contextBenchArgs(t, "--family", "sudoku_sketchpad", "--level", "t4", "--seat", "alias-seat", "--endpoint", "http://fake.invalid", "--out", out)); err != nil {
		t.Fatal(err)
	}
	if got != "alias-seat" {
		t.Fatalf("--seat should name the model, got %q", got)
	}
	for name, args := range map[string][]string{
		"route":            {"--route", "remote"},
		"delivery":         {"--delivery", "pigeon"},
		"reserve>=budget":  {"--budget", "1000", "--reserve", "1000"},
		"level with all":   {"--level", "R130"},
		"foreign level":    {"--family", "kv_store", "--level", "t4"},
		"no tasks":         {"--tasks", "0"},
		"seat/model clash": {"--seat", "a", "--model", "b"},
	} {
		if err := runCompactionContextBench(contextBenchArgs(t, append(args, "--endpoint", "http://fake.invalid")...)); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
}

// With nothing listening, every task is infra_error, none is task_fail, and the verb
// says nothing was measured instead of reporting a score.
func TestContextBenchWithNoSeatReachableEveryTaskIsInfraError(t *testing.T) {
	out := filepath.Join(t.TempDir(), "down.json")
	err := runCompactionContextBench(contextBenchArgs(t, "--family", "all", "--tasks", "1", "--endpoint", "http://127.0.0.1:1",
		"--model", "seat-x", "--request-timeout", "5", "--out", out))
	if err == nil || !strings.Contains(err.Error(), "nothing was measured") {
		t.Fatalf("err = %v", err)
	}
	raw, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("the report must still be written: %v", rerr)
	}
	var rep compeval.CBReport
	if jerr := json.Unmarshal(raw, &rep); jerr != nil {
		t.Fatal(jerr)
	}
	if len(rep.Results) != 4 || rep.Counts[compeval.CBOutcomeInfraError] != 4 || rep.Counts[compeval.CBOutcomeTaskFail] != 0 {
		t.Fatalf("counts = %v", rep.Counts)
	}
	for _, r := range rep.Results {
		if r.Reward != nil {
			t.Errorf("%s: an unmeasured task carries no reward", r.ID)
		}
	}
}

func TestContextBenchWithNoEndpointConfiguredIsInfraError(t *testing.T) {
	// The default seat refuses a call with no endpoint, whatever the config says.
	seat := contextBenchSeat("", "m", time.Second)
	if _, err := seat(context.Background(), nil, 1); err == nil {
		t.Fatal("an empty endpoint must be an error, not a silent success")
	}
}

// A budget too small for the instruction and the first operation measures nothing. The
// verb says so up front, before any seat call, instead of reporting a graded failure.
func TestContextBenchRefusesABudgetThatCannotHoldTheFirstOperation(t *testing.T) {
	calls := 0
	withFakeContextBenchSeat(t, func(context.Context, []agent.Msg, int) (string, error) {
		calls++
		return compeval.CBAdvanceMarker, nil
	})
	out := filepath.Join(t.TempDir(), "tiny.json")
	err := runCompactionContextBench(contextBenchArgs(t, "--family", "kv_store", "--budget", "2000", "--reserve", "100",
		"--endpoint", "http://fake.invalid", "--model", "seat-x", "--out", out))
	if err == nil || !strings.Contains(err.Error(), "--budget") {
		t.Fatalf("a budget below the instruction plus the first operation must be refused, err = %v", err)
	}
	if calls != 0 {
		t.Fatalf("the seat was called %d time(s) on a run that cannot hold its first send", calls)
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Fatal("a refused run writes no report")
	}
}

// --delivery, --budget and --reserve reach the run: the report names them, and a budget
// this small makes the ladder compact where the default one would not.
func TestContextBenchDeliveryBudgetAndReserveReachTheRun(t *testing.T) {
	withFakeContextBenchSeat(t, func(context.Context, []agent.Msg, int) (string, error) {
		return compeval.CBAdvanceMarker, nil
	})
	run := func(extra ...string) compeval.CBReport {
		t.Helper()
		out := filepath.Join(t.TempDir(), "r.json")
		args := append(contextBenchArgs(t, "--family", "kv_store", "--level", "R130", "--endpoint", "http://fake.invalid", "--model", "seat-x", "--out", out), extra...)
		if err := runCompactionContextBench(args); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var rep compeval.CBReport
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	roomy := run()
	if roomy.Delivery != compeval.CBDeliveryToolResult || roomy.Budget != compeval.CBDefaultBudget || roomy.Reserve != compeval.CBDefaultReserve {
		t.Fatalf("defaults = %+v", roomy)
	}
	if c := roomy.Results[0].Compactions; c != 0 {
		t.Fatalf("with the default budget nothing is compacted, got %d", c)
	}
	tight := run("--delivery", "user", "--budget", "8000", "--reserve", "500")
	if tight.Delivery != compeval.CBDeliveryUser || tight.Budget != 8000 || tight.Reserve != 500 {
		t.Fatalf("the flags did not reach the report: %+v", tight)
	}
	r := tight.Results[0]
	if r.Compactions == 0 || r.PeakSentTokens == 0 || r.PeakSentTokens > 8000-500 {
		t.Fatalf("an 8000-token budget must compact and never send more than the usable 7500: %+v", r)
	}
}
