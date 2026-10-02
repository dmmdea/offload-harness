package sttclient

// unload_models_test.go pins the second round of register C-91's review: what the last call
// out of a burst unloads. The idle test of UnloadIfIdle is process-wide (the in-line count),
// but it used to free only the model of the call that happened to be last, so a burst that
// mixed stt_model and stt_model_hq (or the two protocols on two models) left the model of
// the first finisher loaded until llama-swap's ttl, with nothing in any log to say so. The
// last call out frees every model the burst warmed on that upstream.

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAMixedBurstUnloadsEveryModelItWarmed: call A (the whisper protocol, stt_model) is on the
// upstream and call B (the OpenAI protocol, stt_model_hq) is in line behind it. A finishes
// first and leaves the unload to B, the last call out; B knows only its own model, but the
// burst warmed both, and both must go.
func TestAMixedBurstUnloadsEveryModelItWarmed(t *testing.T) {
	resetClientState(t)
	isolateKeepSet(t)
	fake := newBurstSwap(t, time.Millisecond)
	fake.models = []string{"whisper-stt", "qwen3-asr"}
	releaseA := make(chan struct{})
	fake.firstGate = func() { <-releaseA } // A's inference stays on the upstream until the test lets it go
	fake.block = make(chan struct{})       // and B's stays there until the test has seen A leave
	c := New(fake.srv.URL, 10*time.Second)
	wav := writeTestWav(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	var aErr, bErr error
	aOut := make(chan struct{})
	wg.Add(2)
	go func() { // A: stt_model on the whisper protocol
		defer wg.Done()
		_, aErr = c.Transcribe(ctx, "whisper-stt", wav, DefaultParams())
		_ = c.UnloadIfIdle(ctx)
		close(aOut)
	}()
	for deadline := time.Now().Add(5 * time.Second); fake.inferences() < 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("call A never reached the upstream")
		}
	}
	go func() { // B: stt_model_hq on the OpenAI protocol, in line behind A
		defer wg.Done()
		_, bErr = c.TranscribeOAI(ctx, "qwen3-asr", wav)
		_ = c.UnloadIfIdle(ctx)
	}()
	waitPending(t, 2)
	close(releaseA)
	select {
	case <-aOut:
	case <-time.After(5 * time.Second):
		t.Fatal("call A never came out")
	}
	if got := fake.unloadedModels(); len(got) != 0 {
		t.Fatalf("call A unloaded %v while call B was in line: B is the last one out", got)
	}
	close(fake.block)
	wg.Wait()

	if aErr != nil || bErr != nil {
		t.Fatalf("the calls failed: A: %v, B: %v", aErr, bErr)
	}
	if got, want := fake.unloadedModels(), []string{"qwen3-asr", "whisper-stt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unloads = %v, want one for each model the burst warmed (%v): the last call out knows only its own, and the other stays loaded until its ttl", got, want)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, u := range fake.unloads {
		if u.inflight != 0 || u.pending != 0 || u.served != 2 {
			t.Errorf("the unload of %s arrived with %d inference(s) in flight, %d of 2 finished and %d call(s) in line: it belongs after the last of them", u.model, u.inflight, u.served, u.pending)
		}
	}
}

// TestAFailedUnloadOfOneModelDoesNotKeepTheOthersLoaded: the models are unloaded one by one and
// a failure on one is neither a reason to leave the rest loaded nor forgotten. It is returned,
// naming the model, the model stays marked warm so the next call out retries it, and the one
// that did go is not unloaded a second time. (qwen3-asr sorts first: it is the failing one that
// is tried before the other, so a loop that stops at the first error never reaches whisper-stt.)
func TestAFailedUnloadOfOneModelDoesNotKeepTheOthersLoaded(t *testing.T) {
	resetClientState(t)
	isolateKeepSet(t)
	fake := newBurstSwap(t, time.Millisecond)
	fake.models = []string{"whisper-stt", "qwen3-asr"}
	c := New(fake.srv.URL, 10*time.Second)
	wav := writeTestWav(t)
	ctx := context.Background()
	if _, err := c.Transcribe(ctx, "whisper-stt", wav, DefaultParams()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.TranscribeOAI(ctx, "qwen3-asr", wav); err != nil {
		t.Fatal(err)
	}

	fake.setUnloadStatus("qwen3-asr", http.StatusInternalServerError)
	err := c.UnloadIfIdle(ctx)
	if err == nil || !strings.Contains(err.Error(), "qwen3-asr") || strings.Contains(err.Error(), "whisper-stt") {
		t.Fatalf("UnloadIfIdle = %v, want the failure of the one model that did not go, naming it and only it", err)
	}
	if got, want := fake.unloadedModels(), []string{"qwen3-asr", "whisper-stt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unloads = %v, want both models tried: one failing is no reason to leave the other loaded", got)
	}

	fake.setUnloadStatus("qwen3-asr", 0)
	if err := c.UnloadIfIdle(ctx); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got, want := fake.unloadedModels(), []string{"qwen3-asr", "qwen3-asr", "whisper-stt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unloads = %v, want exactly one more, of the model that failed: it stays marked warm for the next call out, and the one that went is not unloaded twice", got)
	}
}

// TestUnloadIfIdleLeavesAnotherBasesModelsAlone: the set a call out frees is the one warmed on
// ITS upstream. Two llama-swaps are two upstreams; a model warmed on the other one is not this
// client's to unload, and it is still unloaded by the client that is last out there.
func TestUnloadIfIdleLeavesAnotherBasesModelsAlone(t *testing.T) {
	resetClientState(t)
	isolateKeepSet(t)
	a, b := newBurstSwap(t, time.Millisecond), newBurstSwap(t, time.Millisecond)
	a.models = []string{"whisper-stt"}
	b.models = []string{"qwen3-asr"}
	ca, cb := New(a.srv.URL, 10*time.Second), New(b.srv.URL, 10*time.Second)
	wav := writeTestWav(t)
	ctx := context.Background()
	if _, err := ca.Transcribe(ctx, "whisper-stt", wav, DefaultParams()); err != nil {
		t.Fatal(err)
	}
	if _, err := cb.TranscribeOAI(ctx, "qwen3-asr", wav); err != nil {
		t.Fatal(err)
	}

	if err := ca.UnloadIfIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := a.unloadedModels(), []string{"whisper-stt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the first upstream saw unloads %v, want %v", got, want)
	}
	if got := b.unloadedModels(); len(got) != 0 {
		t.Errorf("the second upstream saw unloads %v from a client that never used it", got)
	}
	if err := cb.UnloadIfIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := b.unloadedModels(), []string{"qwen3-asr"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the second upstream saw unloads %v, want %v: its own last call out frees what it warmed", got, want)
	}
}
