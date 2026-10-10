package mediacap

// What a ComfyUI API-format graph names that a node must HOLD to run it (graphfiles.go): the loader files and the
// input images. The graphs here are small and written out, not generated: the point of each is one rule of the scan.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScanGraphFindsEveryLoaderFileOnce(t *testing.T) {
	graph := `{
		"3":  {"class_type":"UNETLoader",      "inputs":{"unet_name":"wan/wan2.2_i2v_high_noise_14B_fp8.safetensors","weight_dtype":"default"}},
		"4":  {"class_type":"CLIPLoader",      "inputs":{"clip_name":"umt5_xxl_fp8.safetensors","type":"wan"}},
		"5":  {"class_type":"DualCLIPLoader",  "inputs":{"clip_name1":"a.GGUF","clip_name2":"b.sft"}},
		"6":  {"class_type":"VAELoader",       "inputs":{"vae_name":"wan_2.1_vae.pth"}},
		"7":  {"class_type":"LoraLoaderModelOnly","inputs":{"lora_name":"lightx2v.pt","strength_model":1,"model":["3",0]}},
		"8":  {"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"old.ckpt"}},
		"9":  {"class_type":"UNETLoader",      "inputs":{"unet_name":"wan/wan2.2_i2v_high_noise_14B_fp8.safetensors"}},
		"10": {"class_type":"SaveImage",       "inputs":{"filename_prefix":"out_name.safetensors","images":["8",0]}},
		"11": {"class_type":"CLIPTextEncode",  "inputs":{"text":"a file_name.safetensors on a desk","clip":["4",0]}},
		"12": {"class_type":"VAELoader",       "inputs":{"vae_name":"builtin"}},
		"13": {"class_type":"UNETLoader",      "inputs":{"unet_name":["99",0]}}
	}`
	got, err := ScanGraph([]byte(graph))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"a.GGUF", "b.sft", "old.ckpt", "umt5_xxl_fp8.safetensors", "wan/wan2.2_i2v_high_noise_14B_fp8.safetensors", "wan_2.1_vae.pth", "lightx2v.pt",
	}
	// Order is by node id then input key (the graph's keys are unordered JSON), so a refusal names the same file on every run.
	if !sameSet(got.Models, want) {
		t.Fatalf("Models = %v, want the set %v", got.Models, want)
	}
	if again, _ := ScanGraph([]byte(graph)); !reflect.DeepEqual(got, again) {
		t.Errorf("a scan must be deterministic: %v then %v", got.Models, again.Models)
	}
	if len(got.Images) != 0 {
		t.Errorf("no LoadImage in this graph: %v", got.Images)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestScanGraphFindsLoadImageInputs(t *testing.T) {
	graph := `{
		"1": {"class_type":"LoadImage",       "inputs":{"image":"ref.png","upload":"image"}},
		"2": {"class_type":"LoadImageMask",   "inputs":{"image":"mask.png","channel":"red"}},
		"3": {"class_type":"LoadImageOutput", "inputs":{"image":"prev/step.png [output]"}},
		"4": {"class_type":"LoadImage",       "inputs":{"image":"kept/in.png [input]"}},
		"5": {"class_type":"LoadImage",       "inputs":{"image":["9",0]}},
		"6": {"class_type":"ImageScale",      "inputs":{"image":["1",0],"width":512}},
		"7": {"class_type":"PreviewImage",    "inputs":{"images":["1",0]}}
	}`
	got, err := ScanGraph([]byte(graph))
	if err != nil {
		t.Fatal(err)
	}
	want := []GraphImage{
		{Name: "ref.png", Dir: "input"}, {Name: "mask.png", Dir: "input"}, {Name: "prev/step.png", Dir: "output"}, {Name: "kept/in.png", Dir: "input"},
	}
	if !reflect.DeepEqual(got.Images, want) {
		t.Fatalf("Images = %+v, want %+v", got.Images, want)
	}
}

func TestScanGraphToleratesWhatItDoesNotRead(t *testing.T) {
	for name, graph := range map[string]string{
		"a node that is not an object":   `{"1": 7, "2": {"class_type":"X"}}`,
		"inputs that are not an object":  `{"1": {"class_type":"UNETLoader","inputs":"x"}}`,
		"a node with no inputs":          `{"1": {"class_type":"X"}}`,
		"a value that is a number":       `{"1": {"class_type":"X","inputs":{"unet_name":3}}}`,
		"a name with no known extension": `{"1": {"class_type":"X","inputs":{"unet_name":"model.bin"}}}`,
	} {
		got, err := ScanGraph([]byte(graph))
		if err != nil || len(got.Models) != 0 || len(got.Images) != 0 {
			t.Errorf("%s: got %+v, %v; want nothing and no error", name, got, err)
		}
	}
	if _, err := ScanGraph([]byte(`[1,2]`)); err == nil {
		t.Error("a graph that is not a JSON object must be an error")
	}
}

// tree writes files (relative to a fresh ComfyUI dir) and returns the dir.
func tree(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestMissingModelFilesLooksUnderEveryClassDirectory(t *testing.T) {
	dir := tree(t,
		"models/diffusion_models/wan/high.safetensors",
		"models/vae/wan_vae.pth",
		"models/loras/Light.PT",
		"models/clip_vision/vis.sft",
		"models/checkpoints/ck.ckpt",
	)
	roots := ModelRoots(dir)
	got := MissingModelFiles(roots, []string{
		"wan/high.safetensors", "wan_vae.pth", "Light.PT", "vis.sft", "ck.ckpt", // present, each in a different class directory
		"krea2_turbo_bf16.safetensors", "wan/low.safetensors", // absent
		"high.safetensors", // present only under a subfolder: the loader opens the name as given
	})
	want := []string{"krea2_turbo_bf16.safetensors", "wan/low.safetensors", "high.safetensors"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
}

func TestMissingModelFilesReadsExtraModelPaths(t *testing.T) {
	dir := tree(t, "models/vae/a.safetensors")
	other := tree(t, "weights/diff/big.safetensors")
	yaml := "other:\n  base_path: " + filepath.ToSlash(other) + "\n  diffusion_models: weights/diff\n"
	if err := os.WriteFile(filepath.Join(dir, "extra_model_paths.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := MissingModelFiles(ModelRoots(dir), []string{"a.safetensors", "big.safetensors"}); len(got) != 0 {
		t.Fatalf("a file on a secondary root is held: missing %v", got)
	}
}

func TestMissingModelFilesNeverLeavesTheRoots(t *testing.T) {
	// A name is data from a tailnet peer: it is looked up under the roots and nowhere else, so a name that climbs out of
	// them or names an absolute path is missing even when the file exists.
	outside := tree(t, "secret.safetensors")
	dir := tree(t, "models/vae/ok.safetensors")
	roots := ModelRoots(dir)
	names := []string{
		"../../" + filepath.Base(outside) + "/secret.safetensors",
		filepath.ToSlash(filepath.Join(outside, "secret.safetensors")),
		`..\..\x.safetensors`,
		"/etc/x.safetensors",
		`C:\Windows\x.safetensors`,
		"",
	}
	if got := MissingModelFiles(roots, names); !reflect.DeepEqual(got, names) {
		t.Fatalf("every name outside the roots must be missing: got %v", got)
	}
}

func TestMissingModelFilesOnANodeWithNoModelsRoot(t *testing.T) {
	if got := MissingModelFiles(nil, []string{"a.safetensors"}); !reflect.DeepEqual(got, []string{"a.safetensors"}) {
		t.Fatalf("a node with no models root holds no model file: %v", got)
	}
}

func TestMissingInputImagesLooksInTheDirTheNameSays(t *testing.T) {
	dir := tree(t, "input/ref.png", "input/kept/in.png", "output/prev/step.png")
	got := MissingInputImages(dir, []GraphImage{
		{Name: "ref.png", Dir: "input"},
		{Name: "kept/in.png", Dir: "input"},
		{Name: "prev/step.png", Dir: "output"},
		{Name: "step.png", Dir: "input"},         // exists only under output/prev
		{Name: "prev/step.png", Dir: "input"},    // exists only under output
		{Name: "../input/ref.png", Dir: "input"}, // climbs
		{Name: "x.png", Dir: "temp"},
	})
	want := []GraphImage{
		{Name: "step.png", Dir: "input"}, {Name: "prev/step.png", Dir: "input"}, {Name: "../input/ref.png", Dir: "input"}, {Name: "x.png", Dir: "temp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %+v, want %+v", got, want)
	}
	if got := MissingInputImages("", []GraphImage{{Name: "ref.png", Dir: "input"}}); len(got) != 1 {
		t.Errorf("a node with no ComfyUI dir holds no input image: %v", got)
	}
}

func TestManifestSourcedModelsAreTheOnesTheNodeFetches(t *testing.T) {
	manifest := `{"models":[
		{"path":"models/diffusion_models/wan/high.safetensors","source_url":"https://example.invalid/high","sha256":"ab"},
		{"path":"models\\vae\\wan_vae.pth","source_url":"https://example.invalid/vae"},
		{"path":"models/loras/nosource.safetensors"},
		{"path":"custom/elsewhere.safetensors","source_url":"https://example.invalid/e"}
	]}`
	got := ManifestSourcedModels([]byte(manifest))
	for _, name := range []string{"wan/high.safetensors", "wan_vae.pth"} {
		if !got[name] {
			t.Errorf("%s is fetched by the satisfier and must be in the set: %v", name, got)
		}
	}
	for _, name := range []string{"nosource.safetensors", "elsewhere.safetensors", "high.safetensors"} {
		if got[name] {
			t.Errorf("%s must not be in the set: %v", name, got)
		}
	}
	if len(ManifestSourcedModels(nil)) != 0 || len(ManifestSourcedModels([]byte(`{`))) != 0 {
		t.Error("no manifest, or one that does not parse, provisions nothing")
	}
}
