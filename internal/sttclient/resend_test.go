package sttclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// On 2026-11-25 a GPU lease was granted while four transcriptions were running. Clearing the cards unloaded whisper
// under them, llama-swap answered each "matrix: model unloaded" with a 500, and all four were filed as failed, although
// the audio was on disk and the same request would have loaded the model back. A call the engine was already working on
// gets a bare 502 instead, and one full of speech was filed as silent.
func TestATranscriptionWhoseModelWentAwayUnderItIsSentAgain(t *testing.T) {
	for name, lose := range map[string]func(http.ResponseWriter){
		"llama-swap still held the call":     func(w http.ResponseWriter) { http.Error(w, "matrix: model unloaded", http.StatusInternalServerError) },
		"the engine was working on the call": func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
	} {
		t.Run(name, func(t *testing.T) {
			resetClientState(t)
			var sent atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/upstream/whisper/") {
					http.NotFound(w, r)
					return
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("request %d carries no form: %v", sent.Load()+1, err)
				}
				if sent.Add(1) == 1 {
					lose(w)
					return
				}
				_, _ = w.Write([]byte(`{"text":"hello","segments":[{"id":0,"start":0,"end":1,"text":"hello"}]}`))
			}))
			defer srv.Close()

			res, err := New(srv.URL, time.Minute).Transcribe(context.Background(), "whisper", fenceTestWav(t), DefaultParams())
			if err != nil {
				t.Fatalf("Transcribe = %v, want the transcript of the second request", err)
			}
			if res.Text != "hello" || sent.Load() != 2 {
				t.Fatalf("text %q after %d request(s), want \"hello\" after 2", res.Text, sent.Load())
			}
		})
	}
}
