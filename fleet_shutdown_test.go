package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// A fleet-serve shutdown waits for the node emitter's background posts: the frame of a card closed
// during the drain reaches PAIR before the process exits, instead of being lost and left for the
// next process's orphan sweep to close as failed.
func TestFleetServeDrainDeliversTheNodeCardsBeforeExit(t *testing.T) {
	e, frames := slowPairEmitter(t)
	fleetServeDrain(fleetnode.NewJobs(time.Hour, 1), e, time.Second)
	if got := frames(); got != 1 {
		t.Fatalf("the ingress had %d frames when the drain returned, want 1: the shutdown did not wait for the node's card posts", got)
	}
}

// slowPairEmitter is a node emitter against a PAIR that answers slowly, with one failed frame already
// posted in the background: the post is still in flight when a drain that does not wait returns.
// frames reports how many frames the ingress has answered so far.
func slowPairEmitter(t *testing.T) (*pairworkloads.Emitter, func() int) {
	t.Helper()
	var mu sync.Mutex
	got := 0
	ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // a PAIR that answers slowly: the post is still in flight when the drain ends
		mu.Lock()
		got++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ingress.Close)
	app := t.TempDir()
	for name, body := range map[string]string{
		"node-id.json":                           `{"node_uuid":"node-s-uuid"}`,
		filepath.Join("cluster", "members.json"): `[{"nodeUuid":"node-s-uuid","name":"node-s"}]`,
	} {
		p := filepath.Join(app, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := pairworkloads.New(pairworkloads.Config{Enabled: true, Endpoint: ingress.URL, AppDir: app, OpenDir: t.TempDir()})
	e.Emit(pairworkloads.Event{JobID: "j1", Model: "m", Engine: "llamacpp", State: "failed", Error: "dropped", CreatedAt: 1, CompletedAt: 2})
	return e, func() int {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

// L2: a Serve that returns on its own (the listener failed) is a shutdown too: the jobs in flight and
// the card posts pending are the same, so it drains and waits for the emitter before the process
// exits, and it still returns Serve's error. An interrupt keeps its own drain-then-close order.
func TestFleetServeAwaitDrainsWhenServeFails(t *testing.T) {
	e, frames := slowPairEmitter(t)
	errCh := make(chan error, 1)
	boom := errors.New("accept failed")
	errCh <- boom
	ln := &countingCloser{}
	got := fleetServeAwait(context.Background(), errCh, ln, fleetnode.NewJobs(time.Hour, 1), e, time.Second)
	if !errors.Is(got, boom) {
		t.Fatalf("fleetServeAwait = %v, want Serve's own error", got)
	}
	if n := frames(); n != 1 {
		t.Fatalf("the ingress had %d frames when a failed Serve returned, want 1: the exit did not wait for the node's card posts", n)
	}
}

func TestFleetServeAwaitDrainsOnInterruptThenClosesTheListener(t *testing.T) {
	e, frames := slowPairEmitter(t)
	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ln := &countingCloser{onClose: func() { errCh <- nil }} // the listener closing is what ends Serve
	if got := fleetServeAwait(ctx, errCh, ln, fleetnode.NewJobs(time.Hour, 1), e, time.Second); got != nil {
		t.Fatalf("an interrupt returns nil, got %v", got)
	}
	if n := frames(); n != 1 {
		t.Fatalf("the ingress had %d frames when the interrupt drain returned, want 1", n)
	}
	if ln.closed != 1 {
		t.Fatalf("the listener closed %d times, want once", ln.closed)
	}
}

type countingCloser struct {
	closed  int
	onClose func()
}

func (c *countingCloser) Close() error {
	c.closed++
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}
