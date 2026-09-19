package mediacap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func touchModel(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// F-31: a configured model name resolves against the class directories the
// render scripts load it from — FOUND where the graph looks, MISPLACED when it
// sits only under a class the graph never opens (the krea2 disk-swap shape),
// MISSING when no class directory holds it. "builtin" names no file.
func TestModelBindingsResolveAgainstTheClassDirectories(t *testing.T) {
	comfy := t.TempDir()
	touchModel(t, filepath.Join(comfy, "models", "checkpoints", "RealVisXL_V5.0_fp16.safetensors"))
	touchModel(t, filepath.Join(comfy, "models", "diffusion_models", "wan", "wan22_high.gguf"))
	touchModel(t, filepath.Join(comfy, "models", "loras", "krea2_turbo.safetensors")) // a DiT dropped into loras/
	cfg := config.Config{
		ComfyDir:         comfy,
		InpaintCkpt:      "RealVisXL_V5.0_fp16.safetensors",
		InpaintVAE:       "builtin",
		VideoGenUnetHigh: "wan/wan22_high.gguf",
		ImageGenCkpt:     "krea2_turbo.safetensors",
		UpscaleModel:     "4x-UltraSharp.pth",
	}
	got := map[string]Binding{}
	for _, b := range ModelBindings(cfg) {
		got[b.Key] = b
	}
	if _, skipped := got["inpaint_vae"]; skipped {
		t.Errorf("builtin must not be checked: %+v", got["inpaint_vae"])
	}
	if b := got["inpaint_ckpt"]; b.State != BindingFound || b.FoundIn[0] != "checkpoints" {
		t.Errorf("inpaint_ckpt: %+v", b)
	}
	if b := got["videogen_unet_high"]; b.State != BindingFound || b.FoundIn[0] != "diffusion_models" {
		t.Errorf("subfolder path must resolve as the loader opens it: %+v", b)
	}
	if b := got["imagegen_ckpt"]; b.State != BindingMisplaced || b.FoundIn[0] != "loras" {
		t.Errorf("a DiT under loras/ is MISPLACED, not found: %+v", b)
	}
	if b := got["upscale_model"]; b.State != BindingMissing {
		t.Errorf("upscale_model: %+v", b)
	}
	if len(got) != 4 {
		t.Errorf("want 4 bindings reported, got %d: %v", len(got), got)
	}
}

// The Qube keeps its weights on another volume through extra_model_paths.yaml:
// base_path plus a class map whose values may be a newline-separated list of
// subdirectories (ComfyUI's own syntax for aliases such as diffusion_models |
// unet). A file under such a root must resolve exactly as ComfyUI resolves it,
// and a name that exists only in the alias directory still counts for the class.
func TestModelBindingsHonourExtraModelPaths(t *testing.T) {
	comfy := t.TempDir()
	vol := t.TempDir()
	touchModel(t, filepath.Join(vol, "unet", "krea2_turbo_bf16.safetensors"))
	touchModel(t, filepath.Join(vol, "vae", "qwen_image_vae.safetensors"))
	touchModel(t, filepath.Join(vol, "clip", "gemma-proj.safetensors")) // text encoder in the legacy alias dir
	yamlText := "qube_optane:\n    base_path: " + filepath.ToSlash(vol) + "/\n    checkpoints: checkpoints\n    diffusion_models: |\n        diffusion_models\n        unet\n    unet: |\n        diffusion_models\n        unet\n    clip: clip\n    text_encoders: text_encoders\n    vae: vae\n"
	if err := os.WriteFile(filepath.Join(comfy, "extra_model_paths.yaml"), []byte(yamlText), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ComfyDir:            comfy, // no models/ under it: only the extra root exists
		ImageGenCkpt:        "krea2_turbo_bf16.safetensors",
		ImageGenVAE:         "qwen_image_vae.safetensors",
		VideoGenTextEncoder: "gemma-proj.safetensors",
		UpscaleModel:        "4x-UltraSharp.pth",
	}
	got := map[string]Binding{}
	for _, b := range ModelBindings(cfg) {
		got[b.Key] = b
	}
	if b := got["imagegen_ckpt"]; b.State != BindingFound {
		t.Errorf("a DiT under the unet alias of the extra root must be FOUND: %+v", b)
	}
	if b := got["imagegen_vae"]; b.State != BindingFound || b.FoundIn[0] != "vae" {
		t.Errorf("imagegen_vae: %+v", b)
	}
	if b := got["videogen_text_encoder"]; b.State != BindingFound {
		t.Errorf("a text encoder in the legacy clip/ alias is what CLIPLoader opens: %+v", b)
	}
	if b := got["upscale_model"]; b.State != BindingMissing {
		t.Errorf("upscale_model: %+v", b)
	}
	roots := ModelRoots(comfy)
	if len(roots) != 1 || roots[0].Label != "qube_optane" || len(roots[0].Classes["diffusion_models"]) != 2 {
		t.Fatalf("roots = %+v", roots)
	}
}

// A basename that moved one level (a file dropped in the class root when the
// config names a subfolder, or the reverse) still counts as present in that
// class: MISSING would be a lie and the operator sees which class holds it.
func TestModelBindingsFindAMovedFileByBasename(t *testing.T) {
	comfy := t.TempDir()
	touchModel(t, filepath.Join(comfy, "models", "vae", "nested", "ae.safetensors"))
	cfg := config.Config{ComfyDir: comfy, ImageGenVAE: "ae.safetensors"}
	bs := ModelBindings(cfg)
	if len(bs) != 1 || bs[0].State != BindingFound || bs[0].FoundIn[0] != "vae" {
		t.Fatalf("got %+v", bs)
	}
}

// A box without ComfyUI (no comfy_dir, or neither models/ nor an extra-paths
// file under it) has nothing to bind: nil, so doctor prints no section there.
func TestModelBindingsAreNilWithoutAModelsRoot(t *testing.T) {
	if bs := ModelBindings(config.Config{ImageGenCkpt: "x.safetensors"}); bs != nil {
		t.Fatalf("no comfy_dir: want nil, got %+v", bs)
	}
	if bs := ModelBindings(config.Config{ComfyDir: t.TempDir(), ImageGenCkpt: "x.safetensors"}); bs != nil {
		t.Fatalf("comfy_dir without models/: want nil, got %+v", bs)
	}
}
