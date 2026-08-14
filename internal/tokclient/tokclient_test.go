package tokclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// piecesHandler answers a llama.cpp-shaped /tokenize with with_pieces: one
// token per WORD (split on spaces, separators attached to the preceding word)
// — a deterministic fake whose pieces reconstruct the input exactly, which is
// the contract Pieces enforces.
func piecesHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Content    string `json:"content"`
			AddSpecial bool   `json:"add_special"`
			WithPieces bool   `json:"with_pieces"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.AddSpecial {
			t.Error("add_special must be false: callers count CONTENT tokens")
		}
		var toks []map[string]any
		rest := req.Content
		for len(rest) > 0 {
			i := strings.IndexByte(rest, ' ')
			var piece string
			if i < 0 {
				piece, rest = rest, ""
			} else {
				piece, rest = rest[:i+1], rest[i+1:]
			}
			if req.WithPieces {
				toks = append(toks, map[string]any{"id": len(toks), "piece": piece})
			} else {
				toks = append(toks, map[string]any{"id": len(toks)})
			}
		}
		if !req.WithPieces {
			// Bare shape: {"tokens":[1,2,3]}
			ids := make([]int, len(toks))
			for i := range ids {
				ids[i] = i
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tokens": ids})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": toks})
	}
}

func TestPiecesViaLlamaSwapUpstream(t *testing.T) {
	var hitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		if r.URL.Path != "/upstream/gemma-4-e4b/tokenize" {
			http.NotFound(w, r)
			return
		}
		piecesHandler(t)(w, r)
	}))
	defer srv.Close()

	// A trailing /v1 must be stripped (the tokenize route lives at the root).
	c := New(srv.URL+"/v1", "gemma-4-e4b", time.Second)
	text := "alpha beta gamma"
	lens, ok := c.Pieces(context.Background(), text)
	if !ok {
		t.Fatal("Pieces failed against a healthy upstream route")
	}
	if hitPath != "/upstream/gemma-4-e4b/tokenize" {
		t.Fatalf("hit %q, want the llama-swap per-model passthrough", hitPath)
	}
	total := 0
	for _, n := range lens {
		total += n
	}
	if total != len(text) {
		t.Fatalf("piece lengths sum %d, want %d (must reconstruct the input)", total, len(text))
	}
	if len(lens) != 3 {
		t.Fatalf("got %d pieces, want 3", len(lens))
	}
}

func TestPiecesFallsBackToBareServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tokenize" {
			piecesHandler(t)(w, r)
			return
		}
		http.NotFound(w, r) // a bare llama-server has no /upstream
	}))
	defer srv.Close()

	c := New(srv.URL, "m", time.Second)
	if _, ok := c.Pieces(context.Background(), "one two"); !ok {
		t.Fatal("Pieces must fall back to the bare-server /tokenize route")
	}
}

func TestPiecesDecodesByteArrayPieces(t *testing.T) {
	// llama.cpp returns a piece as an ARRAY OF BYTES when it is not valid
	// UTF-8 (byte-fallback tokens mid-rune). Both shapes carry byte length.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Content != "a\xc3\xa1" { // "aá": 1 byte + a 2-byte rune split across tokens
			t.Errorf("unexpected content %q", req.Content)
		}
		fmt.Fprint(w, `{"tokens":[{"id":1,"piece":"a"},{"id":2,"piece":[195]},{"id":3,"piece":[161]}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "m", time.Second)
	lens, ok := c.Pieces(context.Background(), "a\xc3\xa1")
	if !ok {
		t.Fatal("Pieces must decode byte-array pieces")
	}
	want := []int{1, 1, 1}
	if len(lens) != 3 || lens[0] != want[0] || lens[1] != want[1] || lens[2] != want[2] {
		t.Fatalf("lens = %v, want %v", lens, want)
	}
}

func TestPiecesFailsOpenWhenPiecesDoNotReconstructInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tokens":[{"id":1,"piece":"only"}]}`) // 4 bytes for a longer input
	}))
	defer srv.Close()

	c := New(srv.URL, "m", time.Second)
	if _, ok := c.Pieces(context.Background(), "a much longer input"); ok {
		t.Fatal("Pieces accepted a piece accounting that does not reconstruct the input — the byte→token mapping would be unreliable")
	}
}

func TestPiecesFailsOpenOnTransportErrors(t *testing.T) {
	// Unreachable endpoint.
	c := New("http://127.0.0.1:1", "m", 200*time.Millisecond)
	if _, ok := c.Pieces(context.Background(), "text"); ok {
		t.Fatal("Pieces must fail open on an unreachable endpoint")
	}
	// Empty base.
	if _, ok := New("", "m", time.Second).Pieces(context.Background(), "text"); ok {
		t.Fatal("Pieces must fail open on an empty base")
	}
	// Server errors on every route.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, ok := New(srv.URL, "m", time.Second).Pieces(context.Background(), "text"); ok {
		t.Fatal("Pieces must fail open on a 500")
	}
}

func TestCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		piecesHandler(t)(w, r)
	}))
	defer srv.Close()

	c := New(srv.URL, "m", time.Second)
	n, ok := c.Count(context.Background(), "one two three four")
	if !ok || n != 4 {
		t.Fatalf("Count = (%d, %v), want (4, true)", n, ok)
	}
	// Count also decodes the with_pieces object shape (a server that always
	// returns objects must not break the length-only caller).
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tokens":[{"id":1,"piece":"x"},{"id":2,"piece":"y"}]}`)
	}))
	defer srv2.Close()
	if n, ok := New(srv2.URL, "m", time.Second).Count(context.Background(), "xy"); !ok || n != 2 {
		t.Fatalf("Count(object shape) = (%d, %v), want (2, true)", n, ok)
	}
}
