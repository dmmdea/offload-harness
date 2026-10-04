package sttclient

import (
	"context"
	"testing"
	"time"
)

// A job a fleet node holds at its stt gate (fleet_stt_max_concurrent, D18) has not joined the line
// yet, but it will transcribe next: the call ahead of it must not free the model it is about to
// use, or every job of a burst would pay a cold start. Queued is how the gate says so.
func TestUnloadIfIdleLeavesTheModelForAJobQueuedBehindTheGate(t *testing.T) {
	isolateKeepSet(t)
	resetClientState(t)
	fake := newBurstSwap(t, time.Millisecond)
	c := New(fake.srv.URL, 10*time.Second)
	ctx := context.Background()
	if _, err := c.Transcribe(ctx, "whisper-stt", writeTestWav(t), DefaultParams()); err != nil {
		t.Fatal(err)
	}

	stopWaiting := Queued()
	if err := c.UnloadIfIdle(ctx); err != nil {
		t.Fatalf("UnloadIfIdle: %v", err)
	}
	if n := fake.unloadCount(); n != 0 {
		t.Fatalf("%d unload(s) sent while a job was queued behind the gate: it would find the model cold", n)
	}

	stopWaiting()
	stopWaiting() // idempotent: a deferred release after an explicit one must not go negative
	if err := c.UnloadIfIdle(ctx); err != nil {
		t.Fatalf("UnloadIfIdle: %v", err)
	}
	if n := fake.unloadCount(); n != 1 {
		t.Fatalf("%d unload(s) once nothing was queued, want 1", n)
	}

	// The double release above took back one job, not two: a count that went negative would let the
	// next queued job be forgotten.
	if _, err := c.Transcribe(ctx, "whisper-stt", writeTestWav(t), DefaultParams()); err != nil {
		t.Fatal(err)
	}
	again := Queued()
	defer again()
	if err := c.UnloadIfIdle(ctx); err != nil {
		t.Fatalf("UnloadIfIdle: %v", err)
	}
	if n := fake.unloadCount(); n != 1 {
		t.Fatalf("%d unload(s) in total after a second job queued, want still 1: a double release must not go negative", n)
	}
}
