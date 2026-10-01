package llamaclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A stream that dies mid-body carries what it had delivered (register C-80). The
// structured re-pack counts the tokens of an attempt that failed and keeps a clip
// of what it wrote; before this an attempt killed by the stall watch, the ceiling
// or a dropped connection reported nothing at all, which is exactly the attempt
// whose cost and shape an operator needs (119 of 119 stalled re-packs).
func TestMidStreamDeathCarriesWhatTheStreamHadDelivered(t *testing.T) {
	cases := []struct {
		name       string
		serve      func(w http.ResponseWriter, r *http.Request)
		cancelAt   int // cancel the call's context once this many deltas were heard (0 = never)
		wantText   string
		wantTokens int
	}{
		{"ended without a finish", func(w http.ResponseWriter, r *http.Request) { sseWrite(w, answerFrames[1:3]...) }, 0, `{"answer":"42"}`, 2},
		{"engine error frame", func(w http.ResponseWriter, r *http.Request) {
			sseWrite(w, answerFrames[1], `{"error":{"message":"EngineCore died","type":"server_error"}}`)
		}, 0, `{"answer":`, 1},
		{"a usage frame's exact count wins over the delta count", func(w http.ResponseWriter, r *http.Request) {
			sseWrite(w, answerFrames[1], answerFrames[2], `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":9}}`)
		}, 0, `{"answer":"42"}`, 9},
		{"the caller's context ended mid-stream", func(w http.ResponseWriter, r *http.Request) {
			sseWrite(w, answerFrames[1], answerFrames[2])
			<-r.Context().Done() // then silence until the client gives up
		}, 2, `{"answer":"42"}`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tc.serve))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			progress := func(n int) {
				if tc.cancelAt > 0 && n >= tc.cancelAt {
					cancel()
				}
			}
			_, err := New(srv.URL, "", "m", 5*time.Second).Generate(ctx, "", "", "u", "", 64, 0, 0, WithProgress(progress))
			var be *BodyError
			if !errors.As(err, &be) {
				t.Fatalf("err = %v (%T), want a *BodyError", err, err)
			}
			if be.Partial.Content != tc.wantText || be.Partial.TokensOut != tc.wantTokens {
				t.Fatalf("partial = %q / %d tokens, want %q / %d: what the stream had delivered when it died", be.Partial.Content, be.Partial.TokensOut, tc.wantText, tc.wantTokens)
			}
		})
	}
}
