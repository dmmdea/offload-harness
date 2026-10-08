// pin.go: a placement pin needs a reason; without one it is a hint (ADR 0078).
//
// route=local and route=remote were hard pins: the engine obeyed them whatever the fleet looked like and kept no
// record of why a caller narrowed placement. On 2026-10-07 a session passed route:"local" to a 78-page
// offload_research call; 46 ledger rows read `route=local forced`, and 32 pages ended `not started: the call ended
// first` because the whole fan-out queued on one seat while four fleet nodes had free workers.
//
// The doors that offer pin_reason (agent_delegate, offload_research and their CLI verbs) now treat a local or remote
// route in one of two ways. With a pin_reason from the closed set below it stays authoritative, exactly as before,
// and the reason is recorded on the ledger row and the result. Authoritative runs to the end of the subtask: a reasoned
// local pin is never retried on another node, and a reasoned remote pin's verification or seat-down retry goes to
// another fleet node or is not run, and never lands on this box's seat (remotePinned). Without a reason it is a HINT
// that placement may override:
//
//   - a reasonless local is placed as route=auto places it (ADR 0076): the idle local seat takes the first `room`
//     subtasks of its run-cap line, the overflow goes through the unchanged remoteEligible gate to the remotes with
//     room, and with none to the capacity wait. A small call therefore still stays local, which is what the caller
//     hinted;
//   - a reasonless remote is placed remotes-first, as the remote route places it (the deal is dealt as if the local
//     seat were busy), and where remote would have deferred for lack of an eligible remote with room, the local seat
//     may run the subtask instead.
//
// Every result's placement reason and every ledger row says in plain words whether the hint was honoured or
// overridden (hintClause), and the server counts both (PinTally), which offload_status publishes.
//
// Only the doors that offer pin_reason opt in (RunOptions.PinNeedsReason). The callers that hand the engine a bare
// route and have no reason channel keep the authoritative pin they always had: fleet-smoke names one node and must
// measure that node, the review lane's fenced fallthrough uses route=remote precisely so that nothing runs on the
// fenced local seat, and offload_ask / agent_run pass their own route through. The ones that run inside the MCP server and
// reach the engine (the review lane's fallthrough, and the remote route of offload_ask and agent_run) are counted as
// unreasoned, so that part of the gap is visible in offload_status. An explicit route local on offload_ask or agent_run
// runs on this box's seat without reaching the engine, and is counted nowhere.

package delegate

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The closed set of pin reasons (ADR 0078). A reason says why a caller may not let placement choose.
const (
	// PinPrivacy: the material may not leave this box (credentials, unreleased or brand-isolated data). Local only.
	PinPrivacy = "privacy"
	// PinLocality: the work needs something only this box has (a local service, files the contract cannot carry
	// inline, this box's own seat as the thing under comparison). Local only.
	PinLocality = "locality"
	// PinMeasurement: the call measures a specific place (a benchmark, a bake, a seat A/B). Local or remote.
	PinMeasurement = "measurement"
	// PinOperator: the operator named this placement for this call. Local or remote.
	PinOperator = "operator"
)

// pinReasonSpec is one member of the closed set and the routes it is valid with.
type pinReasonSpec struct {
	reason string
	remote bool // also valid with route remote (every reason is valid with route local)
}

// pinReasonSpecs is the closed set, in the order the errors and the schemas list it.
var pinReasonSpecs = []pinReasonSpec{
	{PinPrivacy, false},
	{PinLocality, false},
	{PinMeasurement, true},
	{PinOperator, true},
}

// PinReasons is the closed set of pin_reason values, in the order the tool schemas list them.
func PinReasons() []string {
	out := make([]string, len(pinReasonSpecs))
	for i, s := range pinReasonSpecs {
		out[i] = s.reason
	}
	return out
}

// pinReasonList renders the closed set with the routes each is valid with, for the error that refuses a value.
func pinReasonList() string {
	parts := make([]string, len(pinReasonSpecs))
	for i, s := range pinReasonSpecs {
		routes := "route local"
		if s.remote {
			routes = "route local or remote"
		}
		parts[i] = s.reason + " (" + routes + ")"
	}
	return strings.Join(parts, ", ")
}

// CheckPinReason refuses a pin_reason that is not in the closed set, one given with the wrong route, and one given
// with a route that is not a pin at all, before any placement is made. route is the call's route after its door's
// default ("" is auto). An empty reason is always fine here: whether a bare local or remote is a hint is the door's
// business (RunOptions.PinNeedsReason), not a validation error.
//
// The error lists the valid set, because a model that guesses a reason needs the list and not a bare refusal. The
// MCP doors and the CLI verbs call it before they spend anything (a research call's page fetch, a delegate call's
// context_paths reads), and runWith calls it again as the engine's own guard.
func CheckPinReason(route, reason string) error {
	if reason == "" {
		return nil
	}
	if route == "" {
		route = "auto"
	}
	var spec pinReasonSpec
	known := false
	for _, s := range pinReasonSpecs {
		if s.reason == reason {
			spec, known = s, true
			break
		}
	}
	switch {
	case !known:
		return fmt.Errorf("delegate: pin_reason %q is not in the closed set; valid: %s", reason, pinReasonList())
	case route != "local" && route != "remote":
		return fmt.Errorf("delegate: pin_reason %q was given with route %s, and only a pin can have a reason: pin the call with route local or remote, or drop pin_reason; valid: %s", reason, route, pinReasonList())
	case route == "remote" && !spec.remote:
		return fmt.Errorf("delegate: pin_reason %q is valid with route local only, and this call's route is remote; valid: %s", reason, pinReasonList())
	}
	return nil
}

// pinCall is what the caller asked of placement with route local or remote, resolved once at intake (resolvePin).
type pinCall struct {
	// asked is "local" or "remote" when the caller named one of them, "" for auto, spread and queue.
	asked string
	// reason is the closed pin_reason, "" when none was given.
	reason string
	// hint is true when asked is a hint: no reason, through a door that offers one (RunOptions.PinNeedsReason).
	hint bool
}

// resolvePin validates the call's pin_reason against its route (CheckPinReason) and decides what the engine applies.
// route is the call's route after defaulting and after the route switch recognised it. The returned route is the
// one the rest of the run places by: a reasonless local or remote through a door that offers pin_reason is a hint
// and is placed as auto (a remote hint with the local seat dealt as busy, see remoteHint), everything else is
// placed by the route the caller named.
func resolvePin(route string, opts *RunOptions) (pinCall, string, error) {
	var reason string
	var needsReason bool
	if opts != nil {
		reason, needsReason = opts.PinReason, opts.PinNeedsReason
	}
	if err := CheckPinReason(route, reason); err != nil {
		return pinCall{}, route, err
	}
	pc := pinCall{reason: reason}
	if route == "local" || route == "remote" {
		pc.asked = route
		pc.hint = reason == "" && needsReason
	}
	if pc.hint {
		route = "auto"
	}
	return pc, route, nil
}

// anyBrowse reports whether any subtask asks for the browse tool, which drives this machine's own browser (ADR 0060).
func anyBrowse(subtasks []core.AgentContract) bool {
	for _, c := range subtasks {
		if c.AllowBrowse {
			return true
		}
	}
	return false
}

// remoteHint reports a reasonless route=remote: placed remotes-first, with the local seat dealt as unavailable
// (the same forcing the remote route has always applied), but, unlike the remote route, allowed to take what no
// remote can. It rides the auto machinery, whose "busy local seat, no eligible remote" outcome is exactly that
// fallback, so the run's route is auto and this flag is the only thing that keeps the idle seat from winning first.
func (r *runner) remoteHint() bool { return r.pin.hint && r.pin.asked == "remote" }

// remotePinned reports a REASONED route=remote: the caller gave a pin_reason, so the call is authoritative and runs on a
// fleet node and on no other place, and that holds for its second chance as much as for its first (alternativeNode).
// It is the reason that makes the pin one this box obeys to the end, because a reasoned pin is what the ledger rows and
// offload_status say placement obeyed. It is not the route: the bare remote route of a caller with no reason channel
// (fleet-smoke, the review lane's fallthrough, offload_ask, agent_run) keeps the retry it has always had, remote -> local,
// and a reasonless remote hint is placed as auto and may retry on the seat (ADR 0078 decisions 3 and 5).
func (r *runner) remotePinned() bool { return r.pin.reason != "" && r.pin.asked == "remote" }

// remotePinWhy is the words a reasoned remote pin adds to a retry that has nowhere to go, or to a retry that went to a
// fleet node, so the note names the pin and not only the absence of a node.
func (r *runner) remotePinWhy() string {
	return "route=remote is pinned (pin_reason " + r.pin.reason + "), which places nothing on the local seat"
}

// queuedLocalReason is the placement reason of a subtask that runs on the local seat because no remote could take
// it. A remote hint never read the seat busy (it dealt the seat as unavailable by preference), so it says what
// happened instead of "local busy".
func (r *runner) queuedLocalReason(why string) string {
	if r.remoteHint() {
		return "no eligible remote — " + why + " (a remote hint falls back to the local seat)"
	}
	return "local busy; no eligible remote — " + why + " (queued-local beats ineligible-remote)"
}

// PinTally is the pin accounting offload_status publishes: how many subtasks callers pinned under each reason, how
// many came as hints and how many of those placement overrode. It belongs to one MCP server, not to the package: the
// server makes one at start (NewPinTally) and hands it to every door it runs the engine through
// (RunOptions.PinTally), so the window it describes is exactly "since this server started" and nothing scans the
// ledger to build it. A call that carries none is not counted. Counts are in subtasks (published results).
type PinTally struct {
	mu         sync.Mutex
	since      time.Time
	reasoned   map[string]int
	hints      int
	overridden int
	unreasoned int
}

// NewPinTally starts an empty accounting whose window opens now.
func NewPinTally() *PinTally {
	return &PinTally{since: time.Now(), reasoned: map[string]int{}}
}

// PinStats is a copy of a PinTally.
type PinStats struct {
	// Since is when the tally started, the start of the window the counts cover.
	Since time.Time
	// Reasoned counts the subtasks of calls pinned under each reason; every reason of the closed set is present.
	Reasoned map[string]int
	// Hints counts the subtasks of calls whose local or remote route had no pin_reason and was placed as a hint, and
	// HintsOverridden how many of those placement did not honour (a local hint placed on a remote node, a remote hint
	// placed on the local seat). A hint nothing ran stays in Hints and in neither.
	Hints           int
	HintsOverridden int
	// Unreasoned counts the subtasks of calls that pinned placement through a caller with no pin_reason channel and
	// reached the engine (the review lane's remote fallthrough and the remote route of offload_ask and agent_run; an
	// explicit local route on those two runs on this box's seat without reaching it, and is counted nowhere): they stay
	// authoritative.
	Unreasoned int
}

// Snapshot copies the tally. A nil tally is an empty one.
func (t *PinTally) Snapshot() PinStats {
	out := PinStats{Reasoned: make(map[string]int, len(pinReasonSpecs))}
	for _, s := range pinReasonSpecs {
		out.Reasoned[s.reason] = 0
	}
	if t == nil {
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out.Since, out.Hints, out.HintsOverridden, out.Unreasoned = t.since, t.hints, t.overridden, t.unreasoned
	for reason, n := range t.reasoned {
		out.Reasoned[reason] = n
	}
	return out
}

// tallyPins adds one finished call's published results to the tally the call carries. It runs once per call, over
// the results the caller receives, so a subtask that was retried or re-placed is counted once and a hint is judged
// by where its published result was placed.
func (r *runner) tallyPins(results []PlacedResult) {
	t := r.tally
	if t == nil || r.pin.asked == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case r.pin.reason != "":
		if t.reasoned == nil {
			t.reasoned = map[string]int{}
		}
		t.reasoned[r.pin.reason] += len(results)
	case r.pin.hint:
		t.hints += len(results)
		for _, pr := range results {
			if r.hintOverridden(pr) {
				t.overridden++
			}
		}
	default:
		t.unreasoned += len(results)
	}
}

// Where a published result was placed.
const (
	placedNowhere = iota // no node took it (a defer, a shed, a refusal chain, a subtask that never started)
	placedLocal
	placedRemote
)

// placedWhere says where a published result was placed. Unplaced results are the ones no node ran (a capacity defer,
// a shed, a subtask the call ended before it started); so are the ones a node refused, alone or as the end of a
// refusal chain (exhausted keeps the dial base of the node that answered last, so the retry note can name it, but it
// clears the node and says in its error that none of them ran the subtask). The others were placed on the local seat
// (ranLocal) or on the node whose dial base they ran on.
func placedWhere(pr PlacedResult) int {
	switch {
	case pr.Unplaced, pr.refused:
		return placedNowhere
	case pr.ranLocal:
		return placedLocal
	case pr.ranBase != "":
		return placedRemote
	}
	return placedNowhere
}

// hintOverridden reports a hint that placement did not honour: a local hint placed on a remote node, a remote hint
// placed on the local seat. Only meaningful for a call whose route was a hint.
func (r *runner) hintOverridden(pr PlacedResult) bool {
	switch placedWhere(pr) {
	case placedRemote:
		return r.pin.asked == "local"
	case placedLocal:
		return r.pin.asked == "remote"
	}
	return false
}

// hintPrefix opens the clause of a call whose route was a hint; stampPin recognises its own clause by it.
func (r *runner) hintPrefix() string {
	return "route=" + r.pin.asked + " was a hint (no pin_reason)"
}

// hintClause is the plain-words sentence a result and its ledger row open with when the call's route was a hint:
// what the caller hinted, whether placement honoured it, and where the result was placed. "" for a call whose route
// was not a hint. The reason that placement chose differently (a spent run-cap line, a busy seat, a lease, no
// eligible remote) is the placement reason the clause is prefixed to.
func (r *runner) hintClause(pr PlacedResult) string {
	if !r.pin.hint {
		return ""
	}
	prefix := r.hintPrefix()
	if pr.abandoned {
		// The call gave up on this subtask while a seat or a node was still running it: it can say neither that a node
		// took it nor that none did, and the tally counts it as no override (placedWhere reads it as placed nowhere).
		if r.pin.asked == "local" {
			return prefix + ": placed as route=auto, and where it was running is not known"
		}
		return prefix + ": placed remotes-first, and where it was running is not known"
	}
	where := "the local seat"
	if placedWhere(pr) == placedRemote {
		where = nodeOrBase(pr)
	}
	switch r.pin.asked {
	case "local":
		switch placedWhere(pr) {
		case placedLocal:
			return prefix + ", honoured: placed on the local seat"
		case placedRemote:
			return prefix + ", overridden: placed on " + where
		}
		return prefix + ": placed as route=auto, and no node took it"
	default:
		switch placedWhere(pr) {
		case placedRemote:
			return prefix + ", honoured: placed on " + where
		case placedLocal:
			return prefix + ", overridden: placed on the local seat"
		}
		return prefix + ": placed remotes-first, and no node took it"
	}
}

// stampPin puts the call's pin on a result it publishes or records: the closed reason a pinned call carries, and the
// hint clause a hinted call's placement reason opens with. It is a pure function of the run's pin and the result, run
// on a copy at each of the two places every result passes (record, for the ledger and corpus row, and the end of
// runWith, for what the caller receives), so the two cannot disagree. A result that already carries its clause is left
// alone.
func (r *runner) stampPin(pr PlacedResult) PlacedResult {
	if r.pin.reason != "" {
		pr.PinReason = r.pin.reason
	}
	if r.pin.hint && !strings.HasPrefix(pr.PlacementReason, r.hintPrefix()) {
		if clause := r.hintClause(pr); clause != "" {
			if pr.PlacementReason == "" {
				pr.PlacementReason = clause
			} else {
				pr.PlacementReason = clause + "; " + pr.PlacementReason
			}
		}
	}
	return pr
}

// pinLedgerFields are the two ledger columns a pinned or hinted call adds to its rows: the closed reason, and the
// route the caller asked for (the row's `route` is the one the engine applied). Both are empty for a call that did
// not go through a door offering pin_reason with route local or remote, so its rows are byte-identical to before.
func (r *runner) pinLedgerFields() (pinReason, routeAsked string) {
	if r.pin.reason != "" || r.pin.hint {
		return r.pin.reason, r.pin.asked
	}
	return "", ""
}
