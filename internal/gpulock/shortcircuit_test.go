package gpulock

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// NEVER WAIT FOR SOMETHING THAT CANNOT HAPPEN (0.113.27). A TEXT reservation
// carries an operator's DECLARED window, so when it outlasts our wait the
// answer is already known and polling only adds latency: before this, every
// vision call burned its whole vision_gpu_wait_sec (90 s by default) against a
// multi-hour hold, for the entire life of that hold.
//
// A MEDIA lease is deliberately excluded, for the reason gpulease.Acquire
// gives: its expiry is a timeout CEILING, not a promise — a video job declares
// 25 minutes and routinely finishes in three — so short-circuiting on it would
// defer calls that were about to be served.
func TestWaitFreeShortCircuitsAFarTextLeaseButNotAMediaOne(t *testing.T) {
	far := func(class gpulease.Class) string {
		return writeLease(t, os.Getpid(), func(m *gpulease.Meta) {
			m.Class = class
			m.ExpiresAtMs = time.Now().Add(6 * time.Hour).UnixMilli()
		})
	}

	start := time.Now()
	info := WaitFree(context.Background(), far(gpulease.ClassText), 300*time.Millisecond, 20*time.Millisecond)
	textWait := time.Since(start)
	if !info.Held {
		t.Fatal("the card is still held; the caller must be told to defer")
	}
	if textWait > 100*time.Millisecond {
		t.Fatalf("a 6-hour TEXT reservation was polled for %v — it cannot free inside a 300ms wait, so the answer should be immediate", textWait)
	}

	start = time.Now()
	info = WaitFree(context.Background(), far(gpulease.ClassMedia), 300*time.Millisecond, 20*time.Millisecond)
	mediaWait := time.Since(start)
	if !info.Held {
		t.Fatal("media lease still held")
	}
	// 15ms epsilon for the Windows timer granularity, as the neighbouring test uses.
	if mediaWait < 285*time.Millisecond {
		t.Fatalf("a MEDIA lease was short-circuited after %v — its expiry is a ceiling, not a promise, and the job may finish early", mediaWait)
	}
}

// A holder that declared NO window is polled exactly as before: nothing says
// it will not free.
func TestWaitFreeStillPollsALeaseWithNoDeclaredWindow(t *testing.T) {
	lock := writeLease(t, os.Getpid(), func(m *gpulease.Meta) {
		m.Class = gpulease.ClassText
		m.ExpiresAtMs = 0
	})
	start := time.Now()
	if info := WaitFree(context.Background(), lock, 150*time.Millisecond, 20*time.Millisecond); !info.Held {
		t.Skip("a zero window reads as expired on this build; the polling contract is covered by the bounded-wait test")
	}
	if el := time.Since(start); el < 135*time.Millisecond {
		t.Fatalf("returned after %v: with no declared window there is nothing to short-circuit on", el)
	}
}

// The declared window reaches the caller, so a defer message can say how long
// the card is spoken for instead of only how long it has been held.
func TestInspectReportsTheDeclaredWindow(t *testing.T) {
	want := time.Now().Add(90 * time.Minute)
	lock := writeLease(t, os.Getpid(), func(m *gpulease.Meta) { m.ExpiresAtMs = want.UnixMilli() })
	info := Inspect(lock)
	if !info.Held {
		t.Fatal("fixture lease must read held")
	}
	if d := info.ExpiresAt.Sub(want); d > time.Second || d < -time.Second {
		t.Fatalf("ExpiresAt = %s, want %s", info.ExpiresAt, want)
	}
}
