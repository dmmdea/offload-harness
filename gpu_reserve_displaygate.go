package main

// The display card's rule for a lease that NAMES its cards: `gpu reserve --devices`, the set the wrapped command
// pins itself to, the cards an allocated lease held when the reserve takes its place in the line again, and the
// detached holder of any of those.
//
// The rule (operator rule 2026-11-07): the display card takes a job only while it is idle (the operator away by the
// presence probe and the card under 15 % busy), only up to gpu_display_cap_pct of its VRAM (default 60, the desktop
// included), and only for a job that declares its footprint (--vram) wherever the cap holds something back. Until
// this file the rule was the allocator's alone: `--cards` asked it, and a set named any other way went straight into
// the line as "the operator's word". Agents name cards too, and one named the display card with --devices while the
// operator was at the desk; the card went to 83 % of its VRAM under a render the operator never asked for. A named
// set that includes the display card is now held to the allocator's rule, in the allocator's words
// (gpulease.DesktopRefusals, gpualloc.GrantCheck):
//
//   - While the card is not open to the request (the operator at the desk, the card busy, a reading that is stale or
//     cannot say which card the monitor is on, the desktop grown past the cap) the request WAITS for that to change,
//     for as long as --wait lasts, and it waits OUTSIDE the line: a request parked on the screen holds no place that
//     keeps another request off the other cards it named. With no wait left it is refused with the reason.
//   - Once the card is open it takes its place in the line with a grant-time check: a turn that comes after the
//     operator is back is refused (the cards go to the next in line) and the request waits again, keeping its
//     arrival time.
//   - What no wait can change is refused at once instead of being waited on for hours: the operator closed the card
//     (gpu_display_cap_pct 0), no footprint declared where the cap holds one back, or a footprint larger than the cap
//     leaves on the card even with nothing else on it.
//
// A set that does not include the display card, and a whole-node lease, are not touched here. Admission is all this
// is: a job admitted while the operator was away keeps its card when he returns.

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpualloc"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// displayGatePollEvery is how often a named request waiting for the display card reads it again. The operator's idle
// threshold is minutes, so a few seconds loses nothing and keeps nvidia-smi and the presence probe quiet.
const displayGatePollEvery = 5 * time.Second

// acquireFn takes a place in the line for opts.Devices: acquireQueued in the foreground reserve, the manager's own
// Acquire in the detached holder (whose stderr the parent reads back).
type acquireFn func(opts gpulease.Options, wait time.Duration) (*gpulease.Lease, error)

// displayHint is what the caller already knows about a named set and the display card, from a card table it read.
type displayHint int

const (
	// displayUnknown: no table was read against the set (a hand-run holder). The gate reads one; when it cannot, the
	// set takes its place in the line as it always did, because nothing shows it is the screen's card.
	displayUnknown displayHint = iota
	// displayClear: the caller's table shows the set clear of the display card: straight to the line, no further read,
	// so a reserve that never names the screen's card costs nothing more than it did.
	displayClear
	// displayNamed: the caller's table shows the set includes the display card (or a card of a reading that could not
	// say which card that is). A table the gate cannot read then takes nothing: it is tried again while --wait lasts.
	displayNamed
)

// hintFrom is the hint a card table gives about ids; a nil table gives none.
func hintFrom(cards []gpuprobe.Card, ids []string) displayHint {
	switch {
	case cards == nil:
		return displayUnknown
	case cardsNameDisplay(cards, ids):
		return displayNamed
	}
	return displayClear
}

// acquireNamedCards is acquire, with the display card's rule put first to a named set that includes that card. build
// reads the allocator's input with the request's declared footprint (buildAllocInput); prefix leads every line this
// writes ("gpu reserve", "gpu hold"); hint is what the caller's own card table said about the set.
func acquireNamedCards(opts gpulease.Options, wait time.Duration, hint displayHint, build func() (gpulease.AllocInput, error),
	acquire acquireFn, out io.Writer, sleep func(time.Duration), now func() time.Time, prefix string) (*gpulease.Lease, error) {
	if len(opts.Devices) == 0 || build == nil || hint == displayClear {
		return acquire(opts, wait) // the whole node, no way to read the cards, or a set shown clear of the screen: as before
	}
	if wait < 0 {
		wait = 0
	}
	deadline := now().Add(wait)
	remaining := func() time.Duration {
		if r := deadline.Sub(now()); r > 0 {
			return r
		}
		return 0
	}
	// budget is the wait handed to the line: the caller's own --wait while nothing here has waited (a card-table read
	// takes milliseconds, and the line's entry and refusal lines name the --wait the caller gave), what is left after.
	waited := false
	budget := func() time.Duration {
		if !waited {
			return wait
		}
		return remaining()
	}
	pause := func() {
		sleep(displayGatePollEvery)
		waited = true
	}
	said := ""
	var queuedSince time.Time
	for {
		if cancelClosed(opts.Cancel) {
			return nil, gpulease.ErrCancelled
		}
		in, err := build()
		if err != nil && hint == displayUnknown && !waited {
			// Only the card table fails a build. Nothing has shown this set to be the screen's card (no table was read
			// against it before), so it takes its place in the line as it did before this rule.
			return acquire(opts, wait)
		}
		if err != nil {
			// A table showed this set includes the display card, and without a fresh one the request cannot be shown to
			// stay within the rule, so nothing is taken: the read is tried again while --wait lasts.
			why := "the card table could not be read (" + err.Error() + "), so these cards cannot be checked against the display card's rule"
			if remaining() <= 0 {
				return nil, fmt.Errorf("%s: %s; nothing was taken", prefix, why)
			}
			if said != "table" {
				fmt.Fprintf(out, "%s: %s; trying again for up to %s\n", prefix, why, remaining().Round(time.Second))
				said = "table"
			}
			pause()
			continue
		}
		if !namesDisplayCard(in, opts.Devices) {
			return acquire(opts, budget())
		}
		if why := displayNeverAdmits(in, opts.Devices); why != "" {
			return nil, fmt.Errorf("%s: %s", prefix, why)
		}
		refusals := gpulease.DesktopRefusals(in, opts.Devices)
		if len(refusals) == 0 {
			if queuedSince.IsZero() {
				queuedSince = now()
			}
			o := opts
			o.QueuedSince = queuedSince
			o.GrantCheck = gpualloc.GrantCheck(build, opts.Devices)
			lease, aerr := acquire(o, budget())
			var refused *gpulease.ErrGrantRefused
			if errors.As(aerr, &refused) && remaining() > 0 {
				fmt.Fprintf(out, "%s: the display card was not open when this request's turn came (%s); waiting for it again\n", prefix, refused.Reason)
				said = ""
				waited = true
				continue
			}
			return lease, aerr
		}
		why := refusalLine(refusals)
		if remaining() <= 0 {
			return nil, fmt.Errorf("%s: %s, so it is not taken now; pass --wait <duration> to wait for it to open (it opens while the operator is away and the card is idle)", prefix, why)
		}
		// Said once per kind of refusal, never per poll: the detail carries the idle seconds, which change every read.
		if key := refusalKey(refusals); key != said {
			fmt.Fprintf(out, "%s: %s; waiting up to %s for it to open\n", prefix, why, remaining().Round(time.Second))
			said = key
		}
		pause()
	}
}

// namesDisplayCard reports whether ids include the display card, or a card of a reading that could not say which card
// the monitor is on.
func namesDisplayCard(in gpulease.AllocInput, ids []string) bool {
	return cardsNameDisplay(in.Cards, ids)
}

// cardsNameDisplay is namesDisplayCard over a card table the caller already holds.
func cardsNameDisplay(cards []gpuprobe.Card, ids []string) bool {
	want := idSet(ids)
	for _, c := range cards {
		if (c.Display || c.DisplayUnknown) && want[c.LeaseID()] {
			return true
		}
	}
	return false
}

// displayNeverAdmits is the refusal no wait can change, for the display card among ids: the operator closed it, the
// request declares no footprint where the cap holds one back, or its footprint is more than the cap leaves on the card
// with nothing else on it. "" when waiting can help.
func displayNeverAdmits(in gpulease.AllocInput, ids []string) string {
	want := idSet(ids)
	for _, c := range in.Cards {
		if !c.Display || !want[c.LeaseID()] {
			continue
		}
		switch {
		case in.DisplayCapPct <= 0:
			return fmt.Sprintf("card %d is the operator's screen and gpu_display_cap_pct is 0: the operator has closed it to jobs; name other cards (or use --cards)", c.NvidiaIndex)
		case in.DisplayCapPct >= 100 && in.DisplayFloorGiB <= 0:
			continue // the whole card is open and nothing is held to a footprint
		case in.FootprintGiB <= 0:
			return fmt.Sprintf("card %d is the operator's screen, which takes a job only up to gpu_display_cap_pct %d %% of its VRAM, and this request declares no footprint: pass --vram <GiB> (what the job loads on that card) so it can be held under the cap", c.NvidiaIndex, in.DisplayCapPct)
		}
		if budget, ok := gpulease.DisplayBudgetGiB(c.VRAMTotalGiB, in.DisplayCapPct, in.DisplayFloorGiB); ok && in.FootprintGiB > budget+1e-6 {
			return fmt.Sprintf("card %d is the operator's screen: the job declares %.1f GiB and gpu_display_cap_pct %d %% of its %.1f GiB leaves at most %.1f GiB for jobs even with nothing else on it", c.NvidiaIndex, in.FootprintGiB, in.DisplayCapPct, c.VRAMTotalGiB, budget)
		}
	}
	return ""
}

// refusalLine is the refusals in the allocator's own words.
func refusalLine(rs []gpulease.Skip) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("card %d: %s", r.Index, r.Detail))
	}
	return strings.Join(parts, "; ")
}

// refusalKey names the kind of each refusal (card and reason), without the detail that changes from one read to the next.
func refusalKey(rs []gpulease.Skip) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%d:%s", r.Index, r.Reason))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func idSet(ids []string) map[string]bool {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	return want
}

// allocated reports whether a holder's request flags are an allocation (--cards) rather than a named set's footprint
// (cards "", vramGiB set), which is how a named set's --vram reaches the detached holder.
func allocated(f *reserveDeviceFlags) bool { return f != nil && f.cards != "" }
