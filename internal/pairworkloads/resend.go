package pairworkloads

import (
	"context"
	"errors"
	"time"
)

// Resending refused in-flight frames.
//
// An in-flight frame (queued, running) gets a marker in the open-card register but no delivery
// bookkeeping: only a terminal frame is parked when its post fails (orphans.go), and a sweep leaves a
// live producer's in-flight marker alone. A queued or running frame posted while PAIR's ingress was
// down was therefore lost for good. The card first appeared with the job's NEXT frame, often its
// terminal one, so a node could run a job for many minutes with no card in PAIR's Jobs list, no
// line and no activity ring (2026-11-09: PAIR restarted mid-delegation; two subtasks ran and queued
// on a remote node for 13 minutes with nothing shown).
//
// The producer now keeps the latest refused in-flight frame of each job and posts it again every
// inflightRetryInterval, and at once after any successful post (PAIR is back), until it lands, the
// job's next frame supersedes it, or it is older than inflightRetryMaxAge. A rejected frame (HTTP
// 400/413/422) is not retried, as for terminal frames. Ordering is safe: PAIR's broker store only
// moves a job's state forward (queued < running < terminal), so a late resend of an older frame can
// never regress a newer one.

const (
	inflightRetryInterval = 10 * time.Second
	// inflightRetryMaxAge drops a refused frame nobody superseded: a job that has been in flight this
	// long is the orphan sweep's business, not a resend's.
	inflightRetryMaxAge = OpenMaxAge
)

// unsentFrame is the latest in-flight frame of one job that PAIR did not take.
type unsentFrame struct {
	pl    sendPlan
	seq   int64
	since time.Time
}

// noteFrame records that ev's frame is the newest one planned for its job and returns its sequence
// number, or 0 for a frame that is not retried (no job id, or terminal: a terminal frame supersedes
// every in-flight one and has its own parking).
func (e *Emitter) noteFrame(ev Event) int64 {
	if ev.JobID == "" {
		return 0
	}
	e.unsentMu.Lock()
	defer e.unsentMu.Unlock()
	if isTerminal(ev.State) {
		delete(e.frameSeq, ev.JobID)
		delete(e.unsent, ev.JobID)
		return 0
	}
	e.frameN++
	if e.frameSeq == nil {
		e.frameSeq = map[string]int64{}
	}
	e.frameSeq[ev.JobID] = e.frameN
	return e.frameN
}

// settleInflight books the outcome of one post. n is noteFrame's number (0 = not retried). A refused
// in-flight frame that is still its job's newest is kept for resending; a delivered one clears an older
// kept frame of its job; any delivery wakes the resender, because PAIR is evidently back.
func (e *Emitter) settleInflight(jobID string, n int64, pl sendPlan, err error) {
	e.unsentMu.Lock()
	if err == nil {
		if u, ok := e.unsent[jobID]; ok && n != 0 && u.seq <= n {
			delete(e.unsent, jobID)
		}
		wake := len(e.unsent) > 0
		e.unsentMu.Unlock()
		if wake {
			e.wakeResender()
		}
		return
	}
	var rej *rejectedError
	if n == 0 || errors.As(err, &rej) || e.frameSeq[jobID] != n {
		e.unsentMu.Unlock()
		return
	}
	if e.unsent == nil {
		e.unsent = map[string]unsentFrame{}
	}
	e.unsent[jobID] = unsentFrame{pl: pl, seq: n, since: e.now()}
	start := !e.resending
	e.resending = true
	e.unsentMu.Unlock()
	if start {
		go e.resendLoop()
	}
}

func (e *Emitter) wakeResender() {
	e.unsentMu.Lock()
	if e.resendWake == nil {
		e.resendWake = make(chan struct{}, 1)
	}
	ch := e.resendWake
	e.unsentMu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

// resendLoop runs while refused in-flight frames are kept, and ends when none are left.
func (e *Emitter) resendLoop() {
	every := e.retryEvery
	if every <= 0 {
		every = inflightRetryInterval
	}
	e.unsentMu.Lock()
	if e.resendWake == nil {
		e.resendWake = make(chan struct{}, 1)
	}
	wake := e.resendWake
	e.unsentMu.Unlock()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-wake:
		}
		if !e.resendUnsent() {
			return
		}
	}
}

// resendUnsent posts every kept frame once and reports whether any are still kept. The loop's end is
// decided under the same lock settleInflight starts it under, so a frame kept meanwhile is never left
// without a loop.
func (e *Emitter) resendUnsent() bool {
	now := e.now()
	e.unsentMu.Lock()
	batch := make(map[string]unsentFrame, len(e.unsent))
	for id, u := range e.unsent {
		if now.Sub(u.since) > inflightRetryMaxAge {
			delete(e.unsent, id)
			continue
		}
		batch[id] = u
	}
	e.unsentMu.Unlock()
	for id, u := range batch {
		err := e.deliver(context.Background(), u.pl)
		var rej *rejectedError
		if err == nil || errors.As(err, &rej) {
			e.unsentMu.Lock()
			if cur, ok := e.unsent[id]; ok && cur.seq == u.seq {
				delete(e.unsent, id)
			}
			e.unsentMu.Unlock()
		}
	}
	e.unsentMu.Lock()
	defer e.unsentMu.Unlock()
	if len(e.unsent) == 0 {
		e.resending = false
		return false
	}
	return true
}
