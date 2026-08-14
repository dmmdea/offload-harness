package tokclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestCountFailsOnTokenlessJSON(t *testing.T) {
	// A 200 JSON body WITHOUT a tokens array (a proxy's error object) must
	// fail, not read as a confident "0 tokens" (review finding 2026-08-14).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":"model loading"}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "m", time.Second)
	if n, ok := c.Count(context.Background(), "some text"); ok {
		t.Fatalf("Count = (%d, true) on a tokenless 200 body — a confident wrong zero", n)
	}
	if c.LastErr() == "" {
		t.Fatal("LastErr must name the failure so a sticky downgrade is diagnosable")
	}
	// An EMPTY tokens array on empty input is a legitimate zero.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tokens":[]}`)
	}))
	defer srv2.Close()
	if n, ok := New(srv2.URL, "m", time.Second).Count(context.Background(), ""); !ok || n != 0 {
		t.Fatalf("Count(empty, tokens:[]) = (%d, %v), want (0, true)", n, ok)
	}
}

func TestLastErrNamesBothFailedRoutes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := New(srv.URL, "gemma-4-e4b", time.Second)
	if _, ok := c.Pieces(context.Background(), "text"); ok {
		t.Fatal("expected failure")
	}
	e := c.LastErr()
	if !strings.Contains(e, "/upstream/gemma-4-e4b/tokenize") || !strings.Contains(e, "HTTP 404") {
		t.Fatalf("LastErr = %q — must name each failed route and its status so the downgrade is diagnosable", e)
	}
}

// --- review round 2 (2026-08-14): shape sniff, failure classification, scaled cap ---

// A 200 whose body is NOT a tokenize response (an interposing proxy's page)
// must count as THAT candidate failing — URL-attributed — and must not mask
// the fallback route, which may answer correctly.
func TestGarbage200FallsThroughToBareRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/upstream/") {
			fmt.Fprint(w, `<html>gateway error page</html>`) // 200, not tokenize-shaped
			return
		}
		if r.URL.Path == "/tokenize" {
			piecesHandler(t)(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := New(srv.URL, "m", time.Second)
	if _, ok := c.Pieces(context.Background(), "one two"); !ok {
		t.Fatalf("Pieces must fall through a garbage-200 first candidate to the healthy bare route (LastErr=%q)", c.LastErr())
	}
}

// When EVERY route answers 200-with-garbage, the recorded reason must name
// each URL (the attribution the reasons machinery exists for) and the failure
// must classify as transient, not definitive.
func TestAllRoutesGarbage200FailsWithURLAttribution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":"model loading"}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "gemma-4-e4b", time.Second)
	if _, ok := c.Pieces(context.Background(), "text"); ok {
		t.Fatal("expected failure when no route returns a tokenize-shaped body")
	}
	e := c.LastErr()
	if !strings.Contains(e, "/upstream/gemma-4-e4b/tokenize") || !strings.Contains(e, "not a tokenize response") {
		t.Fatalf("LastErr = %q — the garbage-200 must be URL-attributed", e)
	}
	if c.LastFailDefinitive() {
		t.Fatal("a garbage 200 is NOT proof the route is absent — it must classify transient")
	}
}

// Failure classification: all-404 is definitive (the route positively does not
// exist); any 5xx in the mix is not (the server may just be swapping a model);
// and the mark tracks the MOST RECENT failure rather than sticking.
func TestLastFailDefinitiveClassification(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusNotFound)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", int(status.Load()))
	}))
	defer srv.Close()

	c := New(srv.URL, "m", time.Second)
	c.Pieces(context.Background(), "text")
	if !c.LastFailDefinitive() {
		t.Fatal("every candidate answered 404 — the failure must classify as definitive route absence")
	}

	// The same client later hits a 503 (model swap in flight): the definitive
	// mark must follow the most recent failure (fail() resets it before post()
	// re-stores), or one old 404 would keep licensing an immediate downgrade.
	status.Store(http.StatusServiceUnavailable)
	c.Pieces(context.Background(), "text")
	if c.LastFailDefinitive() {
		t.Fatal("a 503 (model swap in flight) must classify as transient, never definitive")
	}
}

// The read cap scales with the input, and the bound must hold in the worst
// legitimate regime: ~1 token/byte content whose wire entries carry llama.cpp's
// REAL shape — `{"id":N,"piece":"a"},` ≈ 26 bytes/token (round-3 finding
// 2026-08-14: a fixture without the id field "proved" a 16× cap that truncated
// real responses). 1 MiB of content × one-byte pieces ≈ 27 MiB of response
// must decode fine.
func TestScaledReadCapAdmitsLargePiecesResponse(t *testing.T) {
	content := strings.Repeat("a", 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tokens":[`))
		entry := []byte(`{"id":1234567,"piece":"a"},`)
		for i := 0; i < len(req.Content); i++ {
			if i == len(req.Content)-1 {
				entry = []byte(`{"id":1234567,"piece":"a"}`)
			}
			_, _ = w.Write(entry)
		}
		_, _ = w.Write([]byte(`]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "m", 30*time.Second)
	lens, ok := c.Pieces(context.Background(), content)
	if !ok {
		t.Fatalf("Pieces failed on a legitimate oversized response — the read cap must scale with the input (LastErr=%q)", c.LastErr())
	}
	if len(lens) != len(content) {
		t.Fatalf("got %d pieces, want %d", len(lens), len(content))
	}
}
