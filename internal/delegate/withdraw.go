// withdraw.go — the delegator's half of taking an unstarted job back from a fleet
// node (ADR 0064): where runRemote gives an acked job up, it asks the node to
// withdraw it (DELETE /fleet/jobs/{id}). Only a CONFIRMED withdrawal changes what
// the delegator does next; every other answer leaves today's behaviour as it was —
// but says, on the row, what the node answered instead.
package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// withdrawTimeout bounds ONE best-effort withdraw. A var so a test can compress
// it; production never mutates it.
var withdrawTimeout = 5 * time.Second

// withdrawBound is how long ONE withdraw may take: withdrawTimeout, and, when the ask is
// made because the call's deadline has passed, no more than three quarters of the unwind
// allowance (ADR 0065). The asking goroutine is inside that allowance, and a node that
// never answers must not turn the truthful cut result (its node and job) into an
// abandoned "did not stop" that names neither. A call without a deadline, or whose
// context ended for any other reason, keeps withdrawTimeout.
func (r *runner) withdrawBound() time.Duration {
	bound := withdrawTimeout
	if r.call.reached() {
		if g := r.call.grace * 3 / 4; g > 0 && g < bound {
			bound = g
		}
	}
	return bound
}

// withdrawnState is the `state` a node answers a successful withdraw with
// (fleetnode.WithdrawnState; the two packages do not import each other, and the
// end-to-end test in withdraw_e2e_test.go runs the real node handler against this
// reader so the pairing cannot drift).
const withdrawnState = "withdrawn"

// withdrawOutcome is what a withdraw attempt established.
type withdrawOutcome int

const (
	// withdrawUnconfirmed: the node did not say it took the job back — an old node
	// (404/405), a 401, a 5xx, a dropped connection, a timeout. Nothing is
	// known: the job may still run there, so the give-up keeps its old shape (the
	// intent stays open for recovery, the result is not re-placed).
	withdrawUnconfirmed withdrawOutcome = iota
	// withdrawConfirmed: the node took the job back before it started. It will
	// never run there.
	withdrawConfirmed
	// withdrawStarted: the node refused because the job has started (or finished).
	// It is not abandoned; the delegator keeps polling it.
	withdrawStarted
)

// withdraw asks the node at base to take jobID back and reports what it said: the
// outcome, and — for every outcome but a confirmation — WHY in words (an HTTP status
// and what it means, or that no answer came), for the row of the give-up that asked.
// An old node with no route, an upgraded node that refused the bearer, and one that
// timed out all leave the job where it was, and without the words their rows are
// byte-identical: a ghost that survives the fix could not be told from a node that
// never had it.
//
// It is best-effort by construction. The request runs on a context that OUTLIVES
// the caller's (context.WithoutCancel), because the commonest reason to withdraw
// is that the caller has just been canceled and a request bound to that context
// would die before it left; it is bounded on its own by withdrawBound, so a node
// that sits on it cannot hold the give-up. It never returns an error: an answer
// that is not a clear yes or a clear "already started" is withdrawUnconfirmed.
func (r *runner) withdraw(ctx context.Context, base, jobID string) (withdrawOutcome, string) {
	bound := r.withdrawBound()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
	defer cancel()
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/jobs/" + jobID
	req, err := http.NewRequestWithContext(wctx, http.MethodDelete, u, nil)
	if err != nil {
		return withdrawUnconfirmed, "the request could not be built: " + clip(err.Error(), 120)
	}
	if r.cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.FleetAuthToken)
	}
	resp, err := fleetClient.Do(req)
	if err != nil {
		log.Printf("delegate: withdraw of %s at %s not confirmed (%v); the give-up stays open for recovery", jobID, base, err)
		return withdrawUnconfirmed, transportWhy(err, bound)
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxFleetBody))
	switch resp.StatusCode {
	case http.StatusOK:
		var wire struct {
			State     string `json:"state"`
			Withdrawn bool   `json:"withdrawn"`
		}
		if rerr == nil && json.Unmarshal(body, &wire) == nil && (wire.Withdrawn || wire.State == withdrawnState) {
			return withdrawConfirmed, ""
		}
		log.Printf("delegate: withdraw of %s at %s answered 200 without a withdrawn verdict; not treated as confirmed, the give-up stays open for recovery", jobID, base)
		return withdrawUnconfirmed, "HTTP 200, but the answer did not say the job was taken back"
	case http.StatusConflict:
		return withdrawStarted, "HTTP 409: the node said the job had already started"
	case http.StatusNotFound:
		// A node without the route or without the job: today's behaviour, and nothing
		// worth a log line — an old node says this every time. The row says it.
		return withdrawUnconfirmed, "HTTP 404: the node does not hold the job, or has no withdraw route"
	case http.StatusMethodNotAllowed:
		return withdrawUnconfirmed, "HTTP 405: the node has no withdraw route (an older node)"
	case http.StatusUnauthorized:
		log.Printf("delegate: withdraw of %s at %s answered 401; not treated as confirmed, the give-up stays open for recovery", jobID, base)
		return withdrawUnconfirmed, "HTTP 401: the node refused this delegator's fleet_auth_token"
	}
	log.Printf("delegate: withdraw of %s at %s answered %d; not treated as confirmed, the give-up stays open for recovery", jobID, base, resp.StatusCode)
	return withdrawUnconfirmed, fmt.Sprintf("HTTP %d", resp.StatusCode)
}

// notConfirmed renders why a withdraw the delegator asked for left the job where it
// was, as the clause a give-up appends to its own reason: "withdraw not confirmed:
// HTTP 405: ...". "" when nothing was asked or the node confirmed (why is empty).
// The clause is detail, never a class: a row's reason_code stays what the give-up
// was (the queue deadline, the cancel), and the words never say "withdrawn", which
// is what a CONFIRMED withdrawal writes.
func notConfirmed(why string) string {
	if why == "" {
		return ""
	}
	return "withdraw not confirmed: " + why
}

// transportWhy words a withdraw that got no HTTP answer at all: a timeout inside its
// own bound (the one the request was given, withdrawBound), or a transport failure with
// the request's URL (which repeats the job id the row already carries) stripped off.
func transportWhy(err error, bound time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("no answer within %s", bound)
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	return "no answer (" + clip(err.Error(), 120) + ")"
}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// giveUp is the exit the give-ups that RETURN share (a canceled caller, an owned or
// unowned poll deadline): it asks the node to take an unstarted job back and records
// the verdict on pr. A confirmed withdrawal settles the intent (the job will never
// run there, so there is nothing for the recovery pass to collect); anything else
// leaves the job the recovery pass's, exactly as before.
//
// lastState is the last state the node reported for the job ("" = it never
// answered). A job last seen RUNNING has started, and the request could only be
// refused, so it is not made.
//
// It returns the clause the caller appends to its own reason when a withdraw was
// ASKED and the node did not confirm it (see notConfirmed), and "" when none was
// asked or the node confirmed.
func (r *runner) giveUp(ctx context.Context, base, jobID, lastState string, pr *PlacedResult) string {
	if lastState == "running" {
		pr.orphanable = true
		return ""
	}
	outcome, why := r.withdraw(ctx, base, jobID)
	if outcome == withdrawConfirmed {
		pr.withdrawn, pr.orphanable = true, false
		return ""
	}
	pr.orphanable = true
	return notConfirmed(why)
}

// refuseAsWithdrawn files a queue-deadline result whose job the node CONFIRMED it
// took back as a CAPACITY refusal (503), so the machinery that re-places a refused
// dispatch — isReplaceable, placements.noteRefusal, the capacity wait — treats it
// as what it is: a node that had no room for this job in time, which never ran it
// anywhere. That is the one condition under which offering the job to another node
// cannot arrange a double run. The status is the delegator's classification of the
// outcome, not something the node sent; queued is the time the job provably spent
// in that node's backlog, which the re-placement is not charged for.
func (pr *PlacedResult) refuseAsWithdrawn(queued time.Duration) {
	pr.withdrawn, pr.orphanable = true, false
	pr.refused, pr.refusalStatus = true, http.StatusServiceUnavailable
	pr.queuedWait = queued
}

// refuseAsNeverRan is refuseAsWithdrawn for a job whose terminal state the poll READ:
// the node's own record (nodeErr, "reaped: ..." or "withdrawn: ...") says it took the
// job out of its backlog without running it, so no seat anywhere holds it and the
// subtask may be offered to another node. The intent closes as never-started, in the
// note recovery uses for the same observation, not as a withdrawal this process asked
// for.
func (pr *PlacedResult) refuseAsNeverRan(nodeErr string, queued time.Duration) {
	pr.nodeNeverRan, pr.orphanable = nodeErr, false
	pr.refused, pr.refusalStatus = true, http.StatusServiceUnavailable
	pr.queuedWait = queued
}
