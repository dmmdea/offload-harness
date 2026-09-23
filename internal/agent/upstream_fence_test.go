package agent

// upstream_fence_test.go pins the agent package's llama-swap probes behind the
// GPU-lease fence (2026-09-22): the served-window probe and the seat-pin probe
// reach llama-swap's /upstream/<seat>/… route, which STARTS a model that is not
// loaded. An agent run admitted just before a video render kept sending them,
// and the 3-card seat was loaded onto the render's cards mid-video.

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

// fenceWithMedia arms the process-wide gate at a private state root and holds a
// media lease there — a render owning the card — for the test's length.
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

// fencedSwap is a llama-swap stand-in: /v1/models and /running answer (the
// fence's non-loading residency view), every /upstream/… request is COUNTED —
// on a real llama-swap each one would start the seat — and served as a warm
// seat would serve it, so an unfenced probe visibly succeeds.
type fencedSwap struct {
	*httptest.Server
	upstream atomic.Int64
	rootHits atomic.Int64
}

func newFencedSwap(t *testing.T, seat, running string) *fencedSwap {
	t.Helper()
	f := &fencedSwap{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + seat + `"}]}`))
		case r.URL.Path == "/running":
			_, _ = w.Write([]byte(running))
		case strings.HasPrefix(r.URL.Path, "/upstream/"):
			f.upstream.Add(1)
			if strings.HasSuffix(r.URL.Path, "/props") {
				_, _ = w.Write([]byte(propsJSON(65536)))
				return
			}
			http.NotFound(w, r)
		default:
			f.rootHits.Add(1)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// The window probe under a render over a cold seat: no /upstream request at
// all, no bare-root fallback either (it answers for whatever is loaded), and a
// typed lease refusal the admission path can defer on.
func TestWindowProbeUnderAFenceNeverReachesUpstream(t *testing.T) {
	fenceWithMedia(t)
	f := newFencedSwap(t, "agent-pool", `{"running":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	n, ok, err := ProbeServedWindowChecked(ctx, f.URL, "agent-pool")
	if ok || n != 0 {
		t.Fatalf("probe = (%d,%v), want no answer under the fence", n, ok)
	}
	if !modelaffinity.IsLeaseRefusal(err) {
		t.Fatalf("err = %v, want the fence's *LeaseError", err)
	}
	if got := f.upstream.Load(); got != 0 {
		t.Fatalf("%d window-probe request(s) reached /upstream under a media lease — each one loads the seat", got)
	}
	if got := f.rootHits.Load(); got != 0 {
		t.Fatalf("the fenced probe fell back to the bare root %d time(s)", got)
	}
	// The unchecked form keeps its (int, bool) contract and the same silence.
	if _, ok := ProbeServedWindow(ctx, f.URL, "agent-pool"); ok || f.upstream.Load() != 0 {
		t.Fatalf("ProbeServedWindow answered (%v) or reached /upstream (%d) under the fence", ok, f.upstream.Load())
	}
}

// A seat llama-swap lists as ready is read under the fence: the request cannot
// start anything, and the run keeps its real window.
func TestWindowProbeReadsAResidentSeatUnderAFence(t *testing.T) {
	fenceWithMedia(t)
	f := newFencedSwap(t, "agent-pool", `{"running":[{"model":"agent-pool","state":"ready"}]}`)
	n, ok, err := ProbeServedWindowChecked(context.Background(), f.URL, "agent-pool")
	if err != nil || !ok || n != 65536 {
		t.Fatalf("probe = (%d,%v,%v), want (65536,true,nil) for a resident seat", n, ok, err)
	}
}

// The post-run seat pin never waits for a card and never reaches /upstream
// under a fence: a 3 s client that sends the request starts a load it then
// abandons (llama-swap logs the start as "failed: aborted").
func TestSeatPinProbeUnderAFenceNeverReachesUpstream(t *testing.T) {
	fenceWithMedia(t)
	f := newFencedSwap(t, "agent-pool", `{"running":[]}`)
	start := time.Now()
	if _, ok := ProbeSeatPin(context.Background(), f.URL, "agent-pool"); ok {
		t.Fatal("a pin was produced under the fence")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("the pin probe waited %s under the fence; it must refuse at once", el)
	}
	if got := f.upstream.Load(); got != 0 {
		t.Fatalf("%d seat-pin request(s) reached /upstream under a media lease", got)
	}
}

// fencedTok is a Tokenizer whose every call is refused by the lease fence.
type fencedTok struct{ calls atomic.Int64 }

func (f *fencedTok) Pieces(context.Context, string) ([]int, bool) { f.calls.Add(1); return nil, false }
func (f *fencedTok) LastFailFenced() bool                         { return true }
func (f *fencedTok) LastErr() string                              { return "gpu-lease timeout" }

// A fenced tokenizer call says nothing about the endpoint: a render held the
// card. Counting it as a strike would downgrade the run to the estimate rung for
// good after two steps under one render.
func TestStickyTokenizerDoesNotCountAFencedFailure(t *testing.T) {
	inner := &fencedTok{}
	s := &stickyTokenizer{inner: inner}
	for i := 0; i < 5; i++ {
		if _, ok := s.Pieces(context.Background(), "x"); ok {
			t.Fatal("a fenced call cannot succeed")
		}
	}
	if why, down := s.degraded(); down {
		t.Fatalf("five fenced calls downgraded the tokenizer: %s", why)
	}
	if inner.calls.Load() != 5 {
		t.Fatalf("the wrapper stopped asking after %d calls; the card frees and the tokenizer must be asked again", inner.calls.Load())
	}
}
