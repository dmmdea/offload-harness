package servedwindow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// A node whose serving config says --ctx-size 49152 --parallel 1 but whose agent_ctx_tokens key was left at
// 131072 advertised 131072, and the delegator dealt it contracts its engine rejected. The serving config
// is the better source, and an engine report on this box is better still.
func TestKnownPrefersWhatTheEngineServesOverTheKey(t *testing.T) {
	dir := t.TempDir()
	yaml := filepath.Join(dir, "llama-swap.yaml")
	if err := os.WriteFile(yaml, []byte(`
models:
  node-a-agent:
    aliases: [agent-seat]
    cmd: /bin/llama-server --model m.gguf --ctx-size 49152 --parallel 1 --port ${PORT}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.ServingConfigPath = yaml

	if w := Known(cfg, "agent-seat", 131072); w.Tokens != 49152 || w.Source != SourceServingConfig || w.Configured != 131072 {
		t.Fatalf("serving config 49152 against key 131072: got %+v", w)
	}
	Remember(cfg, "agent-seat", 40000)
	if w := Known(cfg, "agent-seat", 131072); w.Tokens != 40000 || w.Source != SourceServed {
		t.Fatalf("engine reported 40000: got %+v", w)
	}
	if w := Known(cfg, "agent-seat", 0); w.Tokens != 0 || w.ForRun() != 40000 {
		t.Fatalf("an unset key must stay unadvertised but a run may use what was served: got %+v ForRun %d", w, w.ForRun())
	}
}
