package fleetnode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// remoteMediaPayloads is one minimal VALID payload per fleet task that writes a media
// file. Every task that produces a file on this node must be listed here: the coverage
// check below fails when fleetTaskOrder grows a writer this table does not know.
var remoteMediaPayloads = map[string]string{
	"image-gen": `{"prompt":"p"}`,
	"video-gen": `{"prompt":"p"}`,
	"animate":   `{"prompt":"p","ref":"r.png","driver":"d.mp4"}`,
	"audio-gen": `{"text":"hola"}`,
	"run-graph": `{"graph":{"1":{"class_type":"X"}}}`,
	ComposeTask: `{"template":"title-card"}`,
}

// Tasks that write no caller-addressed file: stt writes its transcript under media_dir
// from the input's own hash, and the agent/accel/vision lanes produce JSON only.
var remoteNonWriters = []string{"stt", "agent", "accel", VisionTask}

func remoteOutCfg() config.Config {
	cfg := fullCfg()
	cfg.AnimateGenScript = "render/comfy-animate.mjs"
	cfg.ComposeScript = "render/compose-hyperframes.mjs"
	cfg.HyperframesDir = "/opt/offload/hyperframes"
	cfg.HyperframesBrowserPath = "/opt/offload/hyperframes/chs"
	return cfg
}

// TestRemoteMediaTaskOutNeverReachesThePipeline: media dispatch is not token-gated
// (ADR 0023), so a remote caller's `out` / `out_dir` must never reach the pipeline —
// the pipeline then derives the path under media_dir itself. Every hostile shape is
// tried against every writer: an absolute path outside media_dir, `..` traversal, a UNC
// share, a drive-relative path, an EXISTING file, and even a harmless-looking bare name.
func TestRemoteMediaTaskOutNeverReachesThePipeline(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("do not overwrite"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostile := []string{
		"C:/Windows/System32/drivers/etc/hosts", "/etc/passwd", "../../escape.png",
		`\\server\share\x.png`, "D:x.png", victim, "evil.png",
	}
	cfg := remoteOutCfg()
	for task, base := range remoteMediaPayloads {
		for _, target := range hostile {
			var payload map[string]any
			if err := json.Unmarshal([]byte(base), &payload); err != nil {
				t.Fatalf("%s: bad fixture: %v", task, err)
			}
			payload["out"] = target
			payload["out_dir"] = target
			raw, _ := json.Marshal(payload)
			req, cleanup, err := BuildRequest(context.Background(), cfg, true, task, raw)
			if err != nil {
				t.Fatalf("%s out=%q: a stray out must be ignored, not refused (current callers keep working): %v", task, target, err)
			}
			cleanup()
			for _, k := range []string{"out", "out_dir"} {
				if v, ok := req.Params[k]; ok {
					t.Errorf("%s: remote %s=%v reached the pipeline", task, k, v)
				}
			}
			for k, v := range req.Params {
				if s, _ := v.(string); s == target {
					t.Errorf("%s: the caller's path %q reached the pipeline as %q", task, target, k)
				}
			}
		}
	}
	if b, _ := os.ReadFile(victim); string(b) != "do not overwrite" {
		t.Fatal("the existing file was touched")
	}
}

// TestEveryFleetWriterIsCoveredByTheOutRule: a new fleet task that writes a file must
// join remoteMediaPayloads (and drop its caller's out) before it can ship.
func TestEveryFleetWriterIsCoveredByTheOutRule(t *testing.T) {
	for _, task := range fleetTaskOrder {
		if slices.Contains(remoteNonWriters, task) {
			continue
		}
		if _, ok := remoteMediaPayloads[task]; !ok {
			t.Errorf("fleet task %q is not in remoteMediaPayloads: add it and prove its builder drops a caller's out", task)
		}
	}
}
