package pipeline

// Named, license-tagged families (ADR 0058) end to end through Pipeline.Run: the
// request's `family` selects the binding, the runner receives that binding's argv and
// launch env, and the result carries the MEASURED size and the license. The runner is a
// node stub that records its argv/env and writes a real PNG of a size the request did
// not ask for — so a payload that echoes the request is caught.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

type runnerProbe struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
}

// writeProbeRunner writes a node stub that records argv + the launch env to
// $RUNNER_PROBE and copies $PNG_SRC (when set) to its first positional (the out path).
func writeProbeRunner(t *testing.T, dir string) string {
	t.Helper()
	stub := filepath.Join(dir, "probe-runner.mjs")
	if err := os.WriteFile(stub, []byte(`import {writeFileSync, copyFileSync} from "node:fs";
const env = {};
for (const k of ["COMFY_CUDA_DEVICE", "COMFY_DYNAMIC_VRAM", "COMFY_EXTRA_ARGS", "COMFY_DIR"]) {
  if (k in process.env) env[k] = process.env[k];
}
if (process.env.RUNNER_PROBE) writeFileSync(process.env.RUNNER_PROBE, JSON.stringify({ argv: process.argv.slice(2), env }));
// batch/run-graph argv start with a flag (--batch, --graph): no positional out, so
// nothing is written — a probe, not a runner, and never a stray file in the cwd.
const out = process.argv[2];
if (out && !out.startsWith("--")) {
  if (process.env.PNG_SRC) copyFileSync(process.env.PNG_SRC, out);
  else writeFileSync(out, "not-an-image");
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return stub
}

func readProbe(t *testing.T, path string) runnerProbe {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the runner never ran: %v", err)
	}
	var p runnerProbe
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("probe: %v (%s)", err, raw)
	}
	return p
}

func flagVal(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

func familyTestCfg(t *testing.T, dir, stub string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.MediaDir = dir
	cfg.ImageGenScript = stub
	cfg.ImageGenFamily = "krea2"
	cfg.ImageGenCkpt = "krea2_turbo_bf16.safetensors"
	cfg.ImageGenSteps, cfg.ImageGenCFG = 8, 1
	cfg.ImageGenPoolVvramGB, cfg.ImageGenPoolCompute, cfg.ImageGenPoolDonor = 12, "cuda:1", "cuda:2"
	cfg.ComfyCudaDevice = "1"
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{
		"qwen-image-2.1": {
			"license": []byte(`"Qwen Research License"`), "commercial_use": []byte(`false`),
			"imagegen_family":    []byte(`"qwen-image-2.1"`),
			"imagegen_ckpt":      []byte(`"qwen_image_2.1_bf16.safetensors"`),
			"imagegen_clip":      []byte(`"qwen3vl_8b_bf16.safetensors"`),
			"imagegen_vae":       []byte(`"qwen_image_2.1_vae_bf16.safetensors"`),
			"imagegen_steps":     []byte(`40`),
			"imagegen_cfg":       []byte(`1`),
			"imagegen_schedule":  []byte(`"official"`),
			"comfy_dynamic_vram": []byte(`"on"`),
		},
	}
	cfg.GenEditScript = stub
	cfg.GenEditUnet = "qwen-image-edit-2511-Q5_1.gguf"
	cfg.GenEditFamilies = map[string]config.FamilyOverlay{
		"qi21-edit": {
			"license": []byte(`"Qwen Research License"`), "commercial_use": []byte(`false`),
			"gen_edit_family":       []byte(`"qwen-image-2.1"`),
			"gen_edit_unet":         []byte(`"qwen_image_2.1_bf16.safetensors"`),
			"gen_edit_clip":         []byte(`"qwen3vl_8b_bf16.safetensors"`),
			"gen_edit_vae":          []byte(`"qwen_image_2.1_vae_bf16.safetensors"`),
			"gen_edit_cache_device": []byte(`"gpu"`),
			"gen_edit_resolution":   []byte(`992`),
		},
	}
	return cfg
}

func TestGenerateImageFamilyRendersTheOverlayAndTagsTheLicense(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	stub := writeProbeRunner(t, dir)
	probe := filepath.Join(dir, "probe.json")
	src := filepath.Join(dir, "src.png")
	writePNG(t, src, 64, 32)
	t.Setenv("RUNNER_PROBE", probe)
	t.Setenv("PNG_SRC", src)
	led, err := ledger.Open(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Pipeline{cfg: familyTestCfg(t, dir, stub), led: led}
	defer func() { _ = led.Close() }()

	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "a fox sticker",
		Params: map[string]any{"family": "qwen-image-2.1", "transparent": true, "width": 2048, "height": 2048}})
	if !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	got := readProbe(t, probe)
	for flag, want := range map[string]string{
		"--family": "qwen-image-2.1", "--ckpt": "qwen_image_2.1_bf16.safetensors", "--clip": "qwen3vl_8b_bf16.safetensors",
		"--vae": "qwen_image_2.1_vae_bf16.safetensors", "--schedule": "official", "--transparent": "1", "--steps": "40",
	} {
		if v, ok := flagVal(got.Argv, flag); !ok || v != want {
			t.Errorf("%s = %q (present %v), want %q; argv %v", flag, v, ok, want, got.Argv)
		}
	}
	for _, gone := range []string{"--pool-vvram", "--pool-compute", "--pool-donor"} {
		if _, ok := flagVal(got.Argv, gone); ok {
			t.Errorf("the family must not inherit the default's pool: %s in %v", gone, got.Argv)
		}
	}
	if got.Env["COMFY_CUDA_DEVICE"] != "1" || got.Env["COMFY_DYNAMIC_VRAM"] != "on" {
		t.Errorf("launch env = %v, want the inherited pin (single-card family) and the family's dynamic VRAM", got.Env)
	}
	var payload map[string]any
	if err := json.Unmarshal(res.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["width"] != float64(64) || payload["height"] != float64(32) {
		t.Errorf("width/height = %v x %v, want the MEASURED 64 x 32 (the request said 2048)", payload["width"], payload["height"])
	}
	if payload["family"] != "qwen-image-2.1" || payload["license"] != "Qwen Research License" || payload["commercial_use"] != false ||
		!strings.Contains(payload["license_note"].(string), "research/evaluation use only under Qwen Research License") ||
		payload["transparent"] != true {
		t.Errorf("payload = %v", payload)
	}
	if res.Meta.License != "Qwen Research License" || res.Meta.Model != "comfyui:qwen_image_2.1_bf16.safetensors" {
		t.Errorf("meta = %+v", res.Meta)
	}
	_ = led.Close()
	raw, _ := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	var row ledger.Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &row); err != nil || row.License != "Qwen Research License" {
		t.Errorf("ledger row license = %q (%v): %s", row.License, err, raw)
	}
}

func TestGenerateImageDefaultBindingIsUnchangedAndPooledSeatIsNotPinned(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	stub := writeProbeRunner(t, dir)
	probe := filepath.Join(dir, "probe.json")
	t.Setenv("RUNNER_PROBE", probe)
	t.Setenv("PNG_SRC", "")
	for _, fam := range []string{"", "krea2"} {
		p := &Pipeline{cfg: familyTestCfg(t, dir, stub)}
		params := map[string]any{"width": 1568, "height": 880}
		if fam != "" {
			params["family"] = fam
		}
		res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "a bottle", Params: params})
		if !res.OK {
			t.Fatalf("family %q deferred: %s", fam, res.Reason)
		}
		got := readProbe(t, probe)
		if v, _ := flagVal(got.Argv, "--family"); v != "krea2" {
			t.Errorf("family %q: default binding must render krea2, argv %v", fam, got.Argv)
		}
		if v, _ := flagVal(got.Argv, "--pool-vvram"); v != "12" {
			t.Errorf("family %q: the pooled default keeps its pool, argv %v", fam, got.Argv)
		}
		if _, ok := flagVal(got.Argv, "--schedule"); ok {
			t.Errorf("the default binding has no schedule: %v", got.Argv)
		}
		if got.Env["COMFY_CUDA_DEVICE"] != "" {
			t.Errorf("a POOLED seat must not be pinned (the pool keys place it): env %v", got.Env)
		}
		var payload map[string]any
		_ = json.Unmarshal(res.Data, &payload)
		if payload["width"] != float64(1568) || payload["height"] != float64(880) {
			t.Errorf("unreadable output: the payload falls back to the request, got %v", payload)
		}
		if _, has := payload["license"]; has {
			t.Errorf("a default binding that declares no license carries none: %v", payload)
		}
		if payload["family"] != "krea2" {
			t.Errorf("family = %v", payload["family"])
		}
	}
}

func TestGenerateImageRefusesUnknownFamilyAndUnsupportedTransparency(t *testing.T) {
	dir := t.TempDir()
	p := &Pipeline{cfg: familyTestCfg(t, dir, filepath.Join(dir, "never-run.mjs"))}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "x",
		Params: map[string]any{"family": "flux-dev"}})
	if !res.Deferred || !strings.Contains(res.Reason, `unknown image family "flux-dev"`) ||
		!strings.Contains(res.Reason, "krea2 (default), qwen-image-2.1 [non-commercial: Qwen Research License]") {
		t.Fatalf("unknown family: %+v", res)
	}
	res = p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "x",
		Params: map[string]any{"transparent": true}})
	if !res.Deferred || !strings.Contains(res.Reason, "transparent output needs the qwen-image-2.1 family") ||
		!strings.Contains(res.Reason, `"krea2" (the default binding)`) {
		t.Fatalf("transparent on krea2: %+v", res)
	}
}

// TestSdcppQwenImage21FamilyOnlyNodeRendersTransparent is D5's end-to-end regression:
// a family-only node (binxarn's shape — NO default image binding, only a named sdcpp
// family; the same shape defect 2's fleet-gate fix admits) must actually carry
// transparent:true through to the sdcpp runner instead of the old unconditional
// engine-based refusal (families.go's SupportsTransparentImage used to read
// `ImageGenEngine != "sdcpp"` and refuse every sdcpp binding regardless of family).
func TestSdcppQwenImage21FamilyOnlyNodeRendersTransparent(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	stub := writeProbeRunner(t, dir)
	probe := filepath.Join(dir, "probe.json")
	src := filepath.Join(dir, "src.png")
	writePNG(t, src, 16, 16)
	t.Setenv("RUNNER_PROBE", probe)
	t.Setenv("PNG_SRC", src)
	stubJSON, _ := json.Marshal(stub)
	cfg := config.Default()
	cfg.MediaDir = dir
	// No cfg.ImageGenScript / cfg.ImageGenEngine at all — the family is the ONLY
	// image binding this node has.
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{
		"qwen-image-2.1": {
			"license": []byte(`"Qwen Research License"`), "commercial_use": []byte(`false`),
			"imagegen_family": []byte(`"qwen-image-2.1"`),
			"imagegen_engine": []byte(`"sdcpp"`),
			"sdcpp_bin":       []byte(`"sd-cli"`),
			"sdcpp_script":    stubJSON,
			"sdcpp_model":     []byte(`"qwen_image_2.1-Q8_0.gguf"`),
		},
	}
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "a fox sticker",
		Params: map[string]any{"family": "qwen-image-2.1", "transparent": true}})
	if !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	got := readProbe(t, probe)
	if v, ok := flagVal(got.Argv, "--transparent"); !ok || v != "1" {
		t.Errorf("--transparent must reach the sdcpp runner (D5 fix): argv %v", got.Argv)
	}
	var payload map[string]any
	if err := json.Unmarshal(res.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["license"] != "Qwen Research License" || payload["commercial_use"] != false || payload["transparent"] != true {
		t.Errorf("payload = %v", payload)
	}
}

// TestGenerateImageFamilyOnlyNodeNoFamilyStillDefers: a family-only node (no default
// binding — ADR 0058/D1) with no `family` in the request falls through to the SAME
// generic "no image-gen route configured" defer an unconfigured node always gave — it
// must never silently render a named family as if it were the default.
func TestGenerateImageFamilyOnlyNodeNoFamilyStillDefers(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.MediaDir = dir
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{
		"qwen-image-2.1": {
			"license": []byte(`"Qwen Research License"`), "commercial_use": []byte(`false`),
			"imagegen_family": []byte(`"qwen-image-2.1"`),
			"imagegen_engine": []byte(`"sdcpp"`),
			"sdcpp_bin":       []byte(`"sd-cli"`),
			"sdcpp_model":     []byte(`"qwen_image_2.1-Q8_0.gguf"`),
		},
	}
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "x"})
	if !res.Deferred || res.Reason != "no image-gen route configured" {
		t.Fatalf("a family-only node with no family param must defer with the existing message, got %+v", res)
	}
}

func TestEditFamilyWiresReferencesAndRefusesMisuse(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	stub := writeProbeRunner(t, dir)
	probe := filepath.Join(dir, "probe.json")
	src := filepath.Join(dir, "src.png")
	writePNG(t, src, 40, 24)
	t.Setenv("RUNNER_PROBE", probe)
	t.Setenv("PNG_SRC", src)
	target, shirt, hat := filepath.Join(dir, "t.png"), filepath.Join(dir, "shirt.png"), filepath.Join(dir, "hat.png")
	for _, f := range []string{target, shirt, hat} {
		writePNG(t, f, 8, 8)
	}
	p := &Pipeline{cfg: familyTestCfg(t, dir, stub)}

	res := p.Run(context.Background(), core.Request{Task: core.TaskEditImageGenerative, Input: "dress <image1> in <image2> and <image3>",
		Params: map[string]any{"image": target, "images": []any{shirt, hat}, "family": "qi21-edit"}})
	if !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	got := readProbe(t, probe)
	if got.Argv[1] != target {
		t.Errorf("image stays the edit target (positional 2): %v", got.Argv)
	}
	var refs []string
	for i, a := range got.Argv {
		if a == "--ref" {
			refs = append(refs, got.Argv[i+1])
		}
	}
	if strings.Join(refs, "|") != shirt+"|"+hat {
		t.Errorf("--ref order = %v", refs)
	}
	for flag, want := range map[string]string{"--family": "qwen-image-2.1", "--unet": "qwen_image_2.1_bf16.safetensors",
		"--cache-device": "gpu", "--resolution": "992"} {
		if v, ok := flagVal(got.Argv, flag); !ok || v != want {
			t.Errorf("%s = %q, want %q; argv %v", flag, v, want, got.Argv)
		}
	}
	if _, ok := flagVal(got.Argv, "--preset"); ok {
		t.Errorf("the default's lightning8 preset must not ride a 2.1 edit: %v", got.Argv)
	}
	if got.Env["COMFY_CUDA_DEVICE"] != "1" {
		t.Errorf("edit is a single-card route: env %v", got.Env)
	}
	var payload map[string]any
	_ = json.Unmarshal(res.Data, &payload)
	if payload["license_note"] == nil || payload["images"] != float64(3) || payload["width"] != float64(40) || payload["height"] != float64(24) {
		t.Errorf("payload = %v", payload)
	}

	for _, tc := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"image": target, "images": []any{shirt}}, "multi-reference edits need a qwen-image-2.1 edit family"},
		{map[string]any{"image": target, "images": []any{filepath.Join(dir, "gone.png")}, "family": "qi21-edit"}, "reference image not found"},
		{map[string]any{"image": target, "images": []any{shirt, shirt, shirt, shirt, shirt, shirt, shirt, shirt, shirt, shirt}, "family": "qi21-edit"}, "at most 10 images in all"},
		{map[string]any{"image": target, "transparent": true}, "transparent output needs a qwen-image-2.1 edit family"},
		{map[string]any{"image": target, "family": "qi21-edit", "preset": "lightning8"}, "preset is a Qwen-Image-Edit 2511"},
		{map[string]any{"image": target, "family": "nope"}, `unknown edit family "nope"`},
	} {
		res := p.Run(context.Background(), core.Request{Task: core.TaskEditImageGenerative, Input: "edit", Params: tc.params})
		if !res.Deferred || !strings.Contains(res.Reason, tc.want) {
			t.Errorf("%v: want a defer containing %q, got %+v", tc.params, tc.want, res)
		}
	}
}

// Every ComfyUI route gets the launch-wide keys; ONLY the single-card routes get the
// device pin. The table is gpuLeaseCases() — the same list the lease-coverage gate
// keeps complete — so a new GPU route cannot join without an expectation here.
func TestLaunchProfileReachesEveryComfyRouteAndThePinOnlyTheSingleCardOnes(t *testing.T) {
	requireNodePipeline(t)
	pinned := map[string]bool{
		"generate_image (comfy)": true, "edit_image_generative": true, "inpaint_image": true, "upscale_image": true,
		"image batch": true, "animate_character": true, "generate_audio (music)": true,
		// Same route shape as "generate_image (comfy)" — a single-card image
		// generation route (gap 4: comfy-render.mjs, reachable directly via
		// imagegen_script for e.g. the qwen-image-2512 family) — expects the
		// same device pin.
		"generate_image (comfy-render direct)": true,
		// generate_video is un-pooled by default (config.Default() leaves
		// videogen_pool_vvram_gb at 0), so it takes the single-card pin like image
		// generation does when un-pooled — see comfyLaunch's doc comment (2026-09-24
		// A/B: an un-pooled video render landed on the display card without this).
		"generate_video": true,
		// A pooled video seat still must NOT be pinned (MultiGPU #220: the pool
		// computes on ComfyUI's default device, which a --cuda-device pin would hide).
		"generate_video (pooled)": false,
		"generate_image (sdcpp)":  false, "run_graph": false, "generate_audio (voice)": false,
	}
	comfyRoute := map[string]bool{"generate_image (sdcpp)": false, "generate_audio (voice)": false}
	for _, tc := range gpuLeaseCases() {
		want, known := pinned[tc.name]
		if !known {
			t.Errorf("GPU route %q has no launch-profile expectation — decide whether the device pin applies", tc.name)
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			probe := filepath.Join(dir, "probe.json")
			t.Setenv("RUNNER_PROBE", probe)
			t.Setenv("PNG_SRC", "")
			cfg := config.Default()
			cfg.MediaDir = dir
			cfg.ComfyCudaDevice = "2"
			cfg.ComfyDynamicVRAM = "on"
			cfg.ComfyExtraArgs = "--verbose"
			stub := writeProbeRunner(t, dir)
			tc.setup(t, &cfg, stub, dir)
			p := &Pipeline{cfg: cfg}
			tc.invoke(t, p, dir)
			got := readProbe(t, probe)
			if want && got.Env["COMFY_CUDA_DEVICE"] != "2" {
				t.Errorf("single-card route: COMFY_CUDA_DEVICE = %q, want 2 (env %v)", got.Env["COMFY_CUDA_DEVICE"], got.Env)
			}
			if !want && got.Env["COMFY_CUDA_DEVICE"] != "" {
				t.Errorf("this route must never be pinned: COMFY_CUDA_DEVICE = %q", got.Env["COMFY_CUDA_DEVICE"])
			}
			isComfy, listed := comfyRoute[tc.name]
			if !listed {
				isComfy = true
			}
			if isComfy && (got.Env["COMFY_DYNAMIC_VRAM"] != "on" || got.Env["COMFY_EXTRA_ARGS"] != "--verbose") {
				t.Errorf("ComfyUI route missing the launch-wide keys: %v", got.Env)
			}
		})
	}
}

// A batch renders the DEFAULT binding, and its results must carry that binding's
// license exactly as a single render does (ADR 0058: every result is tagged). Before
// the fix only the ledger rows carried it; the items the caller reads did not.
func TestImageBatchItemsCarryTheDefaultBindingsLicense(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ImageGenScript = writeBatchArgvStub(t, dir)
	cfg.MediaDir = dir
	cfg.ImageGenFamily = "krea2"
	cfg.ImageGenLicense = "Research-Only Test License"
	no := false
	cfg.ImageGenCommercialUse = &no
	p := refinerTestPipeline(t, cfg)
	items, err := p.RunImageBatch(context.Background(), []ImageBatchJob{{Prompt: "a red bike"}, {Prompt: "a blue car"}})
	if err != nil {
		t.Fatalf("batch failed: %v", err)
	}
	for i, it := range items {
		if it.Family != "krea2" || it.License != "Research-Only Test License" || it.CommercialUse == nil || *it.CommercialUse ||
			!strings.Contains(it.LicenseNote, "research/evaluation use only under Research-Only Test License") {
			t.Errorf("item %d = %+v, want the default binding's family, license, commercial_use:false and license_note", i, it)
		}
		b, _ := json.Marshal(it)
		for _, k := range []string{`"family":"krea2"`, `"license":"Research-Only Test License"`, `"commercial_use":false`, `"license_note":`} {
			if !strings.Contains(string(b), k) {
				t.Errorf("item %d JSON lacks %s: %s", i, k, b)
			}
		}
	}

	// A default binding that declares no license tags nothing but the family —
	// never an invented commercial_use.
	cfg.ImageGenLicense, cfg.ImageGenCommercialUse = "", nil
	items, err = refinerTestPipeline(t, cfg).RunImageBatch(context.Background(), []ImageBatchJob{{Prompt: "a green boat"}})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(items[0]); strings.Contains(string(b), "license") || strings.Contains(string(b), "commercial_use") {
		t.Errorf("an undeclared license must not be tagged: %s", b)
	}
}
