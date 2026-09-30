package pipeline

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A seat that refuses the streamed re-pack is asked again as JSON and remembered as
// JSON-only for half an hour, and on it the re-pack is one silent request under its
// allowance again. That is the shape of the defect the streaming fix (PR-12) was
// written for, indistinguishable on the wire from the pre-fix behaviour unless the
// re-pack says so: the fix did not apply to this seat.
func TestRepackSaysWhenTheSeatRefusedTheStream(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repackStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			if body["stream"] == true {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"stream is not supported together with structured outputs"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"answer\":\"42\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":7}}`))
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)
	for run := 1; run <= 2; run++ { // run 1 meets the refusal; run 2 is the remembered JSON-only seat
		wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, testContract())))
		if wire.Deferred || !strings.Contains(string(wire.Structured), `"answer":"42"`) {
			t.Fatalf("run %d: deferred=%v %s / %q, want the JSON answer delivered", run, wire.Deferred, wire.DeferClass, wire.Reason)
		}
		if !strings.Contains(wire.RepackNote, "streaming refused by this seat") {
			t.Fatalf("run %d: repack_note = %q, want the refusal named on the wire", run, wire.RepackNote)
		}
	}
}

// A seat that streams says nothing: the note exists for the exception.
func TestRepackSaysNothingAboutStreamingOnASeatThatStreams(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred || wire.RepackNote != "" {
		t.Fatalf("deferred=%v repack_note=%q, want a clean success with no note", wire.Deferred, wire.RepackNote)
	}
}

// The note rides a FAILED re-pack too, where it explains the most: a seat that
// refused the stream and then answered the wrong shape everywhere reads as a model
// that cannot follow the schema, and the operator is owed the other half.
func TestRepackFailureNamesTheStreamRefusalToo(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repackStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			if body["stream"] == true {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"stream is not supported together with structured outputs"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"wrong\":\"shape\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":7}}`))
		},
		chatFallback: func(int64) string { return doneChat(`{"wrong":"shape"}`) },
	}
	srv := fake.server(t)
	defer srv.Close()
	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || !strings.HasPrefix(wire.Reason, "output failed schema") {
		t.Fatalf("deferred=%v %s / %q, want the wrong-shape defer", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if !strings.Contains(wire.RepackNote, "streaming refused by this seat") {
		t.Fatalf("repack_note = %q, want the streaming refusal beside the failure", wire.RepackNote)
	}
}
