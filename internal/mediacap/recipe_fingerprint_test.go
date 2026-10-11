package mediacap

// F59: a family's identity covers the CONTENT of its weight files, not only their names and sizes. The fixtures here
// are real files in a temp ComfyUI tree (the fingerprint reads bytes, so a stat table is not enough); the three files
// are tiny, which the fingerprint hashes whole.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// fpTreeFiles are the weight files of the qwen-image-2.1 bf16 family, each with the byte it is filled with.
var fpTreeFiles = map[string]byte{
	"models/diffusion_models/qwen_image_2.1_uc_bf16.safetensors": 'a',
	"models/text_encoders/qwen3vl_8b_bf16.safetensors":           'b',
	"models/vae/qwen_image_2.1_vae_bf16.safetensors":             'c',
}

// fpTree writes fpTreeFiles into a fresh ComfyUI directory, 4096 bytes each; over maps a file to other bytes
// (same length) so a tree can differ from another in content alone.
func fpTree(t *testing.T, over map[string]byte) string {
	t.Helper()
	dir := t.TempDir()
	for rel, b := range fpTreeFiles {
		if o, ok := over[rel]; ok {
			b = o
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte{b}, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fpRecipe is the recipe of the bf16 family bound over the ComfyUI tree at dir, with the production fingerprinter.
func fpRecipe(t *testing.T, dir string) Recipe {
	t.Helper()
	cfg := bare()
	cfg.ImageGenScript = "render/comfy-generate.mjs"
	cfg.ComfyDir = dir
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{"qwen-image-2.1": overlay(t, nodeABf16)}
	eff, fi, err := cfg.ResolveImageFamily("qwen-image-2.1")
	if err != nil {
		t.Fatal(err)
	}
	r, ok := ImageRecipe(eff, fi, ModelRoots(dir), nil)
	if !ok {
		t.Fatal("no recipe")
	}
	return r
}

// THE F59 HOLE. Two machines hold files of one name and one size whose bytes differ (a same-named re-release with the
// same tensor layout, a copy that was altered at full length). Before the fingerprint they digested alike and
// matched.
func TestSameNameAndSizeWithDifferentContentIsAnotherRecipe(t *testing.T) {
	a := fpRecipe(t, fpTree(t, nil))
	b := fpRecipe(t, fpTree(t, map[string]byte{"models/diffusion_models/qwen_image_2.1_uc_bf16.safetensors": 'z'}))
	if a.Digest() == b.Digest() {
		t.Fatal("a checkpoint of the same name and size but other bytes is not the same recipe")
	}
}

// The same bytes in two trees are one recipe: the delegator's own recipe and a node's agree when their files are copies
// of each other, whatever the path they sit under.
func TestIdenticalFilesInTwoTreesDigestAlike(t *testing.T) {
	a, b := fpRecipe(t, fpTree(t, nil)), fpRecipe(t, fpTree(t, nil))
	if a.Digest() != b.Digest() {
		t.Fatalf("copies of the same files must digest alike; differs in: %v", a.Diff(b))
	}
	if len(a.Unfingerprinted()) != 0 || len(a.Missing()) != 0 {
		t.Errorf("a complete, hashed recipe has nothing missing or pending: %v %v", a.Missing(), a.Unfingerprinted())
	}
	for _, f := range a.Files {
		if !IsFingerprint(f.Fingerprint) {
			t.Errorf("%s carries %q, want a sha256", f.Name, f.Fingerprint)
		}
	}
}

// The diff names the file whose content differs, in the config key's own word, and nothing else.
func TestContentDifferenceIsNamedInTheDiff(t *testing.T) {
	a := fpRecipe(t, fpTree(t, nil))
	b := fpRecipe(t, fpTree(t, map[string]byte{"models/text_encoders/qwen3vl_8b_bf16.safetensors": 'z'}))
	d := a.Diff(b)
	if len(d) != 1 || !strings.HasPrefix(d[0], "imagegen_clip: qwen3vl_8b_bf16.safetensors has the same size on both and different content (fingerprint ") {
		t.Fatalf("the diff must name the text encoder as different content and only it: %v", d)
	}
	if a.WithoutContent().Digest() != b.WithoutContent().Digest() {
		t.Error("WithoutContent is the same recipe by name, size and sampling")
	}
}

// A file that is there but not hashed yet is not an identity: Unfingerprinted names it with its state, whether the caller
// supplied no source, was told pending, or could not read it; a file that is missing is Missing's, not Unfingerprinted's.
func TestAPendingFileIsNeverAnIdentity(t *testing.T) {
	cfg := bare()
	cfg.ImageGenScript = "render/comfy-generate.mjs"
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{"qwen-image-2.1": overlay(t, nodeABf16)}
	eff, fi, err := cfg.ResolveImageFamily("qwen-image-2.1")
	if err != nil {
		t.Fatal(err)
	}
	roots, stat := recipeEnv(nil)
	for name, fp := range map[string]FingerprintFunc{
		"no source": nil,
		"pending":   func(string, int64) string { return FingerprintPending },
	} {
		r, ok := ImageRecipeWith(eff, fi, roots, stat, fp)
		if !ok {
			t.Fatal("no recipe")
		}
		want := []string{"qwen_image_2.1_uc_bf16.safetensors (pending)", "qwen3vl_8b_bf16.safetensors (pending)", "qwen_image_2.1_vae_bf16.safetensors (pending)"}
		if got := r.Unfingerprinted(); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: Unfingerprinted = %v, want %v", name, got, want)
		}
	}
	r, _ := ImageRecipeWith(eff, fi, roots, stat, func(path string, _ int64) string {
		if strings.Contains(filepath.ToSlash(path), "/vae/") {
			return FingerprintUnreadable
		}
		return fixtureFingerprint(path, 1)
	})
	if got := r.Unfingerprinted(); len(got) != 1 || got[0] != "qwen_image_2.1_vae_bf16.safetensors (unreadable)" {
		t.Errorf("only the unreadable VAE is unfingerprinted: %v", got)
	}
	gone := func(path string) (int64, bool) {
		if strings.Contains(filepath.ToSlash(path), "text_encoders") {
			return 0, false
		}
		return stat(path)
	}
	r, _ = ImageRecipeWith(eff, fi, roots, gone, fixtureFingerprint)
	if len(r.Missing()) != 1 || len(r.Unfingerprinted()) != 0 {
		t.Errorf("a missing file is missing, not unfingerprinted: %v / %v", r.Missing(), r.Unfingerprinted())
	}
	if f := r.Files[1]; f.Bytes != -1 || f.Fingerprint != "" {
		t.Errorf("a missing file has no size and no fingerprint: %+v", f)
	}
}

// The fingerprint is IN the digest: with every name and size equal, a recipe whose one fingerprint differs digests
// differently, and the version of the recipe is not the one before the fingerprint existed.
func TestDigestCoversTheFingerprintAndItsVersion(t *testing.T) {
	r := recipeOf(t, baseBinding())
	o := r
	o.Files = append([]RecipeFile(nil), r.Files...)
	o.Files[0].Fingerprint = strings.Repeat("0", 64)
	if r.Digest() == o.Digest() {
		t.Error("a different fingerprint on one file must change the digest")
	}
	if RecipeVersion < 2 {
		t.Errorf("RecipeVersion = %d: a digest that covers the fingerprint must not collide with the size-only one", RecipeVersion)
	}
}

// The causes of a node's 412 are words of the wire: a node and a delegator of two builds must read them alike, and the docs quote
// them. Two of them are the words of the file states they come from (a pending refusal is files that read pending).
func TestTheRefusalCausesAreTheWordsOfTheWire(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{RecipeRefusalCauseKey, "cause"},
		{RecipeRefusalPending, "pending"},
		{RecipeRefusalUnreadable, "unreadable"},
		{RecipeRefusalChanged, "changed"},
		{RecipeRefusalPending, FingerprintPending},
		{RecipeRefusalUnreadable, FingerprintUnreadable},
	} {
		if tc.got != tc.want {
			t.Errorf("a word of the wire changed: %q, want %q", tc.got, tc.want)
		}
	}
}

// ImageRecipe (the blocking spelling: the delegator's identity and the node's 412 check read through ImageRecipeWith with the
// AwaitContext of their own request) reads through the shared fingerprinter and waits for it.
func TestImageRecipeWaitsForTheSharedFingerprinter(t *testing.T) {
	r := fpRecipe(t, fpTree(t, nil))
	for _, f := range r.Files {
		if !IsFingerprint(f.Fingerprint) {
			t.Errorf("ImageRecipe must wait for %s's fingerprint, got %q", f.Name, f.Fingerprint)
		}
	}
}

// Content that cannot be compared (one side has no hash yet) is said so, in the diff, and is not called "different".
func TestDiffSaysWhenContentIsNotComparable(t *testing.T) {
	a := fpRecipe(t, fpTree(t, nil))
	b := a
	b.Files = append([]RecipeFile(nil), a.Files...)
	for i := range b.Files {
		if b.Files[i].Role == RecipeRoleVAE {
			b.Files[i].Fingerprint = FingerprintPending
		}
	}
	d := a.Diff(b)
	if len(d) != 1 || !strings.HasPrefix(d[0], "imagegen_vae: qwen_image_2.1_vae_bf16.safetensors has the same size on both and its content is not comparable (fingerprint ") ||
		!strings.HasSuffix(d[0], " vs pending)") || strings.Contains(d[0], "different content") {
		t.Errorf("diff = %v", d)
	}
}
