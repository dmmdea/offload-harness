package mediacap

// ModelFileSizes (graphfiles.go): the SIZES of the weight files a graph names, for the host-RAM estimate of a graph
// (internal/hostneed.ForGraph). It looks a name up exactly as MissingModelFiles does (any class directory of any root, through
// safeRel, the roots listed once) and answers with the size instead of "held". The fixtures are sparse files: os.Truncate sets
// the length, nothing is written, so no test holds a real weight file.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const mib = 1 << 20

// sizedTree writes one sparse file per entry (a path relative to a fresh ComfyUI dir, its length in bytes) and returns the dir.
func sizedTree(t *testing.T, files map[string]int64) string {
	t.Helper()
	dir := t.TempDir()
	for rel, n := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(n); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Each name is found in whichever class directory of whichever root holds it (a graph carries no class), and the sizes come back
// in the order of the names.
func TestModelFileSizesFindsEachNameInAnyClassDirectoryOfAnyRoot(t *testing.T) {
	dir := sizedTree(t, map[string]int64{
		"models/checkpoints/ck.safetensors":       3 * mib,
		"models/text_encoders/te/enc.safetensors": 5 * mib,
		"models/vae/ae.pth":                       2 * mib,
	})
	other := sizedTree(t, map[string]int64{"weights/diff/big.safetensors": 7 * mib})
	yaml := "other:\n  base_path: " + filepath.ToSlash(other) + "\n  diffusion_models: weights/diff\n"
	if err := os.WriteFile(filepath.Join(dir, "extra_model_paths.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	sizes, missing, ok := ModelFileSizes(ModelRoots(dir), []string{"big.safetensors", "te/enc.safetensors", "ck.safetensors", "ae.pth"}, nil)
	if !ok || missing != "" {
		t.Fatalf("every name is held: ok=%v missing=%q", ok, missing)
	}
	if want := []int64{7 * mib, 5 * mib, 3 * mib, 2 * mib}; !reflect.DeepEqual(sizes, want) {
		t.Fatalf("sizes = %v, want %v (in the order of the names)", sizes, want)
	}
}

// ONE name that does not resolve ends the answer: a size that is not known must never be left out of a sum. The name that failed
// is said, the first one in the graph's order.
func TestModelFileSizesIsNotOKWhenAnyNameDoesNotResolve(t *testing.T) {
	dir := sizedTree(t, map[string]int64{"models/checkpoints/ck.safetensors": 3 * mib})
	sizes, missing, ok := ModelFileSizes(ModelRoots(dir), []string{"ck.safetensors", "absent.safetensors", "also-absent.safetensors"}, nil)
	if ok || missing != "absent.safetensors" || len(sizes) != 0 {
		t.Fatalf("a missing name = (%v, %q, %v), want no sizes, the first missing name and ok=false", sizes, missing, ok)
	}
	// no ComfyUI install at all: nothing is held
	if _, missing, ok := ModelFileSizes(nil, []string{"ck.safetensors"}, nil); ok || missing != "ck.safetensors" {
		t.Errorf("no roots: missing=%q ok=%v, want ck.safetensors and ok=false", missing, ok)
	}
}

// A name is data from the caller's graph: it is looked up under the roots and nowhere else, as MissingModelFiles does, so a name
// that climbs out of them does not resolve even when the file it points at exists.
func TestModelFileSizesNeverLeavesTheRoots(t *testing.T) {
	dir := sizedTree(t, map[string]int64{"models/vae/ok.safetensors": mib, "secret.safetensors": 9 * mib})
	for _, name := range []string{"../../secret.safetensors", `..\..\secret.safetensors`, "/secret.safetensors", ""} {
		if _, missing, ok := ModelFileSizes(ModelRoots(dir), []string{name}, nil); ok || missing != name {
			t.Errorf("%q: missing=%q ok=%v, want the name missing", name, missing, ok)
		}
	}
}

// A file of no length is not a size: it reads as unresolved (the way hostneed.ForSdcpp treats one), so an empty placeholder
// can never shrink a figure.
func TestModelFileSizesTreatsAnEmptyFileAsUnresolved(t *testing.T) {
	dir := sizedTree(t, map[string]int64{"models/checkpoints/empty.safetensors": 0})
	if _, missing, ok := ModelFileSizes(ModelRoots(dir), []string{"empty.safetensors"}, nil); ok || missing != "empty.safetensors" {
		t.Fatalf("an empty file: missing=%q ok=%v, want it unresolved", missing, ok)
	}
}

// The graph names a file, not a class: when the same name is held under two class directories, the loader may open either, so
// the LARGER is the size (the smaller would shrink a declaration).
func TestModelFileSizesTakesTheLargerOfTwoSameNamedFiles(t *testing.T) {
	dir := sizedTree(t, map[string]int64{
		"models/checkpoints/ae.safetensors":      mib,
		"models/diffusion_models/ae.safetensors": 6 * mib,
		"models/vae/ae.safetensors":              2 * mib,
	})
	sizes, _, ok := ModelFileSizes(ModelRoots(dir), []string{"ae.safetensors"}, nil)
	if !ok || !reflect.DeepEqual(sizes, []int64{6 * mib}) {
		t.Fatalf("sizes = %v ok=%v, want the largest, %d", sizes, ok, 6*mib)
	}
}

// The sizes come through the stat seam when one is given (a table in tests, as ResolveModelFile has it), and each models root is
// listed once per call, not once per name.
func TestModelFileSizesUsesTheStatSeamAndListsEachRootOnce(t *testing.T) {
	dir := sizedTree(t, map[string]int64{"models/checkpoints/a.safetensors": mib, "models/vae/b.safetensors": mib})
	table := map[string]int64{"a.safetensors": 11 * mib, "b.safetensors": 13 * mib}
	stat := func(p string) (int64, bool) { n, ok := table[filepath.Base(p)]; return n, ok }

	var reads int
	prev := readDir
	readDir = func(name string) ([]os.DirEntry, error) { reads++; return os.ReadDir(name) }
	t.Cleanup(func() { readDir = prev })

	sizes, _, ok := ModelFileSizes(ModelRoots(dir), []string{"a.safetensors", "b.safetensors"}, stat)
	if !ok || !reflect.DeepEqual(sizes, []int64{11 * mib, 13 * mib}) {
		t.Fatalf("sizes = %v ok=%v, want the table's", sizes, ok)
	}
	if reads != 1 {
		t.Errorf("a call lists a models root once; two names caused %d directory reads", reads)
	}
}
