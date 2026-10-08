package fleetnode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	// The media-input door extracts a bundle onto this node (ADR 0076); with no bundle it is the
	// inner task alone, which is the shape every hostile `out` below is tried against.
	MediaJobTask: `{"job_id":"mj-out","task_type":"image-gen","payload":{"prompt":"p"}}`,
}

// Tasks that write no caller-addressed file: stt writes its transcript under media_dir
// from the input's own hash, the stt upload door's payload has no path field at all (a strict decode
// refuses one: TestSTTUploadPayloadValidation) and names its private file itself, and the
// agent/accel/vision/text lanes produce JSON only.
var remoteNonWriters = []string{"stt", STTUploadTask, "agent", "accel", VisionTask, TextTask}

func remoteOutCfg() config.Config {
	cfg := fullCfg()
	cfg.AnimateGenScript = "render/comfy-animate.mjs"
	cfg.ComposeScript = "render/compose-hyperframes.mjs"
	cfg.HyperframesDir = "/opt/offload/hyperframes"
	cfg.HyperframesBrowserPath = "/opt/offload/hyperframes/chs"
	// The project-bundle door writes too (it extracts a bundle), so it is open here.
	cfg.FleetComposeProjects = true
	cfg.FleetAuthToken = "tok"
	cfg.ComposeCacheDir = composeProjectTestCache
	// ... and so does the media-input door (ADR 0076).
	cfg.FleetMediaInputs = true
	cfg.MediaDir = composeProjectTestCache
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
					// The project door names its own render under media_dir (composeproj-<hex>.<ext>,
					// media_gate.go) whenever the node has a media_dir, which this config now does for the
					// media-input door: that out is the NODE's, never the caller's, and the loop below
					// still proves it is not the caller's path.
					if s, _ := v.(string); k == "out" && task == ComposeProjectTask && isNodeProjectOut(cfg, s) {
						continue
					}
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

// isNodeProjectOut reports whether out is a file the project door chose itself: directly under this
// node's media_dir, named with the door's own stem.
func isNodeProjectOut(cfg config.Config, out string) bool {
	return out != "" && filepath.Dir(out) == filepath.Clean(cfg.MediaDir) &&
		strings.HasPrefix(filepath.Base(out), projectOutputPrefix)
}
