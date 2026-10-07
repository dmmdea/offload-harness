package gpulease

import "fmt"

// ErrGrantRefused is what Acquire (and VetGrant) returns when the caller's GrantCheck found, with the
// cards just granted, that the request no longer qualifies for them. It is not contention: the cards
// were free, and the answer will not change by waiting in this place in line. The grant has been
// released again. The caller decides again from fresh readings (take another card the request still
// fits, or queue on what qualifies now) and may hand the arrival time back in Options.QueuedSince to
// keep its position.
type ErrGrantRefused struct {
	// Reason is the check's own words.
	Reason string
}

func (e *ErrGrantRefused) Error() string {
	return "gpulease: the cards were free but no longer qualify for this request: " + e.Reason
}

// VetGrant puts opts.GrantCheck to a lease that has just been granted. A nil check is nil. A check
// that returns an error refuses the grant: the lease is released and the error is ErrGrantRefused,
// so the cards are free again for the next in line; a release that itself fails is reported as such
// and is NOT an ErrGrantRefused, because the cards are then still held and the caller must not go on
// as if they were free.
//
// WHY THIS EXISTS: a request queued while the operator was away was decided against the presence
// guard and the desktop floor at ENQUEUE. FIFO can take hours; the operator may have come back, or a
// game may have taken the card's memory, by the time the front of the line is reached. Nothing
// re-asked, so the queue handed the screen's card to a job the guard would now refuse.
//
// WHY AFTER THE GRANT AND NOT BEFORE IT: a check made before the claim has to know that the cards are
// free, and a lease that is mid-release reads as held for an instant and is then claimable, so a
// pre-check can be skipped by exactly the grant it was meant to vet (seen in test). Checking what the
// request already holds has no such window; the cost is a claim that is taken and given back, which
// the next waiter sees as one more poll tick. The check therefore sees its own claim on the cards
// and must not read it as a rival's.
func (m *Manager) VetGrant(l *Lease, opts Options) error {
	if opts.GrantCheck == nil || l == nil {
		return nil
	}
	return vetGrant(opts.GrantCheck, l.Release)
}

// vetGrant is VetGrant over the check and the way to give the grant back, so each outcome is
// exercised without staging a filesystem fault.
func vetGrant(check, release func() error) error {
	cerr := check()
	if cerr == nil {
		return nil
	}
	if rerr := release(); rerr != nil {
		return fmt.Errorf("gpulease: the grant no longer qualifies (%v) and releasing it failed, so the cards are still held: %w", cerr, rerr)
	}
	return &ErrGrantRefused{Reason: cerr.Error()}
}
