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
	"strings"
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
	// rosterFail, when set, decides per roster read (1-based) whether GET
	// /v1/models answers 500 instead of the roster — a contended box timing out
	// or erroring on the one read the alias match depends on.
	rosterFail func(n int64) bool
	polls      atomic.Int64
	rosters    atomic.Int64
}

func (f *swapFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/running":
			_, _ = w.Write([]byte(f.running(f.polls.Add(1))))
		case "/v1/models":
			if n := f.rosters.Add(1); f.rosterFail != nil && f.rosterFail(n) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
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

// TestALoadedSeatBoundByAliasIsNotReadAsAbsent is the 2026-11-21 incident's own
// shape: the seat is bound by its ALIAS, /running lists it READY under the
// CANONICAL id, and nothing on the endpoint is mid-swap. The pre-flight admitted
// at once but reported the seat not ready, because it resolved the alias only
// when another model was mid-swap; the cold-load host-RAM gate then judged a
// seat that was already loaded, against a commit charge that already held it,
// and deferred the contract in milliseconds with "the seat was not loaded".
// A loaded seat reads ready and never absent, whatever name it is bound by.
func TestALoadedSeatBoundByAliasIsNotReadAsAbsent(t *testing.T) {
	fake := &swapFake{
		canonical: "seat-canonical",
		alias:     "seat-alias",
		running: func(int64) string {
			return `{"running":[{"model":"seat-canonical","state":"ready","cmd":"y"},` +
				`{"model":"embedder","state":"ready","cmd":"e"}]}`
		},
	}
	srv := fake.server(t)

	waited, note, loadSeen, ready, absent := AwaitSeatAdmissionState(context.Background(), srv.URL, "seat-alias", 30*time.Second)
	if !ready || absent {
		t.Fatalf("ready=%v absent=%v, want ready and not absent: the seat's own /running row is ready under its canonical id, and a door that reads it absent puts a loaded seat to the cold-load gate", ready, absent)
	}
	if waited != 0 || note != "" || !loadSeen.IsZero() {
		t.Fatalf("waited=%v note=%q loadSeen=%v, want an immediate admission with nothing to report", waited, note, loadSeen)
	}

	// The control: the same endpoint without the seat's row. Only then is the seat absent, and only an absent seat is a
	// cold load a contract would start.
	fake.running = func(int64) string { return `{"running":[{"model":"embedder","state":"ready","cmd":"e"}]}` }
	if _, _, _, ready, absent = AwaitSeatAdmissionState(context.Background(), srv.URL, "seat-alias", 30*time.Second); ready || !absent {
		t.Fatalf("ready=%v absent=%v, want not ready and absent: no row is this seat's under either name", ready, absent)
	}
}

// TestSeatAdmissionReadsTheRosterOnlyWhenItWouldOtherwiseWait: the alias
// resolution is a second HTTP call, so it is paid only when the bare name
// matched nothing AND /running lists a row that could be the seat under its
// canonical id. An endpoint that lists nothing admits on the first probe alone.
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

// TestSeatAdmissionRetriesTheAliasResolutionAfterATransientRosterError: the
// roster read that resolves the alias is ONE HTTP call, and the moment it
// matters most — a box contended enough that another model is mid-swap — is
// exactly the moment it can time out or 500. A resolution that latched
// "already tried" on the failed attempt disabled the alias match for the rest
// of the wait, so the seat burned the whole admission budget for a swap it had
// no stake in: S-08's own symptom, back as an intermittent one, and silent.
//
// The roster fails once and then answers. The wait must end at the SECOND poll.
func TestSeatAdmissionRetriesTheAliasResolutionAfterATransientRosterError(t *testing.T) {
	fake := &swapFake{
		canonical:  "seat-canonical",
		alias:      "seat-alias",
		rosterFail: func(n int64) bool { return n == 1 },
		running: func(int64) string {
			return `{"running":[{"model":"seat-canonical","state":"ready","cmd":"y"},` +
				`{"model":"other-heavy","state":"starting","cmd":"x"}]}`
		},
	}
	srv := fake.server(t)

	waited, note := awaitSeatAdmission(context.Background(), srv.URL, "seat-alias", 30*time.Second)
	// One poll interval plus the failed first roster read, which is charged to
	// the wait (a roster that times out must not let the loop outrun its
	// budget) — never a second interval.
	if waited < admissionPoll || waited >= 2*admissionPoll {
		t.Fatalf("waited=%v, want one poll interval (plus the failed roster read): the second roster read resolves the alias and the seat is already ready", waited)
	}
	if !strings.Contains(note, "alias resolution") {
		t.Errorf("note = %q, want the transient roster failure named — a probe that failed silently is the defect this PR exists to remove", note)
	}
	if fake.rosters.Load() < 2 {
		t.Errorf("roster reads = %d, want the failed one RETRIED on the next poll", fake.rosters.Load())
	}
}

// TestSeatAdmissionProceedsWhenTheRosterNeverAnswers is the control: a roster
// that is down for the whole budget cannot resolve anything, so the wait behaves
// exactly as it did before the alias match existed — bounded by the budget, then
// into the wall. What must NOT happen is that it does so silently.
func TestSeatAdmissionProceedsWhenTheRosterNeverAnswers(t *testing.T) {
	fake := &swapFake{
		canonical:  "seat-canonical",
		alias:      "seat-alias",
		rosterFail: func(int64) bool { return true },
		running: func(int64) string {
			return `{"running":[{"model":"seat-canonical","state":"ready","cmd":"y"},` +
				`{"model":"other-heavy","state":"starting","cmd":"x"}]}`
		},
	}
	srv := fake.server(t)

	waited, note := awaitSeatAdmission(context.Background(), srv.URL, "seat-alias", 4*time.Second)
	if waited < admissionPoll {
		t.Fatalf("waited=%v, want the budget spent: nothing could resolve the alias", waited)
	}
	if !strings.Contains(note, "alias resolution") {
		t.Errorf("note = %q, want the unreadable roster named beside the spent budget", note)
	}
	if !strings.Contains(note, "budget spent") {
		t.Errorf("note = %q, want the spent budget still reported", note)
	}
}
