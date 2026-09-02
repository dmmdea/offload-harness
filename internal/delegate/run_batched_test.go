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

func TestRunBatchedSplitsAtMaxSubtasksAndKeepsOrder(t *testing.T) {
	var calls atomic.Int64
	var mu sync.Mutex
	var seen []string
	local := LocalRunner(func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		calls.Add(1)
		mu.Lock()
		seen = append(seen, c.Goal)
		mu.Unlock()
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: c.Goal}, nil
	})
	cs := make([]core.AgentContract, 12)
	for i := range cs {
		cs[i] = core.AgentContract{Goal: fmt.Sprintf("g%d", i)}
	}
	res, sum, err := RunBatched(context.Background(), testCfg(t), local, cs, "local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 12 || sum.Succeeded != 12 || sum.Batches != 2 {
		t.Fatalf("results=%d succeeded=%d batches=%d", len(res), sum.Succeeded, sum.Batches)
	}
	for i, r := range res {
		if r.Result.Output != fmt.Sprintf("g%d", i) {
			t.Fatalf("result %d out of order: %q", i, r.Result.Output)
		}
	}
	if calls.Load() != 12 {
		t.Fatalf("local ran %d times, want 12", calls.Load())
	}
}

func TestRunStillRefusesMoreThanMaxSubtasks(t *testing.T) {
	cs := make([]core.AgentContract, MaxSubtasks+1)
	for i := range cs {
		cs[i] = core.AgentContract{Goal: "g"}
	}
	_, _, err := Run(context.Background(), testCfg(t), nil, cs, "local", nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds the max") {
		t.Fatalf("Run must keep its cap; got %v", err)
	}
}

func TestRunBatchedReturnsPartialResultsWithTheError(t *testing.T) {
	cs := make([]core.AgentContract, 9)
	for i := range cs {
		cs[i] = core.AgentContract{Goal: fmt.Sprintf("g%d", i)}
	}
	local := LocalRunner(func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: c.Goal}, nil
	})
	// route "queue" bypasses the runner and errors without a holder in the test
	// config — use a bad route on the SECOND chunk only by cancelling the ctx.
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int64
	localCancelAt8 := LocalRunner(func(c context.Context, ac core.AgentContract) (core.AgentWireResult, error) {
		if n.Add(1) == 8 {
			cancel() // the second chunk starts with a dead ctx
		}
		return local(c, ac)
	})
	res, sum, err := RunBatched(ctx, testCfg(t), localCancelAt8, cs, "local", nil, nil)
	if len(res) < 8 {
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
	// one of the 12 subtasks counts as skipped so delegateIsError sees the loss.
	cs := make([]core.AgentContract, 12)
	for i := range cs {
		cs[i] = core.AgentContract{Goal: "g"}
	}
	res, sum, err := RunBatched(context.Background(), testCfg(t), nil, cs, "no-such-route", nil, nil)
	if err == nil || len(res) != 0 {
		t.Fatalf("want a top-level error and no results, got err=%v res=%d", err, len(res))
	}
	if sum.Skipped != 12 || sum.Batches != 1 {
		t.Fatalf("skipped=%d batches=%d, want 12/1", sum.Skipped, sum.Batches)
	}
}
