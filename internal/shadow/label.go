package shadow

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/dmmdea/local-offload/internal/core"
	"github.com/dmmdea/local-offload/internal/ledger"
)

// LabelDeps holds the injected functions for LabelQueue, so the logic is fully
// unit-testable with fakes and no model or network.
type LabelDeps struct {
	// Escalation is the model alias to run each item through (counterfactual tier).
	Escalation string
	// E2B is the entry-tier E2B model alias ("gemma4-e2b"). Used for B1 router labels.
	E2B string
	// RunTier runs req on the named model and returns (result, ok).
	// It must NOT write to the savings ledger or shadow queue (record=false path).
	RunTier func(ctx context.Context, req core.Request, model string) (core.Result, bool)
	// AnswersAgree judges whether the entry-tier candidate and the escalation
	// tier's output agree on the class field. ok=false means un-judgeable (skip).
	// candidate is the raw EntryOutput string; finalData is the escalation result's Data bytes.
	AnswersAgree func(task string, candidate string, finalData []byte) (bool, bool)
	// Ground checks whether the escalation result's structured output is grounded
	// in the source input. Used for extract tasks. ok=false means not applicable.
	Ground func(task core.TaskType, input string, data []byte) (grounded bool, ok bool)
	// Similar computes the semantic similarity between two strings (0..1).
	// Used for the B2 summarize judge. Injected from judge.Embedder.Similar.
	Similar func(a, b string) (float64, error)
	// SummarizeSimThreshold is the minimum similarity to count as "agreed".
	// Caller sets it; default is 0.80.
	SummarizeSimThreshold float64
	// AppendLabel appends a label entry to the confhead sidecar at path.
	// Must NOT touch the main ledger.
	AppendLabel func(path string, e ledger.Entry) error
	// LabelsPath is the confhead sidecar file (confhead-labels.jsonl).
	LabelsPath string
	// RouterLabelsPath is the router sidecar file for B1 E2B counterfactual labels.
	RouterLabelsPath string
	// Embed returns the embedding vector for an item input (injected from
	// judge.Embedder.Embed). Used to build the kNN entry-tier substrate. nil =
	// kNN substrate building disabled.
	Embed func(text string) ([]float64, error)
	// AppendKNN appends one kNN substrate row (task, vec, E2B-accept). nil =
	// disabled. Must NOT touch the savings ledger.
	AppendKNN func(task string, vec []float64, accept bool) error
}

// summaryText extracts the "summary" field from a JSON object, or returns
// the raw string if the input is not structured JSON with that field.
func summaryText(raw string) string {
	var v struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal([]byte(raw), &v) == nil && v.Summary != "" {
		return v.Summary
	}
	return raw
}

// LabelQueue drains up to cap items from the shadow queue, runs a counterfactual
// escalation tier on each, judges the result, and appends a confhead label row.
// Fail-safe: a single item error is skipped, never aborts the batch.
// Returns the count of labels successfully written.
func LabelQueue(ctx context.Context, items []Item, cap int, d LabelDeps) (written int) {
	for i, it := range items {
		if i >= cap {
			break
		}
		req := core.Request{Task: core.TaskType(it.Task), Input: it.Input, Params: it.Params}
		res, ok := d.RunTier(ctx, req, d.Escalation)
		if !ok {
			continue
		}

		task := strings.ToLower(it.Task)
		entry := ledger.Entry{
			TS:         it.TS,
			Task:       it.Task,
			ModelTier:  it.EntryTier,
			Feat:       it.Feat,
			InputChars: len(it.Input),
		}
		if entry.TS == 0 {
			entry.TS = time.Now().Unix()
		}

		switch task {
		case "classify", "triage":
			agreed, jok := d.AnswersAgree(it.Task, it.EntryOutput, res.Data)
			if !jok {
				continue
			}
			entry.Grounded = nil
			entry.EscalatedAgreed = &agreed

			// A4: capture only enqueues E2B-ENTRY rows, so the router/kNN substrate
			// is fed HERE — from the escalation-agreement (`agreed`) the rerun above
			// already computed, with zero new inference. The label is `agreed`, NOT a
			// constant accept: Escalations==0 means only that the runtime gate passed,
			// not that E2B was correct. The non-E2B B1 block below stays as the
			// negative-region feeder for rows that entered above E2B.
			if it.EntryTier == d.E2B {
				if d.RouterLabelsPath != "" {
					routerEntry := ledger.Entry{
						TS:          entry.TS,
						Task:        it.Task,
						ModelTier:   d.E2B,
						Feat:        it.Feat,
						InputChars:  len(it.Input),
						Escalations: 0,
						Deferred:    false,
						Grounded:    &agreed,
					}
					_ = d.AppendLabel(d.RouterLabelsPath, routerEntry)
				}
				if d.Embed != nil && d.AppendKNN != nil {
					if vec, eerr := d.Embed(it.Input); eerr == nil {
						_ = d.AppendKNN(it.Task, vec, agreed)
					}
				}
			}

		case "extract":
			// extract uses a different (oracle) label than classify/triage: instead of
			// entry-vs-escalation agreement, it labels whether the ESCALATION tier's output
			// is grounded in the input (EntryOutput is not compared here). This trains the
			// confhead, on the entry-tier features, to predict escalation-tier groundedness
			// as the correctness proxy for this task. Intentional asymmetry — not a bug.
			// res.Data is json.RawMessage (= []byte); empty yields empty bytes, which grounding.Check handles.
			dataBytes := []byte(res.Data)
			g, gok := d.Ground(core.TaskType(it.Task), it.Input, dataBytes)
			if !gok {
				continue
			}
			entry.EscalatedAgreed = nil
			entry.Grounded = &g

		case "summarize":
			// B2: judge escalation summary against the stored entry summary via embedding similarity.
			entrySummary := summaryText(it.EntryOutput)
			escSummary := summaryText(string(res.Data))
			if entrySummary == "" || escSummary == "" {
				continue // un-judgeable: no summary text to compare
			}
			sim, err := d.Similar(entrySummary, escSummary)
			if err != nil {
				continue
			}
			agreed := sim >= d.SummarizeSimThreshold
			entry.Grounded = nil
			entry.EscalatedAgreed = &agreed

		default:
			continue
		}

		if err := d.AppendLabel(d.LabelsPath, entry); err != nil {
			continue
		}
		written++

		// B1: for classify/triage rows that entered at a tier OTHER than E2B, run E2B
		// as a counterfactual and synthesize a router training label in the router sidecar.
		// This is a best-effort side effect — failures are silently skipped; the confhead
		// label and written count above are unaffected.
		if (task == "classify" || task == "triage") && d.E2B != "" && it.EntryTier != d.E2B && d.RouterLabelsPath != "" {
			e2bRes, ok := d.RunTier(ctx, req, d.E2B)
			if !ok {
				continue
			}
			agreed, jok := d.AnswersAgree(it.Task, it.EntryOutput, e2bRes.Data)
			if !jok {
				continue
			}
			routerEntry := ledger.Entry{
				TS:          entry.TS,
				Task:        it.Task,
				ModelTier:   d.E2B,
				Feat:        it.Feat,
				InputChars:  len(it.Input),
				Escalations: 0,
				Deferred:    false,
				Grounded:    &agreed,
			}
			_ = d.AppendLabel(d.RouterLabelsPath, routerEntry)

			// kNN substrate: the same E2B-counterfactual agreement is the accept
			// label for a non-E2B-entry input.
			if d.Embed != nil && d.AppendKNN != nil {
				if vec, eerr := d.Embed(it.Input); eerr == nil {
					_ = d.AppendKNN(it.Task, vec, agreed)
				}
			}
		}
	}
	return written
}
