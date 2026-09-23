package seatload

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOccupantsListsEveryRunningModelWithoutAskingAnySeat: Occupants is the
// whole of /running — every model and its state — read the way Running and
// Inflight read it, and nothing else. It is what the cascade seat guard reads
// to learn which vLLM seat holds the cards, so it must never touch a seat's
// own address (that is Inflight's second half) nor any /upstream path (which
// loads a model on demand and resets its idle timer).
func TestOccupantsListsEveryRunningModelWithoutAskingAnySeat(t *testing.T) {
	f := &fakeSwap{id: "qwen3.8-27b-vllm-3card", alias: "agent-pool", roster: true}
	f.loaded.Store(true)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	rows, err := Occupants(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Occupants: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "qwen3.8-27b-vllm-3card" || rows[0].State != "ready" {
		t.Fatalf("rows = %+v, want the one ready seat by its canonical id", rows)
	}
	if !strings.Contains(rows[0].Proxy, "/direct/qwen3.8-27b-vllm-3card") {
		t.Fatalf("Proxy = %q, want the seat's own address as /running reports it", rows[0].Proxy)
	}
	f.starting.Store(true)
	if rows, err = Occupants(context.Background(), srv.Client(), srv.URL); err != nil || len(rows) != 1 || rows[0].State != "starting" {
		t.Fatalf("starting seat: rows = %+v err = %v", rows, err)
	}
	if n := f.metricsHits.Load() + f.upstreamHits.Load(); n != 0 {
		t.Fatalf("Occupants issued %d seat or /upstream request(s); it reads /running only", n)
	}
}

// TestOccupantsFailsLoudly: an unreadable /running is an error, never an
// empty list — "nothing is running" and "could not tell" are different facts,
// and the guard fails toward protecting the seat on the second.
func TestOccupantsFailsLoudly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	if rows, err := Occupants(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatalf("a body that is not /running's shape decoded to %+v with no error", rows)
	}
	srv.Close()
	if _, err := Occupants(context.Background(), srv.Client(), srv.URL); err == nil || !strings.Contains(err.Error(), "llama-swap /running") {
		t.Fatalf("a dead endpoint: err = %v, want a llama-swap /running error", err)
	}
}

// TestOccupantsRefusesANon2xxOrShapelessAnswer (review CRITICAL 1): a
// restarting or failing llama-swap can answer /running with a JSON body that
// decodes cleanly into "nothing running" — `{"running":null}` on a 503, or an
// `{"error":…}` object. Read as an empty list, the seat guard then saw no
// loaded seat and let an evicting rung through without a word. Any non-2xx is
// an error, and so is a 2xx body with no `running` field at all; only a 2xx
// that carries the field (null included — Go writes an empty list that way)
// is a reading.
func TestOccupantsRefusesANon2xxOrShapelessAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		ok     bool
	}{
		"503 with running null":  {http.StatusServiceUnavailable, `{"running":null}`, false},
		"503 with an error body": {http.StatusServiceUnavailable, `{"error":"restarting"}`, false},
		"500 with a full list":   {http.StatusInternalServerError, `{"running":[{"model":"m","state":"ready"}]}`, false},
		"200 with an error body": {http.StatusOK, `{"error":"x"}`, false},
		"200 with running null":  {http.StatusOK, `{"running":null}`, true},
		"200 with an empty list": {http.StatusOK, `{"running":[]}`, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			// A test fake of llama-swap's JSON API that must send the exact
			// (including malformed) bytes under test; nothing renders it as HTML.
			_, _ = w.Write([]byte(tc.body)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter
		}))
		rows, err := Occupants(context.Background(), srv.Client(), srv.URL)
		srv.Close()
		if tc.ok && (err != nil || len(rows) != 0) {
			t.Errorf("%s: rows=%+v err=%v, want an empty reading", name, rows, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: decoded to %+v with no error — the guard would read it as nothing loaded", name, rows)
		}
		// The error text is what the guard's warning line prints; a 2xx with
		// no list must say so, not "unexpected end of JSON input".
		if tc.status == http.StatusOK && !tc.ok && (err == nil || !strings.Contains(err.Error(), "carries no running list")) {
			t.Errorf("%s: err = %v, want it to say the answer carries no running list", name, err)
		}
	}
}
