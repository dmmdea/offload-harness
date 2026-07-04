package trajectory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// ReadLabels loads all manufactured labels from the sidecar (corrupt lines skipped).
func ReadLabels(path string) ([]Label, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Label
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var l Label
		if json.Unmarshal([]byte(line), &l) == nil {
			out = append(out, l)
		}
	}
	return out, nil
}

// Label is one manufactured agent-trajectory label: the captured trajectory plus
// the goal-satisfaction judge's verdict. Written to a SIDECAR (never the savings
// ledger) so the flywheel's ledger-pristine invariant holds by construction.
type Label struct {
	Schema      int      `json:"schema"`
	TS          int64    `json:"ts"`
	ID          string   `json:"id"`
	Goal        string   `json:"goal"`
	Envelope    []string `json:"envelope"`
	Tools       []string `json:"tools"`
	Steps       int      `json:"steps"`
	StopReason  string   `json:"stop_reason"`
	Decision    string   `json:"decision"`     // judge verdict: yes | no | unsure
	GoalReached bool     `json:"goal_reached"` // decision == "yes"
	JudgeReason string   `json:"judge_reason,omitempty"`
}

var labelMu sync.Mutex

// AppendLabel appends one label as a JSON line to the sidecar (concurrency-safe).
// Mirrors ledger.AppendLabel's mechanics; it is NOT ledger.Record — the savings
// ledger is never touched.
func AppendLabel(path string, l Label) error {
	if l.TS == 0 {
		l.TS = time.Now().Unix()
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	labelMu.Lock()
	defer labelMu.Unlock()
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}

// LabelDeps are the injected dependencies for LabelQueue — fully fakeable for a
// unit test with no model or filesystem.
type LabelDeps struct {
	// RunTier runs req on the named model (record=false — it must NOT write the
	// savings ledger, cache, or shadow). The judge routes a triage through it.
	RunTier func(ctx context.Context, req core.Request, model string) (core.Result, bool)
	// JudgeModel is the model the goal-satisfaction judge runs on.
	JudgeModel string
	// AppendLabel writes one manufactured label to the sidecar.
	AppendLabel func(path string, l Label) error
	// LabelsPath is the trajectory-label sidecar file.
	LabelsPath string
}

// JudgeGoalReached asks a local triage model whether the agent's final answer
// accomplished the goal — the signal StopReason "done" does NOT provide. Reuses
// the proven triage task + its GBNF grammar via RunTier(record=false); returns
// (decision yes|no|unsure, reason, ok). ok=false ⇒ un-judgeable, caller skips.
func JudgeGoalReached(ctx context.Context, runTier func(context.Context, core.Request, string) (core.Result, bool), model, goal, output string) (decision, reason string, ok bool) {
	input := "GOAL:\n" + goal + "\n\nAGENT'S FINAL ANSWER:\n" + output
	q := "Reading the GOAL and the AGENT'S FINAL ANSWER in the text, did the agent's answer clearly and correctly accomplish the goal? Answer yes only if it did."
	res, rok := runTier(ctx, core.Request{Task: core.TaskTriage, Input: input, Params: map[string]any{"question": q}}, model)
	if !rok || res.Deferred {
		return "", "", false
	}
	var v struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal(res.Data, &v) != nil || v.Decision == "" {
		return "", "", false
	}
	return v.Decision, v.Reason, true
}

// LabelQueue judges up to cap trajectories (cap<=0 = all) for goal satisfaction and
// appends a label per judged item to the sidecar. Fail-safe: an un-judgeable or
// un-writable item is skipped, never aborts the batch. Returns labels written.
func LabelQueue(ctx context.Context, items []Item, cap int, d LabelDeps) (written int) {
	for i, it := range items {
		if cap > 0 && i >= cap {
			break
		}
		if it.Goal == "" {
			continue
		}
		decision, reason, ok := JudgeGoalReached(ctx, d.RunTier, d.JudgeModel, it.Goal, it.Output)
		if !ok {
			continue // un-judgeable → skip, never abort
		}
		l := Label{
			Schema: SchemaVersion, TS: it.TS, ID: it.ID, Goal: it.Goal,
			Envelope: it.Envelope, Tools: it.Tools, Steps: it.Steps, StopReason: it.StopReason,
			Decision: decision, GoalReached: decision == "yes", JudgeReason: reason,
		}
		if d.AppendLabel(d.LabelsPath, l) == nil {
			written++
		}
	}
	return written
}
