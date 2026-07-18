package fleetnode

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// fullCfg returns a config with every fleet-served route bound.
func fullCfg() config.Config {
	return config.Config{
		ImageGenScript: "render/comfy-generate.mjs",
		VideoGenScript: "render/comfy-video.mjs",
		STTModel:       "large-v3-turbo",
		VoiceGenScript: "render/tts.mjs",
		MusicGenScript: "render/comfy-music.mjs",
		RunGraphScript: "render/comfy-run-graph.mjs",
	}
}

// --- SupportedTasks / Families derivation (advertised = actually configured) ---

func TestSupportedTasksDerivation(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{"nothing configured", config.Config{}, nil},
		{"all configured", fullCfg(), []string{"image-gen", "video-gen", "stt", "audio-gen", "run-graph"}},
		{"image only", config.Config{ImageGenScript: "x.mjs"}, []string{"image-gen"}},
		{"video only", config.Config{VideoGenScript: "x.mjs"}, []string{"video-gen"}},
		{"stt only", config.Config{STTModel: "large-v3-turbo"}, []string{"stt"}},
		{"audio via voice", config.Config{VoiceGenScript: "tts.mjs"}, []string{"audio-gen"}},
		{"audio via music", config.Config{MusicGenScript: "music.mjs"}, []string{"audio-gen"}},
		{"run-graph only", config.Config{RunGraphScript: "rg.mjs"}, []string{"run-graph"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SupportedTasks(c.cfg); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("SupportedTasks = %v, want %v", got, c.want)
			}
		})
	}
}

func TestFamiliesDerivation(t *testing.T) {
	t.Run("all configured, default image family", func(t *testing.T) {
		want := []string{"sdxl", "wan2.2", "whisper", "acestep", "comfy-graph"}
		if got := Families(fullCfg()); !reflect.DeepEqual(got, want) {
			t.Fatalf("Families = %v, want %v", got, want)
		}
	})
	t.Run("imagegen_family override", func(t *testing.T) {
		cfg := config.Config{ImageGenScript: "x.mjs", ImageGenFamily: "hidream-o1"}
		if got := Families(cfg); !reflect.DeepEqual(got, []string{"hidream-o1"}) {
			t.Fatalf("Families = %v", got)
		}
	})
	t.Run("deduplicated", func(t *testing.T) {
		// A (contrived) machine whose image binding is named like the video family
		// must not advertise the family twice.
		cfg := config.Config{ImageGenScript: "x.mjs", ImageGenFamily: "wan2.2", VideoGenScript: "v.mjs"}
		if got := Families(cfg); !reflect.DeepEqual(got, []string{"wan2.2"}) {
			t.Fatalf("Families = %v, want deduped [wan2.2]", got)
		}
	})
	t.Run("nothing configured", func(t *testing.T) {
		if got := Families(config.Config{}); got != nil {
			t.Fatalf("Families = %v, want nil", got)
		}
	})
}

// --- BuildRequest: per-task translation (mirrors the MCP handlers) ---

func mustBuild(t *testing.T, cfg config.Config, task string, payload string) (core.Request, func()) {
	t.Helper()
	req, cleanup, err := BuildRequest(cfg, task, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("BuildRequest(%s): %v", task, err)
	}
	return req, cleanup
}

func TestBuildRequestImageGen(t *testing.T) {
	t.Run("full payload", func(t *testing.T) {
		req, cleanup := mustBuild(t, fullCfg(), "image-gen",
			`{"prompt":"a red fox","negative":"people","out":"C:/x.png","width":1024,"height":768,"steps":30,"seed":7}`)
		defer cleanup()
		if req.Task != core.TaskGenerateImage || req.Input != "a red fox" {
			t.Fatalf("req = %+v", req)
		}
		want := map[string]any{"negative": "people", "out": "C:/x.png", "width": 1024, "height": 768, "steps": 30, "seed": 7}
		if !reflect.DeepEqual(req.Params, want) {
			t.Fatalf("params = %#v, want %#v", req.Params, want)
		}
	})
	t.Run("minimal payload omits zero params", func(t *testing.T) {
		req, cleanup := mustBuild(t, fullCfg(), "image-gen", `{"prompt":"p"}`)
		defer cleanup()
		if len(req.Params) != 0 {
			t.Fatalf("zero/empty optionals must be omitted, got %#v", req.Params)
		}
	})
	t.Run("prompt required", func(t *testing.T) {
		_, _, err := BuildRequest(fullCfg(), "image-gen", json.RawMessage(`{"width":512}`))
		if err == nil || !strings.Contains(err.Error(), "prompt") {
			t.Fatalf("err = %v, want prompt-required", err)
		}
	})
}

func TestBuildRequestVideoGen(t *testing.T) {
	t.Run("full payload", func(t *testing.T) {
		req, cleanup := mustBuild(t, fullCfg(), "video-gen",
			`{"prompt":"waves","still":"C:/s.png","model":"wan","negative":"blurry","out":"C:/v.mp4",
			  "frames":33,"width":960,"height":544,"steps":4,"seed":9,"reserve_vram":2.5,
			  "fast":true,"hero":true,"upscale":true}`)
		defer cleanup()
		if req.Task != core.TaskGenerateVideo || req.Input != "waves" {
			t.Fatalf("req = %+v", req)
		}
		want := map[string]any{
			"still": "C:/s.png", "model": "wan", "negative": "blurry", "out": "C:/v.mp4",
			"frames": 33, "width": 960, "height": 544, "steps": 4, "seed": 9,
			"reserve_vram": "2.5", // stringified, matching the MCP wire shape
			"fast": true, "hero": true, "upscale": true,
		}
		if !reflect.DeepEqual(req.Params, want) {
			t.Fatalf("params = %#v, want %#v", req.Params, want)
		}
	})
	t.Run("prompt required", func(t *testing.T) {
		_, _, err := BuildRequest(fullCfg(), "video-gen", json.RawMessage(`{"still":"s.png"}`))
		if err == nil || !strings.Contains(err.Error(), "prompt") {
			t.Fatalf("err = %v, want prompt-required", err)
		}
	})
}

func TestBuildRequestSTT(t *testing.T) {
	t.Run("full payload", func(t *testing.T) {
		req, cleanup := mustBuild(t, fullCfg(), "stt", `{"audio":"C:/a.wav","language":"es","hq":true}`)
		defer cleanup()
		if req.Task != core.TaskTranscribe || req.Audio != "C:/a.wav" {
			t.Fatalf("req = %+v", req)
		}
		want := map[string]any{"language": "es", "hq": true}
		if !reflect.DeepEqual(req.Params, want) {
			t.Fatalf("params = %#v, want %#v", req.Params, want)
		}
	})
	t.Run("minimal", func(t *testing.T) {
		req, cleanup := mustBuild(t, fullCfg(), "stt", `{"audio":"a.wav"}`)
		defer cleanup()
		if len(req.Params) != 0 {
			t.Fatalf("params = %#v, want empty", req.Params)
		}
	})
	t.Run("audio required", func(t *testing.T) {
		_, _, err := BuildRequest(fullCfg(), "stt", json.RawMessage(`{"language":"es"}`))
		if err == nil || !strings.Contains(err.Error(), "audio") {
			t.Fatalf("err = %v, want audio-required", err)
		}
	})
}

func TestBuildRequestAudioGen(t *testing.T) {
	t.Run("full payload", func(t *testing.T) {
		req, cleanup := mustBuild(t, fullCfg(), "audio-gen",
			`{"text":"hola","kind":"music","voice":"finetuned","clone":"C:/ref.wav","lang":"es",
			  "seconds":20,"out":"C:/o.wav","seed":3,"reserve_vram":1.5}`)
		defer cleanup()
		if req.Task != core.TaskGenerateAudio || req.Input != "hola" {
			t.Fatalf("req = %+v", req)
		}
		want := map[string]any{
			"kind": "music", "voice": "finetuned", "clone": "C:/ref.wav", "lang": "es",
			"seconds": 20, "out": "C:/o.wav", "seed": 3, "reserve_vram": "1.5",
		}
		if !reflect.DeepEqual(req.Params, want) {
			t.Fatalf("params = %#v, want %#v", req.Params, want)
		}
	})
	t.Run("minimal — pipeline defaults kind", func(t *testing.T) {
		req, cleanup := mustBuild(t, fullCfg(), "audio-gen", `{"text":"hola"}`)
		defer cleanup()
		if len(req.Params) != 0 {
			t.Fatalf("params = %#v, want empty (mirrors the MCP handler)", req.Params)
		}
	})
	t.Run("text required", func(t *testing.T) {
		_, _, err := BuildRequest(fullCfg(), "audio-gen", json.RawMessage(`{"kind":"voice"}`))
		if err == nil || !strings.Contains(err.Error(), "text") {
			t.Fatalf("err = %v, want text-required", err)
		}
	})
}

// --- run-graph: strict validation + raw-JSON materialization ---

func TestBuildRequestRunGraphValid(t *testing.T) {
	graph := `{"1":{"class_type":"KSampler","inputs":{}}}`
	manifest := `{"nodes":["KSampler"]}`
	req, cleanup := mustBuild(t, fullCfg(), "run-graph",
		`{"graph":`+graph+`,"manifest":`+manifest+`,"out_dir":"C:/outs","reserve_vram":"2.0"}`)
	defer cleanup()

	if req.Task != core.TaskRunGraph {
		t.Fatalf("task = %v", req.Task)
	}
	gp, _ := req.Params["graph_path"].(string)
	mp, _ := req.Params["manifest_path"].(string)
	if gp == "" || mp == "" {
		t.Fatalf("graph/manifest not materialized: %#v", req.Params)
	}
	if b, err := os.ReadFile(gp); err != nil || string(b) != graph {
		t.Fatalf("materialized graph = %q (%v), want the raw payload JSON", b, err)
	}
	if b, err := os.ReadFile(mp); err != nil || string(b) != manifest {
		t.Fatalf("materialized manifest = %q (%v)", b, err)
	}
	if req.Params["out_dir"] != "C:/outs" || req.Params["reserve_vram"] != "2.0" {
		t.Fatalf("params = %#v", req.Params)
	}

	cleanup()
	if _, err := os.Stat(gp); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the graph temp file (stat err = %v)", err)
	}
	if _, err := os.Stat(mp); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the manifest temp file (stat err = %v)", err)
	}
}

// TestBuildRequestRunGraphModelFamilyPassthrough: a payload-declared
// model_family reaches the pipeline params (the passive footprint recorder
// keys the render by it); absent = no key (the pipeline defaults
// "comfy-graph").
func TestBuildRequestRunGraphModelFamilyPassthrough(t *testing.T) {
	req, cleanup := mustBuild(t, fullCfg(), "run-graph",
		`{"graph":{"1":{"class_type":"X"}},"model_family":"flux-dev"}`)
	defer cleanup()
	if got, _ := req.Params["model_family"].(string); got != "flux-dev" {
		t.Fatalf("model_family = %q, want \"flux-dev\"", got)
	}
}

func TestBuildRequestRunGraphManifestOptional(t *testing.T) {
	req, cleanup := mustBuild(t, fullCfg(), "run-graph", `{"graph":{"1":{"class_type":"X"}}}`)
	defer cleanup()
	if mp, _ := req.Params["manifest_path"].(string); mp != "" {
		t.Fatalf("absent manifest must map to empty manifest_path, got %q", mp)
	}
	if req.Params["out_dir"] != "" || req.Params["reserve_vram"] != "" {
		t.Fatalf("params = %#v", req.Params)
	}
}

func TestBuildRequestRunGraphValidationMatrix(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantSub string
	}{
		{"graph missing", `{"manifest":{}}`, "graph"},
		{"graph null", `{"graph":null}`, "graph"},
		{"graph is array", `{"graph":[1,2]}`, "object"},
		{"graph is string", `{"graph":"not-an-object"}`, "object"},
		{"graph is number", `{"graph":42}`, "object"},
		{"graph empty object", `{"graph":{}}`, "non-empty"},
		{"manifest is array", `{"graph":{"1":{}},"manifest":[1]}`, "manifest"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, cleanup, err := BuildRequest(fullCfg(), "run-graph", json.RawMessage(c.payload))
			if cleanup != nil {
				cleanup()
			}
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("err = %v, want mention of %q", err, c.wantSub)
			}
		})
	}
}

// --- error taxonomy: unknown / unconfigured / malformed ---

func TestBuildRequestUnknownTaskListsSupported(t *testing.T) {
	_, _, err := BuildRequest(fullCfg(), "mine-bitcoin", nil)
	if err == nil {
		t.Fatal("unknown task_type must error")
	}
	for _, want := range []string{"unsupported task_type", "mine-bitcoin", "image-gen", "video-gen", "stt", "audio-gen", "run-graph"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q must contain %q (the supported set)", err.Error(), want)
		}
	}
}

func TestBuildRequestUnconfiguredTaskUnsupported(t *testing.T) {
	// image-gen is a KNOWN type, but this box has no image route — the dispatcher
	// only dispatches advertised tasks, so this is the same 400 as unknown.
	cfg := config.Config{STTModel: "large-v3-turbo"}
	_, _, err := BuildRequest(cfg, "image-gen", json.RawMessage(`{"prompt":"p"}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported task_type") {
		t.Fatalf("err = %v, want unsupported", err)
	}
	if !strings.Contains(err.Error(), "stt") {
		t.Fatalf("err %q must list this box's supported set", err.Error())
	}
}

func TestBuildRequestMalformedPayload(t *testing.T) {
	for _, task := range []string{"image-gen", "video-gen", "stt", "audio-gen", "run-graph"} {
		if _, _, err := BuildRequest(fullCfg(), task, json.RawMessage(`{not json`)); err == nil {
			t.Fatalf("%s: malformed payload must error", task)
		}
	}
}

func TestBuildRequestNilPayloadHitsRequiredFields(t *testing.T) {
	// nil payload = no fields: every task's required-field check fires (not a JSON
	// parse crash).
	for task, want := range map[string]string{
		"image-gen": "prompt", "video-gen": "prompt", "stt": "audio",
		"audio-gen": "text", "run-graph": "graph",
	} {
		_, _, err := BuildRequest(fullCfg(), task, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s with nil payload: err = %v, want %q mentioned", task, err, want)
		}
	}
}

// TestBuildRequestCleanupNeverNil: callers defer cleanup unconditionally.
func TestBuildRequestCleanupNeverNil(t *testing.T) {
	_, cleanup, err := BuildRequest(fullCfg(), "image-gen", json.RawMessage(`{"prompt":"p"}`))
	if err != nil || cleanup == nil {
		t.Fatalf("cleanup must be non-nil on success (err=%v)", err)
	}
	cleanup()
}
