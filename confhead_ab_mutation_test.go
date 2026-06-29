package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/eval"
)

// TestRedirectWritesNeverHitsLiveBase: every writable path in the redirected
// config lives under the scratch dir, and none under the live ~/.local-offload
// base. The read-only confhead inputs are NOT redirected here (the ON arm wires
// the staged paths separately) — this guards the WRITE surface.
func TestRedirectWritesNeverHitsLiveBase(t *testing.T) {
	cfg := config.Default() // points every path at ~/.local-offload
	liveBase := filepath.Dir(cfg.CachePath)
	scratch := t.TempDir()

	c := redirectWrites(cfg, scratch)
	writable := []string{
		c.CachePath, c.LedgerPath, c.ThresholdsPath, c.TierOverridesPath,
		c.RouterWeightsPath, c.RouterLabelsPath, c.ConfHeadLabelsPath,
		c.ShadowQueuePath, c.KNNIndexPath, c.ExemplarsDir, c.MediaDir, c.SVGDir,
	}
	for _, p := range writable {
		if !strings.HasPrefix(p, scratch) {
			t.Fatalf("writable path %q not under scratch %q", p, scratch)
		}
		if strings.HasPrefix(p, liveBase) {
			t.Fatalf("writable path %q leaks into the live base %q", p, liveBase)
		}
	}
}

// stubCascadeServer returns valid classify/triage JSON for any model so an A/B
// can run end-to-end without live inference. Records each request so the test
// can confirm the cascade was actually exercised.
func stubCascadeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAllBody(r)
		w.Header().Set("Content-Type", "application/json")
		// Pick a response that satisfies whichever grammar/task is in flight. The
		// harness validates against the task's schema; classify wants {"label",...},
		// triage wants {"decision",...}. We sniff the request body for the field.
		content := `{"label":"billing","confidence":0.95}`
		if strings.Contains(body, "decision") || strings.Contains(body, "yes") || strings.Contains(body, "Does this") {
			content = `{"decision":"yes","reason":"the text reports an error"}`
		}
		resp := map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 8},
		}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}))
}

func readAllBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String(), nil
}

// TestConfheadABNoLiveMutation: a full A/B run against a stub server must leave a
// designated "live" base dir completely untouched — all writes land in the
// process-temp scratch dir the runner creates. We point the config's writable
// paths at a sentinel live dir, snapshot it before, run, and assert it is
// unchanged afterward.
func TestConfheadABNoLiveMutation(t *testing.T) {
	srv := stubCascadeServer(t)
	defer srv.Close()

	liveBase := t.TempDir() // stand-in for ~/.local-offload
	staged := t.TempDir()   // stand-in for .testrun/bootstrap

	// Stage minimal valid confhead weights + thresholds (thresholds=0 => inert,
	// matching the real staged files; the ON arm reads these, never writes them).
	writeFile(t, filepath.Join(staged, "confhead-weights.json"), `{"tasks":{}}`)
	writeFile(t, filepath.Join(staged, "confhead-thresholds.json"), `{"classify":0,"triage":0}`)

	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.MaxRetries = 0
	// Point ALL writable paths at the sentinel live base; the runner must redirect
	// these away to scratch so this dir stays empty.
	cfg.CachePath = filepath.Join(liveBase, "cache.db")
	cfg.LedgerPath = filepath.Join(liveBase, "ledger.jsonl")
	cfg.ThresholdsPath = filepath.Join(liveBase, "thresholds.json")
	cfg.TierOverridesPath = filepath.Join(liveBase, "tier_overrides.json")
	cfg.RouterWeightsPath = filepath.Join(liveBase, "router-weights.json")
	cfg.RouterLabelsPath = filepath.Join(liveBase, "router-labels.jsonl")
	cfg.ConfHeadLabelsPath = filepath.Join(liveBase, "confhead-labels.jsonl")
	cfg.ConfHeadPath = filepath.Join(liveBase, "confhead-weights.json")
	cfg.ConfHeadThresholdsPath = filepath.Join(liveBase, "confhead-thresholds.json")
	cfg.ShadowQueuePath = filepath.Join(liveBase, "shadow-queue.jsonl")
	cfg.KNNIndexPath = filepath.Join(liveBase, "knn-index.jsonl")
	cfg.ExemplarsDir = filepath.Join(liveBase, "exemplars")
	cfg.MediaDir = filepath.Join(liveBase, "media")
	cfg.SVGDir = filepath.Join(liveBase, "svg")

	cases := []eval.Case{
		{Task: "classify", Input: "I returned the upgrade but the refund hasn't shown on my statement yet, please help.", Params: map[string]any{"labels": []any{"billing", "technical", "account"}}, Expect: "billing"},
		{Task: "triage", Input: "RuntimeError: CUDA out of memory. Tried to allocate two gigabytes on the device.", Params: map[string]any{"question": "Does this text indicate an error or failure?"}, Expect: "yes"},
	}

	before := snapshotDir(t, liveBase)

	if err := runConfheadAB(cfg, cases, staged, 0.0, 1.0, 0.15, false, false); err != nil {
		t.Fatalf("runConfheadAB: %v", err)
	}

	after := snapshotDir(t, liveBase)
	if before != after {
		t.Fatalf("live base mutated during A/B run:\n before=%q\n after =%q", before, after)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshotDir returns a stable string of the dir's recursive file list + sizes,
// so any created/modified file changes the snapshot. Missing dir => "".
func snapshotDir(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		sb.WriteString(p)
		sb.WriteString(":")
		sb.WriteString(itoa(info.Size()))
		sb.WriteString("\n")
		return nil
	})
	return sb.String()
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
