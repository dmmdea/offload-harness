package gpuactivity

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// holdersOf describes every live lease an inspection saw. It returns the per-lease list
// (only when more than one is live: a single lease is Holder alone, the shape every
// consumer already reads) and the lease the verdict is about, which is the MOST ESCALATED
// one, so a stalled lease is never hidden behind a healthy sibling with a lower epoch.
//
// Standing is derived here, once, from the facts on each lease's record and the session
// registry (gpulease.Standing), so `gpu status`, offload_status and the fleet health all
// read a lease the same way. It stamps or clears the orphan marker; that is the one write
// of this read path, and it is a sidecar.
func holdersOf(leaseDir string, info gpulease.Info, opts Options, now time.Time) ([]Holder, *Holder) {
	leases := info.Each()
	obs := gpulease.ObserverAt(leaseDir)
	hs := make([]Holder, 0, len(leases))
	for _, l := range leases {
		h := Holder{
			Devices: l.EffectiveDevices(),
			PID:     l.PID, Alive: gpulease.PIDAlive(l.PID), Class: string(l.Class), Epoch: l.Epoch,
			Reason: l.Reason, Origin: l.Origin, Command: l.Command,
			Exclusive: l.Exclusive, Draining: l.Draining,
			AgeSec: int(l.Age.Seconds()), ExpiresAt: l.ExpiresAt,
			Expired: l.Expired, ExpiredWhy: l.ExpiredWhy,
			TermSec: int(l.Term / time.Second), RequestedSec: int(l.Requested / time.Second),
		}
		if !l.HardEnd.IsZero() {
			h.HardEnd = l.HardEnd.UTC().Format(time.RFC3339)
		}
		if !l.HeartbeatAt.IsZero() {
			h.HeartbeatAgeSec = int(now.Sub(l.HeartbeatAt).Seconds())
		}
		if l.Owner != nil {
			h.OwnerPID = l.Owner.PID
		}
		h.applyStanding(obs.Standing(l, opts.OrphanGrace), opts.OrphanGrace, now)
		h.Facts = legacyFacts(opts.ComfyDir, l, len(leases) == 1, now)
		hs = append(hs, h)
	}
	best := 0
	for i := range hs {
		if hs[i].rank() > hs[best].rank() {
			best = i
		}
	}
	head := hs[best]
	if len(hs) == 1 {
		return nil, &head
	}
	return hs, &head
}

// rank orders the standing verdicts by escalation, for choosing the headline lease.
func (h *Holder) rank() int {
	switch {
	case h.TreeOrphan:
		return 5
	case h.Stalled:
		return 4
	case h.Orphaned:
		return 3
	case h.Overdue:
		return 2
	}
	return 1
}

// applyStanding copies a gpulease.Standing onto the Holder in the JSON shape consumers read.
func (h *Holder) applyStanding(st gpulease.Standing, grace time.Duration, now time.Time) {
	if grace <= 0 {
		grace = gpulease.DefaultOrphanGrace
	}
	h.Unattended = st.Unattended
	h.OwnerState = string(st.Owner)
	h.OwnerSession = st.OwnerSession
	h.OrphanGraceS = int(grace.Seconds())
	h.Orphaned = st.Orphaned
	if !st.OrphanedSince.IsZero() {
		h.OrphanedSince = st.OrphanedSince.UTC().Format(time.RFC3339)
		h.OrphanedForS = int(now.Sub(st.OrphanedSince).Seconds())
	}
	h.Overdue = st.Overdue
	h.OverdueBySec = int(st.OverdueBy.Seconds())
	h.Stalled = st.Stalled
	h.OwnerNote = st.OwnerNote
	h.OrphanMarkErr = st.OrphanMarkErr
	if st.Progress.Declared {
		h.Progress = &ProgressState{
			File: st.Progress.File, State: st.Progress.State,
			AgeSec: int(st.Progress.Age.Seconds()), StallSec: int(st.Progress.Stall.Seconds()), Detail: st.Progress.Detail,
			Problem: st.Progress.Problem,
		}
	}
}

// ---------------------------------------------------------------------------
// The sentences behind the standing verdicts
// ---------------------------------------------------------------------------

// takeoverHint is the command that frees a lease whose holder will not: written into every
// escalated note so a waiter reading it knows the next step. Nothing in this package runs
// it; taking a lease from a living holder is always an explicit command.
func takeoverHint(h *Holder) string {
	return fmt.Sprintf("; to take it over, a human-authorised session runs: local-offload gpu takeover --epoch %d (not in this build yet: until it ships, ask whoever owns it or the operator)", h.Epoch)
}

func secs(n int) string { return (time.Duration(n) * time.Second).Round(time.Second).String() }

func ownerWho(h *Holder) string {
	switch {
	case h.OwnerSession != "":
		return "session " + h.OwnerSession
	case h.OwnerPID > 0:
		return fmt.Sprintf("process %d", h.OwnerPID)
	}
	return "the process that asked for it"
}

func orphanedNote(h *Holder) string {
	return fmt.Sprintf("the lease's owner (%s) has been gone for %s, past the %s grace: nobody is expected back for it. Its holder is still alive and heartbeating, so nothing is reclaimed or killed",
		ownerWho(h), secs(h.OrphanedForS), secs(h.OrphanGraceS))
}

func stalledNote(h *Holder) string {
	n := "the lease promised progress and made none"
	if p := h.Progress; p != nil {
		n = fmt.Sprintf("the lease promised progress and made none: %s showed no activity for %s, past its %s stall window", filepath.Base(p.File), secs(p.AgeSec), secs(p.StallSec))
		if p.Detail != "" {
			n += "; its last line: " + p.Detail
		}
	}
	return n + ". Its holder is still alive and heartbeating, so nothing is reclaimed or killed"
}

func overdueNote(h *Holder) string {
	if h.Expired {
		return fmt.Sprintf("the lease has expired: its term ended %s ago and its holder did not renew it because %s. The holder is still alive and heartbeating, so nothing is reclaimed or killed (an expired lease is a label, not a release: it is held until its holder lets go or it is taken over). Queue behind it, or ask who holds it",
			secs(h.OverdueBySec), expiredWhy(h))
	}
	return fmt.Sprintf("the lease is past its declared window by %s and its holder is still renewing: nothing is reclaimed or killed (a declared window is not a ceiling for a holder that is alive). Queue behind it, or ask who holds it",
		secs(h.OverdueBySec))
}

// expiredWhy is the recorded reason a term was not renewed, or the plain fact when a tick
// stamped the label without one.
func expiredWhy(h *Holder) string {
	if h.ExpiredWhy != "" {
		return h.ExpiredWhy
	}
	return "nothing vouched for it"
}

// progressSentence is "<file> moved <age> ago[, <detail>]".
func progressSentence(h *Holder) string {
	p := h.Progress
	s := fmt.Sprintf("%s last activity %s ago", filepath.Base(p.File), secs(p.AgeSec))
	if p.Detail != "" {
		s += ", " + p.Detail
	}
	return s
}

// evidenceTail says what a held-working or held-idle reading rests on. Card utilisation
// alone cannot tell a live job from a hung one, so when that is all there is the note says so.
func evidenceTail(h *Holder) string {
	switch {
	case h == nil:
		return ""
	case h.Progress == nil:
		return "; evidence: card utilisation only (util only, no progress contract), so a live job and a hung one look alike"
	case h.Progress.State == "advancing":
		return "; its progress file is advancing (" + progressSentence(h) + ")"
	case h.Progress.State == "unknown":
		return "; its progress file (" + filepath.Base(h.Progress.File) + ") " + h.Progress.unknownWhy() + ", so progress is unknown (never read as stalled: the verdict does not change)"
	}
	return ""
}

// unknownWhy says why a progress file's state is unknown, in the words a reader can act on:
// not written yet (inside the stall window), still missing past it (the job has not written
// to it or the path is wrong), or a stat failure that is not "not found". Age is how long
// the lease has been waiting for the file.
func (p *ProgressState) unknownWhy() string {
	switch {
	case p.Problem == "" || p.Problem == "does not exist":
		if p.StallSec > 0 && p.AgeSec > p.StallSec {
			return fmt.Sprintf("has not appeared in %s, past its %s stall window: the job has not written to it or the path is wrong",
				secs(p.AgeSec), secs(p.StallSec))
		}
		return "does not exist yet"
	}
	return p.Problem
}

// UnknownWhy is unknownWhy for the status lines the CLI prints; "" unless the state is unknown.
func (p *ProgressState) UnknownWhy() string {
	if p == nil || p.State != "unknown" {
		return ""
	}
	return p.unknownWhy()
}

// factsTail appends the information a legacy lease can give (never a verdict input).
func factsTail(h *Holder) string {
	if h == nil || len(h.Facts) == 0 {
		return ""
	}
	return "; activity facts (information, not a verdict input): " + strings.Join(h.Facts, "; ")
}

// StandingWord is the standing verdict word the holder carries, in the precedence the
// verdicts outrank each other (tree-orphan, held-stalled, held-orphaned, held-overdue), or ""
// for a lease that is healthy. It is the word a surface leads with even when the SEAT's own
// verdict is `working` (work in flight outranks the standing verdicts but must not hide them).
func (h *Holder) StandingWord() string {
	switch {
	case h == nil:
		return ""
	case h.TreeOrphan:
		return VerdictTreeOrphan
	case h.Stalled:
		return VerdictHeldStalled
	case h.Orphaned:
		return VerdictHeldOrphaned
	case h.Overdue:
		return VerdictHeldOverdue
	}
	return ""
}

// standingParts says, in short clauses, what is wrong with the lease itself (nil when
// nothing is).
func standingParts(h *Holder) []string {
	if h == nil {
		return nil
	}
	var parts []string
	if h.TreeOrphan {
		parts = append(parts, "its wrapper is gone but the job it started still holds the cards")
	}
	if h.Stalled && h.Progress != nil {
		parts = append(parts, fmt.Sprintf("its progress file has shown no activity for %s", secs(h.Progress.AgeSec)))
	}
	if h.Orphaned {
		parts = append(parts, fmt.Sprintf("its owner (%s) has been gone for %s", ownerWho(h), secs(h.OrphanedForS)))
	}
	if h.Overdue && h.Expired {
		parts = append(parts, fmt.Sprintf("its term ended %s ago and was not renewed (%s)", secs(h.OverdueBySec), expiredWhy(h)))
	} else if h.Overdue {
		parts = append(parts, fmt.Sprintf("it is past its declared window by %s", secs(h.OverdueBySec)))
	}
	return parts
}

// standingHead names, at the FRONT of a `working` reading, what the lease itself is doing:
// work in flight outranks the standing verdicts, but it must not hide them, and a note
// that is clipped to a fixed width keeps its front. "" for a healthy lease.
func standingHead(v View) string {
	parts := standingParts(v.Holder)
	if len(parts) == 0 {
		return ""
	}
	return "the lease itself is not healthy (" + strings.Join(parts, ", ") + "), although work is in flight on the seat: "
}

// ownerCaveat says, on a reading that rests on the owner, when what the owner state says is
// not to be trusted from here: the session registry could not be read, or the moment the
// owner was first seen gone could not be recorded. "" otherwise (an owner that was simply
// never tracked is shown by `gpu status`, not repeated in every note).
func ownerCaveat(h *Holder) string {
	switch {
	case h == nil:
		return ""
	case h.OrphanMarkErr != "":
		return "; " + h.OrphanMarkErr + ", so how long the owner has been gone cannot be tracked from here"
	case strings.Contains(h.OwnerNote, "registry could not be read"):
		return "; " + h.OwnerNote
	}
	return ""
}

// HeadlineOf returns the record of the lease the verdict is about (the Holder) from an
// inspection of the live leases: Info itself describes only the lowest epoch, so a surface
// that prints a lease's owner, progress contract or takeover command beside the verdict reads
// it here and never from the lowest. info itself when there is one lease or the holder is not
// among them.
func HeadlineOf(info gpulease.Info, h *Holder) gpulease.Info {
	if h == nil || h.Epoch == 0 {
		return info
	}
	if l, ok := info.Lease(h.Epoch); ok {
		return l
	}
	return info
}
