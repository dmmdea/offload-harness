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
	if err := SweepKVSlotDir(dir, 1500); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"k1-old.bin": false, "k1-mid.bin": false, "k1-new.bin": true, "not-a-slot.txt": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Errorf("%s: exists=%v, want %v", name, err == nil, want)
		}
	}
}
