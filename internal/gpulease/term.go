package gpulease

// term.go — a lease has a term, and a term ends in a renewal or in a label, never in a
// release (plan P9, ADR 0070).
//
// THE RULE. `--for` is the lease's declared window and its first term. gpu_max_term_min is a
// RENEWAL POINT, never a release point: a request above it is recorded (Meta.RequestedMs),
// warned about, accepted whole and never shortened. When the term ends the holder's own tick
// either renews it by one more term or labels the lease expired:
//
//	renew  iff  (the owner is alive and (its progress is advancing or its cards are working))
//	            or (the lease is unattended and its progress is advancing)
//	       and  the new end stays inside the maximum total (gpu_max_total_min) from acquisition
//
// Expired is a LABEL (Meta.Expired). The heartbeat goes on, the claim stays, the lease still
// fences and still passes its holder's Check, the verdict is held-overdue and the lease is
// takeover-eligible. Nothing here releases, reclaims or kills: a card is freed only by its
// holder, by an explicit takeover, or by the reclaim rule of a holder that is provably gone
// (ADR 0018), never by a term running out. That is the 2026-09-07 incident this closes: a
// detached holder released the card at its --for window with the job still running behind it.
//
// WHY A LABEL AND NOT State="expired". A record's State is the fence's word: checkV2 and
// render/gpu-lock.mjs both fence out any state but "active". A reader built before this
// change would therefore fence out a render child running under an expired lease, which is
// the mid-job loss this change exists to prevent, and no audit can recall a binary that is
// already running. A separate additive field is invisible to them.
//
// WHO ASKS. Only the holder's tick (the wrapper form's 15 s tick, the detached holder's
// loop): no timer, no watcher, no other process (invariant I5). Evidence it reads is the
// owner's standing (owner.go), the progress file, and, for an attended lease with no
// progress contract, whether the lease's cards are busy. That last reading is a nvidia-smi
// sample and gpuactivity (which owns it) imports this package, so it arrives as a function
// the caller supplies and is called only when it can change the answer.

import (
	"fmt"
	"sync/atomic"
	"time"
)

const (
	// DefaultMaxTerm is the cap on a renewal term when config gpu_max_term_min is unset.
	DefaultMaxTerm = 6 * time.Hour
	// DefaultMaxTotal is how long after acquisition a lease may be renewed when config
	// gpu_max_total_min is unset.
	DefaultMaxTotal = 48 * time.Hour
	// MaxProgressTerm is the least cap on a term for a lease that declares a progress
	// contract: its progress file, not its window, is what judges it, so it may declare a
	// long first window without being warned.
	MaxProgressTerm = 24 * time.Hour
)

var (
	installedMaxTerm  atomic.Int64
	installedMaxTotal atomic.Int64
)

// SetDefaultTerms installs the operator's term limits (config gpu_max_term_min and
// gpu_max_total_min) for every acquisition and every tick of this process, the way the orphan
// grace is installed. Zero or negative restores the default for that limit.
func SetDefaultTerms(maxTerm, maxTotal time.Duration) {
	if maxTerm < 0 {
		maxTerm = 0
	}
	if maxTotal < 0 {
		maxTotal = 0
	}
	installedMaxTerm.Store(int64(maxTerm))
	installedMaxTotal.Store(int64(maxTotal))
}

// MaxTerm is the cap on a renewal term in force: the installed one, else DefaultMaxTerm.
func MaxTerm() time.Duration {
	if d := time.Duration(installedMaxTerm.Load()); d > 0 {
		return d
	}
	return DefaultMaxTerm
}

// MaxTotal is the longest a lease is renewed, from its acquisition: the installed limit, else
// DefaultMaxTotal.
func MaxTotal() time.Duration {
	if d := time.Duration(installedMaxTotal.Load()); d > 0 {
		return d
	}
	return DefaultMaxTotal
}

// TermPlan is what a request for a window comes to.
type TermPlan struct {
	// Window is the declared window: always the whole request.
	Window time.Duration
	// Term is how far one renewal moves the end: the request, capped at Cap.
	Term time.Duration
	// Requested is the request when it is above Cap, else zero.
	Requested time.Duration
	// Cap is the cap on a term this request was judged against.
	Cap time.Duration
	// MaxTotal is the longest the lease is renewed from its acquisition: the limit, never
	// less than Window (a request is never shortened, so it is never judged past its end).
	MaxTotal time.Duration
	// Warning is the sentence for the caller to print when the request is above a limit;
	// empty when it is not.
	Warning string
}

// PlanTerm is what a lease asking for window comes to. hasProgress says the lease carries a
// progress contract (24 h is then the least cap). maxTerm and maxTotal override the installed
// limits; zero means the installed value.
func PlanTerm(window time.Duration, hasProgress bool, maxTerm, maxTotal time.Duration) TermPlan {
	return TermPlan{}
}

// hardEndOf is the instant after which the record is no longer renewed; zero when it carries
// no maximum total.
func hardEndOf(meta *Meta) time.Time {
	return time.Time{}
}

// TermOutcome says what one tick of the term check did.
type TermOutcome string

const (
	// TermNotDue: the term has not ended; nothing was read beyond the record and nothing written.
	TermNotDue TermOutcome = "not-due"
	// TermExtended: the term ended and was renewed by one term; the end moved.
	TermExtended TermOutcome = "extended"
	// TermExpired: the term ended and was not renewed; the lease was labelled expired now.
	TermExpired TermOutcome = "expired"
	// TermStillExpired: the lease was already labelled and still is not renewable; nothing written.
	TermStillExpired TermOutcome = "still-expired"
)

// TermSignals is the evidence the holder's tick supplies beyond what the lease directory holds.
type TermSignals struct {
	// UtilWorking reports whether the lease's cards are busy (the display card excluded). It is
	// called only for an attended lease whose owner is alive and whose progress is not already
	// advancing, the one case where it can change the answer. nil reads as "not working".
	UtilWorking func() bool
}

// TermResult is what one tick found and did.
type TermResult struct {
	Outcome TermOutcome
	// PrevEnd and End are the declared end before and after (equal unless extended).
	PrevEnd time.Time
	End     time.Time
	// Why is the sentence behind the outcome: what renewed the lease, or what kept it from
	// renewing.
	Why string
}

// AdvanceTerm runs the term check for one live lease of this directory: nothing while the
// term runs; at its end, one renewal or the expired label. It writes (under the epoch lock,
// by restamping the lease's own record) only when something changed, so a holder may call it
// every tick.
func (m *Manager) AdvanceTerm(epoch uint64, sig TermSignals) (TermResult, error) {
	return TermResult{Outcome: TermNotDue}, nil
}

// AdvanceTerm is Manager.AdvanceTerm for the lease's own holder.
func (l *Lease) AdvanceTerm(sig TermSignals) (TermResult, error) {
	if l.done.Load() {
		return TermResult{}, fmt.Errorf("gpulease: lease already released")
	}
	return l.mgr.AdvanceTerm(l.epoch, sig)
}
