package pairworkloads

// C02 / C03 / C04 (2026-10-09): what a swept card says. The sweep closes the card of a producer that died
// with the failure text "harness process exited before the job finished", the same words whether the
// process was killed an hour into a render, died while its job was still queued, or was a delegator whose
// job a node went on to finish and a recovery pass filed. The decision on evidence: where the job died
// with its process, the red card is correct and is KEPT, and now says what the sweep knows (the process,
// whether the job had started, how long it had run, that no outcome was reported); where the card was
// never a harness job (a seat-watch stretch, whose watcher died) it is closed as the stretch it was, not
// as a failed job; and a delegation's card says that the node may have finished the job and where the
// result lands. Nothing here makes a failure green: whether PAIR lets a later completed frame replace a
// card that is already failed is not something this tree can establish, so none is sent.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sweepNow(startedAtMs, afterMin int64) func() time.Time {
	return func() time.Time { return time.UnixMilli(startedAtMs + afterMin*60_000) }
}

func errorOf(t *testing.T, f map[string]any) string {
	t.Helper()
	s, _ := f["error"].(string)
	if s == "" {
		t.Fatalf("a failed frame with no error: %v", f)
	}
	return s
}

// A job that died while queued never started: the card says so.
func TestSweepSaysAJobThatDiedQueuedNeverStarted(t *testing.T) {
	r := newOrphanRig(t)
	e := r.emitter(nil, nil)
	e.Emit(Event{JobID: "lease-1-9", Model: "gpu reserve", Engine: "comfyui", State: "queued", Requester: "offload-harness/s1", CreatedAt: 1_000})
	e.Wait()
	s := r.emitter(dead, nil)
	s.now = sweepNow(1_000, 3)
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("closed %d, want 1", n)
	}
	got := errorOf(t, r.failedFrames()[0])
	want := fmt.Sprintf("%s: pid %d gone while queued: it never started, no outcome reported", OrphanError, os.Getpid())
	if got != want {
		t.Errorf("error = %q\nwant    %q", got, want)
	}
}

// A job that died running says how long into the run, and that nobody reported an outcome.
func TestSweepSaysHowLongAJobHadRunWhenItsProcessWasGone(t *testing.T) {
	r := newOrphanRig(t)
	runningJob(r.emitter(nil, nil), "lease-1-9")
	s := r.emitter(dead, nil)
	s.now = sweepNow(2_000, 7)
	s.SweepOrphans(context.Background())
	got := errorOf(t, r.failedFrames()[0])
	want := fmt.Sprintf("%s: pid %d gone 7m into the run, no outcome reported (killed, console closed or reboot)", OrphanError, os.Getpid())
	if got != want {
		t.Errorf("error = %q\nwant    %q", got, want)
	}
	// A long render reads in hours.
	r2 := newOrphanRig(t)
	runningJob(r2.emitter(nil, nil), "lease-2-9")
	s2 := r2.emitter(dead, nil)
	s2.now = sweepNow(2_000, 7*60+5)
	s2.SweepOrphans(context.Background())
	if got := errorOf(t, r2.failedFrames()[0]); !strings.Contains(got, "gone 7h5m into the run") {
		t.Errorf("a 7 hour render reads in hours: %q", got)
	}
}

// Every variant fits the card whole: CardError cuts at CardErrorMax with an ellipsis, and a cut sweep
// message loses exactly the part that says what happened.
func TestEverySweptCardErrorFitsTheCardWhole(t *testing.T) {
	variants := map[string]openMarker{
		"queued":        {PID: 1234567, Info: map[string]json.RawMessage{"state": json.RawMessage(`"queued"`), "createdAt": json.RawMessage(`1000`)}},
		"running":       {PID: 1234567, Info: map[string]json.RawMessage{"state": json.RawMessage(`"running"`), "startedAt": json.RawMessage(`2000`)}},
		"transcribe":    {PID: 1234567, Info: map[string]json.RawMessage{"id": json.RawMessage(`"call-1000-1"`), "model": json.RawMessage(`"transcribe"`), "state": json.RawMessage(`"queued"`), "createdAt": json.RawMessage(`1000`)}},
		"relayed":       {PID: 0, Info: map[string]json.RawMessage{"state": json.RawMessage(`"running"`), "startedAt": json.RawMessage(`2000`)}},
		"agd-open":      {PID: 1234567, delegation: delegationOpen, Info: map[string]json.RawMessage{"id": json.RawMessage(`"agd-1"`), "state": json.RawMessage(`"running"`), "startedAt": json.RawMessage(`2000`)}},
		"agd-recovered": {PID: 1234567, delegation: delegationRecovered, Info: map[string]json.RawMessage{"id": json.RawMessage(`"agd-1"`), "state": json.RawMessage(`"running"`), "startedAt": json.RawMessage(`2000`)}},
	}
	for name, m := range variants {
		for _, afterMin := range []int64{0, 45, 24*60 + 59} {
			text := orphanText(m, time.UnixMilli(2_000+afterMin*60_000))
			if !strings.HasPrefix(text, OrphanError) {
				t.Errorf("%s: %q does not start with the failure text consumers match", name, text)
			}
			if got := CardError(text); got != text || len([]rune(text)) > CardErrorMax {
				t.Errorf("%s (+%dm): %d runes, the card shows %d: %q", name, afterMin, len([]rune(text)), len([]rune(got)), text)
			}
		}
	}
	if got := orphanText(variants["relayed"], time.UnixMilli(2_000+60_000)); strings.Contains(got, "pid 0") {
		t.Errorf("a relayed card has no pid of this box: %q", got)
	}
}

// ---- a delegation whose delegator died -------------------------------------------------------------

func writeIntents(t *testing.T, r *orphanRig, lines ...string) {
	t.Helper()
	p := filepath.Join(filepath.Dir(r.dir), "delegate-intent.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sweepOne(t *testing.T, r *orphanRig, id string, afterMin int64) string {
	t.Helper()
	runningJob(r.emitter(nil, nil), id)
	s := r.emitter(dead, nil)
	s.now = sweepNow(2_000, afterMin)
	s.SweepOrphans(context.Background())
	failed := r.failedFrames()
	if len(failed) != 1 {
		t.Fatalf("%s: want one failed frame, got %d", id, len(failed))
	}
	return errorOf(t, failed[0])
}

func TestSweepSaysADelegationsNodeMayHaveFinishedTheJob(t *testing.T) {
	r := newOrphanRig(t)
	writeIntents(t, r, `{"e":"d","job":"agd-1","base":"http://node-a:18811","ts":1}`)
	got := sweepOne(t, r, "agd-1", 7)
	want := fmt.Sprintf("%s: delegator pid %d gone 7m in; the node may still finish it, see delegate-recovered/", OrphanError, os.Getpid())
	if got != want {
		t.Errorf("error = %q\nwant    %q", got, want)
	}
}

func TestSweepSaysARecoveredDelegationsNodeReportedItsEndAndWhereItLanded(t *testing.T) {
	r := newOrphanRig(t)
	writeIntents(t, r,
		`{"e":"d","job":"agd-2","base":"http://node-a:18811","ts":1}`,
		`{"e":"ok","job":"agd-2","note":"recovered to delegate-recovered/agd-2.json","ts":2}`)
	got := sweepOne(t, r, "agd-2", 7)
	want := fmt.Sprintf("%s: delegator pid %d gone; the node reported the job's end; see delegate-recovered/", OrphanError, os.Getpid())
	if got != want {
		t.Errorf("error = %q\nwant    %q", got, want)
	}
	// The recovery pass files a node's ERRORED job the same way it files a finished one (the intent closes
	// with "recovered to ..." for state done and state error alike), so the card must not say "finished".
	if strings.Contains(strings.TrimPrefix(got, OrphanError), "finished") {
		t.Errorf("the recovered note covers an errored job too; the card must not claim it finished: %q", got)
	}
}

func TestSweepIgnoresTheIntentLedgerWhenItHasNothingToSay(t *testing.T) {
	generic := fmt.Sprintf("%s: pid %d gone 7m into the run, no outcome reported (killed, console closed or reboot)", OrphanError, os.Getpid())
	for name, lines := range map[string][]string{
		"no ledger at all":          nil,
		"another job":               {`{"e":"d","job":"agd-other","base":"http://x","ts":1}`},
		"closed: terminal observed": {`{"e":"d","job":"agd-3","base":"http://x","ts":1}`, `{"e":"ok","job":"agd-3","note":"terminal observed","ts":2}`},
		"closed: node lost the job": {`{"e":"d","job":"agd-3","base":"http://x","ts":1}`, `{"e":"ok","job":"agd-3","note":"node no longer holds the job (restarted?) - result lost","ts":2}`},
		"garbage lines are skipped": {"not json", `{"e":"d"}`},
	} {
		r := newOrphanRig(t)
		if lines != nil {
			writeIntents(t, r, lines...)
		}
		if got := sweepOne(t, r, "agd-3", 7); got != generic {
			t.Errorf("%s: error = %q\nwant      %q", name, got, generic)
		}
	}
	// A card that is not a delegation never reads the ledger.
	r := newOrphanRig(t)
	writeIntents(t, r, `{"e":"d","job":"lease-5-9","base":"http://x","ts":1}`)
	if got := sweepOne(t, r, "lease-5-9", 7); got != strings.Replace(generic, "agd-3", "lease-5-9", 1) {
		t.Errorf("a lease card read the delegation ledger: %q", got)
	}
}

// ---- a seat-watch stretch whose WATCHER died ---------------------------------------------------------

// The stretch is not a harness job: its producer is the fleet-serve that was watching, and that process
// dying says nothing about the seat or its callers. It is closed as completed (what a clean stop does)
// and never as "harness process exited before the job finished".
func TestSweepClosesADeadWatchersSeatCardAsCompletedNotFailed(t *testing.T) {
	r := newOrphanRig(t)
	e := r.emitter(nil, nil)
	e.Emit(Event{JobID: "seat-qwen3.8-27b-vllm-1000", Model: "qwen3.8-27b-vllm", Engine: "vllm", State: "running", Requester: SeatRequester, CreatedAt: 1_000, StartedAt: 1_000})
	e.Wait()
	s := r.emitter(dead, nil)
	s.now = sweepNow(1_000, 11)
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("closed %d, want 1", n)
	}
	if f := r.failedFrames(); len(f) != 0 {
		t.Fatalf("a seat stretch whose watcher died was closed as a failure: %v", f)
	}
	var done map[string]any
	for i := 0; i < r.pair.count(); i++ {
		if r.pair.method(i) == "workload:completed" {
			done = r.pair.info(i)
		}
	}
	if done == nil {
		t.Fatalf("no completed frame was sent; methods: %v", methods(r))
	}
	if done["state"] != "completed" || done["id"] != "seat-qwen3.8-27b-vllm-1000" || done["requesterId"] != SeatRequester {
		t.Errorf("completed frame wrong: %v", done)
	}
	if _, has := done["error"]; has {
		t.Errorf("a completed frame carries no error: %v", done["error"])
	}
	if got := int64(done["completedAt"].(float64)); got != 1_000+11*60_000 {
		t.Errorf("completedAt = %d, want the moment the sweep found the watcher gone", got)
	}
	if files := r.files(t); len(files) != 0 {
		t.Errorf("the marker must go with the card: %v", files)
	}
	// A harness job with the same shape of frame is still a failure.
	r2 := newOrphanRig(t)
	if got := sweepOne(t, r2, "agd-9", 5); !strings.HasPrefix(got, OrphanError) {
		t.Errorf("a real orphan lost its failure text: %q", got)
	}
}

func methods(r *orphanRig) []string {
	var out []string
	for i := 0; i < r.pair.count(); i++ {
		out = append(out, fmt.Sprint(r.pair.method(i)))
	}
	return out
}
