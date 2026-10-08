package delegate

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// batchedLocalRun runs n contracts through RunBatched on a local seat that answers each with its own goal, and
// returns what a caller of RunBatched sees plus how many times the seat was asked.
func batchedLocalRun(t *testing.T, n int) ([]PlacedResult, Summary, int64) {
	t.Helper()
	var calls atomic.Int64
	var mu sync.Mutex
	var seen []string
	local := LocalRunner(func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		calls.Add(1)
		mu.Lock()
		seen = append(seen, c.Goal)
		mu.Unlock()
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: c.Goal}, nil
	})
	cs := make([]core.AgentContract, n)
	for i := range cs {
		cs[i] = core.AgentContract{Goal: fmt.Sprintf("g%d", i)}
	}
	res, sum, err := RunBatched(context.Background(), testCfg(t), local, cs, "local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != n || sum.Succeeded != n {
		t.Fatalf("results=%d succeeded=%d, want %d of each", len(res), sum.Succeeded, n)
	}
	for i, r := range res {
		if r.Result.Output != fmt.Sprintf("g%d", i) {
			t.Fatalf("result %d out of order: %q", i, r.Result.Output)
		}
	}
	return res, sum, calls.Load()
}

// TestRunBatchedDealsUpToMaxBatchSubtasksAsOneBatch: a 12-page research call is ONE joint deal (ADR 0076). It used to be
// cut into chunks of eight that ran strictly one after the other, so the second waited on the slowest page of the first.
// The edges are the bound itself: 16 is one batch, 17 is the first call that is cut.
func TestRunBatchedDealsUpToMaxBatchSubtasksAsOneBatch(t *testing.T) {
	for _, n := range []int{MaxSubtasks + 1, 12, MaxBatchSubtasks} {
		_, sum, calls := batchedLocalRun(t, n)
		if sum.Batches != 1 || calls != int64(n) {
			t.Errorf("%d contracts: batches=%d seat calls=%d, want one batch and %d calls", n, sum.Batches, calls, n)
		}
	}
}

// TestRunBatchedStillCutsAListLongerThanTheBatchBound: past 16 the list is consecutive deals of 16, in order, with the
// counters summed.
func TestRunBatchedStillCutsAListLongerThanTheBatchBound(t *testing.T) {
	for n, wantBatches := range map[int]int{MaxBatchSubtasks + 1: 2, 2 * MaxBatchSubtasks: 2, 2*MaxBatchSubtasks + 1: 3} {
		_, sum, calls := batchedLocalRun(t, n)
		if sum.Batches != wantBatches || calls != int64(n) {
			t.Errorf("%d contracts: batches=%d seat calls=%d, want %d batches and %d calls", n, sum.Batches, calls, wantBatches, n)
		}
	}
}

// TestRunStillRefusesMoreThanMaxSubtasks: the bound of eight is the MCP door's and the CLI verb's, and it did not move
// when RunBatched's did. RunWith is the entry both use.
func TestRunStillRefusesMoreThanMaxSubtasks(t *testing.T) {
	cs := make([]core.AgentContract, MaxSubtasks+1)
	for i := range cs {
		cs[i] = core.AgentContract{Goal: "g"}
	}
	_, _, err := Run(context.Background(), testCfg(t), nil, cs, "local", nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds the max of 8") {
		t.Fatalf("Run must keep its cap; got %v", err)
	}
	_, _, err = RunWith(context.Background(), testCfg(t), nil, cs, "local", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds the max of 8") {
		t.Fatalf("RunWith must keep its cap; got %v", err)
	}
}

func TestRunBatchedReturnsPartialResultsWithTheError(t *testing.T) {
	cs := make([]core.AgentContract, MaxBatchSubtasks+2)
	for i := range cs {
		cs[i] = core.AgentContract{Goal: fmt.Sprintf("g%d", i)}
	}
	local := LocalRunner(func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: c.Goal}, nil
	})
	// route "queue" bypasses the runner and errors without a holder in the test
	// config — use a bad route on the SECOND chunk only by cancelling the ctx.
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int64
	localCancelAtLastOfFirstChunk := LocalRunner(func(c context.Context, ac core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		if n.Add(1) == MaxBatchSubtasks {
			cancel() // the second chunk starts with a dead ctx
		}
		return local(c, ac, LocalOptions{})
	})
	res, sum, err := RunBatched(ctx, testCfg(t), localCancelAtLastOfFirstChunk, cs, "local", nil, nil)
	if len(res) < MaxBatchSubtasks {
		t.Fatalf("the first chunk's results must be returned, got %d (err=%v)", len(res), err)
	}
	if sum.Batches < 1 {
		t.Fatalf("batches=%d", sum.Batches)
	}
	// A cancelled ctx on the second chunk is not a top-level Run error today (the
	// subtask defers instead), so Skipped stays 0 here; the arithmetic itself is
	// pinned by TestRunBatchedCountsNeverAttemptedSubtasks.
	_ = err
}

func TestAddSummaryCoversEveryIntField(t *testing.T) {
	var one Summary
	v := reflect.ValueOf(&one).Elem()
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() == reflect.Int {
			v.Field(i).SetInt(1)
		}
	}
	sum := addSummary(one, one)
	sv := reflect.ValueOf(sum)
	for i := 0; i < sv.NumField(); i++ {
		if sv.Field(i).Kind() == reflect.Int && sv.Field(i).Int() != 2 {
			t.Fatalf("addSummary drops field %s", sv.Type().Field(i).Name)
		}
	}
}

func TestRunBatchedCountsNeverAttemptedSubtasks(t *testing.T) {
	// A bad route is a top-level error on the FIRST chunk: no results, and every
	// one of the subtasks counts as skipped so delegateIsError sees the loss —
	// the later chunks of a list past the batch bound included.
	for _, n := range []int{12, MaxBatchSubtasks + 4} {
		cs := make([]core.AgentContract, n)
		for i := range cs {
			cs[i] = core.AgentContract{Goal: "g"}
		}
		res, sum, err := RunBatched(context.Background(), testCfg(t), nil, cs, "no-such-route", nil, nil)
		if err == nil || len(res) != 0 {
			t.Fatalf("%d subtasks: want a top-level error and no results, got err=%v res=%d", n, err, len(res))
		}
		if sum.Skipped != n || sum.Batches != 1 {
			t.Fatalf("%d subtasks: skipped=%d batches=%d, want %d/1", n, sum.Skipped, sum.Batches, n)
		}
	}
}

func TestWireResponseCarriesWaitFieldsAndBatchCounters(t *testing.T) {
	pr := PlacedResult{Result: core.AgentWireResult{ContentionWaitSec: 3, AdmissionWaitSec: 6, AdmissionNote: "budget spent while other:starting"}}
	w := WireResponse([]PlacedResult{pr}, Summary{Quarantined: 1, Batches: 2, Skipped: 4}, [][]string{nil})
	if w.Results[0].ContentionWaitSec != 3 || w.Results[0].AdmissionWaitSec != 6 || w.Results[0].AdmissionNote == "" {
		t.Fatalf("wait fields must reach the wire: %+v", w.Results[0])
	}
	if w.Summary.Quarantined != 1 || w.Summary.Batches != 2 || w.Summary.Skipped != 4 {
		t.Fatalf("batch/quarantine counters must reach the wire: %+v", w.Summary)
	}
}
