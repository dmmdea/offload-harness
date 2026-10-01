package pipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// repackOpts.fit is pure arithmetic over explicit instants, so these tests read
// no clock: the wall ends 100 s after `now`, the grace is 30 s, the seat makes 5
// tokens a second, so 130 s buys 650 tokens.
func TestRepackFit(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	o := repackOpts{WallEnd: now.Add(100 * time.Second), Grace: 30 * time.Second, TokS: 5, RateBasis: "the seat-rates store"}
	far := now.Add(time.Hour)
	cases := []struct {
		name          string
		o             repackOpts
		now, ctx      time.Time
		budget, least int
		wantTokens    int
		wantClamped   bool
		wantSkip      bool
		wantNote      []string // substrings of the note
	}{
		{name: "the budget fits as it is", o: o, now: now, ctx: far, budget: 600, least: 100, wantTokens: 600},
		{name: "a budget that buys exactly what is left is not clamped", o: o, now: now, ctx: far, budget: 650, least: 100, wantTokens: 650},
		{name: "a budget over what the time buys is clamped to it", o: o, now: now, ctx: far, budget: 1000, least: 100, wantTokens: 650, wantClamped: true,
			wantNote: []string{"max_tokens 1000 clamped to 650", "130 s left to the wall + 30 s grace", "5.0 tok/s"}},
		{name: "what the time buys is exactly the answer: still sent", o: o, now: now, ctx: far, budget: 1000, least: 650, wantTokens: 650, wantClamped: true},
		{name: "under the answer's size: skipped, with the arithmetic", o: o, now: now, ctx: far, budget: 1000, least: 700, wantSkip: true,
			wantNote: []string{"re-pack skipped:", "130 s left to the wall + 30 s grace", "at 5.0 tok/s", "buys 650 tokens < the answer's 700", "(rate: the seat-rates store)"}},
		{name: "the ceiling binds when it is nearer than the wall", o: o, now: now, ctx: now.Add(20 * time.Second), budget: 1000, least: 50, wantTokens: 100, wantClamped: true,
			wantNote: []string{"20 s left to the ceiling"}},
		{name: "past the wall and its grace nothing is left", o: o, now: now.Add(200 * time.Second), ctx: far, budget: 1000, least: 69, wantSkip: true,
			wantNote: []string{"0 s left", "buys 0 tokens < the answer's 69"}},
		{name: "no rate: no arithmetic, whatever the clock says", o: repackOpts{WallEnd: now.Add(-time.Hour), Grace: time.Second}, now: now, ctx: now.Add(time.Second), budget: 1000, least: 100, wantTokens: 1000},
		{name: "no deadline at all: nothing to measure against", o: repackOpts{TokS: 5}, now: now, budget: 1000, least: 100, wantTokens: 1000},
		{name: "the zero options leave every caller as it was", o: repackOpts{}, now: now, ctx: far, budget: 1000, least: 100, wantTokens: 1000},
		{name: "a ceiling alone bounds a call with no wall (the rescue's shape)", o: repackOpts{TokS: 5}, now: now, ctx: now.Add(60 * time.Second), budget: 1000, least: 100, wantTokens: 300, wantClamped: true,
			wantNote: []string{"60 s left to the ceiling"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.o.fit(tc.now, tc.ctx, tc.budget, tc.least)
			if got.Skip != tc.wantSkip || got.Clamped != tc.wantClamped || (!tc.wantSkip && got.Tokens != tc.wantTokens) {
				t.Fatalf("fit = %+v, want tokens %d clamped=%v skip=%v", got, tc.wantTokens, tc.wantClamped, tc.wantSkip)
			}
			for _, want := range tc.wantNote {
				if !strings.Contains(got.Note, want) {
					t.Errorf("note = %q, want it to say %q", got.Note, want)
				}
			}
			if !tc.wantSkip && !tc.wantClamped && got.Note != "" {
				t.Errorf("an attempt that fits carries no note, got %q", got.Note)
			}
		})
	}
}

// The incident, replayed on the new arithmetic: a seat at ~5.6 tok/s ends its loop
// 219 s into a 600 s wall; the answer is 2,782 bytes. The first request (1,439
// tokens, 257 s) fits and is sent as it was. What it leaves is not enough for the
// next one, so nothing more is sent: the re-pack ends about 476 s into the run
// instead of 1,907 s (three requests, 1,439 + 8,192 + 1,439 tokens, 1,670 s).
func TestRepackFitReplaysTheIncident(t *testing.T) {
	answer := strings.Repeat("x", 2782)
	budget, least := repackBudget(answer), expectedRepackTokens(answer)
	if budget != 1439 || least != 991 {
		t.Fatalf("fixture: budget %d, least %d, want the incident's 1439 and 991", budget, least)
	}
	start := time.Unix(1_000_000, 0)
	o := repackOpts{WallEnd: start.Add(600 * time.Second), Grace: 30 * time.Second, TokS: 5.6, RateBasis: "the seat-rates store"}
	ceiling := start.Add(1800 * time.Second)

	at := start.Add(219 * time.Second) // the loop ended
	first := o.fit(at, ceiling, budget, least)
	if first.Skip || first.Clamped || first.Tokens != 1439 {
		t.Fatalf("first attempt: %+v, want the whole 1,439-token budget (411 s buys 2,301)", first)
	}
	at = at.Add(257 * time.Second) // 1,439 tokens at 5.6 tok/s, cut at the budget
	second := o.fit(at, ceiling, budget, least)
	if !second.Skip {
		t.Fatalf("second attempt: %+v, want it skipped: 154 s buys 862 tokens, under the answer's %d", second, least)
	}
	for _, want := range []string{"re-pack skipped:", "154 s left", "buys 862 tokens < the answer's 991"} {
		if !strings.Contains(second.Note, want) {
			t.Errorf("skip note = %q, want it to say %q", second.Note, want)
		}
	}
	// The escalation the old code sent unconditionally would have been clamped
	// out as well: 8,192 tokens at 5.6 tok/s is 1,463 s.
	if esc := o.fit(at, ceiling, agentRepackMaxTokensCap, least); !esc.Skip {
		t.Errorf("an 8,192-token request with 154 s left: %+v, want it skipped", esc)
	}
}

func TestRepackRatePrefersTheStoreThenTheObservedThenTheConfigured(t *testing.T) {
	cases := []struct {
		name                 string
		known                seatrate.Seat
		observed, configured float64
		want                 float64
		basis                string
	}{
		{"the store wins", seatrate.Seat{TokS: 5.6}, 9, 30, 5.6, "the seat-rates store"},
		{"this run's own rate when the store has none", seatrate.Seat{}, 9, 30, 9, "this run's observed rate"},
		{"the configured rate last", seatrate.Seat{}, 0, 30, 30, "agent_seat_tok_s"},
		{"nothing known", seatrate.Seat{}, 0, 0, 0, ""},
	}
	for _, tc := range cases {
		if got, basis := repackRate(tc.known, tc.observed, 0, tc.configured); got != tc.want || basis != tc.basis {
			t.Errorf("%s: repackRate = %v, %q, want %v, %q", tc.name, got, basis, tc.want, tc.basis)
		}
	}
}

// A run that answered as one JSON body has no observed rate (the monitor hears
// deltas), but its loop's completions measured the seat: the effective rate over the
// completions of 1,024 tokens or more. It sizes the re-pack before the configured
// rate does, and after the run's own streamed one; with none of them the bound is off.
func TestRepackRateReadsTheRunsOwnMeasuredRateBeforeTheConfiguredOne(t *testing.T) {
	cases := []struct {
		name                           string
		known                          seatrate.Seat
		observed, measured, configured float64
		want                           float64
		basis                          string
	}{
		{"the measured rate when nothing streamed and the store is empty", seatrate.Seat{}, 0, 7, 30, 7, "this run's measured rate"},
		{"the streamed rate outranks the measured one", seatrate.Seat{}, 9, 7, 30, 9, "this run's observed rate"},
		{"the store outranks both", seatrate.Seat{TokS: 5.6}, 9, 7, 30, 5.6, "the seat-rates store"},
		{"the configured rate when the run measured nothing", seatrate.Seat{}, 0, 0, 30, 30, "agent_seat_tok_s"},
	}
	for _, tc := range cases {
		if got, basis := repackRate(tc.known, tc.observed, tc.measured, tc.configured); got != tc.want || basis != tc.basis {
			t.Errorf("%s: repackRate = %v, %q, want %v, %q", tc.name, got, basis, tc.want, tc.basis)
		}
	}
}
