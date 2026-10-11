package gpuactivity

// F8 follow-up (2026-10-10): the wrapper's own progress file exists only while its command runs. During the
// drain that precedes the command (an hour is ordinary: `--drain --drain-timeout 90m`) and during the seat's
// warm-back after it, the lease declares the file and the file is absent, and the status line told a
// healthy lease "has not appeared in 1h, past its 15m stall window: the job has not written to it or the path
// is wrong". The verdict was always right (unknown is never stalled); the words accused the lease.

import (
	"strings"
	"testing"
	"time"
)

func TestAbsentMintedProgressFileIsNotBlamedOnTheJobOrThePath(t *testing.T) {
	busy := []GPU{{Index: 0, Name: "RTX", UtilPct: 80, UtilKnown: true, MemTotalMiB: 16000}}
	assess := func(p *ProgressState) (string, string) {
		h := &Holder{PID: 7, Alive: true, Class: "media", Epoch: 9, Unattended: true, Progress: p}
		return Assess(View{At: time.Now(), Held: true, Holder: h, GPUs: busy, Seat: SeatState{Name: "agent-pool"}})
	}
	// An hour into a drain: past the 15 minute window, the minted file does not exist because the command has not started.
	minted := &ProgressState{File: "/state/progress/wrap-4242-1791000000000000000.jsonl", State: "unknown", Problem: "does not exist", AgeSec: 3600, StallSec: 900}
	v, n := assess(minted)
	if v != VerdictHeldWorking {
		t.Fatalf("a missing file never changes the verdict: %s (%s)", v, n)
	}
	for _, want := range []string{"wrap-4242-1791000000000000000.jsonl", "creates it when its command starts", "removes it when the command ends", "drains"} {
		if !strings.Contains(n, want) {
			t.Errorf("the note lacks %q: %s", want, n)
		}
	}
	for _, bad := range []string{"path is wrong", "has not written", "has not appeared"} {
		if strings.Contains(n, bad) {
			t.Errorf("the note accuses a healthy lease (%q): %s", bad, n)
		}
	}
	// A file the CALLER named keeps the original words: there a missing file past its window is worth a look.
	_, n = assess(&ProgressState{File: "/w/log.jsonl", State: "unknown", Problem: "does not exist", AgeSec: 3600, StallSec: 900})
	if !strings.Contains(n, "path is wrong") {
		t.Errorf("a caller's own missing file lost its hint: %s", n)
	}
	// The status line the CLI prints reads the same words.
	if why := minted.UnknownWhy(); !strings.Contains(why, "creates it when its command starts") {
		t.Errorf("UnknownWhy = %q", why)
	}
}
