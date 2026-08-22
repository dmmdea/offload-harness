package hailoclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fakeSidecar(t *testing.T, enabled bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"enabled": enabled, "hefs_missing": []string{}})
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		var args map[string]any
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": true, "kind": "bad_request"})
			return
		}
		switch r.URL.Path {
		case "/v1/face_embed":
			json.NewEncoder(w).Encode(map[string]any{"faces": []any{map[string]any{"embedding": []float64{0.1, 0.2}}}, "count": 1, "seen": args["image_path"]})
		case "/v1/depth":
			json.NewEncoder(w).Encode(map[string]any{"error": true, "kind": "image_missing", "message": "nope"})
		default:
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"error": true, "kind": "unknown_tool"})
		}
	})
	return httptest.NewServer(mux)
}

func TestCallReturnsResultAndEchoesArgs(t *testing.T) {
	srv := fakeSidecar(t, true)
	defer srv.Close()
	c := New(srv.URL, 5*time.Second)
	out, err := c.Call(context.Background(), "face_embed", map[string]any{"image_path": "a.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if out["seen"] != "a.jpg" || out["count"].(float64) != 1 {
		t.Fatalf("unexpected result %v", out)
	}
}

func TestStructuredErrorIsAResultNotAnError(t *testing.T) {
	srv := fakeSidecar(t, true)
	defer srv.Close()
	out, err := New(srv.URL, time.Second).Call(context.Background(), "depth", map[string]any{})
	if err != nil {
		t.Fatalf("a 200 with error:true is a structured result; got err %v", err)
	}
	if out["kind"] != "image_missing" {
		t.Fatalf("kind = %v", out["kind"])
	}
}

func TestHTTPErrorSurfacesBody(t *testing.T) {
	srv := fakeSidecar(t, true)
	defer srv.Close()
	if _, err := New(srv.URL, time.Second).Call(context.Background(), "teleport", map[string]any{}); err == nil {
		t.Fatal("404 must be an error")
	}
}

func TestHealth(t *testing.T) {
	srv := fakeSidecar(t, false)
	defer srv.Close()
	h, err := New(srv.URL, time.Second).Health(context.Background())
	if err != nil || h["enabled"] != false {
		t.Fatalf("health = %v, %v", h, err)
	}
}

func TestUnreachableIsAnError(t *testing.T) {
	if _, err := New("http://127.0.0.1:1", 200*time.Millisecond).Health(context.Background()); err == nil {
		t.Fatal("closed port must error")
	}
}
