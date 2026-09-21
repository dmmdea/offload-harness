package fleetnode

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// kvSlotStub imitates llama-swap: /v1/models lists the seat, and
// /upstream/<seat>/slots/0?action=… answers like llama-server does.
func kvSlotStub(t *testing.T, seat string, slotDir string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": seat}}})
		case r.URL.Path == "/running":
			// The lane refuses to START a seat, so every happy-path case needs one
			// that /running already reports ready.
			_ = json.NewEncoder(w).Encode(map[string]any{"running": []map[string]any{{"model": seat, "state": "ready", "cmd": "x"}}})
		case strings.HasPrefix(r.URL.Path, "/upstream/"+seat+"/slots/0"):
			var body struct {
				Filename string `json:"filename"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch r.URL.Query().Get("action") {
			case "save":
				_ = os.WriteFile(filepath.Join(slotDir, body.Filename), bytes.Repeat([]byte{1}, 1024), 0o644)
				_ = json.NewEncoder(w).Encode(map[string]any{"id_slot": 0, "filename": body.Filename, "n_saved": 6000, "n_written": 1024})
			case "restore":
				if _, err := os.Stat(filepath.Join(slotDir, body.Filename)); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Unable to restore slot, no available space in KV cache or invalid slot save file"}}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id_slot": 0, "filename": body.Filename, "n_restored": 6000, "n_read": 1024})
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func kvSlotServer(t *testing.T, endpoint, slotDir string) *Server {
	t.Helper()
	return New(nil, nil, Options{
		NodeID:           "test-node",
		LoopbackListener: true,
		Cfg:              config.Config{Endpoint: endpoint},
		KVSlotDir:        slotDir,
		KVSlotCapGiB:     1,
	})
}

func kvSlotPost(t *testing.T, h http.Handler, path string, body any) (int, KVSlotResponse) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var out KVSlotResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

// kvTestKey is a well-formed key fixture: "k1-" + 64 hex chars, built by
// repetition so it reads as a fixture (and so the repo's secret scanner does
// not mistake a literal 64-hex string for a credential).
var kvTestKey = "k1-" + strings.Repeat("0123456789abcdef", 4)

func TestKVSlotSaveThenRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	up := kvSlotStub(t, "qwen3.5-4b-agent", dir)
	defer up.Close()
	h := kvSlotServer(t, up.URL, dir).Handler()

	code, out := kvSlotPost(t, h, KVSlotRestorePath, KVSlotRequest{Seat: "qwen3.5-4b-agent", Key: kvTestKey})
	if code != http.StatusNotFound || out.Status != "miss" {
		t.Fatalf("restore before save: want 404 miss, got %d %+v", code, out)
	}
	code, out = kvSlotPost(t, h, KVSlotSavePath, KVSlotRequest{Seat: "qwen3.5-4b-agent", Key: kvTestKey})
	if code != http.StatusOK || out.Status != "ok" || out.Tokens != 6000 || out.Bytes != 1024 {
		t.Fatalf("save: want 200 ok 6000 tokens 1024 bytes, got %d %+v", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, kvTestKey+".bin")); err != nil {
		t.Fatalf("slot file not written: %v", err)
	}
	code, out = kvSlotPost(t, h, KVSlotRestorePath, KVSlotRequest{Seat: "qwen3.5-4b-agent", Key: kvTestKey})
	if code != http.StatusOK || out.Status != "ok" || out.Tokens != 6000 {
		t.Fatalf("restore after save: want 200 ok 6000 tokens, got %d %+v", code, out)
	}
}

// TestKVSlotNeverReportsOkWithoutEvidence: a 2xx from llama-server is not proof
// that anything moved. A body that does not parse (a renamed field, a truncated
// read) and a body that reports zero tokens must BOTH fail to produce
// status:"ok" — a delegator that believed either would skip a prefill it never
// restored. This is the defect the free-seat reviewer pointed at.
func TestKVSlotNeverReportsOkWithoutEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		action     string
		path       string
		wantCode   int
		wantStatus string
	}{
		{"unparseable save body", `not json at all`, "save", KVSlotSavePath, http.StatusBadGateway, "unparsed"},
		{"unparseable restore body", `{"n_restored":`, "restore", KVSlotRestorePath, http.StatusBadGateway, "unparsed"},
		{"renamed save field reads as zero", `{"id_slot":0,"tokens_saved":3240}`, "save", KVSlotSavePath, http.StatusOK, "empty"},
		{"restore of zero tokens is a miss", `{"id_slot":0,"n_restored":0,"n_read":0}`, "restore", KVSlotRestorePath, http.StatusNotFound, "miss"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// the restore cases need the file present, or the handler 404s before the upstream call
			if err := os.WriteFile(filepath.Join(dir, kvTestKey+".bin"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "seat-x"}}})
					return
				}
				if r.URL.Path == "/running" {
					_ = json.NewEncoder(w).Encode(map[string]any{"running": []map[string]any{{"model": "seat-x", "state": "ready", "cmd": "x"}}})
					return
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer up.Close()
			h := kvSlotServer(t, up.URL, dir).Handler()
			code, out := kvSlotPost(t, h, tc.path, KVSlotRequest{Seat: "seat-x", Key: kvTestKey})
			if code != tc.wantCode || out.Status != tc.wantStatus {
				t.Fatalf("want %d/%q, got %d/%q (note %q)", tc.wantCode, tc.wantStatus, code, out.Status, out.Note)
			}
			if out.Status == "ok" {
				t.Fatal(`reported "ok" without evidence`)
			}
		})
	}
}

func TestKVSlotRefusesBadKeysAndUnknownSeats(t *testing.T) {
	dir := t.TempDir()
	up := kvSlotStub(t, "qwen3.5-4b-agent", dir)
	defer up.Close()
	h := kvSlotServer(t, up.URL, dir).Handler()
	for _, tc := range []struct {
		name string
		req  KVSlotRequest
		want int
	}{
		{"path-shaped key", KVSlotRequest{Seat: "qwen3.5-4b-agent", Key: "../etc/passwd"}, http.StatusBadRequest},
		{"short hash", KVSlotRequest{Seat: "qwen3.5-4b-agent", Key: "k1-abc"}, http.StatusBadRequest},
		{"unknown seat", KVSlotRequest{Seat: "no-such-seat", Key: kvTestKey}, http.StatusNotFound},
		{"seat with a slash", KVSlotRequest{Seat: "a/b", Key: kvTestKey}, http.StatusBadRequest},
	} {
		if code, _ := kvSlotPost(t, h, KVSlotSavePath, tc.req); code != tc.want {
			t.Errorf("%s: want %d, got %d", tc.name, tc.want, code)
		}
	}
}

func TestKVSlotLaneIsNotImplementedWithoutADirectory(t *testing.T) {
	up := kvSlotStub(t, "qwen3.5-4b-agent", t.TempDir())
	defer up.Close()
	h := kvSlotServer(t, up.URL, "").Handler()
	if code, _ := kvSlotPost(t, h, KVSlotSavePath, KVSlotRequest{Seat: "qwen3.5-4b-agent", Key: kvTestKey}); code != http.StatusNotImplemented {
		t.Fatalf("want 501 without a slot dir, got %d", code)
	}
}

func TestKVSlotBearerGateOnANonLoopbackListener(t *testing.T) {
	dir := t.TempDir()
	up := kvSlotStub(t, "qwen3.5-4b-agent", dir)
	defer up.Close()
	s := New(nil, nil, Options{NodeID: "n", LoopbackListener: false, Cfg: config.Config{Endpoint: up.URL, FleetAuthToken: "secret"}, KVSlotDir: dir})
	h := s.Handler()
	if code, _ := kvSlotPost(t, h, KVSlotSavePath, KVSlotRequest{Seat: "qwen3.5-4b-agent", Key: kvTestKey}); code != http.StatusUnauthorized {
		t.Fatalf("want 401 without the bearer, got %d", code)
	}
	b, _ := json.Marshal(KVSlotRequest{Seat: "qwen3.5-4b-agent", Key: kvTestKey})
	req := httptest.NewRequest(http.MethodPost, KVSlotSavePath, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 with the bearer, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSweepKVSlotDirDeletesOldestFirst(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, bytes.Repeat([]byte{7}, 1000), 0o644)
		_ = os.Chtimes(p, time.Now().Add(-age), time.Now().Add(-age))
	}
	mk("k1-old.bin", 3*time.Hour)
	mk("k1-mid.bin", 2*time.Hour)
	mk("k1-new.bin", time.Hour)
	_ = os.WriteFile(filepath.Join(dir, "not-a-slot.txt"), []byte("keep"), 0o644)
	if err := SweepKVSlotDir(dir, 1500, ""); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"k1-old.bin": false, "k1-mid.bin": false, "k1-new.bin": true, "not-a-slot.txt": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Errorf("%s: exists=%v, want %v", name, err == nil, want)
		}
	}
}

// TestKVSlotRefusesToStartAColdSeat: `/upstream/<seat>/…` STARTS an unloaded
// seat, so without a residency gate this lane is a model-load primitive — it
// would defeat the 5-minute idle unload, ignore a drain or a GPU lease, and
// save an EMPTY slot over a good file. A seat /running does not list is a 409.
func TestKVSlotRefusesToStartAColdSeat(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, kvTestKey+".bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var upstreamHits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "seat-x"}}})
		case r.URL.Path == "/running":
			_ = json.NewEncoder(w).Encode(map[string]any{"running": []map[string]any{}}) // nothing loaded
		default:
			upstreamHits++
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer up.Close()
	h := kvSlotServer(t, up.URL, dir).Handler()
	for _, p := range []string{KVSlotSavePath, KVSlotRestorePath} {
		code, out := kvSlotPost(t, h, p, KVSlotRequest{Seat: "seat-x", Key: kvTestKey})
		if code != http.StatusConflict || out.Status != "seat-cold" {
			t.Errorf("%s on a cold seat: want 409 seat-cold, got %d %q", p, code, out.Status)
		}
	}
	if upstreamHits != 0 {
		t.Fatalf("the lane reached /upstream %d times against a cold seat — that STARTS the model", upstreamHits)
	}
}

// TestKVSlotRejectsNonJSONContentType: a cross-origin browser fetch sends
// text/plain by default, which is a CORS simple request — no preflight, the
// side effect lands. The sibling lanes reject exactly that; so must this one.
func TestKVSlotRejectsNonJSONContentType(t *testing.T) {
	dir := t.TempDir()
	up := kvSlotStub(t, "qwen3.5-4b-agent", dir)
	defer up.Close()
	h := kvSlotServer(t, up.URL, dir).Handler()
	b, _ := json.Marshal(KVSlotRequest{Seat: "qwen3.5-4b-agent", Key: kvTestKey})
	req := httptest.NewRequest(http.MethodPost, KVSlotSavePath, bytes.NewReader(b))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a text/plain POST, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestSweepNeverDeletesTheFileTheSaveJustWrote: the just-written file is the
// newest by mtime, so the LRU pass reaches it only when it ALONE exceeds the
// cap — and a full 32k slot is ~1.1 GB, so that is reachable. Deleting it while
// the response says ok with a byte count would be a lie.
func TestSweepNeverDeletesTheFileTheSaveJustWrote(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "k1-fresh.bin")
	if err := os.WriteFile(fresh, bytes.Repeat([]byte{1}, 4000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SweepKVSlotDir(dir, 10, fresh); err != nil { // cap far below the file
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("the sweep deleted the file the save just wrote")
	}
	if err := SweepKVSlotDir(dir, 10, ""); err != nil { // unprotected: it goes
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); err == nil {
		t.Fatal("without the guard the file must be evicted — the test would not bite")
	}
}
