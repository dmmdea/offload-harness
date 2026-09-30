// withdraw.go — the delegator's half of taking an unstarted job back from a fleet
// node (ADR 0064): where runRemote gives an acked job up, it asks the node to
// withdraw it (DELETE /fleet/jobs/{id}). Only a CONFIRMED withdrawal changes what
// the delegator does next; every other answer leaves today's behaviour as it was.
package delegate

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// withdrawTimeout bounds ONE best-effort withdraw. A var so a test can compress
// it; production never mutates it.
var withdrawTimeout = 5 * time.Second

// withdrawnState is the `state` a node answers a successful withdraw with
// (fleetnode.WithdrawnState; the two packages do not import each other, and the
// end-to-end test in withdraw_e2e_test.go runs the real node handler against this
// reader so the pairing cannot drift).
const withdrawnState = "withdrawn"

// withdrawOutcome is what a withdraw attempt established.
type withdrawOutcome int

const (
	// withdrawUnconfirmed: the node did not say it took the job back — an old
	// node (404/405), a 401, a 5xx, a dropped connection, a timeout. Nothing is
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

// withdraw asks the node at base to take jobID back and reports what it said.
//
// It is best-effort by construction. The request runs on a context that OUTLIVES
// the caller's (context.WithoutCancel), because the commonest reason to withdraw
// is that the caller has just been canceled and a request bound to that context
// would die before it left; it is bounded on its own by withdrawTimeout, so a node
// that sits on it cannot hold the give-up. It never returns an error: an answer
// that is not a clear yes or a clear "already started" is withdrawUnconfirmed.
func (r *runner) withdraw(ctx context.Context, base, jobID string) withdrawOutcome {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), withdrawTimeout)
	defer cancel()
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/jobs/" + jobID
	req, err := http.NewRequestWithContext(wctx, http.MethodDelete, u, nil)
	if err != nil {
		return withdrawUnconfirmed
	}
	if r.cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.FleetAuthToken)
	}
	resp, err := fleetClient.Do(req)
	if err != nil {
		log.Printf("delegate: withdraw of %s at %s not confirmed (%v); the give-up stays open for recovery", jobID, base, err)
		return withdrawUnconfirmed
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxFleetBody))
	switch resp.StatusCode {
	case http.StatusOK:
		var wire struct {
			State     string `json:"state"`
			Withdrawn bool   `json:"withdrawn"`
		}
		if json.Unmarshal(body, &wire) == nil && (wire.Withdrawn || wire.State == withdrawnState) {
			return withdrawConfirmed
		}
	case http.StatusConflict:
		return withdrawStarted
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		// A node without the route (404/405) or without the job (404): today's
		// behaviour, and nothing worth a line — an old node says this every time.
		return withdrawUnconfirmed
	}
	log.Printf("delegate: withdraw of %s at %s answered %d; not treated as confirmed, the give-up stays open for recovery", jobID, base, resp.StatusCode)
	return withdrawUnconfirmed
}

// giveUp is the exit the give-ups that RETURN share (a canceled caller, an owned
// poll deadline): it asks the node to take an unstarted job back and records the
// verdict on pr. A confirmed withdrawal settles the intent (the job will never run
// there, so there is nothing for the recovery pass to collect); anything else
// leaves the job the recovery pass's, exactly as before.
//
// lastState is the last state the node reported for the job ("" = it never
// answered). A job last seen RUNNING has started, and the request could only be
// refused, so it is not made.
func (r *runner) giveUp(ctx context.Context, base, jobID, lastState string, pr *PlacedResult) {
	if lastState != "running" && r.withdraw(ctx, base, jobID) == withdrawConfirmed {
		pr.withdrawn, pr.orphanable = true, false
		return
	}
	pr.orphanable = true
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
