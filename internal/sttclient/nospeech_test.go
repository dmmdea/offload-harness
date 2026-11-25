package sttclient

// nospeech_test.go pins the honest half of register C-91: ErrUpstreamNoSpeech is the verdict
// "the audio had no speech", and it is an upstream that ANSWERED with an empty transcript,
// never a guess from a failure to get an answer. A call whose model was unloaded from under
// it got an empty-body 5xx or a body that was cut off mid-read, was filed as no speech, and
// reached its caller as "empty transcript (no speech detected)" for audio that has speech;
// such a call is ErrUpstreamVanished, a failure a caller can retry.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// crashSwap is a llama-swap stand-in whose upstream answers every inference with a bare 5xx
// and no body, which is what llama-swap says when its connection to the model it proxies
// drops.
type crashSwap struct {
	srv    *httptest.Server
	status int
}

func newCrashSwap(t *testing.T) *crashSwap {
	t.Helper()
	f := &crashSwap{status: http.StatusBadGateway}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"whisper-stt"}]}`))
		case r.URL.Path == "/running":
			_, _ = w.Write([]byte(`{"running":[{"model":"whisper-stt","state":"ready","ttl":300}]}`))
		case strings.HasPrefix(r.URL.Path, "/upstream/"):
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
		t.Errorf("%s: err = %v, want ErrUpstreamVanished: the call got no answer, so it failed", who, err)
	}
	if errors.Is(err, ErrUpstreamNoSpeech) {
		t.Errorf("%s: err = %v reads as no speech although nothing was answered", who, err)
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

// TestTheOpenAIPathNeverReadsNoSpeechOffAFailure: the bare 5xx is a vanished upstream on the
// OpenAI protocol too.
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
