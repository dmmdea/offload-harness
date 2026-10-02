package sttclient

// nospeech_test.go pins the honest half of register C-91: ErrUpstreamNoSpeech is the
// verdict "the upstream ANSWERED, and the answer was an empty transcript". It is never a
// guess drawn from a failure to get an answer. A call whose model was unloaded from under
// it got an empty-body 5xx or a body that was cut off mid-read, was filed as no speech, and
// reached its caller as "empty transcript (no speech detected)" for audio that has speech.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// TestAnAnswerThatNeverArrivedIsNotReportedAsNoSpeech: every shape of "no answer" is an
// error a caller can retry, on both protocols, and none is the no-speech verdict.
func TestAnAnswerThatNeverArrivedIsNotReportedAsNoSpeech(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		// wantWhisper is what the whisper protocol's error must say, when it can say it: a
		// read that was cut off is reported as that, not as an empty answer.
		wantWhisper string
	}{
		{"a 5xx whose body was cut off mid-read", cutOff(http.StatusInternalServerError), "cut off"},
		{"a 200 whose body was cut off mid-read", cutOff(http.StatusOK), ""},
		{"llama-swap's model-unloaded envelope", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"unspecific error: matrix: model unloaded","src":"llama-swap"}`, http.StatusInternalServerError)
		}, "model unloaded"},
		{"a bare 502 with no body", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }, "vanished"},
		{"a bare 500 with no body", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, "vanished"},
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
			_, oerr := c.TranscribeOAI(context.Background(), "whisper-stt", wav)
			if oerr == nil || errors.Is(oerr, ErrUpstreamNoSpeech) {
				t.Errorf("openai protocol: err = %v, want a failure that is not the no-speech verdict", oerr)
			}
		})
	}
}

// TestAnUpstreamThatAnsweredWithAnEmptyTranscriptIsNoSpeech: the one producer of the
// verdict. The call went through and the upstream heard nothing; there is nothing to
// retry, and the caller is told so calmly instead of being handed an empty result.
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
