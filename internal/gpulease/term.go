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
	"strings"
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
// limits; zero means the installed value. The window is never changed: a request above a limit
// is recorded and warned about, and the caller gets the sentence to print.
func PlanTerm(window time.Duration, hasProgress bool, maxTerm, maxTotal time.Duration) TermPlan {
	if window <= 0 {
		window = DefaultTTL
	}
	if maxTerm <= 0 {
		maxTerm = MaxTerm()
	}
	if maxTotal <= 0 {
		maxTotal = MaxTotal()
	}
	limit := maxTerm
	if hasProgress && limit < MaxProgressTerm {
		limit = MaxProgressTerm
	}
	p := TermPlan{Window: window, Term: window, Cap: limit, MaxTotal: maxTotal}
	var notes []string
	if window > limit {
		p.Term, p.Requested = limit, window
		which := "gpu_max_term_min"
		if hasProgress && maxTerm < MaxProgressTerm {
			which += "; " + MaxProgressTerm.String() + " with a progress contract"
		}
		notes = append(notes, fmt.Sprintf("the requested window %s is above the %s cap on a term (%s): it is accepted whole and never shortened, and it renews in terms of %s, only while its owner is alive and the job is progressing, up to %s from now; a term that is not renewed is labelled expired, never released",
			window, limit, which, limit, maxTotal))
	}
	if maxTotal < window {
		p.MaxTotal = window
		notes = append(notes, fmt.Sprintf("the requested window %s is above the %s maximum total (gpu_max_total_min): it is accepted whole and never shortened, and never renewed past its end",
			window, maxTotal))
	}
	p.Warning = strings.Join(notes, "; and ")
	return p
}

// stampTerm writes the plan for the window being declared onto a new record.
func (m *Manager) stampTerm(meta *Meta, opts Options, window time.Duration) {
	p := PlanTerm(window, opts.ProgressFile != "", opts.MaxTerm, opts.MaxTotal)
	meta.TermMs = p.Term.Milliseconds()
	meta.RequestedMs = p.Requested.Milliseconds()
	meta.MaxTotalMs = p.MaxTotal.Milliseconds()
}

// hardEndOf is the instant after which the record is no longer renewed; zero when it carries
// no maximum total.
func hardEndOf(meta *Meta) time.Time {
	if meta.MaxTotalMs <= 0 || meta.AcquiredAtMs <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(meta.AcquiredAtMs + meta.MaxTotalMs)
}

// totalMs is how long after acquisition the record may be renewed: its own stamp, else the
// installed limit, and never less than the window it already declared (a request is never
// judged past its end).
func totalMs(rec *Meta) int64 {
	if rec.MaxTotalMs > 0 {
		return rec.MaxTotalMs
	}
	total := MaxTotal().Milliseconds()
	if w := rec.ExpiresAtMs - rec.AcquiredAtMs; w > total {
		total = w
	}
	return total
}

// termMs is how far one renewal moves the end: the record's own term, else its declared window
// under the installed cap (a record written before terms was one term of its window).
func termMs(rec *Meta) int64 {
	if rec.TermMs > 0 {
		return rec.TermMs
	}
	limit := MaxTerm().Milliseconds()
	if w := rec.ExpiresAtMs - rec.AcquiredAtMs; w > 0 && w < limit {
		return w
	}
	return limit
}

// nextEnd is the declared end after one renewal: one term past the old end (so terms run back
// to back), or one term from now when the holder missed a whole term (a suspended machine),
// clipped to the hard end. ok is false when the hard end leaves nothing to renew.
func nextEnd(rec *Meta, now time.Time) (end int64, ok bool) {
	hard := rec.AcquiredAtMs + totalMs(rec)
	cur := rec.ExpiresAtMs
	if cur >= hard {
		return 0, false
	}
	nowMs := now.UnixMilli()
	end = cur + termMs(rec)
	if end <= nowMs {
		end = nowMs + termMs(rec)
	}
	if end > hard {
		end = hard
	}
	if end <= nowMs {
		return 0, false
	}
	return end, true
}

// termDue reports whether the record's declared window has ended. A record that declares no
// end has no term to end.
func termDue(rec *Meta, now time.Time) bool {
	return rec.ExpiresAtMs > 0 && now.UnixMilli() > rec.ExpiresAtMs
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
	rec, err := m.recordOf(epoch)
	if err != nil {
		return TermResult{}, err
	}
	now := m.now()
	if !termDue(rec, now) {
		return notDue(rec), nil
	}

	// The term has ended. Read what vouches for the lease BEFORE the lock (a registry read, a
	// stat and perhaps a nvidia-smi sample are not for holding the epoch lock across); the lock
	// then re-checks the record and applies the answer, so two ticks of one term end cannot
	// renew it twice.
	info := infoFrom(rec, now)
	owner, _ := m.ownerStateWhy(info.Owner)
	prog := m.progressOf(info, now)
	renewable, why := termRenewable(rec, owner, prog, sig.UtilWorking)

	// Already labelled, and for the same reason: nothing to write.
	if !renewable && rec.Expired && rec.ExpiredWhy == why {
		return TermResult{Outcome: TermStillExpired, PrevEnd: endOf(rec), End: endOf(rec), Why: why}, nil
	}

	var res TermResult
	err = m.Restamp(epoch, func(cur *Meta) {
		if !termDue(cur, now) { // another tick renewed it between our read and the lock
			res = notDue(cur)
			return
		}
		prev := endOf(cur)
		reason := why
		if renewable {
			if end, ok := nextEnd(cur, now); ok {
				// A record written before terms is pinned to its terms the first time it is
				// renewed, so its hard end cannot slide forward with every renewal.
				cur.TermMs, cur.MaxTotalMs = termMs(cur), totalMs(cur)
				cur.ExpiresAtMs = end
				cur.Expired, cur.ExpiredWhy = false, ""
				res = TermResult{Outcome: TermExtended, PrevEnd: prev, End: endOf(cur), Why: why}
				return
			}
			reason = fmt.Sprintf("it reached its maximum total of %s from acquisition (gpu_max_total_min): however healthy its owner and its progress look, it is no longer renewed",
				time.Duration(totalMs(cur))*time.Millisecond)
		}
		outcome := TermExpired
		if cur.Expired && cur.ExpiredWhy == reason {
			outcome = TermStillExpired
		}
		cur.Expired, cur.ExpiredWhy = true, reason
		res = TermResult{Outcome: outcome, PrevEnd: prev, End: endOf(cur), Why: reason}
	})
	if err != nil {
		return TermResult{}, err
	}
	return res, nil
}

// AdvanceTerm is Manager.AdvanceTerm for the lease's own holder.
func (l *Lease) AdvanceTerm(sig TermSignals) (TermResult, error) {
	if l.done.Load() {
		return TermResult{}, fmt.Errorf("gpulease: lease already released")
	}
	return l.mgr.AdvanceTerm(l.epoch, sig)
}

func endOf(rec *Meta) time.Time {
	if rec.ExpiresAtMs <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(rec.ExpiresAtMs)
}

func notDue(rec *Meta) TermResult {
	return TermResult{Outcome: TermNotDue, PrevEnd: endOf(rec), End: endOf(rec)}
}

// recordOf reads the record of one live lease by epoch: its own e/<epoch>.json, else the
// whole-node meta.json when that is its epoch.
func (m *Manager) recordOf(epoch uint64) (*Meta, error) {
	if rec, err := readEpochRecord(m.leaseDir(), epoch); err == nil {
		return rec, nil
	}
	meta, err := m.readMeta()
	if err != nil || meta == nil {
		return nil, fmt.Errorf("gpulease: the lease is gone (epoch %d)", epoch)
	}
	if meta.Epoch != epoch {
		return nil, fmt.Errorf("gpulease: epoch %d is not a lease held here (the whole-node record is epoch %d)", epoch, meta.Epoch)
	}
	return meta, nil
}

// termRenewable is the rule. An unattended lease is vouched for by its progress file alone: no
// one is expected at the desk, so neither an owner nor a busy card says anything. An attended
// one needs its owner alive AND something moving: its progress file advancing or, the weaker
// evidence the plan allows, its cards working. A gone or unknown owner is never rescued by a
// busy card: a card in use proves a process, not that anyone wants the result.
func termRenewable(rec *Meta, owner OwnerState, prog ProgressView, util func() bool) (bool, string) {
	advancing := prog.Declared && prog.State == ProgressAdvancing
	if rec.Unattended || owner == OwnerRemote {
		if advancing {
			return true, "it is unattended and its progress file is advancing"
		}
		return false, "it is unattended and its progress contract is not advancing (" + progressSummary(prog) + ")"
	}
	switch owner {
	case OwnerAlive:
		if advancing {
			return true, "its owner is alive and its progress file is advancing"
		}
		if util != nil && util() {
			return true, "its owner is alive and its cards are working"
		}
		return false, "its owner is still there but neither its progress file nor its cards show work"
	case OwnerGone:
		return false, "its owner is gone"
	}
	return false, "no owner is recorded as present for it (its owner cannot be told), so nothing vouches for it"
}

// progressSummary is the clause behind a progress contract that is not advancing.
func progressSummary(p ProgressView) string {
	switch {
	case !p.Declared:
		return "it declares none"
	case p.State == ProgressStalled:
		return fmt.Sprintf("no activity for %s, past its %s stall window", p.Age.Round(time.Second), p.Stall)
	}
	if p.Problem != "" {
		return "its progress file " + p.Problem
	}
	return "its progress file cannot be read"
}
