package fleetnode

// chat_lane_fence_test.go pins the fleet chat lane behind the GPU-lease fence
// (2026-09-22). The lane forwards another box's cascade call to THIS node's
// llama-swap /v1/chat/completions, a model-dispatched route that swaps the named
// model in. On a node that runs media under a lease, an inbound cascade call
// for a model that is not resident must not reach llama-swap: it waits for the
// card inside the caller's own budget, then answers 503 with the typed lease
// refusal the caller files as congestion.

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

// fencedChatSwap is a llama-swap stand-in that also answers the fence's
// residency view and counts every chat completion it is handed.
func fencedChatSwap(t *testing.T, running string, chats *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gemma-4-e4b"}]}`))
		case "/running":
			_, _ = w.Write([]byte(running))
		case "/v1/chat/completions":
			chats.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func holdMediaLease(t *testing.T) {
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

// chatWithDeadline posts one chat-lane call whose context ends after d — the
// caller's own budget, which the node sees as the request context.
func chatWithDeadline(t *testing.T, s *Server, d time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, ChatLanePath, strings.NewReader(chatBody)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// THE DEFECT, on the lane: a render holds this node's card and the requested
// model is cold. Nothing may reach llama-swap's chat route, the node holds the
// call inside the caller's budget, and the answer is a typed 503 naming the
// render — never a forward that loads the model onto the render's cards.
func TestChatLaneUnderAFenceNeverReachesLlamaSwap(t *testing.T) {
	holdMediaLease(t)
	var chats atomic.Int64
	up := fencedChatSwap(t, `{"running":[]}`, &chats)
	s := servingNode(t, chatCfg(up.URL, ""), true, "gemma-4-e4b")

	start := time.Now()
	rec := chatWithDeadline(t, s, 400*time.Millisecond)
	if got := chats.Load(); got != 0 {
		t.Fatalf("%d chat completion(s) reached llama-swap under a media lease — each one loads the model onto the render's cards", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"gpu-lease timeout", "video render"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal must carry %q so the caller files it as congestion and names the holder: %s", want, body)
		}
	}
	if el := time.Since(start); el < 350*time.Millisecond {
		t.Errorf("answered after %s: a held card is waited for inside the caller's budget, not refused on sight", el)
	}
}

// A model this node's llama-swap lists as READY is forwarded under the fence:
// the request starts nothing.
func TestChatLaneForwardsAResidentModelUnderAFence(t *testing.T) {
	holdMediaLease(t)
	var chats atomic.Int64
	up := fencedChatSwap(t, `{"running":[{"model":"gemma-4-e4b","state":"ready"}]}`, &chats)
	s := servingNode(t, chatCfg(up.URL, ""), true, "gemma-4-e4b")
	rec := chatWithDeadline(t, s, 5*time.Second)
	if rec.Code != http.StatusOK || chats.Load() != 1 {
		t.Fatalf("status = %d, chats = %d; want the resident model forwarded once (body %s)", rec.Code, chats.Load(), rec.Body.String())
	}
}
