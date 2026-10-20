// wire.go renders Run's outcome into the ONE response shape both delegator
// surfaces publish (MCP agent_delegate and the CLI verb print the same JSON,
// so operators and scripts read one vocabulary). Summary comes FIRST — roast
// delta 14: eight quiet defers must read as a loud outcome, not eight green
// jobs — and struct FIELD ORDER is what guarantees that in the marshaled
// JSON; a map would alphabetize "results" ahead of "summary".

package delegate

import (
	"encoding/json"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

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
	// LostToStack is the half of Infrastructure that is LOST WORK: subtasks whose
	// CONTRACTED OUTPUT never arrived because the stack failed them, as opposed to
	// a successful local placement annotated with "the fleet was down". omitempty —
	// a run that lost nothing publishes byte-identically to before this field
	// existed.
	//
	// `output` on such a result may be NON-EMPTY, and a reader must not treat this
	// count as "the result is blank": an agent loop that finished and then could
	// not reach the re-pack seat publishes its prose beside
	// defer_class:"infrastructure" with `structured` absent. It still counts as
	// lost, because a contract carrying an output_schema asked for a checkable
	// deliverable and unchecked prose is not one.
	//
	// It is published rather than kept internal because a partial result is no
	// longer error-flagged (C-75): `infrastructure: 1` beside `succeeded: 1` does
	// not say on its own whether a subtask was eaten, and this count is the
	// caller's loud signal for it. The MCP error flag now marks only a call that
	// delivered nothing.
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
	// Retried / RetryRecovered: second attempts on a different node after a
	// failed_verification or abstention, and how many of them recovered.
	// omitempty — a run with no retry publishes byte-identically to before.
	Retried        int `json:"retried,omitempty"`
	RetryRecovered int `json:"retry_recovered,omitempty"`
	// Replaced / ReplacementRecovered: subtasks a node REFUSED at dispatch and
	// the delegator re-placed on another node, and how many of those then
	// reached a node that took the work. omitempty — a run where nothing was
	// refused publishes byte-identically to before this existed.
	//
	// They are published rather than kept internal because a fleet quietly
	// shedding load onto one box is otherwise invisible: every subtask still
	// completes, the four buckets read green, and nothing anywhere says the
	// roster is saturated. `replaced: 6` on a healthy-looking run is the line
	// that sends an operator to fleet_max_queue_depth before the next run has
	// nowhere left to go.
	Replaced             int `json:"replaced,omitempty"`
	ReplacementRecovered int `json:"replacement_recovered,omitempty"`
	Quarantined          int `json:"quarantined,omitempty"`
	Batches              int `json:"batches,omitempty"`
	Skipped              int `json:"skipped,omitempty"`
	// Waited / Shed (0.113.18): subtasks that waited for fleet capacity before a
	// node took them (the capacity wait: the call's deadline less a reserve, else
	// agent_placement_wait_sec, ADR 0073), and
	// sheddable subtasks (priority -1) that found no idle node and were shed
	// (defer_class capacity). omitempty — a run that neither waited nor shed
	// publishes byte-identically to before. `waited: 5` on a green run is the
	// line that says the fleet was full and the wait is what saved the work.
	Waited int `json:"waited,omitempty"`
	Shed   int `json:"shed,omitempty"`
}

// The values of ResultWire.RanOn.
const (
	RanOnLocal = "local"
	RanOnNode  = "node"
)

// ResultWire is one subtask's published outcome. Failed marks a
// transport/config failure (distinct from a defer — the node never reported);
// wall_ms is the DELEGATOR-observed round trip, not the node's own wall
// (which stays inside the node's result and the delegation log).
type ResultWire struct {
	Node string `json:"node"`
	// NodeReported is the id the executing node stamped on its OWN result, present only when it differs (case-insensitively)
	// from Node: Node is the id the placement used (the node_id /fleet/health publishes, the name the placement text carries),
	// so the two differ in two cases: a node on a build before 0.185.0 (it stamps its hostname where it was launched with
	// another --node-id) and a local run attributed to another box's engine (register C-58: the runner stamps its own box, the
	// placement names the engine's). A node on this build stamps the id it advertises, so it never differs there. Absent when
	// the two are the same and when no node took the subtask. Stamped in one place, WireResponse, from NodeReported().
	NodeReported string `json:"node_reported,omitempty"`
	// Seat is the agent seat that ran the subtask.
	Seat string `json:"seat"`
	// Placement is the placement REASON, a string — read as text (council R4) — so the composite decision is
	// published beside it as Placed, never folded into it. Where the subtask RAN is RanOn, not a word in this text:
	// the opencode plugin reads RanOn first and this text only when a result has none.
	Placement string `json:"placement"`
	// RanOn (D-32) says where the subtask RAN, as a closed value: RanOnLocal, the delegator's own seat, or RanOnNode, a
	// fleet node (a route queue answer a claiming node filed reads node). Absent when no seat ran the work (a defer, a shed,
	// a refusal chain, a subtask that never started, a job a node took back or whose record says it never ran, even when the
	// call deadline cut the row afterwards) and on a result from a delegator that predates it. A job the deadline cut while a
	// node still held it reads node: it was placed and dispatched there and may start. It is stamped in one place,
	// WireResponse, from placedWhere: the decision that also writes the hint clause and the pin tallies, so it cannot
	// disagree with them. Placement stays the reason, in words; a reader that wants the location reads this, never the word
	// "local" in that text.
	RanOn string `json:"ran_on,omitempty"`
	// PinReason (ADR 0078) is the closed reason the caller pinned the call under (privacy, locality, measurement,
	// operator). Present only on a call pinned with one; a route local or remote that gave none is a hint, and the
	// placement above opens by saying whether it was honoured or overridden. omitempty — every other result
	// publishes byte-identically to before.
	PinReason string `json:"pin_reason,omitempty"`
	// Placed (ADR 0052, 0.116.0) is the composite placement block: tier,
	// layer, role, seat, device pin, reason, and on a defer the guard that
	// refused. omitempty — a plain box publishes byte-identically to before.
	Placed             *core.Placed    `json:"placed,omitempty"`
	JobID              string          `json:"job_id"`
	Output             string          `json:"output,omitempty"`
	Structured         json.RawMessage `json:"structured,omitempty"`
	Deferred           bool            `json:"deferred"`
	Reason             string          `json:"reason,omitempty"`
	DeferClass         string          `json:"defer_class,omitempty"`
	Failed             bool            `json:"failed,omitempty"`
	AcceptanceFailures []string        `json:"acceptance_failures,omitempty"`
	// AcceptanceAuthority is the authority the contract closed at: "answer", "field" or "grounded", the weakest
	// authority among its checks, present only when every check passed, nothing else failed the result and every check
	// was authority-qualified (contracts/README.md, "Authority-graded checks"). omitempty: a result with an unqualified
	// check, a failure, or no graded answer publishes byte-identically to before.
	AcceptanceAuthority string `json:"acceptance_authority,omitempty"`
	WallMs              int64  `json:"wall_ms"`
	// ContentionWaitSec / AdmissionWaitSec / AdmissionNote carry the agent
	// result's own wait accounting (ADR 0032) up to the delegating caller —
	// without them the CLI/MCP reader could not see that a wait fired at all
	// (found by the parallel-sessions gate, 2026-09-02).
	ContentionWaitSec float64 `json:"contention_wait_sec,omitempty"`
	AdmissionWaitSec  float64 `json:"admission_wait_sec,omitempty"`
	AdmissionNote     string  `json:"admission_note,omitempty"`
	// QueuedMs is the wall the run spent in the busy hold (ADR 0061): waiting
	// on a seat whose engine was working for other requests. Carried to the
	// caller beside the other wait figures, so a slow answer that was queued
	// reads as contention, not as a slow seat.
	QueuedMs int64 `json:"queued_ms,omitempty"`
	// SeatRecoveries / SeatDownWaitSec (ADR 0066): how many times the run's seat
	// went down under it and the run waited for the seat and re-issued the failed
	// step instead of ending, and the wall it spent waiting on a downed seat.
	// Carried to the caller beside the other wait figures, so a slow answer that
	// survived a seat restart reads as one, not as a slow seat.
	SeatRecoveries  int     `json:"seat_recoveries,omitempty"`
	SeatDownWaitSec float64 `json:"seat_down_wait_sec,omitempty"`
	// CoherenceNote carries the node's post-warm coherence probe (register
	// D-118) up to the delegating caller: without it a run that deferred before
	// its wall started, because the seat was emitting NaN tokens, would reach
	// the caller as a bare infrastructure defer with nothing saying the seat
	// itself was tested and failed.
	CoherenceNote string `json:"coherence_note,omitempty"`
	// RetriedOn / RetryNote: the published result is the better of two attempts
	// when a retry ran; the note says what the other attempt did.
	RetriedOn string `json:"retried_on,omitempty"`
	RetryNote string `json:"retry_note,omitempty"`
	// Replacements / ReplacementNote: how many times a node refused this
	// subtask at dispatch before one took it, and which nodes those were with
	// what each of them said.
	Replacements    int    `json:"replacements,omitempty"`
	ReplacementNote string `json:"replacement_note,omitempty"`
	// CapacityWaitSec (0.113.18): how long this subtask waited for a node to
	// have room (the call's deadline less a reserve, else agent_placement_wait_sec) — not charged to timeout_sec.
	CapacityWaitSec float64 `json:"capacity_wait_sec,omitempty"`
	// PlaceKeeping / RetryAfterSec (GPU routing P7): when the capacity wait ended with nothing
	// taking the work, the places in line it stood in (node, what it waited behind, when that is
	// expected to clear) and the soonest of those in seconds. Absent on every other result.
	PlaceKeeping  []PlaceWait `json:"place_keeping,omitempty"`
	RetryAfterSec int         `json:"retry_after_sec,omitempty"`
	// PollNote (register D-116) names the bound the DELEGATOR polled this
	// subtask at and where that number came from: "sized from <node>'s
	// seat_rate X tok/s: N s", "the node's own wall N s", or "cap: no seat
	// rate advertised". Present only for a contract the caller left unsized
	// (timeout_auto) — one that named its own timeout_sec is polled at
	// timeout_sec + grace and publishes nothing here, byte-identically to
	// before. Delegator-side: it is NOT a field on core.AgentWireResult.
	PollNote string `json:"poll_note,omitempty"`
	// Calls (register D-99) is the node's per-completion record for this
	// subtask — finish reason, token counts, thinking_off, sampling — bounded
	// to the LAST wireCallsMax completions (the final answer and the re-pack
	// are what a reader wants; a 12-step run records more than that). Before
	// this field a delegator could read the record only from the node's own
	// GET /fleet/jobs/{id} with the fleet token, although the tool description
	// had promised results[].calls. omitempty: a node that recorded nothing
	// publishes a row byte-identical to before.
	Calls []core.AgentCallRecord `json:"calls,omitempty"`
	// AcceptanceLint carries the intake lint's warnings for THIS subtask's
	// acceptance (delegate.LintAcceptance): parrot-passable / ungrounded /
	// shape-only. Warn-only — the run above happened regardless. It rides the
	// response so the CALLER (usually a model mid-session) sees it beside the
	// result it weakens, and can fix the acceptance before the next call.
	AcceptanceLint []string `json:"acceptance_lint,omitempty"`
	// A1 config pins, passed through from the node's wire result (see
	// core.AgentWireResult) so a caller can verify WHAT served its subtask
	// without opening the delegation log. Absent = the node predates pinning:
	// unknown, never a value.
	HarnessVersion     string `json:"harness_version,omitempty"`
	HarnessBuildSHA256 string `json:"harness_build_sha256,omitempty"`
	SeatConfigSHA256   string `json:"seat_config_sha256,omitempty"`
	SeatConfigBasis    string `json:"seat_config_basis,omitempty"`
	// Steps / StopReason / StopNote / OutputTruncated (0.115.20): the node's
	// loop accounting, passed through from core.AgentWireResult. Without them
	// a `done` that the forced final step extracted at the step cap (D-89,
	// stop_note "forced final answer: …") and a `length`-cut partial were
	// indistinguishable from a voluntary, complete answer unless the caller
	// opened the delegation log — the D-89 acceptance run read as "all four
	// legs answered" with no trace of WHICH ones the cap forced. omitempty:
	// a pre-0.115.20 node's result publishes as before.
	Steps           int    `json:"steps,omitempty"`
	StopReason      string `json:"stop_reason,omitempty"`
	StopNote        string `json:"stop_note,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
	// Wall sizing (0.115.21, register D-03), passed through from the node: the
	// run's measured decode rate, the wall the node estimated the contract
	// needed on that seat, the retry floor it implies, and the arithmetic.
	SeatTokS        float64 `json:"seat_tok_s,omitempty"`
	WallEstimateSec int     `json:"wall_estimate_sec,omitempty"`
	MinTurnSec      int     `json:"min_turn_sec,omitempty"`
	WallNote        string  `json:"wall_note,omitempty"`
	// WallSec (register D-03): the wall the node ran under when IT sized it (a
	// timeout_auto contract); omitted when the contract named its own timeout_sec.
	WallSec int `json:"wall_sec,omitempty"`
	// Re-pack accounting (0.115.23, register D-91), passed through.
	RepackMs       int64  `json:"repack_ms,omitempty"`
	RepackAttempts int    `json:"repack_attempts,omitempty"`
	RepackNote     string `json:"repack_note,omitempty"`
	// RepackAttemptsDetail (register C-80) is the node's record of each re-pack
	// attempt (lane, max_tokens, tokens generated, finish reason, a clip of what the
	// seat wrote, why it failed or was skipped), passed through bounded to the first
	// wireRepackAttemptsMax: the earliest attempt shows what went wrong, and a node
	// sends at most three and a skip. Before it, only the node's own job result and
	// the delegation log held it, so the caller who asked what a failed re-pack did
	// could not read it. omitempty: a node that predates the field publishes a row
	// byte-identical to before.
	RepackAttemptsDetail []core.AgentRepackAttempt `json:"repack_attempts_detail,omitempty"`
	// Final-budget fit and the list-cap re-issue (0.122.1, register D-95),
	// passed through from the node. Without them a caller sizing its next
	// contract cannot tell an answer that fitted from one the wall narrowed,
	// nor a partial that was asked again from one that never was.
	FinalBudgetFit int    `json:"final_budget_fit,omitempty"`
	BudgetNote     string `json:"budget_note,omitempty"`
	FinalReissue   string `json:"final_reissue,omitempty"`
	// The write door (0.122.0, register D-06), passed through from the node.
	// Diff is a unified patch the CALLER applies — the harness never does —
	// DiffFiles names the paths it touches, and WriteNote says what the door
	// did when there is no diff to read. All omitempty: a read-only
	// delegation publishes byte-identically to before the door existed.
	Diff      string   `json:"diff,omitempty"`
	DiffFiles []string `json:"diff_files,omitempty"`
	WriteNote string   `json:"write_note,omitempty"`
	// QuoteCheck (register F4) is the research lane's finding about this digest's
	// `quotes` against the page it was written from. Only offload_research and the
	// research verb set it, after the engine returns; every other door publishes
	// no such block, byte-identically to before.
	QuoteCheck *QuoteCheck `json:"quote_check,omitempty"`
}

// QuoteCheck reports what the delegator found when it looked each item of a
// research digest's `quotes` up in the page the digest was written from. A quote
// is the one field of a digest that promises the page's own words, and a seat
// that paraphrases it hands a fact-checker something no search of the page can
// find (register F4). The check never fails a digest: an acceptance failure
// discards a whole run (register C15), so a quote that is not on the page is
// FLAGGED here and left where the seat put it.
type QuoteCheck struct {
	// Checked counts the non-empty quotes looked up.
	Checked int `json:"checked"`
	// Verbatim counts the quotes that are the page's own bytes.
	Verbatim int `json:"verbatim"`
	// Corrected counts the quotes found on the page only after case, spacing,
	// quote-mark and dash differences were set aside; each was replaced in the
	// published digest by the page's own span.
	Corrected int `json:"corrected,omitempty"`
	// Unverified lists the quotes found nowhere on the page, exactly as the seat
	// wrote them. They stay in the digest's `quotes`.
	Unverified []string `json:"unverified,omitempty"`
	// Note says in one line what the Unverified list means; empty when there is none.
	Note string `json:"note,omitempty"`
}

// ResponseWire is the full response: summary first, then per-subtask results
// in submission order.
type ResponseWire struct {
	Summary SummaryWire  `json:"summary"`
	Results []ResultWire `json:"results"`
}

// lintFor is bounds-safe indexing into the per-subtask lint slice.
func lintFor(lints [][]string, i int) []string {
	if i < len(lints) {
		return lints[i]
	}
	return nil
}

// NodeReported is the id the executing node stamped on its own result, when that
// differs from the node the placement used (pr.Node); "" otherwise. See
// ResultWire.NodeReported.
func NodeReported(pr PlacedResult) string {
	id := pr.Result.NodeID
	if pr.Node == "" || strings.EqualFold(id, pr.Node) {
		return ""
	}
	return id
}

// wireCallsMax bounds ResultWire.Calls (register D-99): the last eight
// completions cover a 12-step run's tail — its tool steps, the final answer,
// a structured re-pack — without republishing every planner turn.
const wireCallsMax = 8

// wireRepackAttemptsMax bounds ResultWire.RepackAttemptsDetail (register C-80): a
// node records one entry per request sent (at most three: two grammar attempts and
// the chat lane) and one for the attempt it skipped, so the bound never cuts what a
// current node sends and only keeps a row from a future one from growing without
// limit.
const wireRepackAttemptsMax = 4

// firstRepackAttempts returns the leading n records of attempts (all of them when
// there are fewer), nil when there are none so omitempty holds.
func firstRepackAttempts(attempts []core.AgentRepackAttempt, n int) []core.AgentRepackAttempt {
	if len(attempts) == 0 {
		return nil
	}
	if len(attempts) > n {
		attempts = attempts[:n]
	}
	out := make([]core.AgentRepackAttempt, len(attempts))
	copy(out, attempts)
	return out
}

// lastCalls returns the trailing n records of calls (all of them when there
// are fewer), nil when there are none so omitempty holds.
func lastCalls(calls []core.AgentCallRecord, n int) []core.AgentCallRecord {
	if len(calls) == 0 {
		return nil
	}
	if len(calls) > n {
		calls = calls[len(calls)-n:]
	}
	out := make([]core.AgentCallRecord, len(calls))
	copy(out, calls)
	return out
}

// WireResponse shapes Run's raw outcome for publication. lints is the intake
// lint per subtask, indexed in SUBMISSION order (the same order results keep);
// nil, or a shorter slice, simply leaves the remaining results unlinted —
// absence of a warning must never be manufactured into an error.
func WireResponse(results []PlacedResult, sum Summary, lints [][]string) ResponseWire {
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
			Retried:             sum.Retried,
			RetryRecovered:      sum.RetryRecovered,

			Replaced:             sum.Replaced,
			ReplacementRecovered: sum.ReplacementRecovered,
			Quarantined:          sum.Quarantined,
			Waited:               sum.Waited,
			Shed:                 sum.Shed,
			Batches:              sum.Batches,
			Skipped:              sum.Skipped,
		},
		Results: make([]ResultWire, 0, len(results)),
	}
	for i, pr := range results {
		rw := ResultWire{
			Node:                pr.Node,
			Seat:                pr.Seat,
			Placement:           pr.PlacementReason,
			PinReason:           pr.PinReason,
			Placed:              pr.Placed,
			JobID:               pr.JobID,
			Output:              pr.Result.Output,
			Structured:          pr.Result.Structured,
			Deferred:            pr.Result.Deferred,
			Reason:              pr.Result.Reason,
			DeferClass:          pr.Result.DeferClass,
			AcceptanceFailures:  pr.AcceptanceFailures,
			AcceptanceAuthority: publishedAuthority(pr.AcceptanceAuthority),
			WallMs:              pr.wallMs,
			ContentionWaitSec:   pr.Result.ContentionWaitSec,
			AdmissionWaitSec:    pr.Result.AdmissionWaitSec,
			AdmissionNote:       pr.Result.AdmissionNote,
			QueuedMs:            pr.Result.QueuedMs,
			SeatRecoveries:      pr.Result.SeatRecoveries,
			SeatDownWaitSec:     pr.Result.SeatDownWaitSec,
			CoherenceNote:       pr.Result.CoherenceNote,
			RetriedOn:           pr.RetriedOn,
			RetryNote:           pr.RetryNote,
			Replacements:        pr.Replacements,
			ReplacementNote:     pr.ReplacementNote,
			CapacityWaitSec:     pr.CapacityWaitSec,
			PlaceKeeping:        pr.PlaceKeeping,
			RetryAfterSec:       pr.RetryAfterSec,
			PollNote:            pr.PollNote,
			Calls:               lastCalls(pr.Result.Calls, wireCallsMax),
			AcceptanceLint:      lintFor(lints, i),
			HarnessVersion:      pr.Result.HarnessVersion,
			HarnessBuildSHA256:  pr.Result.HarnessBuildSHA256,
			SeatConfigSHA256:    pr.Result.SeatConfigSHA256,
			SeatConfigBasis:     pr.Result.SeatConfigBasis,
			Steps:               pr.Result.Steps,
			StopReason:          pr.Result.StopReason,
			StopNote:            pr.Result.StopNote,
			OutputTruncated:     pr.Result.OutputTruncated,
			SeatTokS:            pr.Result.SeatTokS,
			WallEstimateSec:     pr.Result.WallEstimateSec,
			MinTurnSec:          pr.Result.MinTurnSec,
			WallSec:             pr.Result.WallSec,
			WallNote:            pr.Result.WallNote,
			RepackMs:            pr.Result.RepackMs,
			RepackAttempts:      pr.Result.RepackAttempts,
			RepackNote:          pr.Result.RepackNote,
			FinalBudgetFit:      pr.Result.FinalBudgetFit,
			BudgetNote:          pr.Result.BudgetNote,
			FinalReissue:        pr.Result.FinalReissue,
			Diff:                pr.Result.Diff,
			DiffFiles:           pr.Result.DiffFiles,
			WriteNote:           pr.Result.WriteNote,
		}
		// Set beside, not inside, the literal above: a key longer than every other would
		// re-align the whole block.
		rw.RepackAttemptsDetail = firstRepackAttempts(pr.Result.RepackAttemptsDetail, wireRepackAttemptsMax)
		// Where it ran (D-32): the one decision that already writes the hint clause and the pin tallies. A result no
		// seat ran (placedNowhere) keeps the key off the row.
		switch placedWhere(pr) {
		case placedLocal:
			rw.RanOn = RanOnLocal
		case placedRemote:
			rw.RanOn = RanOnNode
		}
		// The id the node's own result named it by, when that is not the one the placement used (Node above).
		rw.NodeReported = NodeReported(pr)
		if pr.Err != "" {
			rw.Failed = true
			rw.Reason = pr.Err
		}
		out.Results = append(out.Results, rw)
	}
	return out
}
