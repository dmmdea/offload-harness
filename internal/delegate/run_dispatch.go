// run_dispatch.go - Job dispatch and polling primitives: notes, poll-failure log, dispatch and job ids.

package delegate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// maxQueueRefreshTries bounds the reads of a queued job's node for its queue budget.
const maxQueueRefreshTries = 3

// queueRefreshNote says, on a queue deadline, that the queue budget the job was abandoned
// at was derived from the placement snapshot because the node's health could not be read
// again once the job was seen queued (and why). "" when the read succeeded or was never
// needed. A safeguard that did not run must leave a trace where its absence matters.
func queueRefreshNote(err error, tries int) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("; the queue budget was derived from the placement snapshot: the node's health could not be read again once the job was seen queued (%d attempt(s), last: %v)", tries, err)
}

// queuedNote renders the backlog credit for a deadline message, and renders
// NOTHING when there was none. The empty case is load-bearing: a node with no
// queue (or a job that started at once) must produce the exact message it
// produced before this accounting existed, so an operator grepping for the old
// wording — and the tests pinning it — still match. When it IS non-empty it
// answers the first question a deadline raises on a queued fleet: was this job
// slow, or was it merely late to start?
// boundNote renders the auto poll bound for a deadline message, and NOTHING
// when there is none — a contract that named its own timeout_sec must produce
// the exact message it produced before D-116, so an operator grepping the old
// wording (and the tests pinning it) still match.
// slack, when non-zero, is the admission allowance still carried in the budget
// — the node's pre-wall window. Naming it is the difference between an
// operator reading "the node had 300 s of wall" and reading the number the
// clock was actually held to; it disappears from the message exactly when it
// disappears from the budget (the node published a started wall).
func boundNote(note string, slack time.Duration) string {
	if note == "" {
		return ""
	}
	if slack > 0 {
		return fmt.Sprintf(" (poll bound: %s, + %s allowed for the node's admission before its wall starts)", note, slack)
	}
	return " (poll bound: " + note + ")"
}

func queuedNote(queuedCredit time.Duration) string {
	if queuedCredit <= 0 {
		return ""
	}
	return fmt.Sprintf(" (+%s credited back for time queued on the node)", queuedCredit.Round(time.Millisecond))
}

// errText renders a possibly-nil error for an operator-facing message. A
// never-answered node with no recorded error means the deadline was already
// spent before the first poll — say that, don't print "<nil>".
func errText(err error) string {
	if err == nil {
		return "no poll completed before the deadline"
	}
	return err.Error()
}

// unownedDetail renders WHY a poll deadline produced a FAILURE rather than a
// defer. THREE shapes, because they send an operator to three different places
// — and because the delegator may only ever report what a node actually said:
//
//	never answered      → a dead box, a wrong address, dropped connections.
//	answered with a 404 → a POSITIVE denial that it ever held the work, which no
//	                      amount of waiting turns into a result and which
//	                      re-dispatching only re-asks.
//	answered otherwise  → reachable, and it said nothing about this job either
//	                      way: a 5xx from it or a proxy, or a 200 carrying a
//	                      state this delegator does not know (a newer peer's
//	                      "queued").
//
// The third shape used to print the SECOND one's sentence: a node answering only
// 503s published "a poll 404 DENIES it ever held it (0 re-dispatch(es) made)" —
// a denial that never happened, invented inside the very message that was
// written to stop the delegator authoring claims for nodes.
func unownedDetail(sawNodeAnswer, saw404 bool, redispatches int, lastPollErr error) string {
	switch {
	case !sawNodeAnswer:
		return fmt.Sprintf("node never answered (last: %s)", errText(lastPollErr))
	case saw404:
		return fmt.Sprintf("the node answered but never reported owning the job — a poll 404 DENIES it ever held it (%d re-dispatch(es) made)%s",
			redispatches, lastErrSuffix(lastPollErr))
	default:
		return fmt.Sprintf("the node answered but never reported owning the job, and never denied holding it either — no poll returned a state this delegator can act on%s",
			lastErrSuffix(lastPollErr))
	}
}

// lastErrSuffix appends the newest UNRETIRED poll failure when there is one, and
// nothing when there is not: "last: no poll completed before the deadline" beside
// a stack of 404 answers reads as a contradiction of the sentence it follows.
func lastErrSuffix(err error) string {
	if err == nil {
		return ""
	}
	return "; last: " + err.Error()
}

// pollFailShapeCap bounds how many DISTINCT failure texts one job LOGS in full
// and enumerates in its summary. A node whose error text varies per poll (a
// changing upstream port, a rotating request id) must not turn the log into an
// unbounded transcript of its own. It does NOT bound the tally: every occurrence
// is counted whatever its shape (see note).
const pollFailShapeCap = 8

// pollFailLog bounds one job's poll-failure logging. Both failure arms in
// runRemote used to log once PER POLL — in the very commit that introduced
// sync.Once because unbounded per-subtask logging buries the results it warns
// about (see warnCorpus/warnLedger). Measured: 53 lines for ONE subtask at the
// compressed test cadence, ~120 in production, ~1000 for an 8-way fan-out at a
// dead node. So: the FIRST occurrence of each distinct shape logs in full, every
// occurrence is counted, and ONE summary line reports the totals on the way out.
type pollFailLog struct {
	jobID, base string
	counts      map[string]int
	order       []string // first-seen order, so the summary reads chronologically
	total       int
}

func newPollFailLog(jobID, base string) *pollFailLog {
	return &pollFailLog{jobID: jobID, base: base, counts: map[string]int{}}
}

// note records one failure, logging it only the first time its shape appears.
// The count is taken BEFORE the cap check: the early return used to fire first,
// so every occurrence of the 9th and later shapes was dropped from the tally
// entirely — 36 failures rendered as a 24-occurrence breakdown, presented as
// complete, with the text of those shapes appearing nowhere at all. The map is
// bounded in practice by the number of polls the deadline allows.
func (p *pollFailLog) note(err error) {
	p.total++
	shape := err.Error()
	_, known := p.counts[shape]
	p.counts[shape]++
	if known || len(p.order) >= pollFailShapeCap {
		return // counted; summarize() reports the omission and its residual
	}
	p.order = append(p.order, shape)
	log.Printf("delegate: poll %s at %s failed: %v", p.jobID, p.base, err)
}

// summarize emits the single end-of-poll line. Silent when nothing failed, so a
// healthy job's output is unchanged. Shapes past the cap are not enumerated —
// that is the cap's whole point — but their COUNT is stated, so the breakdown
// can never again read as complete when it is not.
func (p *pollFailLog) summarize() {
	if p.total == 0 {
		return
	}
	parts := make([]string, 0, len(p.order)+1)
	enumerated := 0
	for _, shape := range p.order {
		parts = append(parts, fmt.Sprintf("%s ×%d", shape, p.counts[shape]))
		enumerated += p.counts[shape]
	}
	if omitted := len(p.counts) - len(p.order); omitted > 0 {
		parts = append(parts, fmt.Sprintf("%d further shape(s) omitted past the log cap ×%d", omitted, p.total-enumerated))
	}
	log.Printf("delegate: poll %s at %s: %d failed poll(s) this job (%s)", p.jobID, p.base, p.total, strings.Join(parts, "; "))
}

// dispatchResult is a node's answer to one dispatch POST — dispatchDetailed's
// return shape (see its own doc for what each field means and when).
type dispatchResult struct {
	refused bool
	status  int
	// retryAfterSec is the node's `Retry-After` header on a 503 (capacity)
	// refusal — a CAPACITY hint, not a promise, so the caller treats it as
	// advisory. 0 when absent or unparsable, or on any status but 503.
	retryAfterSec int
	err           error
}

// dispatch is dispatchDetailed without the Retry-After hint — the plain
// 3-value shape every caller but runRemote uses. runRemote reads the hint
// itself (PlacedResult.retryAfterSec) and files it as the node's cooldown;
// nothing sleeps on it (ADR 0063).
func (r *runner) dispatch(ctx context.Context, base, jobID string, payload json.RawMessage) (refused bool, status int, err error) {
	res := r.dispatchDetailed(ctx, base, jobID, payload)
	return res.refused, res.status, res.err
}

// dispatchDetailed POSTs the job envelope, expecting the contract's one
// acceptance shape (202). A transport-level failure is retried ONCE with the
// same job id — if the first POST actually landed, the node's known-job path
// re-acks idempotently, so the retry can never buy a second run. That bounded
// retry (dispatchAttempts) is about DOUBT over one node's transport and is
// entirely separate from re-placement, which is about a node that answered
// no.
//
// The result says which of three things happened:
//
//	err == nil                     the node acked 202.
//	err != nil, refused == true    the node DECLINED (status = what it sent) or
//	                               could not be reached at all (status = 0).
//	                               placeAndRun classifies it; nothing here
//	                               decides routing.
//	err != nil, refused == false   a DELEGATOR-side fault (an envelope that
//	                               will not marshal, a request that will not
//	                               build). No node said anything, so there is
//	                               nothing for another node to say differently.
func (r *runner) dispatchDetailed(ctx context.Context, base, jobID string, payload json.RawMessage) dispatchResult {
	envelope := map[string]any{
		"job_id":    jobID,
		"task_type": string(core.TaskAgentRun),
		"payload":   payload,
	}
	// Scheduling band (0.113.18) rides the envelope's contract-reserved
	// `priority` — a field every node since v2 accepts (older ones ignore it),
	// sent only when non-zero so a band-0 run's bytes are unchanged. The tenant
	// rides a header (fleetnode.TenantHeader) because the node's envelope decode
	// rejects unknown FIELDS, and a new one would 400 on every node a release
	// behind; a header is ignored by nodes that do not read it.
	if r.priority != 0 {
		envelope["priority"] = r.priority
	}
	env, merr := json.Marshal(envelope)
	if merr != nil {
		return dispatchResult{err: fmt.Errorf("marshaling dispatch envelope: %w", merr)}
	}
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/dispatch"
	var lastErr error
	for attempt := 0; attempt < dispatchAttempts; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, dispatchRequestTimeout)
		req, rerr := http.NewRequestWithContext(rctx, http.MethodPost, u, bytes.NewReader(env))
		if rerr != nil {
			cancel()
			return dispatchResult{err: fmt.Errorf("dispatch request: %w", rerr)}
		}
		req.Header.Set("Content-Type", "application/json")
		if r.cfg.FleetAuthToken != "" {
			req.Header.Set("Authorization", "Bearer "+r.cfg.FleetAuthToken)
		}
		if r.tenant != "" {
			req.Header.Set(core.TenantHeader, r.tenant)
		}
		// Who asked, and whether the serving node must card the job because this box will not
		// (D7/D11): a header, for the same reason the tenant is one.
		r.pair.SetWireHeaders(req.Header)
		if r.cardless() {
			// A call pinned for measurement opens no card anywhere: this box opens none (pairInflight), so a box
			// with no emitter of its own must not ask the serving node to open one for it either (register F48).
			req.Header.Del(core.PairCardHeader)
			// And the node's row for it is a measurement too (ADR 0087). A process class, when this process has
			// one, is already on the request and wins, as it does in the ledger's own settleRunClass.
			if req.Header.Get(core.RunClassHeader) == "" {
				req.Header.Set(core.RunClassHeader, ledger.ClassMeasurement)
			}
		}
		resp, derr := fleetClient.Do(req)
		if derr != nil {
			cancel()
			lastErr = fmt.Errorf("dispatch %s: %w", u, derr)
			continue // transport doubt → one more POST, same job id
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxFleetBody))
		retryAfter := 0
		if resp.StatusCode == http.StatusServiceUnavailable {
			retryAfter = parseRetryAfterSeconds(resp.Header.Get("Retry-After"))
		}
		resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusAccepted {
			// A non-202 is the node REFUSING (400/401/403/409/503) — an
			// answer, not doubt; re-POSTing the same bytes to the SAME node
			// would get the same answer. Whether ANOTHER node is worth asking
			// is replaceableRefusal's decision, made from this status.
			return dispatchResult{refused: true, status: resp.StatusCode, retryAfterSec: retryAfter,
				err: fmt.Errorf("dispatch %s: status %d: %s", u, resp.StatusCode, truncate(body, 256))}
		}
		return dispatchResult{status: resp.StatusCode}
	}
	// Both POSTs failed at transport level: the delegator never reached this
	// node. Status 0 says exactly that — no node authored this answer.
	return dispatchResult{refused: true, err: lastErr}
}

// parseRetryAfterSeconds reads a `Retry-After` header's DELTA-SECONDS form
// (the shape this fleet's own nodes send — never the HTTP-date form, which
// this parser deliberately does not attempt). Empty, unparsable or negative
// is 0 = no hint, the safe "the caller decides its own pacing" reading.
// retryAfterUnparsableWarnOnce bounds parseRetryAfterSeconds' log to ONCE per
// process — review round 1, LOW item 6: a future proxy in front of a node
// could start sending the HTTP-date form (RFC 9110 §10.2.3), which this
// parser deliberately does not attempt (this fleet's own nodes only ever
// send delta-seconds), and that would otherwise silently discard the hint on
// every single 503 forever with no trace anywhere.
var retryAfterUnparsableWarnOnce sync.Once

func parseRetryAfterSeconds(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		retryAfterUnparsableWarnOnce.Do(func() {
			log.Printf("delegate: Retry-After %q is not a delta-seconds integer (an HTTP-date form is not parsed); ignoring it as a capacity hint — logged once per process", v)
		})
		return 0
	}
	return n
}

// pollWaitSec is what runRemote's poll asks a node to hold ONE connection
// open for (?wait=12) before answering (item 7): at fleetnode.MaxJobWaitSec,
// one second inside pollRequestTimeout (15 s) so the long poll always gets an
// answer before the client gives up on the exchange — a pairing pinned on
// the node side by a test that can read this package's own source. An older
// node (pre-0.127) does not read the parameter at all and answers at once,
// exactly as before: the fallback IS the parameter being a no-op there, not
// a second code path — the existing pollEvery sleep between polls is
// unchanged either way.
const pollWaitSec = 12

// pollOnce GETs the job state, long-polling up to pollWaitSec when the node
// supports it (`?wait=`). The error covers transport-level failure only; an
// HTTP answer (any status) comes back as a jobPoll with a nil error.
func (r *runner) pollOnce(ctx context.Context, base, jobID string) (jobPoll, error) {
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/jobs/" + jobID + fmt.Sprintf("?wait=%d", pollWaitSec)
	return pollJobOnceAt(ctx, r.cfg, u)
}

// mintJobID mints the delegator-owned job id: "agd-" + 24 hex chars from
// crypto/rand (roast delta 14; no new deps — hex over ULID). crypto/rand
// failure falls back to a nanotime suffix rather than panicking: a weaker id
// still dedupes correctly against this delegator's own re-dispatches.
func mintJobID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("agd-t%d", time.Now().UnixNano())
	}
	return "agd-" + hex.EncodeToString(b[:])
}
