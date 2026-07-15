package judge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEmbedUsesConfiguredModel is the model-agnostic guard: the embedder must send
// the model it was constructed with, not a hardcoded name. embed.go used to hardcode
// "embeddinggemma"; a machine whose resident embedder is named differently would then
// request a model it does not serve.
func TestEmbedUsesConfiguredModel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		gotModel = body.Model
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()

	if _, err := NewEmbedder(srv.URL, "custom-embedder", time.Second).Embed("hi"); err != nil {
		t.Fatal(err)
	}
	if gotModel != "custom-embedder" {
		t.Fatalf("embed request model = %q, want %q (must use the configured model, not a hardcode)", gotModel, "custom-embedder")
	}
}

func TestSimilar_Cosine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// two identical-direction vectors -> cosine 1.0
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[2,0]}]}`))
	}))
	defer srv.Close()
	e := NewEmbedder(srv.URL, "test-embed", time.Second)
	s, err := e.Similar("the cat sat", "a cat was sitting")
	if err != nil {
		t.Fatal(err)
	}
	if s < 0.999 {
		t.Fatalf("expected ~1.0 cosine, got %v", s)
	}
}

func TestSimilar_ErrorsOnBadEndpoint(t *testing.T) {
	e := NewEmbedder("http://127.0.0.1:1", "test-embed", 200*time.Millisecond)
	if _, err := e.Similar("x", "y"); err == nil {
		t.Fatal("expected error from unreachable endpoint")
	}
}

func TestSimilar_Cosine_Reordered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// index 1 arrives first — must pair by the index field, not position
		w.Write([]byte(`{"data":[{"index":1,"embedding":[2,0]},{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()
	s, err := NewEmbedder(srv.URL, "test-embed", time.Second).Similar("a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if s < 0.999 {
		t.Fatalf("expected ~1.0 with reordered response, got %v", s)
	}
}

func TestSimilar_ErrorsOnStatus500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	if _, err := NewEmbedder(srv.URL, "test-embed", time.Second).Similar("a", "b"); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestSimilar_ErrorsOnWrongCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`)) // only 1 vector
	}))
	defer srv.Close()
	if _, err := NewEmbedder(srv.URL, "test-embed", time.Second).Similar("a", "b"); err == nil {
		t.Fatal("expected error on wrong vector count")
	}
}

func TestSimilar_ErrorsOnBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	if _, err := NewEmbedder(srv.URL, "test-embed", time.Second).Similar("a", "b"); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}

func TestEmbedReturnsVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()
	v, err := NewEmbedder(srv.URL, "test-embed", time.Second).Embed("hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 3 || v[0] != 0.1 {
		t.Fatalf("want [0.1 0.2 0.3], got %v", v)
	}
}

func TestEmbedBadStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	if _, err := NewEmbedder(srv.URL, "test-embed", time.Second).Embed("x"); err == nil {
		t.Fatal("want error on 500")
	}
}

func TestEmbedWrongCountErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	if _, err := NewEmbedder(srv.URL, "test-embed", time.Second).Embed("x"); err == nil {
		t.Fatal("want error when zero vectors returned")
	}
}
