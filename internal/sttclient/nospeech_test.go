package sttclient

// nospeech_test.go pins the honest half of register C-91: ErrUpstreamNoSpeech is the verdict
// "the audio had no speech". It is an upstream that ANSWERED with an empty transcript and,
// on the whisper protocol only, the F-35 crash (an empty-body 5xx) in a call this process can
// vouch for: it ran alone, with no other transcription in line and no unload sent or going
// out (TestAnEmptyBody5xxOfACallThatRanAloneIsTheNoSpeechCrash). It is never guessed from a
// failure to get an answer that something else may have caused. A call whose model was
// unloaded from under it got an empty-body 5xx or a body that was cut off mid-read, was filed
// as no speech, and reached its caller as "empty transcript (no speech detected)" for audio
// that has speech; such a call is ErrUpstreamVanished, a failure a caller can retry.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// cutOff answers with the given status and a body length it never delivers, then drops the
// connection: the read of the answer is aborted, not empty.
func cutOff(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(status)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
}

// gate is a one-shot latch: the stand-in opens it when something has happened, or waits on it
// to hold something back until the test lets it go. Opening twice is harmless, so a test can
// release what it holds on the way out and a failing test never hangs the server's Close.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

func (g *gate) wait() { <-g.ch }

// waitFor blocks until the gate is open, or fails the test after five seconds.
func (g *gate) waitFor(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen within 5s", what)
	}
}

// crashSwap is a llama-swap stand-in whose upstream answers every inference with a bare 5xx
// and no body, which is what llama-swap says when its connection to the model it proxies
// drops. The test decides what else is going on while the call fails: it can hold the first
// inference open, and it can hold an unload's answer.
type crashSwap struct {
	srv    *httptest.Server
	status int
	// holdFirst, when set, keeps the first inference from answering until it is opened;
	// onFirst opens when that inference has arrived.
	holdFirst *gate
	onFirst   *gate
	// holdUnload, when set, keeps every unload from answering until it is opened; onUnload
	// opens when the first unload has arrived.
	holdUnload *gate
	onUnload   *gate

	mu         sync.Mutex
	inferences int
	unloads    int
}

func newCrashSwap(t *testing.T) *crashSwap {
	t.Helper()
	f := &crashSwap{status: http.StatusBadGateway, onFirst: newGate(), onUnload: newGate()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"whisper-stt"}]}`))
		case r.URL.Path == "/running":
			_, _ = w.Write([]byte(`{"running":[{"model":"whisper-stt","state":"ready","ttl":300}]}`))
		case strings.HasPrefix(r.URL.Path, "/api/models/unload/"):
			f.mu.Lock()
			f.unloads++
			f.mu.Unlock()
			f.onUnload.open()
			if f.holdUnload != nil {
				f.holdUnload.wait()
			}
		case strings.HasPrefix(r.URL.Path, "/upstream/"):
			f.mu.Lock()
			f.inferences++
			first := f.inferences == 1
			f.mu.Unlock()
			if first {
				f.onFirst.open()
				if f.holdFirst != nil {
					f.holdFirst.wait()
				}
			}
			w.WriteHeader(f.status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// wantVanished fails the test unless err says the upstream vanished and says nothing about
// the audio.
func wantVanished(t *testing.T, who string, err error) {
	t.Helper()
	if !errors.Is(err, ErrUpstreamVanished) {
		t.Errorf("%s: err = %v, want ErrUpstreamVanished: something else may have taken the model away, so the call failed", who, err)
	}
	if errors.Is(err, ErrUpstreamNoSpeech) {
		t.Errorf("%s: err = %v reads as no speech although the call was not alone", who, err)
	}
}

// TestAnAnswerThatNeverArrivedIsNotReportedAsNoSpeech: every shape of "no answer" that is not
// the bare 5xx is an error a caller can retry, on both protocols, and none is the no-speech
// verdict. An answer cut off mid-read is a vanished upstream whatever its status.
func TestAnAnswerThatNeverArrivedIsNotReportedAsNoSpeech(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		// wantWhisper is what the whisper protocol's error must say, when it can say it: a
		// read that was cut off is reported as that, not as an empty answer.
		wantWhisper string
		// vanished is whether the call must come back as ErrUpstreamVanished on each protocol.
		vanishedWhisper, vanishedOAI bool
	}{
		{"a 5xx whose body was cut off mid-read", cutOff(http.StatusInternalServerError), "cut off", true, true},
		{"a 200 whose body was cut off mid-read", cutOff(http.StatusOK), "", true, true},
		{"llama-swap's model-unloaded envelope", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"unspecific error: matrix: model unloaded","src":"llama-swap"}`, http.StatusInternalServerError)
		}, "model unloaded", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClientState(t)
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c := New(srv.URL, 10*time.Second)
			wav := writeTestWav(t)

			_, werr := c.Transcribe(context.Background(), "whisper-stt", wav, DefaultParams())
			if werr == nil || errors.Is(werr, ErrUpstreamNoSpeech) {
				t.Errorf("whisper protocol: err = %v, want a failure that is not the no-speech verdict", werr)
			} else if !strings.Contains(werr.Error(), tc.wantWhisper) {
				t.Errorf("whisper protocol: err = %v, want it to say %q", werr, tc.wantWhisper)
			}
			if got := errors.Is(werr, ErrUpstreamVanished); got != tc.vanishedWhisper {
				t.Errorf("whisper protocol: errors.Is(err, ErrUpstreamVanished) = %v, want %v (err = %v)", got, tc.vanishedWhisper, werr)
			}
			_, oerr := c.TranscribeOAI(context.Background(), "whisper-stt", wav)
			if oerr == nil || errors.Is(oerr, ErrUpstreamNoSpeech) {
				t.Errorf("openai protocol: err = %v, want a failure that is not the no-speech verdict", oerr)
			}
			if got := errors.Is(oerr, ErrUpstreamVanished); got != tc.vanishedOAI {
				t.Errorf("openai protocol: errors.Is(err, ErrUpstreamVanished) = %v, want %v (err = %v)", got, tc.vanishedOAI, oerr)
			}
		})
	}
}

// TestAnUpstreamThatAnsweredWithAnEmptyTranscriptIsNoSpeech: the one producer of the
// verdict that needs no knowledge of the process. The call went through and the upstream
// heard nothing; there is nothing to retry, and the caller is told so calmly instead of being
// handed an empty result.
func TestAnUpstreamThatAnsweredWithAnEmptyTranscriptIsNoSpeech(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		oai        bool
	}{
		{"whisper, empty text and no segments", `{"language":"en","duration":1.5,"text":"","segments":[]}`, false},
		{"whisper, whitespace only", `{"text":" \n","segments":[]}`, false},
		{"openai, empty text", `{"type":"transcript.text.done","text":""}`, true},
		{"openai, a language span with nothing after it", `{"text":"language None<asr_text>"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetClientState(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer srv.Close()
			c := New(srv.URL, 10*time.Second)
			wav := writeTestWav(t)

			var err error
			if tc.oai {
				_, err = c.TranscribeOAI(context.Background(), "whisper-stt", wav)
			} else {
				_, err = c.Transcribe(context.Background(), "whisper-stt", wav, DefaultParams())
			}
			if !errors.Is(err, ErrUpstreamNoSpeech) {
				t.Fatalf("err = %v, want ErrUpstreamNoSpeech: the upstream answered with an empty transcript", err)
			}
		})
	}
}

// TestAnEmptyBody5xxWhileAnotherTranscriptionWasInLineIsAVanishedUpstream: call 0 is on the
// upstream and call 1 is in line behind it when the upstream answers both with a bare 5xx.
// Neither ran alone. Call 0 had company from the moment call 1 joined; call 1 had it from the
// moment it joined, even though call 0 is gone by the time its own answer comes. Either may
// have been hurt by the other, so neither is read as no speech.
func TestAnEmptyBody5xxWhileAnotherTranscriptionWasInLineIsAVanishedUpstream(t *testing.T) {
	resetClientState(t)
	isolateKeepSet(t)
	fake := newCrashSwap(t)
	fake.holdFirst = newGate()
	defer fake.holdFirst.open()
	c := New(fake.srv.URL, 10*time.Second)
	wav := writeTestWav(t)
	ctx := context.Background()

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, errs[0] = c.Transcribe(ctx, "whisper-stt", wav, DefaultParams()) }()
	fake.onFirst.waitFor(t, "call 0 reaching the upstream")
	go func() { defer wg.Done(); _, errs[1] = c.Transcribe(ctx, "whisper-stt", wav, DefaultParams()) }()
	waitPending(t, 2)
	fake.holdFirst.open()
	wg.Wait()

	wantVanished(t, "the call that was on the upstream when another joined", errs[0])
	wantVanished(t, "the call that joined behind it", errs[1])
}

// TestAnEmptyBody5xxWhileAnUnloadWasSentIsAVanishedUpstream: the call runs alone, but an
// unload is sent while it is on the upstream. A bare Client.Unload holds nothing (UnloadIfIdle
// would have waited for the call), and what it does to a model in use is exactly what the
// bare 5xx looks like.
func TestAnEmptyBody5xxWhileAnUnloadWasSentIsAVanishedUpstream(t *testing.T) {
	resetClientState(t)
	isolateKeepSet(t)
	fake := newCrashSwap(t)
	fake.holdFirst = newGate()
	defer fake.holdFirst.open()
	c := New(fake.srv.URL, 10*time.Second)
	wav := writeTestWav(t)
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		_, err := c.Transcribe(ctx, "whisper-stt", wav, DefaultParams())
		done <- err
	}()
	fake.onFirst.waitFor(t, "the call reaching the upstream")
	if err := c.Unload(ctx, "whisper-stt"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	fake.holdFirst.open()

	wantVanished(t, "the call an unload was sent during", <-done)
}

// TestAnEmptyBody5xxWhileAnUnloadWasGoingOutIsAVanishedUpstream: an unload that began BEFORE
// the call is still taking the model down when the call joins and goes to the upstream.
// Nothing is sent during the call, so the count of unloads, which the call read after that
// unload had begun, cannot see it: the call has to notice an unload that is going out when
// it joins.
func TestAnEmptyBody5xxWhileAnUnloadWasGoingOutIsAVanishedUpstream(t *testing.T) {
	resetClientState(t)
	isolateKeepSet(t)
	fake := newCrashSwap(t)
	fake.holdUnload = newGate()
	defer fake.holdUnload.open()
	c := New(fake.srv.URL, 10*time.Second)
	wav := writeTestWav(t)
	ctx := context.Background()

	unloaded := make(chan error, 1)
	go func() { unloaded <- c.Unload(ctx, "whisper-stt") }()
	fake.onUnload.waitFor(t, "the unload reaching the upstream")

	_, err := c.Transcribe(ctx, "whisper-stt", wav, DefaultParams())
	wantVanished(t, "the call that joined while an unload was going out", err)

	fake.holdUnload.open()
	if err := <-unloaded; err != nil {
		t.Fatalf("Unload: %v", err)
	}
}

// TestTheOpenAIPathNeverReadsNoSpeechOffAFailure: the crash on audio without speech is
// whisper.cpp's. Nothing says llama-server's mtmd path exits on it, so on the OpenAI protocol
// the same bare 5xx is a vanished upstream even for a call that ran alone.
func TestTheOpenAIPathNeverReadsNoSpeechOffAFailure(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			resetClientState(t)
			fake := newCrashSwap(t)
			fake.status = status
			c := New(fake.srv.URL, 10*time.Second)
			_, err := c.TranscribeOAI(context.Background(), "whisper-stt", writeTestWav(t))
			wantVanished(t, "an empty-body "+strconv.Itoa(status)+" on the openai protocol", err)
		})
	}
}
