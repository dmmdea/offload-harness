package sttclient

// unload_burst_test.go pins register C-91: concurrent transcriptions used to unload whisper
// from under each other. The zero-always-warm caller unloaded after EVERY call, holding
// nothing (inferMu is released when Transcribe returns), so a finishing call's unload landed
// while the next call's inference was on the upstream: llama-swap answered the victim
// "matrix: model unloaded" with a 500, and every call queued behind it paid a cold start.
// These tests drive Transcribe and TranscribeOAI against a llama-swap stand-in that aborts
// what an unload finds in flight, the way the real proxy does.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// unloadSeen is what one unload request found when it arrived.
type unloadSeen struct {
	inflight int // inference requests being served at that moment
	served   int // inference requests already finished
	pending  int // transcriptions in this process still waiting for the upstream or running on it
}

// burstSwap stands in for llama-swap on the routes a transcription and its unload use. An
// unload that arrives while an inference is in flight ABORTS it with the 500 the real proxy
// answers, so a call unloaded from under fails here as it does there.
type burstSwap struct {
	srv *httptest.Server
	// hold is how long one inference takes.
	hold time.Duration
	// running is the /running body (guarded by mu): by default the model is ready
	// (resident), which is what lets a request through a fenced card.
	running string
	// firstGate, when set, runs once inside the first inference request, before it is
	// answered: the tests use it to hold the burst until every call is in line.
	firstGate func()
	// block, when set, holds every inference after the first until it is closed.
	block chan struct{}
	// unloadHold, when set, holds every unload's answer until it is closed; unloadSeen is
	// signalled (once) when the first unload arrives.
	unloadHold chan struct{}
	unloadSeen chan struct{}

	mu                              sync.Mutex
	gated                           bool
	abort                           chan struct{} // closed by an unload that finds an inference in flight
	inflight, peak, served, aborted int
	events                          []string // "inference" and "unload", in the order they ARRIVED
	unloads                         []unloadSeen
}

func newBurstSwap(t *testing.T, hold time.Duration) *burstSwap {
	t.Helper()
	s := &burstSwap{
		hold:    hold,
		running: `{"running":[{"model":"whisper-stt","state":"ready","ttl":300}]}`,
		abort:   make(chan struct{}),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *burstSwap) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/models":
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"whisper-stt","meta":{"llamaswap":{"aliases":["whisper","stt"]}}}]}`))
	case r.URL.Path == "/running":
		s.mu.Lock()
		body := s.running
		s.mu.Unlock()
		_, _ = w.Write([]byte(body))
	case strings.HasPrefix(r.URL.Path, "/api/models/unload/"):
		s.unload()
	case strings.HasPrefix(r.URL.Path, "/upstream/"):
		s.inference(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *burstSwap) unload() {
	s.mu.Lock()
	s.events = append(s.events, "unload")
	s.unloads = append(s.unloads, unloadSeen{inflight: s.inflight, served: s.served, pending: Pending()})
	if s.inflight > 0 {
		close(s.abort)
		s.abort = make(chan struct{})
	}
	first := len(s.unloads) == 1
	s.mu.Unlock()
	if first && s.unloadSeen != nil {
		close(s.unloadSeen)
	}
	if s.unloadHold != nil {
		<-s.unloadHold
	}
}

// inferences is how many inference requests have arrived so far.
func (s *burstSwap) inferences() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e == "inference" {
			n++
		}
	}
	return n
}

func (s *burstSwap) inference(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.inflight++
	if s.inflight > s.peak {
		s.peak = s.inflight
	}
	s.events = append(s.events, "inference")
	abort, first := s.abort, !s.gated
	s.gated = true
	s.mu.Unlock()

	if first && s.firstGate != nil {
		s.firstGate()
	}
	// A nil channel blocks forever, so a receive on one is never the case that fires: the
	// first inference (and every one when block is unset) waits out hold, the rest wait
	// for block.
	var done <-chan time.Time
	if first || s.block == nil {
		done = time.After(s.hold)
	}
	aborted := false
	select {
	case <-done:
	case <-s.block:
	case <-abort:
		aborted = true
	}

	// Accounted BEFORE the answer goes out: the client acts on the answer, and an unload it
	// sends next must find this inference finished, not still counted as in flight.
	s.mu.Lock()
	s.inflight--
	s.served++
	if aborted {
		s.aborted++
	}
	s.mu.Unlock()

	if aborted {
		http.Error(w, `{"error":"unspecific error: matrix: model unloaded","src":"llama-swap"}`, http.StatusInternalServerError)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/v1/audio/transcriptions") {
		_, _ = w.Write([]byte(`{"type":"transcript.text.done","text":"language English<asr_text>hello"}`))
		return
	}
	_, _ = w.Write([]byte(`{"language":"english","duration":1,"text":"hello","segments":[{"id":0,"start":0,"end":1,"text":"hello"}]}`))
}

// waitPending blocks until n transcriptions are in line, or fails the test after five
// seconds: a burst is only a burst once every call has registered, which is what keeps
// the first finisher from seeing an empty line.
func waitPending(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if Pending() >= n {
			return
		}
	}
	t.Errorf("only %d of %d calls were in line after 5s: a call is not counted from the top of its Transcribe", Pending(), n)
}

func writeTestWav(t *testing.T) string {
	t.Helper()
	wav := filepath.Join(t.TempDir(), "a.wav")
	if err := os.WriteFile(wav, []byte("RIFF"), 0o644); err != nil {
		t.Fatal(err)
	}
	return wav
}

// TestABurstOfTranscriptionsPaysOneUnloadAfterTheLastCall is the C-91 reproduction. Six
// calls hit one upstream at once, each followed by the zero-always-warm unload the pipeline
// sends. The stand-in aborts any inference an unload finds in flight, so the old behaviour
// (an unload after each call) fails here three ways: calls come back "matrix: model
// unloaded", more than one unload goes out, and one of them lands before the burst is over.
// What must hold: every call is answered, and exactly one unload is sent, after the last
// inference, with nobody in line.
func TestABurstOfTranscriptionsPaysOneUnloadAfterTheLastCall(t *testing.T) {
	isolateKeepSet(t)
	const n = 6
	for _, tc := range []struct {
		name string
		oai  func(i int) bool
	}{
		{"whisper protocol", func(int) bool { return false }},
		{"openai protocol", func(int) bool { return true }},
		{"both entry points at once", func(i int) bool { return i%2 == 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newBurstSwap(t, 20*time.Millisecond)
			fake.firstGate = func() { waitPending(t, n) }
			c := New(fake.srv.URL, 10*time.Second)
			wav := writeTestWav(t)

			errs := make([]error, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if tc.oai(i) {
						_, errs[i] = c.TranscribeOAI(context.Background(), "whisper-stt", wav)
					} else {
						_, errs[i] = c.Transcribe(context.Background(), "whisper-stt", wav, DefaultParams())
					}
					_ = c.UnloadIfIdle(context.Background(), "whisper-stt")
				}(i)
			}
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Errorf("call %d failed: %v", i, err)
				}
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.aborted != 0 {
				t.Errorf("%d inference(s) were aborted by an unload that landed on them", fake.aborted)
			}
			if fake.peak != 1 {
				t.Errorf("peak inference concurrency = %d, want 1 (the single-slot server is serialized)", fake.peak)
			}
			if len(fake.unloads) != 1 {
				t.Fatalf("%d unload(s) sent for a burst of %d calls, want exactly 1 — the last call out", len(fake.unloads), n)
			}
			if u := fake.unloads[0]; u.inflight != 0 || u.served != n || u.pending != 0 {
				t.Errorf("the unload arrived with %d inference(s) in flight, %d of %d finished and %d call(s) still in line: it belongs after the last of them", u.inflight, u.served, n, u.pending)
			}
			if last := fake.events[len(fake.events)-1]; last != "unload" {
				t.Errorf("the last request the upstream saw was %q, want the unload (events: %v)", last, fake.events)
			}
		})
	}
}

// TestACallThatNeverReachedTheUpstreamUnloadsNothing: a call the fence refused sent no
// request to whisper, so it has nothing to unload — and under a fence the box is a
// render's, so an unload of a model this process never loaded is noise on a busy card.
func TestACallThatNeverReachedTheUpstreamUnloadsNothing(t *testing.T) {
	isolateKeepSet(t)
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(modelaffinity.DisarmGPULease)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "video render", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	fake := newBurstSwap(t, time.Millisecond)
	fake.running = `{"running":[]}` // whisper is not resident, so the fence holds the request
	c := New(fake.srv.URL, 200*time.Millisecond)

	if _, terr := c.Transcribe(context.Background(), "whisper-stt", writeTestWav(t), DefaultParams()); !modelaffinity.IsLeaseRefusal(terr) {
		t.Fatalf("Transcribe = %v, want the fence's lease refusal", terr)
	}
	if err := c.UnloadIfIdle(context.Background(), "whisper-stt"); err != nil {
		t.Fatalf("UnloadIfIdle: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.unloads) != 0 || fake.served != 0 {
		t.Fatalf("%d unload(s) and %d inference(s) reached the upstream for a call the fence refused", len(fake.unloads), fake.served)
	}
}

// TestEveryExitFromATranscribeLeavesTheLine: a call is counted from its first statement,
// so every way out of it — an unreadable wav, a refused request, an upstream error — must
// count it out again, or one failure would hold every later unload back for good.
func TestEveryExitFromATranscribeLeavesTheLine(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model loading", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	c := New(failing.URL, 5*time.Second)
	wav := writeTestWav(t)
	missing := filepath.Join(t.TempDir(), "absent.wav")

	for name, call := range map[string]func() error{
		"whisper, unreadable wav": func() error { _, e := c.Transcribe(context.Background(), "m", missing, DefaultParams()); return e },
		"whisper, upstream error": func() error { _, e := c.Transcribe(context.Background(), "m", wav, DefaultParams()); return e },
		"openai, unreadable wav":  func() error { _, e := c.TranscribeOAI(context.Background(), "m", missing); return e },
		"openai, upstream error":  func() error { _, e := c.TranscribeOAI(context.Background(), "m", wav); return e },
		"whisper, cancelled context": func() error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, e := c.Transcribe(ctx, "m", wav, DefaultParams())
			return e
		},
	} {
		if err := call(); err == nil {
			t.Fatalf("%s: want an error", name)
		}
		if n := Pending(); n != 0 {
			t.Fatalf("%s: %d call(s) still counted in line after it returned", name, n)
		}
	}
}

// TestAnUnloadIsSentOncePerWarmUp: the unload is the last word on a model that was used,
// not an act each finishing call repeats. Three UnloadIfIdle calls after one transcription
// send one unload; a transcription after it warms the model again and earns the next.
func TestAnUnloadIsSentOncePerWarmUp(t *testing.T) {
	isolateKeepSet(t)
	fake := newBurstSwap(t, time.Millisecond)
	c := New(fake.srv.URL, 10*time.Second)
	wav := writeTestWav(t)
	unloads := func() int { fake.mu.Lock(); defer fake.mu.Unlock(); return len(fake.unloads) }

	if _, err := c.Transcribe(context.Background(), "whisper-stt", wav, DefaultParams()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := c.UnloadIfIdle(context.Background(), "whisper-stt"); err != nil {
			t.Fatalf("UnloadIfIdle %d: %v", i, err)
		}
	}
	if got := unloads(); got != 1 {
		t.Fatalf("%d unload(s) after one transcription and three UnloadIfIdle calls, want 1", got)
	}
	if _, err := c.Transcribe(context.Background(), "whisper-stt", wav, DefaultParams()); err != nil {
		t.Fatal(err)
	}
	if err := c.UnloadIfIdle(context.Background(), "whisper-stt"); err != nil {
		t.Fatal(err)
	}
	if got := unloads(); got != 2 {
		t.Fatalf("%d unload(s) after a second warm-up, want 2: the model was used again", got)
	}
}

// TestAnUnloadHoldsTheSlotAgainstTheNextCall: the unload is sent holding inferMu, so a
// call that arrives while it is going out waits for it instead of putting an inference on
// the upstream the unload is about to take down.
func TestAnUnloadHoldsTheSlotAgainstTheNextCall(t *testing.T) {
	isolateKeepSet(t)
	fake := newBurstSwap(t, time.Millisecond)
	fake.unloadHold = make(chan struct{})
	fake.unloadSeen = make(chan struct{})
	c := New(fake.srv.URL, 10*time.Second)
	wav := writeTestWav(t)
	var released sync.Once
	release := func() { released.Do(func() { close(fake.unloadHold) }) }
	defer release()

	if _, err := c.Transcribe(context.Background(), "whisper-stt", wav, DefaultParams()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = c.UnloadIfIdle(context.Background(), "whisper-stt") }()
	select {
	case <-fake.unloadSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("the unload never reached the upstream")
	}
	go func() {
		defer wg.Done()
		_, _ = c.Transcribe(context.Background(), "whisper-stt", wav, DefaultParams())
	}()
	waitPending(t, 1)
	time.Sleep(100 * time.Millisecond)
	if got := fake.inferences(); got != 1 {
		t.Errorf("%d inference(s) have reached the upstream while an unload is in flight, want only the first: the unload does not hold the slot", got)
	}
	release()
	wg.Wait()
	if got := fake.inferences(); got != 2 {
		t.Errorf("%d inference(s) in all, want the second call's to follow the unload", got)
	}
}

// TestAFinishedCallIsNotHeldBackByTheNextCallsInference: a call that finishes while
// another is queued behind it must be handed its own answer at once. The second call's
// inference is held open here; the first call's UnloadIfIdle has to come back anyway,
// because the second call is in line and is the one that unloads when it is done. A
// UnloadIfIdle that waited for the slot would sit through the whole inference (a
// 30-minute transcription would delay a ten-second one's answer by 30 minutes).
func TestAFinishedCallIsNotHeldBackByTheNextCallsInference(t *testing.T) {
	isolateKeepSet(t)
	fake := newBurstSwap(t, time.Millisecond)
	fake.block = make(chan struct{})
	fake.firstGate = func() { waitPending(t, 2) }
	var released sync.Once
	release := func() { released.Do(func() { close(fake.block) }) }
	defer release()
	c := New(fake.srv.URL, 10*time.Second)
	wav := writeTestWav(t)

	// Each call goes to UnloadIfIdle only once the gate opens, and the gate opens when the
	// second inference is on the upstream, so the first call to finish reaches UnloadIfIdle
	// with the second one holding the slot — whichever call that turns out to be.
	gate := make(chan struct{})
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, _ = c.Transcribe(context.Background(), "whisper-stt", wav, DefaultParams())
			<-gate
			_ = c.UnloadIfIdle(context.Background(), "whisper-stt")
			done <- struct{}{}
		}()
	}
	for deadline := time.Now().Add(5 * time.Second); fake.inferences() < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the second inference never reached the upstream")
		}
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the first call was still waiting after 3s with the second call's inference open: UnloadIfIdle holds a finished call back")
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the second call never finished")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.unloads) != 1 || fake.unloads[0].served != 2 {
		t.Errorf("unloads = %+v, want one, after both inferences", fake.unloads)
	}
}

func (s *burstSwap) setRunning(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = body
}

func (s *burstSwap) unloadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.unloads)
}

// TestTheLastCallOutUnloadsEvenWhenTheFenceRefusedIt: a call is in line from its first
// statement, so while one waits for a fenced card the call that finished before it leaves
// the model warm for it. When that call is refused in the end it is the last one out, and
// it frees the model the earlier call used: the pipeline calls UnloadIfIdle after every
// call, whatever its outcome, and that is what makes "the last call out" a promise.
func TestTheLastCallOutUnloadsEvenWhenTheFenceRefusedIt(t *testing.T) {
	isolateKeepSet(t)
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(modelaffinity.DisarmGPULease)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "video render", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	fake := newBurstSwap(t, time.Millisecond) // the model is resident, so call A passes the fence
	c := New(fake.srv.URL, 600*time.Millisecond)
	wav := writeTestWav(t)
	ctx := context.Background()

	if _, err := c.Transcribe(ctx, "whisper-stt", wav, DefaultParams()); err != nil {
		t.Fatalf("call A: %v", err)
	}
	fake.setRunning(`{"running":[]}`) // no longer resident: call B waits for the card
	bErr := make(chan error, 1)
	go func() {
		_, err := c.Transcribe(ctx, "whisper-stt", wav, DefaultParams())
		bErr <- err
	}()
	waitPending(t, 1)
	if err := c.UnloadIfIdle(ctx, "whisper-stt"); err != nil {
		t.Fatalf("call A's UnloadIfIdle: %v", err)
	}
	if n := fake.unloadCount(); n != 0 {
		t.Fatalf("%d unload(s) sent while call B waits for the card: it is in line and unloads last", n)
	}
	if err := <-bErr; !modelaffinity.IsLeaseRefusal(err) {
		t.Fatalf("call B = %v, want the fence's lease refusal", err)
	}
	if err := c.UnloadIfIdle(ctx, "whisper-stt"); err != nil {
		t.Fatalf("call B's UnloadIfIdle: %v", err)
	}
	if n := fake.unloadCount(); n != 1 {
		t.Fatalf("%d unload(s) after the refused call went out, want 1: the last call out frees what the first warmed", n)
	}
}
