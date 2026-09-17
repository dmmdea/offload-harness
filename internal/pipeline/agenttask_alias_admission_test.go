// agenttask_alias_admission_test.go pins the admission pre-flight's SEAT MATCH
// (register S-08 / W-04).
//
// The defect: awaitSeatAdmission matched llama-swap's GET /running by the seat's
// BOUND name, while /running names models by their CANONICAL id only. On the
// reference boxes the harness binds seats by alias (`agent-pool` ->
// `qwen3.8-27b-vllm`), so the "my own seat is already ready" fast path could never
// fire, and a READY seat slept the whole admission budget whenever any OTHER model
// on the endpoint happened to be mid-swap. 406 delegation-log rows carry the
// symptom verbatim: "/running lists the seat under another id".
//
// internal/seatload shipped exactly this resolution for the drain (C-11); these
// tests hold the second reader to the same answer.

package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// swapFake is a minimal llama-swap: a roster that maps one alias onto one
// canonical id, and a scripted GET /running. Nothing else is served, because
// nothing else is under test here.
type swapFake struct {
	canonical string
	alias     string
	running   func(n int64) string
	polls     atomic.Int64
	rosters   atomic.Int64
}

func (f *swapFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/running":
			_, _ = w.Write([]byte(f.running(f.polls.Add(1))))
		case "/v1/models":
			f.rosters.Add(1)
			entry := map[string]any{"id": f.canonical}
			if f.alias != "" {
				entry["meta"] = map[string]any{"llamaswap": map[string]any{"aliases": []string{f.alias}}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{entry}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSeatAdmissionMatchesTheSeatUnderItsCanonicalID: the contract's seat is the
// ALIAS, /running lists it ready under the CANONICAL id, and another model is
// mid-swap. The pre-flight's job is to keep another session's swap out of this
// contract's wall — not to wait for a swap the seat it needs is not part of. It
// must admit at once: zero wait, no note.
func TestSeatAdmissionMatchesTheSeatUnderItsCanonicalID(t *testing.T) {
	fake := &swapFake{
		canonical: "seat-canonical",
		alias:     "seat-alias",
		running: func(int64) string {
			return `{"running":[{"model":"seat-canonical","state":"ready","cmd":"y"},` +
				`{"model":"other-heavy","state":"starting","cmd":"x"}]}`
		},
	}
	srv := fake.server(t)

	waited, note := awaitSeatAdmission(context.Background(), srv.URL, "seat-alias", 30*time.Second)
	if waited != 0 || note != "" {
		t.Fatalf("waited=%v note=%q, want an immediate admission: the seat's own /running row is ready under its canonical id", waited, note)
	}
	if fake.polls.Load() != 1 {
		t.Fatalf("/running polls = %d, want exactly 1 — a ready seat costs one probe and no sleep", fake.polls.Load())
	}
}

// TestSeatAdmissionStillWaitsWhenItsOwnRowIsAbsent is the control: the alias
// resolution must not turn the pre-flight off. The seat is nowhere in /running
// and another model is starting, so the loop sleeps its budget and says what it
// was waiting for — exactly as before W-04.
func TestSeatAdmissionStillWaitsWhenItsOwnRowIsAbsent(t *testing.T) {
	fake := &swapFake{
		canonical: "seat-canonical",
		alias:     "seat-alias",
		running: func(int64) string {
			return `{"running":[{"model":"other-heavy","state":"starting","cmd":"x"}]}`
		},
	}
	srv := fake.server(t)

	start := time.Now()
	waited, note := awaitSeatAdmission(context.Background(), srv.URL, "seat-alias", 4*time.Second)
	if waited < admissionPoll || time.Since(start) > 3*admissionPoll {
		t.Fatalf("waited=%v (elapsed %v), want the pre-flight to sleep its budget while another model is mid-swap", waited, time.Since(start))
	}
	if note == "" {
		t.Fatalf("a spent budget must say what it was waiting for; got an empty note")
	}
}

// TestSeatAdmissionReadsTheRosterOnlyWhenItWouldOtherwiseWait: the alias
// resolution is a second HTTP call, so it is paid only when the bare name
// matched nothing AND something is mid-swap. An endpoint with nothing swapping
// admits on the first probe alone.
func TestSeatAdmissionReadsTheRosterOnlyWhenItWouldOtherwiseWait(t *testing.T) {
	fake := &swapFake{
		canonical: "seat-canonical",
		alias:     "seat-alias",
		running:   func(int64) string { return `{"running":[]}` },
	}
	srv := fake.server(t)

	waited, note := awaitSeatAdmission(context.Background(), srv.URL, "seat-alias", 30*time.Second)
	if waited != 0 || note != "" {
		t.Fatalf("waited=%v note=%q, want an immediate admission on an idle endpoint", waited, note)
	}
	if fake.rosters.Load() != 0 {
		t.Fatalf("roster reads = %d, want 0: a seat bound by its id must not pay a second round trip", fake.rosters.Load())
	}
}
