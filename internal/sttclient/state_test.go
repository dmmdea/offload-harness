package sttclient

// state_test.go pins the test-side half of register C-91's review: the transcription client
// keeps process-global state between calls (the set of models warmed on each llama-swap
// base), and a test that left some behind changed the verdict of a later one.
//
// The set is keyed by base URL, and httptest hands out ephemeral ports that the OS reuses, so
// a test that warmed a model on a base and never unloaded it left that entry for any later
// test whose stand-in got the same port. An UnloadIfIdle there then sent an unload for a
// model that test never used, and TestACallThatNeverReachedTheUpstreamUnloadsNothing was
// reported flaky under -count (a port is only sometimes reused: twenty repeated runs on the
// unfixed code passed). resetClientState is what every test that transcribes calls first;
// the test below shares ONE base across its subtests, which is what a reused port does by
// chance, so the leak is reproduced on every run instead of now and then.

import (
	"context"
	"testing"
	"time"
)

// resetClientState forgets what this process believes it has warmed, now and again when the
// test ends, so no test sees another's state whatever port its stand-in was given.
func resetClientState(t *testing.T) {
	t.Helper()
	forget := func() {
		inferMu.Lock()
		defer inferMu.Unlock()
		warmed = map[string]map[string]bool{}
	}
	forget()
	t.Cleanup(forget)
}

// TestClientStateDoesNotLeakFromOneTestToTheNext: the first subtest warms a model and never
// unloads it; the next one, on the very same base, must find nothing warm. It proves both
// halves of the helper: a test that reset leaves the state clean for one that does not, and
// a test that resets starts clean even after one that left state behind without resetting.
func TestClientStateDoesNotLeakFromOneTestToTheNext(t *testing.T) {
	isolateKeepSet(t)
	fake := newBurstSwap(t, time.Millisecond) // ONE base for every subtest, as a reused port is
	c := New(fake.srv.URL, 10*time.Second)
	wav := writeTestWav(t)
	ctx := context.Background()
	freed := func(t *testing.T) {
		t.Helper()
		before := fake.unloadCount()
		if err := c.UnloadIfIdle(ctx); err != nil {
			t.Fatalf("UnloadIfIdle: %v", err)
		}
		if n := fake.unloadCount() - before; n != 0 {
			t.Fatalf("%d unload(s) sent for a model no call of THIS test warmed: the previous test's state leaked", n)
		}
	}

	t.Run("a test that resets, warms a model and never unloads it", func(t *testing.T) {
		resetClientState(t)
		if _, err := c.Transcribe(ctx, "whisper-stt", wav, DefaultParams()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("leaves nothing warm for a test that does not reset", freed)

	t.Run("a test that never resets leaves a model warm", func(t *testing.T) {
		inferMu.Lock()
		defer inferMu.Unlock()
		c.markWarm("whisper-stt")
	})
	t.Run("and a test that resets starts without it", func(t *testing.T) {
		resetClientState(t)
		freed(t)
	})
}
