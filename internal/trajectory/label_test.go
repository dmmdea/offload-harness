package trajectory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/local-offload/internal/core"
)

// fakeRunTier returns a triage result with the given decision, or ok=false.
func fakeRunTier(decision string, ok bool) func(context.Context, core.Request, string) (core.Result, bool) {
	return func(_ context.Context, req core.Request, _ string) (core.Result, bool) {
		if !ok {
			return core.Result{}, false
		}
		data, _ := json.Marshal(map[string]string{"decision": decision, "reason": "because"})
		return core.Result{OK: true, Data: data}, true
	}
}

func TestAppendLabelRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "labels.jsonl")
	if err := AppendLabel(p, Label{ID: "g1", Goal: "do", GoalReached: true, Decision: "yes"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"goal_reached":true`) || !strings.Contains(string(b), `"decision":"yes"`) {
		t.Errorf("label not written as expected: %s", b)
	}
	if ts := strings.Count(string(b), "\n"); ts != 1 {
		t.Errorf("expected 1 line; got %d", ts)
	}
}

func TestLabelQueueJudgesAndWrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "labels.jsonl")
	items := []Item{
		{ID: "a", Goal: "list files", Output: "here they are", Tools: []string{"list_dir"}, Steps: 2, StopReason: "done"},
		{ID: "b", Goal: "another", Output: "answer", Steps: 1, StopReason: "done"},
	}
	w := LabelQueue(context.Background(), items, 0, LabelDeps{
		RunTier: fakeRunTier("yes", true), JudgeModel: "m",
		AppendLabel: AppendLabel, LabelsPath: p,
	})
	if w != 2 {
		t.Fatalf("expected 2 labels written; got %d", w)
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), `"goal_reached":true`) != 2 {
		t.Errorf("both should be judged goal_reached=true; got %s", b)
	}
}

func TestLabelQueueSkipsUnjudgeableAndEmptyGoal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "labels.jsonl")
	// RunTier returns ok=false for every item => all un-judgeable => skipped.
	w := LabelQueue(context.Background(), []Item{{ID: "a", Goal: "x", Output: "y"}}, 0, LabelDeps{
		RunTier: fakeRunTier("", false), JudgeModel: "m", AppendLabel: AppendLabel, LabelsPath: p,
	})
	if w != 0 {
		t.Errorf("un-judgeable items must be skipped; wrote %d", w)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("no label file should be written when nothing is judged")
	}
	// An item with an empty goal is skipped without even calling the judge.
	called := false
	w2 := LabelQueue(context.Background(), []Item{{ID: "a", Goal: "", Output: "y"}}, 0, LabelDeps{
		RunTier: func(context.Context, core.Request, string) (core.Result, bool) {
			called = true
			return core.Result{}, false
		},
		JudgeModel: "m", AppendLabel: AppendLabel, LabelsPath: p,
	})
	if w2 != 0 || called {
		t.Errorf("an empty-goal item must be skipped without judging; wrote=%d judged=%v", w2, called)
	}
}

func TestLabelQueueCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "labels.jsonl")
	items := []Item{{ID: "a", Goal: "g", Output: "o"}, {ID: "b", Goal: "g", Output: "o"}, {ID: "c", Goal: "g", Output: "o"}}
	w := LabelQueue(context.Background(), items, 2, LabelDeps{
		RunTier: fakeRunTier("no", true), JudgeModel: "m", AppendLabel: AppendLabel, LabelsPath: p,
	})
	if w != 2 {
		t.Errorf("cap=2 should label only 2; got %d", w)
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), `"goal_reached":false`) != 2 {
		t.Errorf(`decision "no" should record goal_reached=false; got %s`, b)
	}
}
