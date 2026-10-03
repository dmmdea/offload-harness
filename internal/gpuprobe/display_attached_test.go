package gpuprobe

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// The display card on the 3-card box (measured 2026-10-03, operator's screen asleep):
// display_active read Disabled on EVERY card while display_attached read Yes on the card
// that drives the monitor. display_active is only true while a display is initialised
// (a game, a lit screen), so a rule built on it alone never fires at the desk.
//
// TWO QUESTIONS, TWO RULES. "May a render be PLACED on this card" is answered for the card table
// and the allocator (ScreenCardUUIDs, BuildCards): display_active Enabled OR display_attached
// Yes, so the monitor's card is never auto-picked, screen on or off. "Whose load is this" (the
// node's work_util_pct, the lease verdict, the foreign-load guard, the delegator's free-card
// count) stays on display_active alone (DisplayCardUUIDs): display_attached is true for the
// whole life of the box, so reading it there would hide the node's OWN work on that card and
// change what every host with card-scoped leases OFF advertises.

const (
	attA = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"
	attB = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff"
	attC = "GPU-cccc3333-dddd-eeee-ffff-000000000000"
)

func TestParseSmiMemoryDevicesDisplayAttached(t *testing.T) {
	out := strings.Join([]string{
		"0, " + attA + ", NVIDIA GeForce RTX 5060 Ti, 16311, 100, 0, Disabled, No",
		"1, " + attB + ", NVIDIA GeForce RTX 5070 Ti, 16303, 900, 2, Disabled, Yes",
		"2, " + attC + ", NVIDIA GeForce RTX 5060 Ti, 16311, 100, 0, Disabled, No",
	}, "\r\n")
	devs, err := ParseSmiMemoryDevices(out)
	if err != nil || len(devs) != 3 {
		t.Fatalf("parse: %v (%d devices)", err, len(devs))
	}
	for i, wantAttached := range []bool{false, true, false} {
		if devs[i].DisplayAttached != wantAttached {
			t.Errorf("card %d: DisplayAttached = %v, want %v", i, devs[i].DisplayAttached, wantAttached)
		}
		if devs[i].DisplayActive {
			t.Errorf("card %d: DisplayActive must stay false (the driver said Disabled)", i)
		}
	}
}

// Only an exact Yes is a yes: "[Not Supported]" / "[N/A]" mean "we do not know" and an
// unknown is never the operator's screen.
func TestDisplayAttachedParsesOnlyAnExactYes(t *testing.T) {
	for _, tc := range []struct {
		name, tail string
		want       bool
	}{
		{"yes", ", Disabled, Yes", true},
		{"yes any case", ", Disabled, yes", true},
		{"no", ", Disabled, No", false},
		{"not supported", ", Disabled, [Not Supported]", false},
		{"n/a", ", Disabled, [N/A]", false},
		{"empty", ", Disabled, ", false},
		{"seven fields, no attached column", ", Disabled", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			devs, err := ParseSmiMemoryDevices("1, " + attB + ", RTX 5070 Ti, 16303, 14027, 33" + tc.tail)
			if err != nil || len(devs) != 1 {
				t.Fatalf("parse: %v (%d devices)", err, len(devs))
			}
			if devs[0].DisplayAttached != tc.want {
				t.Fatalf("DisplayAttached = %v, want %v", devs[0].DisplayAttached, tc.want)
			}
		})
	}
}

func TestDrivesDisplayIsActiveOrAttached(t *testing.T) {
	for _, tc := range []struct {
		d    Device
		want bool
	}{
		{Device{}, false},
		{Device{DisplayActive: true}, true},
		{Device{DisplayAttached: true}, true},
		{Device{DisplayActive: true, DisplayAttached: true}, true},
	} {
		if got := tc.d.DrivesDisplay(); got != tc.want {
			t.Errorf("%+v: DrivesDisplay = %v, want %v", tc.d, got, tc.want)
		}
	}
}

// The measured case: display_active Disabled everywhere, display_attached Yes on card 1. The
// card table and the allocator's rule see the monitor's card; the load-attribution rule does
// NOT (an attached monitor is not a game being played).
func TestOnlyTheCardTableCountsAnAttachedCardAsTheDisplayCard(t *testing.T) {
	devs := []Device{
		{Index: 0, UUID: attA},
		{Index: 1, UUID: attB, DisplayAttached: true},
		{Index: 2, UUID: attC},
	}
	got := ScreenCardUUIDs(devs)
	if !got[attB] || got[attA] || got[attC] || len(got) != 1 {
		t.Fatalf("want only card 1 flagged by the placement rule, got %v", got)
	}
	if load := DisplayCardUUIDs(devs); load != nil {
		t.Fatalf("an attached monitor is not a display in use: the load-attribution rule must flag nothing, got %v", load)
	}
	// And a display that IS initialised is flagged by both.
	active := []Device{{Index: 0, UUID: attA}, {Index: 1, UUID: attB, DisplayActive: true}}
	if a, b := DisplayCardUUIDs(active), ScreenCardUUIDs(active); !a[attB] || !b[attB] {
		t.Fatalf("display_active must flag card 1 in both rules, got %v / %v", a, b)
	}
	// The card table (what the allocator reads) agrees with the placement rule.
	cards, _ := BuildCards([]Device{
		{Index: 0, UUID: attA, TotalGiB: 16}, {Index: 1, UUID: attB, TotalGiB: 16, DisplayAttached: true}, {Index: 2, UUID: attC, TotalGiB: 16},
	}, "")
	if cards[0].Display || !cards[1].Display || cards[2].Display {
		t.Fatalf("card table Display = %v %v %v, want false true false", cards[0].Display, cards[1].Display, cards[2].Display)
	}
}

// The single-card guard is unchanged: a box whose only card is attached to the screen
// runs its seats there by necessity.
func TestScreenCardUUIDsAttachedSoleCardExcludesNothing(t *testing.T) {
	if got := ScreenCardUUIDs([]Device{{Index: 0, UUID: attB, DisplayAttached: true}}); got != nil {
		t.Fatalf("a single-card box must exclude nothing, got %v", got)
	}
	// Two cards, both on a screen: nothing is left to score, so nothing is excluded.
	if got := ScreenCardUUIDs([]Device{{UUID: attA, DisplayAttached: true}, {UUID: attB, DisplayActive: true}}); got != nil {
		t.Fatalf("a box with no non-display card must exclude nothing, got %v", got)
	}
}

func TestQueryArgsCarryDisplayAttachedOnlyInTheFullQuery(t *testing.T) {
	full, legacy := strings.Join(smiQueryArgs, " "), strings.Join(smiQueryArgsNoAttached, " ")
	if !strings.Contains(full, ",display_active,display_attached") {
		t.Errorf("the full query must end display_active,display_attached: %s", full)
	}
	if strings.Contains(legacy, "display_attached") || !strings.HasSuffix(strings.Fields(legacy)[0], ",display_active") {
		t.Errorf("the fallback query must be the query the harness always ran, without display_attached: %s", legacy)
	}
}

// fakeAttachedClock is an injectable clock for the fallback's re-probe window.
type fakeAttachedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeAttachedClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeAttachedClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func withAttachedClock(t *testing.T) *fakeAttachedClock {
	t.Helper()
	c := &fakeAttachedClock{t: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)}
	old := smiClock
	smiClock = c.now
	resetAttachedGate()
	t.Cleanup(func() { smiClock = old; resetAttachedGate() })
	return c
}

var errUnknownField = errors.New(`exit status 2: Field "display_attached" is not a valid field to query.`)

func TestRunDisplayAwareHealthyDriverNeverRunsTheFallback(t *testing.T) {
	withAttachedClock(t)
	var calls []bool
	out, err := RunDisplayAware(func(with bool) (string, error) { calls = append(calls, with); return "full", nil })
	if err != nil || out != "full" || len(calls) != 1 || !calls[0] {
		t.Fatalf("out=%q err=%v calls=%v, want one call with the full query", out, err, calls)
	}
}

// An older driver (or a headless build) rejects the field with a non-zero exit. The
// reader must still answer, from the query it always ran, instead of failing every card.
func TestRunDisplayAwareFallsBackWhenTheFieldIsUnsupported(t *testing.T) {
	withAttachedClock(t)
	var calls []bool
	out, err := RunDisplayAware(func(with bool) (string, error) {
		calls = append(calls, with)
		if with {
			return "", errUnknownField
		}
		return "legacy", nil
	})
	if err != nil || out != "legacy" {
		t.Fatalf("out=%q err=%v, want the fallback output", out, err)
	}
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("calls = %v, want the full query then the fallback", calls)
	}
}

// A 2 s health sampler must not pay a doomed nvidia-smi call every tick on an old driver:
// after a fallback succeeds the full query is skipped for a while, then tried again (a
// driver upgrade is picked up, and one transient failure is not a permanent downgrade).
func TestRunDisplayAwareSkipsTheDoomedQueryThenRetriesAfterTheWindow(t *testing.T) {
	clock := withAttachedClock(t)
	var calls []bool
	run := func(with bool) (string, error) {
		calls = append(calls, with)
		if with {
			return "", errUnknownField
		}
		return "legacy", nil
	}
	if _, err := RunDisplayAware(run); err != nil {
		t.Fatal(err)
	}
	calls = nil
	clock.advance(time.Minute)
	if _, err := RunDisplayAware(run); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] {
		t.Fatalf("inside the window calls = %v, want only the fallback", calls)
	}
	calls = nil
	clock.advance(attachedRetryEvery)
	if _, err := RunDisplayAware(run); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !calls[0] {
		t.Fatalf("after the window calls = %v, want the full query tried again", calls)
	}
}

// When both queries fail nvidia-smi itself is the problem (a wedged driver, no binary): the
// error is the full query's, and nothing is remembered, so the next call tries the full
// query again.
func TestRunDisplayAwareBothFailingIsAnErrorAndRemembersNothing(t *testing.T) {
	withAttachedClock(t)
	hang := errors.New("nvidia-smi: driver not responding")
	var calls []bool
	_, err := RunDisplayAware(func(with bool) (string, error) { calls = append(calls, with); return "", hang })
	if !errors.Is(err, hang) {
		t.Fatalf("err = %v, want the nvidia-smi error", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want both queries tried", calls)
	}
	calls = nil
	_, _ = RunDisplayAware(func(with bool) (string, error) { calls = append(calls, with); return "full", nil })
	if len(calls) != 1 || !calls[0] {
		t.Fatalf("after a double failure the next call must try the full query first, got %v", calls)
	}
}
