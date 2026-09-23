package modelaffinity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// stubSwap is a llama-swap stand-in that answers the two NON-loading reads the
// fence makes (/v1/models, /running) and counts every request to the
// auto-loading /upstream/… route — the count the fence exists to keep at zero.
type stubSwap struct {
	srv      *httptest.Server
	upstream atomic.Int64
	running  atomic.Value // string: the /running body
}

func newStubSwap(t *testing.T, roster map[string][]string) *stubSwap {
	t.Helper()
	s := &stubSwap{}
	s.running.Store(`{"running":[]}`)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			type meta struct {
				Llamaswap struct {
					Aliases []string `json:"aliases,omitempty"`
				} `json:"llamaswap"`
			}
			type m struct {
				ID   string `json:"id"`
				Meta *meta  `json:"meta,omitempty"`
			}
			var data []m
			for id, al := range roster {
				e := m{ID: id}
				if len(al) > 0 {
					e.Meta = &meta{}
					e.Meta.Llamaswap.Aliases = al
				}
				data = append(data, e)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		case r.URL.Path == "/running":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(s.running.Load().(string)))
		case strings.HasPrefix(r.URL.Path, "/upstream/"):
			s.upstream.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func holdLease(t *testing.T, m *gpulease.Manager, class gpulease.Class, opts gpulease.Options) *gpulease.Lease {
	t.Helper()
	l, err := m.TryAcquire(class, opts)
	if err != nil {
		t.Fatalf("acquire %s: %v", class, err)
	}
	t.Cleanup(func() { _ = l.Release() })
	return l
}

// An armed gate with nobody holding the card returns the URL at once and reads
// nothing: the unfenced box pays one lease ReadFile, never a /running read.
func TestAwaitUpstreamIsFreeWhenNoLeaseHeld(t *testing.T) {
	armLease(t)
	s := newStubSwap(t, map[string][]string{"seat": nil})
	start := time.Now()
	u, err := AwaitUpstream(context.Background(), s.srv.URL+"/v1", "seat", "props", time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("AwaitUpstream = %v, want the URL", err)
	}
	if want := s.srv.URL + "/upstream/seat/props"; u != want {
		t.Fatalf("URL = %q, want %q (the /v1 suffix stripped, the leading slash added)", u, want)
	}
	if el := time.Since(start); el > leasePollInterval/2 {
		t.Fatalf("an unfenced request waited %s", el)
	}
}

// THE DEFECT, at the gate: a media render holds the card and the seat is cold.
// The request must not be released onto the route that would load it; the wait
// ends on the caller's deadline with a typed *LeaseError naming the render.
func TestAwaitUpstreamRefusesAColdSeatUnderAMediaLease(t *testing.T) {
	m := armLease(t)
	s := newStubSwap(t, map[string][]string{"qwen3.8-27b-vllm-3card": {"agent-pool"}})
	holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "video render", Origin: "pipeline"})

	start := time.Now()
	u, err := AwaitUpstream(context.Background(), s.srv.URL, "agent-pool", "/tokenize", time.Now().Add(300*time.Millisecond))
	if err == nil {
		t.Fatalf("AwaitUpstream released %q while a media render held the card over a cold seat", u)
	}
	var le *LeaseError
	if !errors.As(err, &le) || le.Class != gpulease.ClassMedia || le.Reason != "video render" {
		t.Fatalf("err = %v (%T), want a *LeaseError naming the video render", err, err)
	}
	if !IsLeaseRefusal(err) {
		t.Fatal("IsLeaseRefusal must recognise the fence's own error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("the refusal must read as congestion for pipeline.classifyErr: %q", err)
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("returned after %s: a held card is waited for inside the caller's deadline, not refused on sight", el)
	}
	if n := s.upstream.Load(); n != 0 {
		t.Fatalf("%d request(s) reached /upstream while the card was fenced", n)
	}
}

// A deadline of now is the no-wait form (the post-run pin, the per-step
// tokenizer): one inspection, an immediate typed refusal.
func TestAwaitUpstreamNoWaitFormRefusesAtOnce(t *testing.T) {
	m := armLease(t)
	s := newStubSwap(t, map[string][]string{"seat": nil})
	holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "render"})
	start := time.Now()
	if _, err := AwaitUpstream(context.Background(), s.srv.URL, "seat", "/props", time.Now()); !IsLeaseRefusal(err) {
		t.Fatalf("err = %v, want a lease refusal", err)
	}
	if el := time.Since(start); el > leasePollInterval/2 {
		t.Fatalf("the no-wait form waited %s", el)
	}
}

// A request for a model llama-swap lists as READY cannot start anything, so the
// fence lets it through — resolved by alias, as the harness names its seats.
func TestAwaitUpstreamAdmitsAResidentSeatUnderAFence(t *testing.T) {
	m := armLease(t)
	s := newStubSwap(t, map[string][]string{"qwen3.8-27b-vllm-3card": {"agent-pool"}})
	s.running.Store(`{"running":[{"model":"qwen3.8-27b-vllm-3card","state":"ready"}]}`)
	holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "render"})
	start := time.Now()
	if _, err := AwaitUpstream(context.Background(), s.srv.URL, "agent-pool", "/props", time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("a ready seat was refused: %v", err)
	}
	if el := time.Since(start); el > leasePollInterval/2 {
		t.Fatalf("a ready seat waited %s", el)
	}
}

// Starting and stopping are not resident: a request to a stopping model is the
// reload after the stop, and a starting one is a load nobody may join under a fence.
func TestAwaitUpstreamTreatsATransitionalSeatAsCold(t *testing.T) {
	for _, state := range []string{"starting", "stopping"} {
		t.Run(state, func(t *testing.T) {
			m := armLease(t)
			s := newStubSwap(t, map[string][]string{"seat": nil})
			s.running.Store(`{"running":[{"model":"seat","state":"` + state + `"}]}`)
			holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "render"})
			if _, err := AwaitUpstream(context.Background(), s.srv.URL, "seat", "/props", time.Now()); !IsLeaseRefusal(err) {
				t.Fatalf("a %s seat passed the fence: %v", state, err)
			}
		})
	}
}

// An unreadable /running proves nothing: the fence refuses rather than assume.
func TestAwaitUpstreamRefusesWhenResidencyIsUnreadable(t *testing.T) {
	m := armLease(t)
	holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "render"})
	if _, err := AwaitUpstream(context.Background(), "http://127.0.0.1:1", "seat", "/props", time.Now()); !IsLeaseRefusal(err) {
		t.Fatalf("an unreadable residency view passed the fence: %v", err)
	}
}

// The wait ends the moment the render releases the card.
func TestAwaitUpstreamProceedsWhenTheFenceLifts(t *testing.T) {
	m := armLease(t)
	s := newStubSwap(t, map[string][]string{"seat": nil})
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "short render"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(300 * time.Millisecond); _ = l.Release() }()
	if _, err := AwaitUpstream(context.Background(), s.srv.URL, "seat", "/props", time.Now().Add(10*time.Second)); err != nil {
		t.Fatalf("the request was refused after the render released the card: %v", err)
	}
}

// The fence is blocksLoad's: an exclusive text hold fences, a plain text
// reservation and a draining hold do not, and the holder's own child (the
// inherited GPU_LEASE_EPOCH) is never fenced by its parent.
func TestAwaitUpstreamFollowsTheLoadPredicate(t *testing.T) {
	cases := []struct {
		name    string
		class   gpulease.Class
		opts    gpulease.Options
		inherit bool
		fenced  bool
	}{
		{"exclusive text", gpulease.ClassText, gpulease.Options{Reason: "bench", Exclusive: true}, false, true},
		{"plain text", gpulease.ClassText, gpulease.Options{Reason: "bench"}, false, false},
		{"draining text", gpulease.ClassText, gpulease.Options{Reason: "bench", Draining: true}, false, false},
		{"draining media", gpulease.ClassMedia, gpulease.Options{Reason: "render", Draining: true}, false, false},
		{"inherited media", gpulease.ClassMedia, gpulease.Options{Reason: "render"}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := armLease(t)
			s := newStubSwap(t, map[string][]string{"seat": nil})
			l := holdLease(t, m, tc.class, tc.opts)
			if tc.inherit {
				t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(l.Epoch(), 10))
			}
			_, err := AwaitUpstream(context.Background(), s.srv.URL, "seat", "/props", time.Now())
			if got := IsLeaseRefusal(err); got != tc.fenced {
				t.Fatalf("fenced = %v (err %v), want %v", got, err, tc.fenced)
			}
		})
	}
}

func TestAwaitUpstreamRejectsAnEmptyEndpoint(t *testing.T) {
	if _, err := AwaitUpstream(context.Background(), "", "seat", "/props", time.Now()); !errors.Is(err, ErrNoUpstreamRoot) {
		t.Fatalf("err = %v, want ErrNoUpstreamRoot", err)
	}
}

// AwaitModelRoute is the same fence for a model-dispatched route: the URL is the
// root plus the route, and a cold model under a render is refused.
func TestAwaitModelRouteFencesAColdModel(t *testing.T) {
	m := armLease(t)
	s := newStubSwap(t, map[string][]string{"gemma": nil})
	u, err := AwaitModelRoute(context.Background(), s.srv.URL+"/v1", "gemma", "v1/embeddings", time.Now())
	if err != nil || u != s.srv.URL+"/v1/embeddings" {
		t.Fatalf("unfenced: (%q, %v), want the root + route", u, err)
	}
	holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "render"})
	if _, err := AwaitModelRoute(context.Background(), s.srv.URL, "gemma", "/v1/embeddings", time.Now()); !IsLeaseRefusal(err) {
		t.Fatalf("fenced cold model: err = %v, want a lease refusal", err)
	}
	s.running.Store(`{"running":[{"model":"gemma","state":"ready"}]}`)
	if _, err := AwaitModelRoute(context.Background(), s.srv.URL, "gemma", "/v1/embeddings", time.Now()); err != nil {
		t.Fatalf("fenced resident model: err = %v, want the route", err)
	}
	if _, err := AwaitModelRoute(context.Background(), "", "gemma", "/v1/embeddings", time.Now()); !errors.Is(err, ErrNoUpstreamRoot) {
		t.Fatalf("empty endpoint: err = %v, want ErrNoUpstreamRoot", err)
	}
}
