package pairworkloads

// 0.180 media-branch final check: the sweep's log line said "closed N PAIR card(s) whose harness process exited
// before the job finished" over every card it closed, and since C04 that count includes the seat-watch stretches it
// closes COMPLETED (their watcher died, the seat and its callers did not) and the terminal frames a producer parked.
// A line that says a harness process exited before its job finished over a seat stretch is a claim the sweep does
// not hold. The count is split by what each card was.

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(old); log.SetFlags(flags) })
	return &buf
}

func sweepLine(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "closed ") && strings.Contains(l, "PAIR card") {
			return l
		}
	}
	return ""
}

func openSeatStretch(e *Emitter, id string) {
	e.Emit(Event{JobID: id, Model: "qwen3.8-27b-vllm", Engine: "vllm", State: "running", Requester: SeatRequester, CreatedAt: 1_000, StartedAt: 1_000})
	e.Wait()
}

func TestTheSweepLogSplitsOrphansFromSeatStretches(t *testing.T) {
	r := newOrphanRig(t)
	e := r.emitter(nil, nil)
	runningJob(e, "agd-1")
	runningJob(e, "agd-2")
	openSeatStretch(e, "seat-a-1000")
	buf := captureLog(t)
	s := r.emitter(dead, nil)
	s.now = sweepNow(1_000, 11)
	if n := s.SweepOrphans(context.Background()); n != 3 {
		t.Fatalf("closed %d, want 3", n)
	}
	line := sweepLine(t, buf)
	if line == "" {
		t.Fatalf("no sweep line logged:\n%s", buf.String())
	}
	if !strings.Contains(line, "closed 3 PAIR card(s)") {
		t.Errorf("the line must still say how many cards it closed in all: %q", line)
	}
	if !strings.Contains(line, "2 whose harness process exited before the job finished") {
		t.Errorf("only the two harness jobs exited before their job finished: %q", line)
	}
	if !strings.Contains(line, "1 seat-watch stretch") || !strings.Contains(line, "closed completed") {
		t.Errorf("the seat stretch is counted apart and said to be closed completed: %q", line)
	}
}

// A sweep that closed only seat stretches must not say any harness process exited.
func TestTheSweepLogSaysNothingOfAnExitedProcessOverSeatStretchesAlone(t *testing.T) {
	r := newOrphanRig(t)
	e := r.emitter(nil, nil)
	openSeatStretch(e, "seat-a-1000")
	openSeatStretch(e, "seat-b-1000")
	buf := captureLog(t)
	s := r.emitter(dead, nil)
	s.now = sweepNow(1_000, 11)
	if n := s.SweepOrphans(context.Background()); n != 2 {
		t.Fatalf("closed %d, want 2", n)
	}
	line := sweepLine(t, buf)
	if line == "" || strings.Contains(line, "exited before the job finished") {
		t.Errorf("seat stretches are not jobs that did not finish: %q", line)
	}
	if !strings.Contains(line, "2 seat-watch stretch") {
		t.Errorf("both stretches are counted: %q", line)
	}
}

// A sweep that closed only orphans keeps the line operators grep for, byte for byte.
func TestTheSweepLogKeepsItsOldLineWhenOnlyOrphansWereClosed(t *testing.T) {
	r := newOrphanRig(t)
	runningJob(r.emitter(nil, nil), "agd-1")
	buf := captureLog(t)
	s := r.emitter(dead, nil)
	s.now = sweepNow(1_000, 11)
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("closed %d, want 1", n)
	}
	if line := sweepLine(t, buf); line != "pairworkloads: closed 1 PAIR card(s) whose harness process exited before the job finished" {
		t.Errorf("line = %q", line)
	}
}

// A terminal frame its producer parked (the post failed) and a later sweep delivers is the producer's own verdict:
// nothing exited early, so it is neither of the two counts above.
func TestTheSweepLogCountsAParkedTerminalFrameApart(t *testing.T) {
	r := newOrphanRig(t)
	e := r.emitter(nil, nil)
	runningJob(e, "agd-9")
	files := r.files(t)
	if len(files) != 1 {
		t.Fatalf("want one marker, got %v", files)
	}
	path := filepath.Join(r.dir, files[0])
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m openMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.Pending = true
	m.Info["state"] = json.RawMessage(`"completed"`)
	m.Info["completedAt"] = json.RawMessage(`3000`)
	out, _ := json.Marshal(m)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	buf := captureLog(t)
	s := r.emitter(dead, nil)
	s.now = sweepNow(1_000, 11)
	if n := s.SweepOrphans(context.Background()); n != 1 {
		t.Fatalf("closed %d, want 1", n)
	}
	line := sweepLine(t, buf)
	if strings.Contains(line, "exited before the job finished") || !strings.Contains(line, "terminal frame") {
		t.Errorf("a parked frame is its producer's own verdict, not an early exit: %q", line)
	}
}
