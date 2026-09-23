package tokclient

// fence_test.go pins /tokenize behind the GPU-lease fence (2026-09-22). The
// per-model passthrough /upstream/<seat>/tokenize STARTS a seat that is not
// loaded; an agent run admitted just before a video render asked it on its next
// step and loaded the 3-card seat onto the render's cards.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

func fenceWithMedia(t *testing.T) {
	t.Helper()
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
	t.Cleanup(func() { _ = l.Release() })
}

// swapStub answers the fence's residency view and counts /upstream requests;
// the passthrough answers like a warm seat, so an unfenced call visibly works.
func swapStub(t *testing.T, running string, upstream *atomic.Int64) *httptest.Server {
	t.Helper()
	pieces := piecesHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"agent-pool"}]}`))
		case r.URL.Path == "/running":
			_, _ = w.Write([]byte(running))
		case strings.HasPrefix(r.URL.Path, "/upstream/"):
			upstream.Add(1)
			pieces(w, r)
		default:
			http.NotFound(w, r) // llama-swap has no root /tokenize route
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Under a render over a cold seat the tokenizer fails OPEN at once, never sends
// the loading request, and reports the failure as fenced — not definitive, so
// the loop's sticky wrapper neither downgrades nor strikes.
func TestTokenizeUnderAFenceNeverReachesUpstream(t *testing.T) {
	fenceWithMedia(t)
	var upstream atomic.Int64
	srv := swapStub(t, `{"running":[]}`, &upstream)
	for _, c := range []*Client{New(srv.URL, "agent-pool", 0), NewUpstreamOnly(srv.URL, "agent-pool", 0)} {
		start := time.Now()
		if _, ok := c.Pieces(context.Background(), "hello world"); ok {
			t.Fatal("tokenize answered under the fence over a cold seat")
		}
		if _, ok := c.Count(context.Background(), "hello world"); ok {
			t.Fatal("count answered under the fence over a cold seat")
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Fatalf("the fenced tokenizer waited %s; it must fail open at once (the completion that follows is the one that waits)", el)
		}
		if !c.LastFailFenced() || c.LastFailDefinitive() {
			t.Fatalf("fenced=%v definitive=%v, want fenced and not definitive (last: %s)", c.LastFailFenced(), c.LastFailDefinitive(), c.LastErr())
		}
		if !strings.Contains(c.LastErr(), "gpu-lease") {
			t.Fatalf("the reason must name the lease: %q", c.LastErr())
		}
	}
	if got := upstream.Load(); got != 0 {
		t.Fatalf("%d tokenize request(s) reached /upstream under a media lease — each one loads the seat", got)
	}
}

// A resident seat is tokenized under the fence: the request starts nothing.
func TestTokenizeReadsAResidentSeatUnderAFence(t *testing.T) {
	fenceWithMedia(t)
	var upstream atomic.Int64
	srv := swapStub(t, `{"running":[{"model":"agent-pool","state":"ready"}]}`, &upstream)
	c := New(srv.URL, "agent-pool", 0)
	if n, ok := c.Count(context.Background(), "hello big world"); !ok || n != 3 {
		t.Fatalf("Count = (%d,%v), want (3,true) for a resident seat (last: %s)", n, ok, c.LastErr())
	}
	if c.LastFailFenced() {
		t.Fatal("a success must clear the fenced mark")
	}
}
