package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/displaywatch"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/placement"
)

// awakeComposite is the shipped composite box with the display layer awake and a state root of its own,
// so a status call reads no machine-wide state.
func awakeComposite(t *testing.T, swapURL string) config.Config {
	t.Helper()
	cfg := shippedComposite()
	for i := range cfg.Layers {
		if cfg.Layers[i].Name == "display" {
			cfg.Layers[i].Dormant = false
		}
	}
	cfg.Endpoint = swapURL
	cfg.StateDir = t.TempDir()
	t.Setenv("NVIDIA_API_KEY", "")
	t.Setenv("NGC_API_KEY", "")
	return cfg
}

func localOf(t *testing.T, cfg config.Config) map[string]any {
	t.Helper()
	s := New(pipeline.New(cfg, nil, nil, nil))
	res, err := s.handleStatus(context.Background(), callReq(`{}`))
	if err != nil {
		t.Fatalf("handleStatus error: %v", err)
	}
	local, _ := decodeResult(t, res)["local"].(map[string]any)
	if local == nil {
		t.Fatal("no local section")
	}
	return local
}

// A composite box says what the display card's gate reads: the mode and the probe's reading. The two
// modes that never touch the OS are deterministic here; auto's readings are pinned on the renderer.
func TestStatusPublishesTheOperatorPresenceReadingAndMode(t *testing.T) {
	swap := coldSwap(t, "agent-pool", "qwen3.8-27b-262k", "gemma-4-26b-agent", "qwen3-vl-8b", "qwen3-vl-32b")

	cfg := awakeComposite(t, swap.URL)
	local := localOf(t, cfg)
	p, _ := local["operator_presence"].(map[string]any)
	if p == nil {
		t.Fatalf("a composite box must publish local.operator_presence: %v", local)
	}
	if p["mode"] != "present" || p["admits"] != false || p["known"] != true {
		t.Fatalf("the default mode reads present and admits nothing: %v", p)
	}
	if _, has := p["caution"]; has {
		t.Fatalf("present needs no caution: %v", p)
	}

	cfg.OperatorPresence = "away"
	p, _ = localOf(t, cfg)["operator_presence"].(map[string]any)
	if p["mode"] != "away" || p["admits"] != true || p["away"] != true {
		t.Fatalf("away reads away and admits: %v", p)
	}
	caution, _ := p["caution"].(string)
	if !strings.Contains(caution, "unconditional") || !strings.Contains(caution, "auto is the supported mode") {
		t.Fatalf("away must say it is an unconditional override that admits at the desk, got %q", caution)
	}
}

func TestPresenceViewForRendersAutoReadings(t *testing.T) {
	cfg := config.Config{OperatorPresence: "auto", OperatorIdleSec: 600}
	for _, tc := range []struct {
		name       string
		p          placement.Presence
		wantAdmits bool
		wantIn     string
	}{
		{"at the desk", placement.Presence{Mode: "auto", Known: true, IdleSec: 12, Note: "idle 12s < threshold 10m0s"}, false, "desk"},
		{"locked", placement.Presence{Mode: "auto", Known: true, Away: true, Locked: true, Note: "console session locked"}, true, "locked"},
		{"unknown", placement.Presence{Mode: "auto", Known: false, Note: "no session is attached to the console"}, false, "unknown"},
	} {
		v := presenceViewFor(cfg, tc.p)
		if v["admits"] != tc.wantAdmits || v["mode"] != "auto" || v["idle_threshold_sec"] != 600 {
			t.Errorf("%s: %v", tc.name, v)
		}
		if reading, _ := v["reading"].(string); !strings.Contains(reading, tc.wantIn) {
			t.Errorf("%s: reading %q must say %q", tc.name, reading, tc.wantIn)
		}
		if _, has := v["caution"]; has {
			t.Errorf("%s: auto carries no caution: %v", tc.name, v)
		}
	}
}

// The post-admission guard's last action is a field of the local block, omitted when there is nothing
// to say, and honest when an awake layer has nothing re-checking it.
func TestStatusPublishesTheDisplayGuardsLastAction(t *testing.T) {
	swap := coldSwap(t, "agent-pool", "qwen3.8-27b-262k", "gemma-4-26b-agent", "qwen3-vl-8b", "qwen3-vl-32b")

	t.Run("an awake layer with no heartbeat says nothing is watching", func(t *testing.T) {
		g, _ := localOf(t, awakeComposite(t, swap.URL))["display_guard"].(map[string]any)
		if g == nil || g["watching"] != false || !strings.Contains(g["note"].(string), "no heartbeat") {
			t.Fatalf("an awake display layer that nothing re-checks must say so: %v", g)
		}
	})

	t.Run("a dormant layer that never acted has no field", func(t *testing.T) {
		cfg := shippedComposite() // the display layer ships dormant
		cfg.Endpoint = swap.URL
		cfg.StateDir = t.TempDir()
		if _, has := localOf(t, cfg)["display_guard"]; has {
			t.Fatal("omitempty: a dormant layer with no history publishes no display_guard")
		}
	})

	t.Run("the heartbeat and the last action come from the watcher's state file", func(t *testing.T) {
		cfg := awakeComposite(t, swap.URL)
		path, err := displaywatch.StatePath(cfg)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		// The wire shape the watcher writes (displaywatch.State), as another process sees it.
		body := `{"checked_at":"` + now.Add(-4*time.Second).Format(time.RFC3339Nano) + `","interval_sec":10,` +
			`"last_action":{"at":"` + now.Add(-time.Minute).Format(time.RFC3339Nano) + `","layer":"display",` +
			`"models":["gemma-4-e4b-display"],"unloaded":["gemma-4-e4b-display"],` +
			`"reasons":["presence: operator at the desk (idle 12s < threshold 15m0s) — refused"]}}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		g, _ := localOf(t, cfg)["display_guard"].(map[string]any)
		if g == nil || g["watching"] != true || g["interval_sec"] != float64(10) {
			t.Fatalf("a fresh heartbeat is a live watcher: %v", g)
		}
		act, _ := g["last_action"].(map[string]any)
		if act == nil || act["layer"] != "display" {
			t.Fatalf("the last action is published: %v", g)
		}
		if un, _ := act["unloaded"].([]any); len(un) != 1 || un[0] != "gemma-4-e4b-display" {
			t.Fatalf("it names what was unloaded: %v", act)
		}
		if why, _ := act["reasons"].([]any); len(why) != 1 || !strings.Contains(why[0].(string), "desk") {
			t.Fatalf("and why: %v", act)
		}
	})

	t.Run("the state file lives under the machine-wide state root", func(t *testing.T) {
		cfg := awakeComposite(t, swap.URL)
		path, _ := displaywatch.StatePath(cfg)
		if filepath.Dir(path) != cfg.StateDir {
			t.Fatalf("state file %q must sit directly under state_dir %q", path, cfg.StateDir)
		}
	})
}
