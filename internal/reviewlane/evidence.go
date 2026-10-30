package reviewlane

// SHA-bound review evidence.
//
// A review result used to say "here is what a seat thought" and nothing about WHAT it
// thought it about. A caller holding two results for two states of one branch could not tell
// them apart, a clean answer on a diff the lane never fully read looked like a clean answer,
// and a finding the caller skipped left no record anywhere. This file binds a result to the
// bytes it judged and records what happened to it:
//
//   - the base and head commit the caller says the diff spans (optional, echoed, never
//     resolved: the lane has no git access and must not guess), and the sha256 of the diff
//     bytes, which is the binding that actually holds;
//   - the receipts of the deterministic gates that ran BEFORE the seat did: the lane's own
//     (diff size, conflict markers) and any the caller hands over (a lint run, a test run);
//   - a receipt for the seat that ran, naming it and digesting its raw answer;
//   - a second fingerprint check right before the result is returned, so a diff that moved
//     while the seat read it cannot be sealed as reviewed;
//   - one outcome from a closed set, decided in one place (Decide);
//   - a durable trace (AppendTrace): one row for the review and one per finding, written
//     whether or not anyone reads or applies the findings.
//
// Everything stays ADVISORY, like the rest of the lane. An outcome is a label on a result a
// lead reads; nothing here blocks a merge or a commit. The additive JSON fields are all
// omitempty or new keys, so a reader written before them keeps working.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/core"
)

// Outcomes of one review, a closed set. PASS is the only one that says anything good, and it
// is hard to earn on purpose: a clean seat, a diff inside the ceiling, every gate green and
// for the reviewed commit, and a diff that did not move.
const (
	// OutcomePass: a seat with a receipt reviewed the whole diff, said NONE, the result carries
	// the diff's fingerprint and a second check that equals it, every gate receipt supports a
	// PASS (GateReceipt.Passed: none is red, stale or unbound), and the diff was unchanged when
	// the result was sealed. A PASS is tied to a commit only when the review named a head_sha
	// (Evidence.HeadUnbound says when it did not), and even then the lane cannot show that the
	// head is the commit the diff bytes came from: the fingerprint binds the bytes, the head
	// is the caller's word.
	OutcomePass = "PASS"
	// OutcomeFindings: a seat reviewed the unchanged diff and reported at least one defect.
	OutcomeFindings = "FINDINGS"
	// OutcomeGateFailed: the seat found nothing, but a deterministic gate is red or its
	// receipt belongs to another commit or names none, so PASS is not available. The review
	// still ran.
	OutcomeGateFailed = "GATE_FAILED"
	// OutcomeNotRun: no clean seat reviewed this diff, or the result is bound to nothing. A node
	// deferral (including one the lane salvaged as a clean NONE), a refusal before any diff was
	// read, a refused binding, an oversize diff, a seat that named nothing, an answer that
	// contradicted itself, evidence with no diff fingerprint, a second check that never ran.
	// Missing evidence is NOT_RUN and not GATE_FAILED: GATE_FAILED is a gate that ran and was
	// red, stale or unbound.
	OutcomeNotRun = "NOT_RUN"
	// OutcomeStale: a seat ran, but the diff was not the same bytes when the result was
	// sealed, so whatever it said is about a diff that no longer exists.
	OutcomeStale = "STALE"
)

// TraceFile is the sidecar every sealed review is appended to, beside the ledger (see
// TracePathFor). It is THE durable trace of a review: no ledger row is written for the verdict.
//
// It is a sidecar and not a ledger row on purpose. Every ledger reader counts rows as calls
// and jobs (jobrows.go: the double-count of one job was exactly a second row for it), the
// pipeline already writes the run's own row with this door's name before the outcome is
// known, and the ledger keeps hashes where this trace has to keep the short finding text.
// The same split the confidence head's label file and the base-policy audit already use. The
// ledger row is the RUN record; this file is the VERDICT record.
const TraceFile = "review-evidence.jsonl"

// MaxGateReceipts bounds the receipts one review accepts, so a trace row stays one small
// O_APPEND-atomic line however many a caller hands over.
const MaxGateReceipts = 16

// maxTraceText bounds the claim and the why a finding row keeps. The prompt asks for 15 and
// 20 words, so this never cuts an honest finding; it only bounds a runaway one. The digest on
// the row covers the full text.
const maxTraceText = 512

// maxGateText bounds a receipt's name and detail for the same reason.
const maxGateText = 256

// ErrBadBinding is the refusal of an evidence binding that cannot be trusted: a malformed
// commit id, a receipt with no name or a status the lane does not know, too many receipts.
// A binding that is silently repaired is not a binding.
var ErrBadBinding = errors.New("reviewlane: evidence binding refused")

// Who produced a gate receipt.
const (
	gateSourceLane   = "lane"
	gateSourceCaller = "caller"
)

const (
	gatePass = "pass"
	gateFail = "fail"
)

// GateReceipt is one deterministic gate's recorded result. The lane's own gates produce
// theirs; a caller hands over the receipts of lints and tests it ran itself. The lane does
// not execute caller commands: a review door that ran arbitrary commands named in its
// arguments would be a command-execution surface, and a receipt is enough to bind.
type GateReceipt struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pass | fail
	Detail string `json:"detail,omitempty"`
	// HeadSHA is the commit the gate ran on, when the caller says. A receipt for a different
	// commit than the review's head is Stale and cannot support a PASS.
	HeadSHA string `json:"head_sha,omitempty"`
	Source  string `json:"source,omitempty"` // lane | caller
	Stale   bool   `json:"stale,omitempty"`
	// Unbound marks a caller's receipt that cannot be shown to belong to the commit under
	// review: it names no commit while the review names one, or it names a commit while the
	// review names none. Either way it cannot support a PASS.
	Unbound bool `json:"unbound,omitempty"`
}

// Passed reports whether the receipt supports a PASS: green, and for the commit under review.
func (g GateReceipt) Passed() bool { return g.Status == gatePass && !g.Stale && !g.Unbound }

// SeatReceipt names the seat that produced (or failed to produce) a review, by identity and
// by the digest of its raw answer. It carries no timing: a receipt says who and what, and a
// pass rule built on a wall would break the program's speed rule.
type SeatReceipt struct {
	Seat       string `json:"seat"`
	NodeID     string `json:"node_id,omitempty"`
	Node       string `json:"node,omitempty"`
	ExecutedOn string `json:"executed_on,omitempty"`
	Steps      int    `json:"steps"`
	StopReason string `json:"stop_reason,omitempty"`
	// Salvaged is how the findings were read when the structured re-pack did not deliver them
	// (output_truncated | wall | repack_flattened); empty for an ordinary delivery.
	Salvaged string `json:"salvaged,omitempty"`
	// Deferred is true when the node filed the run as a deferral (the receipt is then the
	// record of an attempt, whatever the lane salvaged from it).
	Deferred     bool   `json:"deferred,omitempty"`
	AnswerSHA256 string `json:"answer_sha256,omitempty"`
}

// Evidence is what a review result is bound to. DiffBytes is always set and DiffSHA256 is set
// whenever a diff was read (a review refused before any diff was read has neither, see Refused);
// everything else is additive and omitted while unknown.
type Evidence struct {
	BaseSHA    string `json:"base_sha,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	DiffSHA256 string `json:"diff_sha256,omitempty"`
	DiffBytes  int    `json:"diff_bytes"`
	// PromptSHA256 is the digest of the exact prompt the seat is shown (task and diff), so a
	// receipt proves what the seat was asked and not only which seat answered.
	PromptSHA256 string `json:"prompt_sha256,omitempty"`
	// Gates are attached at construction, before any seat exists, in the order they ran: the
	// lane's own first, then the caller's.
	Gates []GateReceipt `json:"gates,omitempty"`
	Seat  *SeatReceipt  `json:"seat,omitempty"`
	// RecheckSHA256 is the diff's fingerprint at the second check, right before the result is
	// returned. Equal to DiffSHA256 when the diff did not move.
	RecheckSHA256 string `json:"recheck_sha256,omitempty"`
	Stale         bool   `json:"stale,omitempty"`
	StaleReason   string `json:"stale_reason,omitempty"`
	// HeadUnbound is true when the review named no head_sha, so nothing ties its gate receipts
	// to a commit and a PASS on it is not tied to one either. Even with a head_sha nothing ties
	// the head to the diff bytes: the head is the caller's word.
	HeadUnbound bool `json:"head_unbound,omitempty"`
	// Refusal is why the binding itself was refused (see ErrBadBinding), when it was.
	Refusal string `json:"refusal,omitempty"`
}

// Fingerprint is the sha256 (lower-case hex) of the diff bytes as the lane holds them. Invalid
// UTF-8 is hashed as it is, although the text may not reach the seat byte for byte (it is
// sanitized for JSON in transit). It is the binding that holds: a commit id the caller typed
// can be wrong, the bytes cannot.
func Fingerprint(diff string) string {
	sum := sha256.Sum256([]byte(diff))
	return hex.EncodeToString(sum[:])
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// normSHA lower-cases and validates a commit id. "" is valid and means unbound.
func normSHA(field, s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if !shaRe.MatchString(s) {
		return "", fmt.Errorf("%w: %s %q is not a hex commit id of 7 to 64 characters", ErrBadBinding, field, capBytes(s, 48))
	}
	return s, nil
}

// sameCommit compares two normalised commit ids, one of which may abbreviate the other.
func sameCommit(a, b string) bool {
	return a != "" && b != "" && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a))
}

// NewEvidence binds a review to its diff, the commits the caller names, and the gate
// receipts, and runs the lane's own deterministic gates. Everything here happens BEFORE a
// seat is contacted, which is what "gates first" means structurally: the Evidence a seat
// receipt is later added to already holds its gates.
//
// A binding that cannot be trusted returns ErrBadBinding AND an Evidence that still carries
// the diff's fingerprint, size and the reason (Refusal), so the NOT_RUN built from it is
// bound to the bytes the caller sent.
func NewEvidence(diff, base, head string, handed []GateReceipt) (Evidence, error) {
	ev := Evidence{DiffSHA256: Fingerprint(diff), DiffBytes: len(diff)}
	refuse := func(err error) (Evidence, error) {
		ev.Refusal = err.Error()
		return ev, err
	}
	var err error
	if ev.BaseSHA, err = normSHA("base_sha", base); err != nil {
		return refuse(err)
	}
	if ev.HeadSHA, err = normSHA("head_sha", head); err != nil {
		return refuse(err)
	}
	if len(handed) > MaxGateReceipts {
		return refuse(fmt.Errorf("%w: %d gate receipts handed, at most %d are accepted", ErrBadBinding, len(handed), MaxGateReceipts))
	}
	ev.HeadUnbound = ev.HeadSHA == ""
	ev.Gates = append(ev.Gates, laneGates(diff)...)
	for i, h := range handed {
		r, rerr := normReceipt(h)
		if rerr != nil {
			return refuse(fmt.Errorf("%w: gate receipt %d: %w", ErrBadBinding, i+1, rerr))
		}
		switch {
		case r.HeadSHA != "" && ev.HeadSHA != "" && !sameCommit(r.HeadSHA, ev.HeadSHA):
			// A receipt for another commit is evidence about a different change.
			r.Stale = true
			r.Detail = strings.TrimSpace(r.Detail + " [receipt is for commit " + capBytes(r.HeadSHA, 12) + ", the review is of " + capBytes(ev.HeadSHA, 12) + "]")
		case r.HeadSHA == "" && ev.HeadSHA != "":
			// Nothing shows this receipt belongs to the commit under review.
			r.Unbound = true
			r.Detail = strings.TrimSpace(r.Detail + " [receipt names no commit, the review is of " + capBytes(ev.HeadSHA, 12) + ": an unbound receipt cannot support a PASS]")
		case r.HeadSHA != "" && ev.HeadSHA == "":
			// The receipt names a commit and the review names none, so nothing shows the diff
			// is that commit's. A receipt that names none either stands as given (the PASS says
			// the review is tied to no commit).
			r.Unbound = true
			r.Detail = strings.TrimSpace(r.Detail + " [receipt is for commit " + capBytes(r.HeadSHA, 12) + ", the review names no head_sha: an unbound receipt cannot support a PASS]")
		}
		ev.Gates = append(ev.Gates, r)
	}
	return ev, nil
}

// normReceipt validates and normalises one handed receipt.
func normReceipt(h GateReceipt) (GateReceipt, error) {
	r := GateReceipt{
		Name:   capBytes(strings.TrimSpace(h.Name), maxGateText),
		Status: strings.ToLower(strings.TrimSpace(h.Status)),
		Detail: capBytes(strings.TrimSpace(h.Detail), maxGateText),
		Source: gateSourceCaller,
	}
	if r.Name == "" {
		return r, errors.New("name is required")
	}
	if r.Status != gatePass && r.Status != gateFail {
		return r, fmt.Errorf("status %q is not %q or %q", capBytes(h.Status, 24), gatePass, gateFail)
	}
	sha, err := normSHA("head_sha", h.HeadSHA)
	if err != nil {
		return r, err
	}
	r.HeadSHA = sha
	return r, nil
}

// laneGates are the deterministic checks the lane can make from the diff bytes alone, with
// no process to run and nothing to install.
func laneGates(diff string) []GateReceipt {
	size := GateReceipt{Name: "diff-size", Status: gatePass, Source: gateSourceLane,
		Detail: fmt.Sprintf("%d of %d bytes", len(diff), MaxDiffBytes)}
	if len(diff) > MaxDiffBytes {
		size.Status = gateFail
		size.Detail = fmt.Sprintf("%d bytes exceeds the %d-byte ceiling", len(diff), MaxDiffBytes)
	}
	markers := GateReceipt{Name: "conflict-markers", Status: gatePass, Source: gateSourceLane, Detail: "no merge-conflict marker on an added line"}
	if n := conflictMarkerLine(diff); n > 0 {
		markers.Status = gateFail
		markers.Detail = "merge-conflict marker on diff line " + strconv.Itoa(n)
	}
	return []GateReceipt{size, markers}
}

// conflictMarkerLine returns the 1-based diff line of the first ADDED line that opens or
// closes a merge-conflict block (seven < or > followed by a space or the end of the line), or
// 0. Removed and context lines do not count, and neither does a bare ======= (a setext
// heading underline in Markdown is not a conflict).
func conflictMarkerLine(diff string) int {
	for i, ln := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(ln, "+") || strings.HasPrefix(ln, "+++") {
			continue
		}
		body := strings.TrimRight(ln[1:], "\r")
		for _, mark := range []string{"<<<<<<<", ">>>>>>>"} {
			if strings.HasPrefix(body, mark) && (len(body) == len(mark) || body[len(mark)] == ' ') {
				return i + 1
			}
		}
	}
	return 0
}

// Refused is the evidence of a review refused before any diff was read: both diff and
// diff_path passed, neither, a diff_path that cannot be read, arguments that do not decode.
// It binds the result to nothing, on purpose: no bytes were chosen, so there is no fingerprint
// to give (a fingerprint of nothing would read as a binding), no gate has anything to run on,
// and DiffBytes is zero. The refusal reason is the whole of it, and Decide turns it into a
// NOT_RUN that says why.
func Refused(reason string) Evidence {
	return Evidence{Refusal: reason}
}

// TracePathFor is where the durable trace lives: TraceFile in the directory of the ledger
// file, so it always sits beside the ledger however an operator has placed it. With no ledger
// path configured it falls back to fallbackDir (the harness home), never the working directory.
func TracePathFor(ledgerPath, fallbackDir string) string {
	dir := fallbackDir
	if ledgerPath != "" {
		dir = filepath.Dir(ledgerPath)
	}
	return filepath.Join(dir, TraceFile)
}

// WithPrompt records the digest of the prompt the seat is shown.
func (e Evidence) WithPrompt(goal string) Evidence {
	e.PromptSHA256 = Fingerprint(goal)
	return e
}

// GatesPassed reports whether every attached receipt supports a PASS.
func (e Evidence) GatesPassed() bool {
	for _, g := range e.Gates {
		if !g.Passed() {
			return false
		}
	}
	return true
}

// failedGates names the receipts that stop a PASS, for the verdict's reason.
func (e Evidence) failedGates() string {
	var names []string
	for _, g := range e.Gates {
		switch {
		case g.Stale:
			names = append(names, g.Name+" (receipt is for another commit)")
		case g.Unbound && g.Status == gatePass && g.HeadSHA == "":
			names = append(names, g.Name+" (receipt names no commit)")
		case g.Unbound && g.Status == gatePass:
			names = append(names, g.Name+" (receipt names a commit, the review names no head_sha)")
		case !g.Passed():
			names = append(names, g.Name)
		}
	}
	return strings.Join(names, ", ")
}

// Recheck is the second fingerprint check. diffNow is the diff re-read right before the
// result is returned (err when it could not be re-read). A diff that differs, or cannot be
// shown to be the same, makes the evidence stale: whatever the seat said was said about
// other bytes.
func (e Evidence) Recheck(diffNow string, err error) Evidence {
	if err != nil {
		e.Stale = true
		e.StaleReason = "the diff could not be re-read to confirm it did not change during the review: " + err.Error()
		return e
	}
	e.RecheckSHA256 = Fingerprint(diffNow)
	if e.RecheckSHA256 != e.DiffSHA256 {
		e.Stale = true
		e.StaleReason = fmt.Sprintf("the diff changed while the seat reviewed it (sha256 %s, now %s)", capBytes(e.DiffSHA256, 12), capBytes(e.RecheckSHA256, 12))
	}
	return e
}

// SeatReceiptOf builds the receipt of one seat run from its wire result and the fleet
// provenance the door collected (executed_on, node). salvaged is the lane's own reading note
// ("" for an ordinary delivery).
func SeatReceiptOf(w core.AgentWireResult, extra map[string]any, salvaged string) SeatReceipt {
	str := func(k string) string { s, _ := extra[k].(string); return s }
	r := SeatReceipt{
		Seat:       w.Seat,
		NodeID:     w.NodeID,
		Node:       str("node"),
		ExecutedOn: str("executed_on"),
		Steps:      w.Steps,
		StopReason: w.StopReason,
		Salvaged:   salvaged,
		Deferred:   w.Deferred,
	}
	if w.Output != "" {
		r.AnswerSHA256 = Fingerprint(w.Output)
	}
	return r
}

// Delivery is what the door knows about a finished run, in the terms Decide needs.
type Delivery struct {
	// Delivered: the lane published a review (findings, or the seat's own clean verdict)
	// rather than a deferral.
	Delivered bool
	// Findings published; Filtered is how many lines the filters that can empty a list took
	// (hollow, ungrounded, echoed).
	Findings int
	Filtered int
	// Reason is the node's own defer reason, when the run was not delivered.
	Reason string
}

// Verdict is the outcome of one review and, for every outcome but FINDINGS, why. A PASS carries
// a reason only when it is not tied to a commit (NoHeadReason).
type Verdict struct {
	Outcome string
	Reason  string
}

// NoHeadReason is the reason a PASS carries when the review named no head_sha.
const NoHeadReason = "no head_sha: gates not tied to a commit"

// Decide is the one place an outcome is chosen, in a fixed order so no result can reach a
// better label than its worst fact allows:
//
//  1. a refused binding or a refusal before any diff was read, an oversize diff, a run that
//     was not delivered, a seat that named nothing, an answer that contradicted itself, a run
//     the node filed as a deferral that the lane read as a clean NONE, or evidence with no
//     diff fingerprint: NOT_RUN;
//  2. a diff that moved: STALE;
//  3. a defect reported: FINDINGS;
//  4. a red, stale or unbound gate: GATE_FAILED;
//  5. a second fingerprint check that is missing or differs from the first: NOT_RUN;
//  6. otherwise PASS.
//
// The oversize rule is repeated here although BuildContract refuses the same diff before any
// seat runs: this function is the contract, and a caller that built its own flow must not be
// able to reach PASS by skipping the builder.
func Decide(ev Evidence, d Delivery) Verdict {
	switch {
	case ev.Refusal != "":
		return Verdict{OutcomeNotRun, ev.Refusal}
	case ev.DiffBytes > MaxDiffBytes:
		return Verdict{OutcomeNotRun, fmt.Sprintf("the diff is %d bytes, which exceeds the %d-byte ceiling for one review: no seat reviewed it, and an unreviewed diff is never a PASS", ev.DiffBytes, MaxDiffBytes)}
	case !d.Delivered:
		why := strings.TrimSpace(d.Reason)
		if why == "" {
			why = "no clean seat ran the review"
		}
		return Verdict{OutcomeNotRun, why}
	case ev.Seat == nil || strings.TrimSpace(ev.Seat.Seat) == "":
		return Verdict{OutcomeNotRun, "no clean seat ran the review: the result carries no seat receipt naming the seat that produced it"}
	case d.Findings == 0 && d.Filtered > 0:
		return Verdict{OutcomeNotRun, fmt.Sprintf("no clean seat ran the review: the seat wrote %d finding-shaped lines and the lane's filters discarded every one, yet its answer read as NONE", d.Filtered)}
	case ev.Seat.Deferred && d.Findings == 0:
		// The lane may publish what a deferred run wrote (a salvage), and findings read from
		// it are still findings. A clean NONE read from a run the node itself filed as a
		// deferral is not a review that ran to its end, so it is never an all-clear.
		return Verdict{OutcomeNotRun, "no clean seat ran the review: the node filed the run as a deferral and the lane read a clean NONE from what the seat had written, which is never a PASS"}
	case ev.DiffSHA256 == "":
		return Verdict{OutcomeNotRun, "the result carries no diff fingerprint (diff_sha256), so it is bound to no bytes and cannot be a PASS"}
	case ev.Stale:
		return Verdict{OutcomeStale, ev.StaleReason + "; whatever the seat said is about bytes that no longer exist, so re-run the review on the current diff"}
	case d.Findings > 0:
		return Verdict{OutcomeFindings, ""}
	case !ev.GatesPassed():
		return Verdict{OutcomeGateFailed, "the seat found nothing, but a deterministic gate is not green: " + ev.failedGates()}
	case ev.RecheckSHA256 != ev.DiffSHA256:
		return Verdict{OutcomeNotRun, "the second fingerprint check did not confirm the diff (recheck_sha256 is missing or differs from diff_sha256), so nothing shows it did not move during the review"}
	case ev.HeadUnbound:
		return Verdict{OutcomePass, NoHeadReason}
	}
	return Verdict{OutcomePass, ""}
}

// Run is what the door observed about the seat run, for Seal.
type Run struct {
	// Delivered is reviewPayload's own answer: the payload is a review, not a deferral.
	Delivered bool
	// Wire is the seat's wire result; nil when no seat ran (a refusal before the run, or a
	// runner error).
	Wire *core.AgentWireResult
	// Extra is the fleet provenance block (executed_on, node, ...) published beside the review.
	Extra map[string]any
	// Recheck re-reads the diff. Seal calls it once, after the seat finished and right before
	// the result is returned; nil means the diff is held in memory and cannot have moved.
	Recheck func() (string, error)
}

// Seal stamps the evidence onto a published payload and leaves the durable trace. It adds
// `outcome`, `outcome_reason` (when the outcome is not a plain FINDINGS or a PASS tied to a
// commit), `evidence`
// and, if the trace could not be written, `trace_error`. It changes nothing else in out, so
// a deferral keeps its deferral shape and a review keeps its findings.
//
// A trace that cannot be written is reported, not hidden and not fatal: the review is still
// worth delivering, and a silent gap in a durable record is the one failure this exists to
// prevent.
func Seal(out map[string]any, ev Evidence, r Run, tracePath string) map[string]any {
	if out == nil {
		out = map[string]any{}
	}
	if r.Wire != nil {
		salvaged, _ := out["salvaged"].(string)
		seat := SeatReceiptOf(*r.Wire, r.Extra, salvaged)
		ev.Seat = &seat
		// The second fingerprint check, last thing before the result leaves the lane. With no
		// seat there was no review during which the diff could move.
		if r.Recheck != nil {
			now, err := r.Recheck()
			ev = ev.Recheck(now, err)
		} else {
			// A diff held in memory cannot have moved: the second check is the fingerprint
			// itself (as it is for an inline diff, whose recheck compares the string with itself).
			ev.RecheckSHA256 = ev.DiffSHA256
		}
	}
	findings, _ := out["findings"].([]Finding)
	d := Delivery{
		Delivered: r.Delivered,
		Findings:  len(findings),
		Filtered:  payloadInt(out, "dropped_ungrounded") + payloadInt(out, "dropped_echo") + payloadInt(out, "dropped_hollow"),
	}
	d.Reason, _ = out["reason"].(string)
	v := Decide(ev, d)

	out["outcome"] = v.Outcome
	if v.Reason != "" {
		out["outcome_reason"] = v.Reason
	}
	out["evidence"] = ev

	if tracePath == "" {
		out["trace_error"] = "no trace path is configured, so this review left no durable trace"
		return out
	}
	if err := AppendTrace(tracePath, traceRows(ev, v, findings, out)...); err != nil {
		out["trace_error"] = "the review's durable trace could not be written: " + err.Error()
	}
	return out
}

// payloadInt reads a count the door publishes only when non-zero.
func payloadInt(out map[string]any, key string) int {
	n, _ := out[key].(int)
	return n
}

// TraceFinding is one finding as the sidecar keeps it: the short text the seat wrote, bounded,
// and a digest of the whole of it.
type TraceFinding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Claim    string `json:"claim"`
	Why      string `json:"why,omitempty"`
	// Also is how many restatements of the same place were folded onto this finding.
	Also   int    `json:"also,omitempty"`
	SHA256 string `json:"sha256"`
}

// TraceRow is one line of the sidecar. A review row (kind "review") carries the evidence and
// the counts; each finding row (kind "finding") carries one finding and joins its review by
// review_id.
type TraceRow struct {
	TS       string         `json:"ts"`
	Kind     string         `json:"kind"`
	ReviewID string         `json:"review_id"`
	Outcome  string         `json:"outcome"`
	Reason   string         `json:"outcome_reason,omitempty"`
	Evidence *Evidence      `json:"evidence,omitempty"`
	Counts   map[string]int `json:"counts,omitempty"`
	Finding  *TraceFinding  `json:"finding,omitempty"`
}

// traceCountKeys are the payload counts a review row keeps beside the number of findings
// shown: what the filters and the cap took out of the list.
var traceCountKeys = []string{"dropped_ungrounded", "dropped_echo", "dropped_hollow", "dropped_duplicate", "truncated_by_cap"}

// traceRows builds the rows for one sealed review: the review row first, then one row per
// published finding, sharing a review id.
func traceRows(ev Evidence, v Verdict, findings []Finding, out map[string]any) []TraceRow {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	id := Fingerprint(ev.DiffSHA256 + "|" + strconv.FormatInt(now.UnixNano(), 10))[:16]
	counts := map[string]int{"findings": len(findings)}
	for _, k := range traceCountKeys {
		if n := payloadInt(out, k); n > 0 {
			counts[k] = n
		}
	}
	evCopy := ev
	rows := []TraceRow{{TS: ts, Kind: "review", ReviewID: id, Outcome: v.Outcome, Reason: v.Reason, Evidence: &evCopy, Counts: counts}}
	for _, f := range findings {
		full, _ := json.Marshal(f)
		rows = append(rows, TraceRow{TS: ts, Kind: "finding", ReviewID: id, Outcome: v.Outcome, Finding: &TraceFinding{
			Severity: f.Severity,
			File:     f.File,
			Line:     f.Line,
			Claim:    capBytes(f.Claim, maxTraceText),
			Why:      capBytes(f.Why, maxTraceText),
			Also:     len(f.Also),
			SHA256:   Fingerprint(string(full)),
		}})
	}
	return rows
}

// traceMu serialises in-process appends; across processes O_APPEND carries each write.
var traceMu sync.Mutex

// AppendTrace appends rows to the sidecar at path, creating the file (owner-only: it holds
// the seat's short statements about someone's code) and its directory. The rows go out in ONE
// write, so one review's rows stay together.
func AppendTrace(path string, rows ...TraceRow) error {
	if len(rows) == 0 {
		return nil
	}
	var buf []byte
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf = append(append(buf, b...), '\n')
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(buf)
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// capBytes cuts s to at most n bytes without splitting a rune: only a trailing partial rune is
// dropped (at most utf8.UTFMax-1 bytes), so an invalid byte earlier in the string costs nothing
// after it.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	cut := n
	for back := 0; back < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(s[cut]); back++ {
		cut--
	}
	return s[:cut]
}
