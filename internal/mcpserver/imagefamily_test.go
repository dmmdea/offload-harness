package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// familyCfg is a box whose default image binding is krea2 and which serves one
// named, non-commercial qwen-image-2.1 image family and edit family (ADR 0058).
func familyCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Endpoint = "http://127.0.0.1:1"
	cfg.MediaDir = t.TempDir()
	cfg.ImageGenScript = "render/comfy-generate.mjs"
	cfg.ImageGenFamily = "krea2"
	cfg.ImageGenCkpt = "krea2_turbo_bf16.safetensors"
	cfg.GenEditScript = "render/comfy-edit.mjs"
	cfg.GenEditUnet = "qwen-image-edit-2511-Q5_1.gguf"
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{"qwen-image-2.1": {
		"license": json.RawMessage(`"Qwen Research License"`), "commercial_use": json.RawMessage(`false`),
		"imagegen_family": json.RawMessage(`"qwen-image-2.1"`),
		"imagegen_ckpt":   json.RawMessage(`"qwen_image_2.1_bf16.safetensors"`),
		"imagegen_clip":   json.RawMessage(`"qwen3vl_8b_bf16.safetensors"`),
		"imagegen_vae":    json.RawMessage(`"qwen_image_2.1_vae_bf16.safetensors"`),
	}}
	cfg.GenEditFamilies = map[string]config.FamilyOverlay{"qwen-image-2.1": {
		"license": json.RawMessage(`"Qwen Research License"`), "commercial_use": json.RawMessage(`false`),
		"gen_edit_family": json.RawMessage(`"qwen-image-2.1"`),
		"gen_edit_unet":   json.RawMessage(`"qwen_image_2.1_bf16.safetensors"`),
		"gen_edit_clip":   json.RawMessage(`"qwen3vl_8b_bf16.safetensors"`),
		"gen_edit_vae":    json.RawMessage(`"qwen_image_2.1_vae_bf16.safetensors"`),
	}}
	return cfg
}

// The two image tools advertise the family surface with the schema the pipeline
// consumes, say that non-commercial families come back license-tagged, and the edit
// tool no longer claims a ~1MP snap its 2511 graph stopped doing.
func TestImageFamilySurfaceIsAdvertised(t *testing.T) {
	tools := map[string]string{}
	descs := map[string]string{}
	for _, tool := range listTools(t, config.Default()) {
		b, _ := json.Marshal(tool.InputSchema)
		tools[tool.Name], descs[tool.Name] = string(b), tool.Description
	}
	gen, edit := tools["offload_generate_image"], tools["offload_edit_image_generative"]
	for _, want := range []string{`"family":{"description"`, `"transparent":{"description"`, `"required":["prompt"]`} {
		if !strings.Contains(gen, want) {
			t.Errorf("offload_generate_image schema lacks %s: %s", want, gen)
		}
	}
	for _, want := range []string{`"family":{"description"`, `"images":{`, `"maxItems":9`, `"items":{"type":"string"}`, `"transparent":{"description"`, `"required":["image","prompt"]`} {
		if !strings.Contains(edit, want) {
			t.Errorf("offload_edit_image_generative schema lacks %s: %s", want, edit)
		}
	}
	for name, wants := range map[string][]string{
		"offload_generate_image":        {"media.image_families", "license-tagged", "license_note", "MEASURED", "transparent"},
		"offload_edit_image_generative": {"media.edit_families", "license-tagged", "<image1>", "10 images in all", "MEASURED"},
	} {
		for _, w := range wants {
			if !strings.Contains(descs[name], w) {
				t.Errorf("%s description lacks %q", name, w)
			}
		}
	}
	if strings.Contains(descs["offload_edit_image_generative"], "~1MP") {
		t.Error("offload_edit_image_generative still claims a ~1MP snap; the 2511 canvas follows the source within 0.9-2.0 MP")
	}
}

// family / transparent / images reach the pipeline: each is observable as the
// pipeline's own refusal, which runs before any lease or render.
func TestImageFamilyParamsReachThePipeline(t *testing.T) {
	cfg := familyCfg(t)
	s := New(pipeline.New(cfg, nil, nil, nil))
	ctx := context.Background()

	res, _ := s.handleGenerateImage(ctx, callReq(`{"prompt":"p","family":"nope"}`))
	m := decodeResult(t, res)
	if m["deferred"] != true || !strings.Contains(m["reason"].(string), `unknown image family "nope"`) ||
		!strings.Contains(m["reason"].(string), "qwen-image-2.1 [non-commercial: Qwen Research License]") {
		t.Errorf("family must reach the pipeline's resolver: %v", m)
	}
	res, _ = s.handleGenerateImage(ctx, callReq(`{"prompt":"p","transparent":true}`))
	if m = decodeResult(t, res); m["deferred"] != true || !strings.Contains(m["reason"].(string), "transparent output needs") {
		t.Errorf("transparent on the krea2 default must reach the pipeline and defer: %v", m)
	}

	src := filepath.Join(t.TempDir(), "src.png")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ = s.handleEditImageGenerative(ctx, callReq(`{"image":"`+filepath.ToSlash(src)+`","prompt":"p","images":["`+filepath.ToSlash(src)+`"]}`))
	if m = decodeResult(t, res); m["deferred"] != true || !strings.Contains(m["reason"].(string), "multi-reference edits need a qwen-image-2.1 edit family") {
		t.Errorf("images on the single-image default must reach the pipeline and defer: %v", m)
	}
	res, _ = s.handleEditImageGenerative(ctx, callReq(`{"image":"`+filepath.ToSlash(src)+`","prompt":"p","family":"nope"}`))
	if m = decodeResult(t, res); m["deferred"] != true || !strings.Contains(m["reason"].(string), `unknown edit family "nope"`) {
		t.Errorf("edit family must reach the pipeline's resolver: %v", m)
	}
	eleven := make([]string, 10)
	for i := range eleven {
		eleven[i] = filepath.ToSlash(src)
	}
	raw, _ := json.Marshal(map[string]any{"image": filepath.ToSlash(src), "prompt": "p", "family": "qwen-image-2.1", "images": eleven})
	res, _ = s.handleEditImageGenerative(ctx, callReq(string(raw)))
	if m = decodeResult(t, res); m["deferred"] != true || !strings.Contains(m["reason"].(string), "at most 10 images") {
		t.Errorf("11 images must defer at the pipeline's limit: %v", m)
	}
}

// offload_status publishes every binding a `family` param can select — the default
// first — with its license and its route verdict. A default that declares no license
// reads null (UNKNOWN), never an invented "commercial" value.
func TestStatusListsImageAndEditFamilies(t *testing.T) {
	s := New(pipeline.New(familyCfg(t), nil, nil, nil))
	res, err := s.handleStatus(context.Background(), callReq(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	media, _ := decodeResult(t, res)["media"].(map[string]any)
	for key, wantCkpt := range map[string][2]string{
		"image_families": {"krea2_turbo_bf16.safetensors", "qwen_image_2.1_bf16.safetensors"},
		"edit_families":  {"qwen-image-edit-2511-Q5_1.gguf", "qwen_image_2.1_bf16.safetensors"},
	} {
		rows, _ := media[key].([]any)
		if len(rows) != 2 {
			t.Fatalf("media.%s = %v, want the default and one named family", key, media[key])
		}
		def, named := rows[0].(map[string]any), rows[1].(map[string]any)
		if def["default"] != true || def["ckpt"] != wantCkpt[0] || def["license"] != nil || def["commercial_use"] != nil {
			t.Errorf("media.%s default row = %v", key, def)
		}
		if named["name"] != "qwen-image-2.1" || named["family"] != "qwen-image-2.1" || named["engine"] != "comfyui" ||
			named["ckpt"] != wantCkpt[1] || named["license"] != "Qwen Research License" || named["commercial_use"] != false ||
			named["default"] != false {
			t.Errorf("media.%s named row = %v", key, named)
		}
		for _, r := range []map[string]any{def, named} {
			if st, _ := r["state"].(string); st == "" {
				t.Errorf("media.%s row carries no route verdict: %v", key, r)
			}
		}
	}
	routes, _ := media["routes"].(map[string]any)
	if _, ok := routes["generate_image:qwen-image-2.1"]; !ok {
		t.Errorf("media.routes lacks the family route: %v", routes)
	}
}
