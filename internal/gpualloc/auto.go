package gpualloc

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// Plan is a request for cards the caller does not name: Min..Max of them, picked by the
// allocator.
type Plan struct {
	Min, Max int
	// Hint is appended to the error when too few cards qualify and the wait ran out (the reserve
	// verb says which flag keeps waiting; a media call says nothing, there is no flag to pass).
	Hint string
}

// PickAuto turns a Plan into a concrete set, from live state. free reports which kind of set it
// is: true = cards that qualify and are free right now (the caller claims them and, if another
// claimant got there first, picks again); false = no qualifying set is free, and ids is the
// fixed set to queue FIFO on (the free cards first, topped up from the cards a live lease
// claims). When too few cards qualify for a reason waiting for a lease does not fix (a display
// card, quarantine, host RAM, VRAM) it polls until they do or wait runs out.
//
// Progress lines go to out (io.Discard is fine); the caller's clock and sleep are injected.
func PickAuto(plan Plan, wait time.Duration, build func() (gpulease.AllocInput, error),
	out io.Writer, sleep func(time.Duration), now func() time.Time) (ids []string, free bool, err error) {
	deadline := now().Add(wait)
	told := false
	for {
		in, err := build()
		if err != nil {
			return nil, false, err
		}
		in.Min, in.Max = plan.Min, plan.Max
		a, aerr := gpulease.Allocate(in)
		if aerr == nil {
			return a.Devices, true, nil
		}
		var none *gpulease.NoCardsError
		if !errors.As(aerr, &none) {
			return nil, false, aerr
		}
		if none.HostReason == "" && len(none.Waitable) >= plan.Min {
			target := none.Waitable[:plan.Min]
			fmt.Fprintf(out, "gpu reserve: no %d card(s) are free right now; queueing for %s (%s)\n", plan.Min, strings.Join(target, ", "), SkipSummary(none))
			return target, false, nil
		}
		if wait <= 0 || !now().Before(deadline) {
			return nil, false, fmt.Errorf("%w%s", none, plan.Hint)
		}
		if !told {
			told = true
			fmt.Fprintf(out, "gpu reserve: fewer than %d card(s) qualify (%s); waiting up to %s for that to change\n", plan.Min, SkipSummary(none), wait)
		}
		sleep(2 * time.Second)
	}
}

// GrantCheck is the check a queued request makes when its turn comes (gpulease.Options.GrantCheck).
// The set it was queued on was chosen against the operator's presence and the desktop floor as they
// were at the enqueue, and a place in line can be hours long: the allocator's input is built again
// (build, the same reader that chose the set), so presence and the display card's free VRAM are read
// as they are at the grant, and the desktop rule (gpulease.DesktopRefusals) is put to ids. A card
// table that cannot be read refuses too: a grant that cannot be shown to keep the desktop its floor
// is not made.
//
// The check runs with the cards already granted to the request (gpulease.VetGrant), so the lease
// directory lists them as claimed; those claims are the request's own and are not read as a rival's.
func GrantCheck(build func() (gpulease.AllocInput, error), ids []string) func() error {
	return func() error {
		in, err := build()
		if err != nil {
			return fmt.Errorf("the grant-time reading failed: %w", err)
		}
		for _, id := range ids {
			delete(in.Claimed, id)
		}
		refusals := gpulease.DesktopRefusals(in, ids)
		if len(refusals) == 0 {
			return nil
		}
		parts := make([]string, 0, len(refusals))
		for _, r := range refusals {
			parts = append(parts, fmt.Sprintf("card %d %s (%s)", r.Index, r.Reason, r.Detail))
		}
		return errors.New(strings.Join(parts, "; "))
	}
}

// SkipSummary is the one line that says why cards were skipped.
func SkipSummary(e *gpulease.NoCardsError) string {
	var parts []string
	if e.HostReason != "" {
		parts = append(parts, e.HostReason)
	}
	for _, s := range e.Skipped {
		parts = append(parts, fmt.Sprintf("card %d %s", s.Index, s.Reason))
	}
	if len(parts) == 0 {
		return "no card qualifies"
	}
	return strings.Join(parts, ", ")
}
