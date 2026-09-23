package judge

// fence_test.go: the embedder posts to llama-swap's /v1/embeddings, a
// model-dispatched route that swaps the embedding model in. Under a GPU-lease
// fence over a cold embedder it must not reach llama-swap (2026-09-22): the
// kNN pre-filter on the request path fails open instead, as it already does on
// a slow embedder.

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

func TestEmbedUnderAFenceNeverReachesLlamaSwap(t *testing.T) {
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
	defer func() { _ = l.Release() }()

	var embeds atomic.Int64
	var running atomic.Value
	running.Store(`{"running":[]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"embeddinggemma"}]}`))
		case "/running":
			_, _ = w.Write([]byte(running.Load().(string)))
		case "/v1/embeddings":
			embeds.Add(1)
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	e := NewEmbedder(srv.URL, "embeddinggemma", 300*time.Millisecond)
	start := time.Now()
	if _, err := e.Embed("hello"); !modelaffinity.IsLeaseRefusal(err) {
		t.Fatalf("Embed = %v, want the fence's lease refusal", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the fenced embed waited %s; it is bounded by the embedder's own timeout", el)
	}
	if got := embeds.Load(); got != 0 {
		t.Fatalf("%d embedding request(s) reached llama-swap under a media lease", got)
	}

	// A resident embedder is served under the fence: nothing loads.
	running.Store(`{"running":[{"model":"embeddinggemma","state":"ready"}]}`)
	if _, err := e.Embed("hello"); err != nil || embeds.Load() != 1 {
		t.Fatalf("resident embedder: err %v, requests %d; want one request served", err, embeds.Load())
	}
}
