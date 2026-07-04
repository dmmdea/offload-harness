package judge

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSimilar_Cosine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// two identical-direction vectors -> cosine 1.0
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[2,0]}]}`))
	}))
	defer srv.Close()
	e := NewEmbedder(srv.URL, time.Second)
	s, err := e.Similar("the cat sat", "a cat was sitting")
	if err != nil {
		t.Fatal(err)
	}
	if s < 0.999 {
		t.Fatalf("expected ~1.0 cosine, got %v", s)
	}
}

func TestSimilar_ErrorsOnBadEndpoint(t *testing.T) {
	e := NewEmbedder("http://127.0.0.1:1", 200*time.Millisecond)
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
	s, err := NewEmbedder(srv.URL, time.Second).Similar("a", "b")
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
	if _, err := NewEmbedder(srv.URL, time.Second).Similar("a", "b"); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestSimilar_ErrorsOnWrongCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`)) // only 1 vector
	}))
	defer srv.Close()
	if _, err := NewEmbedder(srv.URL, time.Second).Similar("a", "b"); err == nil {
		t.Fatal("expected error on wrong vector count")
	}
}

func TestSimilar_ErrorsOnBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	if _, err := NewEmbedder(srv.URL, time.Second).Similar("a", "b"); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}

func TestEmbedReturnsVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()
	v, err := NewEmbedder(srv.URL, time.Second).Embed("hello")
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
	if _, err := NewEmbedder(srv.URL, time.Second).Embed("x"); err == nil {
		t.Fatal("want error on 500")
	}
}

func TestEmbedWrongCountErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	if _, err := NewEmbedder(srv.URL, time.Second).Embed("x"); err == nil {
		t.Fatal("want error when zero vectors returned")
	}
}
