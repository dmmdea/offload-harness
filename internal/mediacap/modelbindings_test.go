package mediacap

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// ---- F-38: a partial/mid-copy download is INCOMPLETE, not FOUND -----------------
// Reproduced live on the Qube, 2026-09-23: doctor's resolveBinding did a bare
// os.Stat and reported `animate_character: OK CONFIGURED` while a 16.65 GB unet
// was still ~70% written. When the configured name is one the harness's own
// installer pins a known-good size for (knownModelSizes, mirroring setup/
// install.ps1's $PINNED), a size mismatch is now reported as INCOMPLETE.

func touchModelSized(t *testing.T, p string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

func TestModelBindingsFlagAPartialDownloadAsIncomplete(t *testing.T) {
	comfy := t.TempDir()
	name := "sd_xl_base_1.0.safetensors" // a knownModelSizes entry (6938078334 bytes)
	want := knownModelSizes[name]
	if want == 0 {
		t.Fatal("test fixture out of sync with knownModelSizes")
	}
	touchModelSized(t, filepath.Join(comfy, "models", "checkpoints", name), want-1000) // mid-copy
	cfg := config.Config{ComfyDir: comfy, ImageGenCkpt: name}
	bs := ModelBindings(cfg)
	if len(bs) != 1 {
		t.Fatalf("want 1 binding, got %+v", bs)
	}
	b := bs[0]
	if b.State != BindingIncomplete {
		t.Fatalf("want INCOMPLETE for a %d-byte file against a %d-byte pin, got %+v", want-1000, want, b)
	}
	haveWant := fmt.Sprintf("have %d of %d bytes", want-1000, want)
	if !strings.Contains(b.Detail, haveWant) {
		t.Errorf("detail must name the byte counts (%q), got: %s", haveWant, b.Detail)
	}
}

func TestModelBindingsExactPinnedSizeIsFound(t *testing.T) {
	comfy := t.TempDir()
	name := "sd_xl_base_1.0.safetensors"
	want := knownModelSizes[name]
	touchModelSized(t, filepath.Join(comfy, "models", "checkpoints", name), want) // exact size = a real, complete file
	cfg := config.Config{ComfyDir: comfy, ImageGenCkpt: name}
	bs := ModelBindings(cfg)
	if len(bs) != 1 || bs[0].State != BindingFound {
		t.Fatalf("an exact-size match against a known pin must be FOUND, got %+v", bs)
	}
}

func TestModelBindingsUnpinnedNameIsNeverFlaggedIncompleteByGuessing(t *testing.T) {
	comfy := t.TempDir()
	// Any small file: no entry in knownModelSizes for this name, so doctor must
	// never fabricate an expected size — this is the overwhelming common case
	// (every ComfyUI weight sourced ad hoc from a peer box, not this installer).
	touchModel(t, filepath.Join(comfy, "models", "diffusion_models", "wan2.2_i2v_high_noise_14B_Q8_0.gguf"))
	cfg := config.Config{ComfyDir: comfy, VideoGenUnetHigh: "wan2.2_i2v_high_noise_14B_Q8_0.gguf"}
	bs := ModelBindings(cfg)
	if len(bs) != 1 || bs[0].State != BindingFound {
		t.Fatalf("an unpinned name must stay exists-only FOUND, got %+v", bs)
	}
}

// TestKnownModelSizesMatchInstaller: knownModelSizes duplicates data from
// setup/install.ps1's $PINNED models section by hand — this parses that file
// directly (name = '...' / size = NNN pairs) and fails the moment the two
// disagree on any filename both claim to track, so a pin bump in one place can
// never silently rot the other.
func TestKnownModelSizesMatchInstaller(t *testing.T) {
	repoRoot := "../.."
	raw, err := os.ReadFile(filepath.Join(repoRoot, "setup", "install.ps1"))
	if err != nil {
		t.Skipf("setup/install.ps1 not reachable from this test's working dir: %v", err)
	}
	// Each $PINNED entry is its own flat, non-nested `'key' = @{ ... }` block
	// (verified against the file's actual shape: url/sha/size/version, and a
	// downloadable model also carries name — no entry nests another @{}), so
	// bounding each block at the first `}` after its `@{` is exact here. Some
	// blocks (the llama.cpp/llama-swap zips, sdcpp-vulkan) have size but no
	// name — those are simply not model bindings and are skipped below.
	blockRe := regexp.MustCompile(`(?s)@\{(.*?)\n\s*\}`)
	nameRe := regexp.MustCompile(`name\s*=\s*'([^']+)'`)
	sizeRe := regexp.MustCompile(`size\s*=\s*(\d+)`)
	blocks := blockRe.FindAllStringSubmatch(string(raw), -1)
	installerSizes := map[string]int64{}
	for _, blk := range blocks {
		nm := nameRe.FindStringSubmatch(blk[1])
		sz := sizeRe.FindStringSubmatch(blk[1])
		if nm == nil || sz == nil {
			continue
		}
		n, err := strconv.ParseInt(sz[1], 10, 64)
		if err != nil {
			t.Fatalf("unparseable size for %s: %v", nm[1], err)
		}
		installerSizes[nm[1]] = n
	}
	if len(installerSizes) < len(knownModelSizes) {
		t.Fatalf("parsed only %d named $PINNED entries from install.ps1, want at least %d (the parser or the file's shape changed) — got: %v", len(installerSizes), len(knownModelSizes), installerSizes)
	}
	checked := 0
	for name, want := range knownModelSizes {
		got, ok := installerSizes[name]
		if !ok {
			t.Errorf("knownModelSizes[%q]=%d has no matching install.ps1 $PINNED entry — drifted or renamed", name, want)
			continue
		}
		checked++
		if got != want {
			t.Errorf("knownModelSizes[%q]=%d disagrees with install.ps1's pinned size %d", name, want, got)
		}
	}
	if checked != len(knownModelSizes) {
		t.Fatalf("checked %d of %d knownModelSizes entries", checked, len(knownModelSizes))
	}
}

// TestModelBindingsIncompleteInOneRootIsRescuedByACompleteCopyInAnother: a
// multi-root config (extra_model_paths.yaml, ADR 0058 families) can genuinely
// hold a second, correctly-sized copy of the same name in a different root —
// the INCOMPLETE verdict from the first root must not win over a real FOUND
// discovered later. Calls resolveBinding directly (same package) to control the
// root order precisely, rather than going through the filesystem-scanning
// ModelRoots/extra_model_paths.yaml path.
func TestModelBindingsIncompleteInOneRootIsRescuedByACompleteCopyInAnother(t *testing.T) {
	name := "sd_xl_base_1.0.safetensors"
	want := knownModelSizes[name]
	rootA, rootB := t.TempDir(), t.TempDir()
	touchModelSized(t, filepath.Join(rootA, "checkpoints", name), want-1000) // mid-copy
	touchModelSized(t, filepath.Join(rootB, "checkpoints", name), want)      // complete, elsewhere
	roots := []ModelRoot{{Label: "a", Dir: rootA}, {Label: "b", Dir: rootB}}
	b := resolveBinding(roots, "imagegen_ckpt", name, []string{"checkpoints"})
	if b.State != BindingFound {
		t.Fatalf("a complete copy in a later root must win over an earlier INCOMPLETE one, got %+v", b)
	}
}

// TestModelBindingsIncompleteEverywhereStillReportsIncomplete: the converse —
// when EVERY root's copy of a pinned name is the wrong size, INCOMPLETE must
// still be the final verdict (not silently swallowed by the multi-root rescue
// logic above).
func TestModelBindingsIncompleteEverywhereStillReportsIncomplete(t *testing.T) {
	name := "sd_xl_base_1.0.safetensors"
	want := knownModelSizes[name]
	rootA, rootB := t.TempDir(), t.TempDir()
	touchModelSized(t, filepath.Join(rootA, "checkpoints", name), want-1000)
	touchModelSized(t, filepath.Join(rootB, "checkpoints", name), want-2000)
	roots := []ModelRoot{{Label: "a", Dir: rootA}, {Label: "b", Dir: rootB}}
	b := resolveBinding(roots, "imagegen_ckpt", name, []string{"checkpoints"})
	if b.State != BindingIncomplete {
		t.Fatalf("every root wrong-sized must still report INCOMPLETE, got %+v", b)
	}
}
