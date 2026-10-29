package hostneed

// A TEMPLATE run is sized by the model files its runtime manifest names (ForManifestModels, ForManifestFile, SourceGraphLoaders), by the
// rule a graph's files are: the sum fitted to the card, the class default for any doubt. The files are sparse and a few MiB long.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

const (
	manifestCkpt = "models/checkpoints/qwen_image_fp8.safetensors"
	manifestClip = "models/text_encoders/qwen_2.5_vl_7b.safetensors"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The defect: two files the manifest names, held by the box, are declared as their sum with the graph-loaders source, and not as the
// class default (here the bound family's 32.8 GiB); the file form reads the same list.
func TestAManifestNamingTwoModelFilesDeclaresTheirSum(t *testing.T) {
	const ckpt, clip = 6 * mib, 16 * mib
	f := sparseTree(t, map[string]int64{"checkpoints/qwen_image_fp8.safetensors": ckpt, "text_encoders/qwen_2.5_vl_7b.safetensors": clip})
	want := gibOf(ckpt) + gibOf(clip)
	n, ok := ForManifestModels([]string{manifestCkpt, manifestClip}, f)
	if !ok || n.Source != SourceGraphLoaders || !sameGiB(n.GiB, want) {
		t.Fatalf("ForManifestModels = %v %v, want %.9f GiB from %q", n, ok, want, SourceGraphLoaders)
	}
	for _, name := range []string{"qwen_image_fp8.safetensors", "qwen_2.5_vl_7b.safetensors", "template's runtime manifest"} {
		if !strings.Contains(n.Detail, name) {
			t.Errorf("the detail names what the figure came from (%q): %q", name, n.Detail)
		}
	}
	file := writeManifest(t, `{"schema_version":1,"workflow":"t","models":[{"path":"`+manifestCkpt+`","source_url":"","sha256":null,"trust":"tofu"},{"path":"`+manifestClip+`"}]}`)
	if byFile, ok := ForManifestFile(file, f); !ok || byFile != n {
		t.Errorf("ForManifestFile = %v %v, want the same as the list's: %v", byFile, ok, n)
	}
}

// A nested model path and a .bin are named by the manifest, so they are files like any other (a graph's scan would not list a .bin); a
// file named twice counts once, and a VAE or a LoRA counts with the checkpoint.
func TestAManifestCountsANestedPathAndADotBinOnceEach(t *testing.T) {
	f := sparseTree(t, map[string]int64{
		"checkpoints/qwen_image_fp8.safetensors": 6 * mib, "loras/sub/dir/style.safetensors": 2 * mib, "diffusers/qwen/pytorch_model.bin": 3 * mib,
	})
	n, ok := ForManifestModels([]string{manifestCkpt, `models\loras\sub\dir\style.safetensors`, "models/diffusers/qwen/pytorch_model.bin", manifestCkpt}, f)
	if want := gibOf(11 * mib); !ok || !sameGiB(n.GiB, want) {
		t.Fatalf("ForManifestModels = %v %v, want %.9f GiB (each file once)", n, ok, want)
	}
}

// ANY doubt is the class default (ok false): one file the box does not hold, a file of no length, an empty list, an entry that is not
// models/<class>/<name>, a name that climbs out of its class directory, a manifest that cannot be read.
func TestAManifestTheLeaseCannotSizeIsNotSized(t *testing.T) {
	f := sparseTree(t, map[string]int64{"checkpoints/qwen_image_fp8.safetensors": 6 * mib, "checkpoints/empty.safetensors": 0})
	outside := filepath.Join(f.Cfg.ComfyDir, "outside.safetensors")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, paths := range map[string][]string{
		"a file the box does not hold, beside one it does": {manifestCkpt, manifestClip},
		"a file of no length":                              {manifestCkpt, "models/checkpoints/empty.safetensors"},
		"a blank path":                                     {manifestCkpt, ""},
		"no class":                                         {"models/qwen_image_fp8.safetensors"},
		"not under models":                                 {"checkpoints/qwen_image_fp8.safetensors"},
		"a class that climbs out":                          {"models/../outside.safetensors"},
		"a name that climbs out":                           {manifestCkpt, "models/checkpoints/../../outside.safetensors"},
		"an absolute name":                                 {"models/checkpoints//etc/x.safetensors"},
	} {
		if n, ok := ForManifestModels(paths, f); ok {
			t.Errorf("%s: sized as %v, want no figure (the class default)", name, n)
		}
	}
	// a manifest whose models list is present and empty is a template that loads no weight file: the ComfyUI process floor
	if n, ok := ForManifestModels([]string{}, f); !ok || n.GiB != 3 || n.CommitGiB != 4 || !n.CommitStated || n.Source != SourceMeasured {
		t.Errorf("an empty list: %v %v, want the process floor", n, ok)
	}
	if want := ClassDefault(f); want.Source != SourceClassDefault || want.GiB < 30 {
		t.Fatalf("the control: the class default of this box is Krea 2's documented 32.8 GiB, got %v", want)
	}
	for name, body := range map[string]string{
		"not JSON":                       `not json`,
		"models is not a list":           `{"models":{"path":"` + manifestCkpt + `"}}`,
		"an entry that is not an object": `{"models":["` + manifestCkpt + `"]}`,
		"a path that is not a string":    `{"models":[{"path":7}]}`,
		"a null path":                    `{"models":[{"path":null},{"path":"` + manifestCkpt + `"}]}`,
		"no models":                      `{"workflow":"t"}`,
		"an empty file":                  ``,
	} {
		if n, ok := ForManifestFile(writeManifest(t, body), f); ok {
			t.Errorf("manifest %s: sized as %v, want no figure", name, n)
		}
	}
	for name, path := range map[string]string{"an empty path": "", "a directory": t.TempDir(), "a file that is not there": filepath.Join(t.TempDir(), "absent.json")} {
		if n, ok := ForManifestFile(path, f); ok {
			t.Errorf("%s: sized as %v, want no figure", name, n)
		}
	}
}

// The box needs a ComfyUI install to look in, and not a render family bound: the files are the template's own, so a box that binds
// no family (its class default is nothing) still declares them.
func TestAManifestOnABoxThatBindsNoRenderFamilyDeclaresItsFiles(t *testing.T) {
	f := sparseTree(t, map[string]int64{"checkpoints/qwen_image_fp8.safetensors": 6 * mib})
	f.Cfg.ImageGenScript, f.Cfg.ImageGenFamily, f.Cfg.ImageGenCkpt = "", "", ""
	if c := ClassDefault(f); c.Source != SourceNone {
		t.Fatalf("the control: this box binds no render family, got %v", c)
	}
	if n, ok := ForManifestModels([]string{manifestCkpt}, f); !ok || n.Source != SourceGraphLoaders || !sameGiB(n.GiB, gibOf(6*mib)) {
		t.Errorf("ForManifestModels = %v %v, want the file's %.9f GiB from %q", n, ok, gibOf(6*mib), SourceGraphLoaders)
	}
	f.Cfg.ComfyDir = "" // no install to look in: nothing resolves, nothing is sized
	if n, ok := ForManifestModels([]string{manifestCkpt}, f); ok {
		t.Errorf("a box with no ComfyUI install sized %v", n)
	}
}

// The family estimate's rule, and the call's own reserve: a 25 GiB card holds the 23.09 GiB set with the default 1 GiB held back (24
// usable) and not with 6 (19 usable); on a card that is not known the set is counted in full.
func TestAManifestIsFittedToTheCardWithTheReserveVRAMOfTheCall(t *testing.T) {
	dir, stat := comfyTree(t, map[string]float64{"qwen_image_fp8.safetensors": 6.76, "qwen_2.5_vl_7b.safetensors": 16.33})
	f := Facts{Cfg: config.Config{ComfyDir: dir}, Stat: stat}
	paths := []string{manifestCkpt, manifestClip}
	for _, c := range []struct {
		name    string
		vram    float64
		reserve float64
		want    float64
	}{
		{"fits with the default margin", 25, 0, 0},
		{"does not fit with a reserve of 6", 25, 6, 6.76 + 16.33},
		{"a card that is not known counts it in full", 0, 0, 6.76 + 16.33},
		{"fits a bigger card", 32, 0, 0},
	} {
		ff := f
		ff.VRAMGiB, ff.ReserveVRAMGiB = c.vram, c.reserve
		n, ok := ForManifestModels(paths, ff)
		if !ok || n.Source != SourceGraphLoaders || !near(n.GiB, c.want) {
			t.Errorf("%s: %v %v, want %.2f GiB from %q", c.name, n, ok, c.want, SourceGraphLoaders)
		}
	}
}
