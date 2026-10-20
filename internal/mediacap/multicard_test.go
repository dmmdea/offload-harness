package mediacap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The Go copy of the card-only recipes (multicard.go) is render/multicard-recipes.json, which the runner reads. A change to either side fails here.

type recipeFile struct {
	Rows []struct {
		ID, Route, Family, Quant, UnetMatch, ClipMatch, DefaultClip, Basis, LoraLoader, Blocked string
		Unet                                                                                    struct {
			Loader     string
			ResidentGb float64
		}
		Clip struct {
			Loader     string
			ResidentGb float64
		}
		PeakGiB        struct{ Compute, Storage float64 }
		ActivationsGiB float64
		MaxMegapixels  float64
		MaxPixels      int64
		Packs          []string
	}
}

func readRecipes(t *testing.T) recipeFile {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "render", "multicard-recipes.json"))
	if err != nil {
		t.Skipf("the render folder is not beside this package: %v", err)
	}
	var file recipeFile
	if err := json.Unmarshal(src, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestTheMulticardRowsAreTheRunnersRecipes(t *testing.T) {
	file := readRecipes(t)
	rows := MulticardRows()
	if len(rows) != len(file.Rows) || len(rows) != 5 {
		t.Fatalf("%d rows here, %d in render/multicard-recipes.json", len(rows), len(file.Rows))
	}
	measured, blocked := 0, 0
	for i, jr := range file.Rows {
		r := rows[i]
		if r.ID != jr.ID || r.Route != jr.Route || r.Family != jr.Family || r.Quant != jr.Quant || r.UnetMatch != jr.UnetMatch || r.ClipMatch != jr.ClipMatch || r.DefaultClip != jr.DefaultClip || r.LoraLoader != jr.LoraLoader {
			t.Errorf("row %d identity: %+v vs %+v", i, r, jr)
		}
		if r.ComputeGiB != jr.PeakGiB.Compute || r.StorageGiB != jr.PeakGiB.Storage || r.UnetLoader != jr.Unet.Loader || r.ResidentGiB != jr.Unet.ResidentGb ||
			r.ClipLoader != jr.Clip.Loader || r.ClipResidentGiB != jr.Clip.ResidentGb || r.MaxMegapixels != jr.MaxMegapixels || r.MaxPixels != jr.MaxPixels || !reflect.DeepEqual(r.Packs, jr.Packs) {
			t.Errorf("row %s figures: %+v vs %+v", jr.ID, r, jr)
		}
		if r.Measured != (jr.Basis == "measured") {
			t.Errorf("row %s: measured %v here, basis %q there", jr.ID, r.Measured, jr.Basis)
		}
		if r.Blocked != jr.Blocked {
			t.Errorf("row %s: held back for %q here, %q there", jr.ID, r.Blocked, jr.Blocked)
		}
		if r.Blocked != "" {
			blocked++
		}
		if r.Measured {
			measured++
		}
	}
	if measured != 1 {
		t.Errorf("%d measured rows; only the 2511 fp8mixed edit ran with the read-only loader", measured)
	}
	if blocked != 2 {
		t.Errorf("%d held-back rows; only the two qwen-image-2.1 ones: int8_convrot (its DiT on cuda:1 no run has made) and bf16 (its text encoder is built whole on the storage card)", blocked)
	}
}

func files(unet, clip string) []ModelFile {
	out := []ModelFile{{Label: "unet", Name: unet, Role: RoleDiffusion}}
	if clip != "" {
		out = append(out, ModelFile{Label: "clip", Name: clip, Role: RoleTextEncoder})
	}
	return out
}

func TestFindMulticardRowPicksTheRecipeOfTheFilesAndRefusesTheRest(t *testing.T) {
	for _, tc := range []struct{ route, family, unet, clip, want string }{
		{"image", "qwen-image", "qwen-image-2512-Q5_1.gguf", "", "qwen-image/gguf-q5_1"},
		{"image", "qwen-image", "qwen-image-2512-Q5_1.gguf", "qwen_2.5_vl_7b_fp8_scaled.safetensors", "qwen-image/gguf-q5_1"},
		{"edit", "qwen-image-edit-2511", "qwen_image_edit_2511_fp8mixed.safetensors", "", "qwen-image-edit-2511/fp8mixed"},
		{"edit", "qwen-image-edit-2511", "QWEN-IMAGE-EDIT-2511-Q5_1.GGUF", "qwen_2.5_vl_7b_fp8_scaled.safetensors", "qwen-image-edit-2511/gguf-q5_1"},
		// no recipe: another family, another quant, the other route, an int8 or a wrong-kind text encoder, nothing named
		// and the two qwen-image-2.1 DiTs, whose rows exist but are held back (int8_convrot: its DiT on cuda:1 was never run; bf16: its text encoder is built whole on
		// the storage card before the split applies; see TestAHeldBackRowIsNotARecipeAndSaysWhy)
		{"image", "qwen-image-2.1", "qwen_image_2.1_bf16.safetensors", "qwen3vl_8b_bf16.safetensors", ""},
		{"image", "qwen-image-2.1", "qwen_image_2.1_uc_bf16.safetensors", "qwen3vl_8b_bf16.safetensors", ""},
		{"image", "qwen-image-2.1", "qwen_image_2.1_int8_convrot.safetensors", "qwen3vl_8b_bf16.safetensors", ""},
		{"image", "qwen-image-2.1", "qwen-image-2.1-UC-int8_convrot.safetensors", "qwen3vl_8b_bf16.safetensors", ""},
		{"image", "krea2", "krea2_turbo_bf16.safetensors", "qwen3vl_4b_bf16.safetensors", ""},
		{"image", "sdxl", "x.safetensors", "", ""},
		{"image", "qwen-image", "qwen_image_2512_bf16.safetensors", "", ""},
		{"image", "qwen-image", "qwen-image-2512-Q4_K_M.gguf", "", ""},
		{"image", "qwen-image-2.1", "qwen_image_2.1_bf16.gguf", "qwen3vl_8b_bf16.safetensors", ""},
		{"image", "qwen-image-2.1", "qwen_image_2.1_bf16.safetensors", "qwen3vl_8b_int8.safetensors", ""},
		// the ban on an int8, int4 or nf4 encoder stands on its own, not only through the bf16/fp8 match: a name that carries both is refused
		{"image", "qwen-image-2.1", "qwen_image_2.1_bf16.safetensors", "qwen3vl_8b_bf16_int8.safetensors", ""},
		{"image", "qwen-image-2.1", "qwen_image_2.1_bf16.safetensors", "qwen3vl_8b_bf16_nf4.safetensors", ""},
		{"edit", "qwen-image-edit-2511", "qwen_image_edit_2511_fp8mixed.safetensors", "qwen_2.5_vl_7b_fp8_int4.safetensors", ""},
		{"image", "qwen-image-2.1", "qwen_image_2.1_bf16.safetensors", "qwen3vl_8b_fp8.safetensors", ""},
		{"image", "qwen-image-2.1", "qwen_image_2.1_bf16.safetensors", "", ""},
		{"image", "qwen-image", "qwen-image-2512-Q5_1.gguf", "qwen_2.5_vl_7b_bf16.safetensors", ""},
		{"edit", "qwen-image-edit-2511", "qwen_image_edit_2511_fp8mixed.safetensors", "qwen_2.5_vl_7b_int8.safetensors", ""},
		{"edit", "qwen-image-2.1", "qwen_image_2.1_bf16.safetensors", "qwen3vl_8b_bf16.safetensors", ""},
		{"image", "qwen-image-edit-2511", "qwen_image_edit_2511_fp8mixed.safetensors", "", ""},
		{"edit", "qwen-image-edit-2511", "", "", ""},
	} {
		var fs []ModelFile
		if tc.unet != "" {
			fs = files(tc.unet, tc.clip)
		}
		got := FindMulticardRow(tc.route, tc.family, fs)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s %s %s %s: row %s, want none", tc.route, tc.family, tc.unet, tc.clip, got.ID)
		case tc.want != "" && (got == nil || got.ID != tc.want):
			t.Errorf("%s %s %s %s: %v, want %s", tc.route, tc.family, tc.unet, tc.clip, got, tc.want)
		}
	}
}

// A row that is held back stays in the table (the proof arm that lifts the hold builds its graph from it) but is no recipe: FindMulticardRow does not return
// it, so the estimate, the doctor and the typed doors treat the binding as one without a recipe, and BlockedMulticardRow says which row and why.
func TestAHeldBackRowIsNotARecipeAndSaysWhy(t *testing.T) {
	fs := files("qwen_image_2.1_int8_convrot.safetensors", "qwen3vl_8b_bf16.safetensors")
	if got := FindMulticardRow("image", "qwen-image-2.1", fs); got != nil {
		t.Fatalf("the held-back int8_convrot row was found as a recipe: %s", got.ID)
	}
	held := BlockedMulticardRow("image", "qwen-image-2.1", fs)
	if held == nil || held.ID != "qwen-image-2.1/int8_convrot" {
		t.Fatalf("the held-back row = %v, want qwen-image-2.1/int8_convrot", held)
	}
	for _, want := range []string{"cuda:1", "illegal memory access", "cuda:0"} {
		if !strings.Contains(held.Blocked, want) {
			t.Errorf("the reason must say why (%q missing): %q", want, held.Blocked)
		}
	}
	// the bf16 row is held back too: a live card-only run built the whole 16.33 GiB text encoder on the 15.9 GiB storage card before the DisTorch2 split applied
	for _, unet := range []string{"qwen_image_2.1_bf16.safetensors", "qwen_image_2.1_uc_bf16.safetensors"} {
		bf := files(unet, "qwen3vl_8b_bf16.safetensors")
		if got := FindMulticardRow("image", "qwen-image-2.1", bf); got != nil {
			t.Fatalf("%s: the held-back bf16 row was found as a recipe: %s", unet, got.ID)
		}
		heldBF := BlockedMulticardRow("image", "qwen-image-2.1", bf)
		if heldBF == nil || heldBF.ID != "qwen-image-2.1/bf16" {
			t.Fatalf("%s: the held-back row = %v, want qwen-image-2.1/bf16", unet, heldBF)
		}
		for _, want := range []string{"--gpu-only", "16.33 GiB bf16 text encoder", "cuda:0, a 15.9 GiB card", "before the DisTorch2 allocation that splits it is applied", "text encoder per card", "measured arm"} {
			if !strings.Contains(heldBF.Blocked, want) {
				t.Errorf("%s: the reason must say why (%q missing): %q", unet, want, heldBF.Blocked)
			}
		}
	}
	// a call with no row at all is not held back, and a launchable row is not either
	if got := BlockedMulticardRow("image", "qwen-image", files("qwen-image-2512-Q5_1.gguf", "")); got != nil {
		t.Errorf("the 2512 Q5_1 row is not held back: %v", got)
	}
	if got := BlockedMulticardRow("edit", "qwen-image-edit-2511", files("qwen_image_edit_2511_fp8mixed.safetensors", "")); got != nil {
		t.Errorf("the 2511 fp8mixed row is not held back: %v", got)
	}
	if got := BlockedMulticardRow("image", "krea2", files("krea2_turbo_bf16.safetensors", "")); got != nil {
		t.Errorf("a family with no row is not held back: %v", got)
	}
	// an int8 text encoder is refused as one before a row is looked for
	if got := BlockedMulticardRow("image", "qwen-image-2.1", files("qwen_image_2.1_int8_convrot.safetensors", "qwen3vl_8b_int8.safetensors")); got != nil {
		t.Errorf("a banned text encoder has no row, held back or not: %v", got)
	}
	// the doctor asks for no MultiGPU class for a binding the runner would refuse
	cfg := bare()
	cfg.ImageGenFamily, cfg.ImageGenCkpt, cfg.ImageGenCLIP, cfg.ImageGenPlacement = "qwen-image-2.1", "qwen_image_2.1_int8_convrot.safetensors", "qwen3vl_8b_bf16.safetensors", "multicard"
	if got := imageNodeClasses(cfg); len(got) != 0 {
		t.Errorf("a held-back binding names no extra node classes: %v", got)
	}
	cfg.ImageGenCkpt = "qwen_image_2.1_bf16.safetensors"
	if got := imageNodeClasses(cfg); len(got) != 0 {
		t.Errorf("the held-back bf16 binding names no extra node classes: %v", got)
	}
}

// Each row's loaders name the classes the runner puts in the graph; the doctor asks ComfyUI for exactly those.
func TestARowsNodeClassesAreItsLoadersClasses(t *testing.T) {
	want := map[string][]string{
		"qwen-image-2.1/int8_convrot":    {"UNETLoaderMultiGPU", "CLIPLoaderDisTorch2MultiGPU", "VAELoaderMultiGPU"},
		"qwen-image-2.1/bf16":            {"UNETLoaderDisTorch2MultiGPU", "CLIPLoaderDisTorch2MultiGPU", "VAELoaderMultiGPU"},
		"qwen-image/gguf-q5_1":           {"UnetLoaderGGUFDisTorch2MultiGPU", "CLIPLoaderMultiGPU", "VAELoaderMultiGPU"},
		"qwen-image-edit-2511/fp8mixed":  {"UNETLoaderDisTorch2MultiGPU", "CLIPLoaderMultiGPU", "VAELoaderMultiGPU"},
		"qwen-image-edit-2511/gguf-q5_1": {"UnetLoaderGGUFDisTorch2MultiGPU", "CLIPLoaderMultiGPU", "VAELoaderMultiGPU"},
	}
	for _, r := range MulticardRows() {
		if got := r.NodeClasses(); !reflect.DeepEqual(got, want[r.ID]) {
			t.Errorf("%s: %v, want %v", r.ID, got, want[r.ID])
		}
	}
}

func TestRouteNeedsListTheMultiGPUClassesForAMulticardBinding(t *testing.T) {
	img := func(family, ckpt, clip string, multicard bool) []string {
		cfg := bare()
		cfg.ImageGenFamily, cfg.ImageGenCkpt, cfg.ImageGenCLIP = family, ckpt, clip
		if multicard {
			cfg.ImageGenPlacement = "multicard"
		}
		return imageNodeClasses(cfg)
	}
	if got := img("qwen-image", "qwen-image-2512-Q5_1.gguf", "", true); strings.Join(got, ",") != "UnetLoaderGGUFDisTorch2MultiGPU,CLIPLoaderMultiGPU,VAELoaderMultiGPU" {
		t.Errorf("a multicard 2512 GGUF binding: %v", got)
	}
	if got := img("qwen-image", "qwen-image-2512-Q5_1.gguf", "", false); strings.Join(got, ",") != "UnetLoaderGGUF" {
		t.Errorf("without the placement the doctor asks for what it always did: %v", got)
	}
	if got := img("qwen-image-2.1", "qwen_image_2.1_bf16.safetensors", "qwen3vl_8b_bf16.safetensors", true); len(got) != 0 {
		t.Errorf("a multicard 2.1 bf16 binding is held back (the runner refuses it by name), so the doctor asks for no extra class: %v", got)
	}
	if got := img("qwen-image-2.1", "qwen_image_2.1_bf16.safetensors", "qwen3vl_8b_bf16.safetensors", false); len(got) != 0 {
		t.Errorf("a single 2.1 binding names only core classes: %v", got)
	}
	if got := img("krea2", "krea2_turbo_bf16.safetensors", "", true); len(got) != 0 {
		t.Errorf("a family with no recipe is refused by the runner, and the doctor asks for nothing extra: %v", got)
	}
	ed := bare()
	ed.GenEditUnet, ed.GenEditPlacement = "qwen_image_edit_2511_fp8mixed.safetensors", "multicard"
	if got := editNodeClasses(ed); strings.Join(got, ",") != "UNETLoaderDisTorch2MultiGPU,CLIPLoaderMultiGPU,VAELoaderMultiGPU" {
		t.Errorf("a multicard 2511 fp8mixed binding: %v", got)
	}
	ed.GenEditUnet = "qwen-image-edit-2511-Q5_1.gguf"
	if got := editNodeClasses(ed); strings.Join(got, ",") != "UnetLoaderGGUFDisTorch2MultiGPU,CLIPLoaderMultiGPU,VAELoaderMultiGPU" {
		t.Errorf("a multicard 2511 GGUF binding: %v", got)
	}
	ed.GenEditPlacement = ""
	if got := editNodeClasses(ed); strings.Join(got, ",") != "UnetLoaderGGUF" {
		t.Errorf("a single 2511 GGUF binding: %v", got)
	}
	// every class a row names has a pack, so the disk check can say what is missing
	dir := t.TempDir()
	installPacks(t, dir, "ComfyUI-MultiGPU")
	var all []string
	for _, r := range MulticardRows() {
		all = append(all, r.NodeClasses()...)
	}
	missing := DiskNodeCheck(dir, all).Missing
	for _, m := range missing {
		if m != "UnetLoaderGGUFDisTorch2MultiGPU" {
			t.Errorf("%s is missing with the MultiGPU pack installed: only the GGUF DisTorch2 loader also needs ComfyUI-GGUF", m)
		}
	}
	if len(missing) == 0 {
		t.Errorf("the GGUF DisTorch2 loader needs ComfyUI-GGUF too, and the disk check does not know the class")
	}
	for _, c := range all {
		if len(classPacks[c]) == 0 {
			t.Errorf("%s has no pack in classPacks", c)
		}
	}
}

// The binding's placement is where the weights sit, not which weights: it never changes the recipe identity (ADR 0082).
func TestRecipeIdentityIgnoresPlacement(t *testing.T) {
	base := baseBinding()
	want := recipeOf(t, base).Digest()
	m := base
	m.ImageGenPlacement = "multicard"
	if got := recipeOf(t, m).Digest(); got != want {
		t.Errorf("imagegen_placement changed the recipe digest")
	}
	m.ImageGenPlacement = "single"
	if got := recipeOf(t, m).Digest(); got != want {
		t.Errorf("imagegen_placement single changed the recipe digest")
	}
}
