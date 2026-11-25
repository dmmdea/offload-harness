// Package seatdrain waits for the calls running on models a GPU lease is about to unload, so that clearing the
// cards cuts nothing that is in flight (ADR 0104).
//
// THE DEFECT (2026-11-25). A lease drained ONE model, the agent seat, and then unloaded every other resident
// model at once, with no look at any of them: four transcriptions were answered "matrix: model unloaded" at the
// second a `gpu reserve --drain --unload-seat` was granted. Two things were missing. Nothing read the other
// models' own in-flight counts. And nothing could have seen a transcription anyway: whisper-server publishes no
// gauge, and the call was registered nowhere outside the process that sent it.
//
// WHAT COUNTS AS IN FLIGHT. The engine's own gauge where it has one (seatload.Inflight), a load in progress
// (a request is waiting on it), and the calls registered on the model (modelaffinity.Call), which is all a
// gauge-less engine has. A model is waited for on positive evidence. A gauge that cannot be read is retried for a
// few seconds and then named and not waited for: an engine nobody can read must not hold a lease for its whole
// queue budget, and the unload that follows is what always happened to it.
//
// WHY IT ENDS. The holder stamps the models on its lease first (gpulease.Meta.Clearing), so new harness calls to
// them queue at the fence and the number in flight only falls; every call has its own timeout; the caller's
// deadline bounds the whole wait; and a state that has not changed for longer than any call may last (StuckAfter)
// ends it. It never interrupts anything: on either of the last two the wait fails and nothing is unloaded.
package seatdrain

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

const (
	// defaultEvery is the poll interval of a wait.
	defaultEvery = 2 * time.Second
	// maxUnreadable is how many reads in a row a gauge may fail before its model is no longer waited for.
	maxUnreadable = 5
	// rosterTimeout bounds the one read of the names llama-swap serves.
	rosterTimeout = 5 * time.Second
	// reminderEvery bounds how often an unchanged wait is said again.
	reminderEvery = 5 * time.Minute
)

// Spec is one wait.
type Spec struct {
	Client   *http.Client
	Endpoint string // llama-swap's root
	// Models are the ids, as /running lists them, that are about to be unloaded.
	Models []string
	// Registry is the machine's run registry; nil reads no registered calls.
	Registry *gpuactivity.Registry
	// Counts says which registered runs are waited for; nil is every one on a model. A caller whose lease already
	// fences the cards counts only the calls that are ON an engine (OnEngine): an agent run or a vision call can be
	// registered while it waits for that very lease.
	Counts func(gpuactivity.Run) bool
	// Gauge also reads each engine's own in-flight count.
	Gauge bool
	// Deadline is the end of the wait; the zero time is no deadline but ctx.
	Deadline time.Time
	// StuckAfter ends the wait when nothing in it has changed for this long; 0 disables it.
	StuckAfter time.Duration
	// Every is the poll interval (0 = 2 s).
	Every time.Duration
	// Say is told each change of what is waited for, and nothing while it stays the same.
	Say func(string)
}

// OnEngine is Spec.Counts for a holder whose lease already fences the cards: the calls whose mark means the
// request is on the engine right now.
func OnEngine(r gpuactivity.Run) bool {
	return r.Kind == gpuactivity.KindSTT || r.Kind == gpuactivity.KindForecast
}

// roster reads the names llama-swap serves; ok is false when it could not be read, and then only the exact ids match.
func roster(ctx context.Context, client *http.Client, endpoint string) (swapclient.Roster, bool) {
	rctx, cancel := context.WithTimeout(ctx, rosterTimeout)
	defer cancel()
	r, err := swapclient.FetchRosterWith(rctx, client, endpoint, rosterTimeout)
	return r, err == nil
}

// Names is every name llama-swap answers to for models (each id and its aliases), for the lease record: a caller
// names a model by whichever it was configured with. A roster that cannot be read leaves the ids.
func Names(ctx context.Context, client *http.Client, endpoint string, models []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if k := strings.ToLower(strings.TrimSpace(n)); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, n)
		}
	}
	for _, m := range models {
		add(m)
	}
	if r, ok := roster(ctx, client, endpoint); ok {
		for _, n := range r.Names() {
			if id, found := r.Canonical(n); found && seen[strings.ToLower(id)] {
				add(n)
			}
		}
	}
	return out
}

// Wait returns nil once no call is in flight on any of s.Models, and an error naming what still was when the
// deadline passed, nothing changed for StuckAfter, or ctx ended. Nothing is interrupted either way.
func Wait(ctx context.Context, s Spec) error {
	if len(s.Models) == 0 {
		return nil
	}
	every := s.Every
	if every <= 0 {
		every = defaultEvery
	}
	say := s.Say
	if say == nil {
		say = func(string) {}
	}
	w := &waiter{Spec: s, say: say, unreadable: map[string]int{}, said: map[string]bool{}, canon: func(n string) string { return n }}
	if r, ok := roster(ctx, s.Client, s.Endpoint); ok {
		w.canon = func(n string) string {
			if id, found := r.Canonical(n); found {
				return id
			}
			return n
		}
	}
	// A gauge can read zero between two requests of one caller, so a gauge-reading wait asks for two idle looks. A mark
	// is written before its call looks at the fence (modelaffinity.Call), so for marks alone one look is the answer.
	confirm := 1
	if s.Gauge {
		confirm = 2
	}
	start, idle := time.Now(), 0
	printed, printedAt := "", time.Time{}
	lastKey, since := "", start
	for {
		now := time.Now()
		text, key := w.look(ctx, now)
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for the calls on %s: %w", strings.Join(s.Models, ", "), err)
		}
		if key == "" {
			if idle++; idle >= confirm {
				if printed != "" {
					say(fmt.Sprintf("no call is in flight on %s any more (waited %s)", strings.Join(s.Models, ", "), span(now.Sub(start))))
				}
				return nil
			}
		} else {
			idle = 0
			switch {
			case key != printed:
				say("waiting for the calls in flight: " + text)
				printed, printedAt = key, now
			case now.Sub(printedAt) >= reminderEvery:
				say(fmt.Sprintf("still waiting after %s for the calls in flight: %s", span(now.Sub(start)), text))
				printedAt = now
			}
			if key != lastKey {
				lastKey, since = key, now
			} else if s.StuckAfter > 0 && now.Sub(since) >= s.StuckAfter {
				return fmt.Errorf("nothing has changed for %s, longer than any call may last (%s): work in flight was not interrupted", span(now.Sub(since)), text)
			}
			if !s.Deadline.IsZero() && !now.Before(s.Deadline) {
				return fmt.Errorf("calls were still in flight after %s (%s): work in flight was not interrupted", span(now.Sub(start)), text)
			}
		}
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("waiting for the calls on %s: %w", strings.Join(s.Models, ", "), ctx.Err())
		case <-t.C:
		}
	}
}

// waiter is one Wait's state between looks.
type waiter struct {
	Spec
	say        func(string)
	canon      func(string) string
	unreadable map[string]int
	said       map[string]bool
}

// look reads every model once. text is what is in flight, for a person; key changes exactly when that changes (it carries
// no age), and "" means nothing is.
func (w *waiter) look(ctx context.Context, now time.Time) (text, key string) {
	var runs []gpuactivity.Run
	if w.Registry != nil {
		runs = w.Registry.List(now)
	}
	var texts, keys []string
	for _, m := range w.Models {
		var what, id []string
		if w.Gauge {
			if t, k := w.gauge(ctx, m); t != "" {
				what, id = append(what, t), append(id, k)
			}
		}
		for _, r := range runs {
			if w.Counts != nil && !w.Counts(r) {
				continue
			}
			if !strings.EqualFold(r.Seat, m) && !strings.EqualFold(w.canon(r.Seat), m) {
				continue
			}
			what, id = append(what, r.Summary(now)), append(id, fmt.Sprintf("run:%s:%d:%s", r.ID, r.Step, r.Phase))
		}
		if len(what) > 0 {
			texts = append(texts, m+": "+strings.Join(what, " | "))
			sort.Strings(id)
			keys = append(keys, m+"="+strings.Join(id, ","))
		}
	}
	return strings.Join(texts, "; "), strings.Join(keys, ";")
}

// gauge is what the model's engine itself says is in flight; "" when it says nothing is, or nothing can be read.
func (w *waiter) gauge(ctx context.Context, model string) (text, key string) {
	rd, err := seatload.Inflight(ctx, w.Client, w.Endpoint, model)
	switch {
	case errors.Is(err, seatload.ErrNoEngineGauge):
		w.once(model+":nogauge", fmt.Sprintf("%s publishes no in-flight count: only the calls registered on it are waited for", model))
		w.unreadable[model] = 0
		return "", ""
	case err != nil:
		w.unreadable[model]++
		if w.unreadable[model] > maxUnreadable {
			w.once(model+":unreadable", fmt.Sprintf("the in-flight count of %s cannot be read (%v): only the calls registered on it are waited for", model, err))
			return "", ""
		}
		return "its in-flight count could not be read (" + err.Error() + ")", "unreadable"
	}
	w.unreadable[model] = 0
	switch {
	case !rd.Loaded, rd.Stopping:
		return "", ""
	case rd.Starting:
		return "a load is in progress, and a request is waiting on it", "starting"
	case rd.Inflight > 0:
		return fmt.Sprintf("%d request(s) in flight", rd.Inflight), fmt.Sprintf("inflight:%d", rd.Inflight)
	}
	return "", ""
}

func (w *waiter) once(id, line string) {
	if !w.said[id] {
		w.said[id] = true
		w.say(line)
	}
}

// span rounds a duration for a message.
func span(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(time.Second)
}
