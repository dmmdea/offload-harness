// wire.go renders Run's outcome into the ONE response shape both delegator
// surfaces publish (MCP agent_delegate and the CLI verb print the same JSON,
// so operators and scripts read one vocabulary). Summary comes FIRST — roast
// delta 14: eight quiet defers must read as a loud outcome, not eight green
// jobs — and struct FIELD ORDER is what guarantees that in the marshaled
// JSON; a map would alphabetize "results" ahead of "summary".

package delegate

import "encoding/json"

// SummaryWire is the top-of-response tally. Infrastructure is a SUBSET of
// Deferred (the defers whose class blames the stack or the config, not the
// work), published beside it so "one node is broken" cannot hide inside a
// green-looking defer count.
type SummaryWire struct {
	Succeeded          int `json:"succeeded"`
	Deferred           int `json:"deferred"`
	FailedVerification int `json:"failed_verification"`
	Failed             int `json:"failed"`
	Infrastructure     int `json:"infrastructure"`
	// LostToStack is the half of Infrastructure that is LOST WORK: subtasks that
	// came back EMPTY because the stack failed them, as opposed to a successful
	// local placement annotated with "the fleet was down". omitempty — a run
	// that lost nothing publishes byte-identically to before this field existed.
	//
	// It is published rather than kept internal because it is what makes the MCP
	// surface's error flag legible: `infrastructure: 1` beside `succeeded: 1` no
	// longer says on its own whether a subtask was eaten, and the caller reading
	// "this call failed" is owed the count that decided it.
	LostToStack int `json:"lost_to_stack,omitempty"`
	// CorpusRows*/LedgerRows* publish telemetry loss to the CALLER. omitempty:
	// a healthy run's response stays byte-identical to before these fields
	// existed. They are not outcome buckets — the four counts above still add
	// up to len(results) — but a delegation that wrote nothing to the standing
	// corpus published identically to one that wrote everything, and the MCP
	// caller had no way to learn it.
	//
	// The *Attempted counts ride ALONGSIDE their losses — Summary carries them
	// only when there IS a loss, so the byte-identity promise above still holds
	// for a healthy run: "N rows lost" is unreadable without the M it was lost
	// out of, and 1 of 8 is a transient while 8 of 8 is a full disk.
	CorpusRowsLost      int `json:"corpus_rows_lost,omitempty"`
	CorpusRowsAttempted int `json:"corpus_rows_attempted,omitempty"`
	LedgerRowsLost      int `json:"ledger_rows_lost,omitempty"`
	LedgerRowsAttempted int `json:"ledger_rows_attempted,omitempty"`
}

// ResultWire is one subtask's published outcome. Failed marks a
// transport/config failure (distinct from a defer — the node never reported);
// wall_ms is the DELEGATOR-observed round trip, not the node's own wall
// (which stays inside the node's result and the delegation log).
type ResultWire struct {
	Node               string          `json:"node"`
	Seat               string          `json:"seat"`
	Placement          string          `json:"placement"`
	JobID              string          `json:"job_id"`
	Output             string          `json:"output,omitempty"`
	Structured         json.RawMessage `json:"structured,omitempty"`
	Deferred           bool            `json:"deferred"`
	Reason             string          `json:"reason,omitempty"`
	DeferClass         string          `json:"defer_class,omitempty"`
	Failed             bool            `json:"failed,omitempty"`
	AcceptanceFailures []string        `json:"acceptance_failures,omitempty"`
	WallMs             int64           `json:"wall_ms"`
}

// ResponseWire is the full response: summary first, then per-subtask results
// in submission order.
type ResponseWire struct {
	Summary SummaryWire  `json:"summary"`
	Results []ResultWire `json:"results"`
}

// WireResponse shapes Run's raw outcome for publication.
func WireResponse(results []PlacedResult, sum Summary) ResponseWire {
	out := ResponseWire{
		Summary: SummaryWire{
			Succeeded:           sum.Succeeded,
			Deferred:            sum.Deferred,
			FailedVerification:  sum.FailedVerification,
			Failed:              sum.Failed,
			Infrastructure:      sum.Infrastructure,
			LostToStack:         sum.LostToStack,
			CorpusRowsLost:      sum.CorpusRowsLost,
			CorpusRowsAttempted: sum.CorpusRowsAttempted,
			LedgerRowsLost:      sum.LedgerRowsLost,
			LedgerRowsAttempted: sum.LedgerRowsAttempted,
		},
		Results: make([]ResultWire, 0, len(results)),
	}
	for _, pr := range results {
		rw := ResultWire{
			Node:               pr.Node,
			Seat:               pr.Seat,
			Placement:          pr.PlacementReason,
			JobID:              pr.JobID,
			Output:             pr.Result.Output,
			Structured:         pr.Result.Structured,
			Deferred:           pr.Result.Deferred,
			Reason:             pr.Result.Reason,
			DeferClass:         pr.Result.DeferClass,
			AcceptanceFailures: pr.AcceptanceFailures,
			WallMs:             pr.wallMs,
		}
		if pr.Err != "" {
			rw.Failed = true
			rw.Reason = pr.Err
		}
		out.Results = append(out.Results, rw)
	}
	return out
}
