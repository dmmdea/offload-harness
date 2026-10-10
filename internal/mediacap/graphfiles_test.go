package mediacap

// What a ComfyUI API-format graph names that a node must HOLD to run it (graphfiles.go): the weight files its loaders open
// and the input files its input loaders read (an image, a video, an audio clip). The graphs here are small and written out,
// not generated: the point of each is one rule of the scan.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
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
		"13": {"class_type":"UNETLoader",      "inputs":{"unet_name":["99",0]}},
		"14": {"class_type":"ModelPatchLoader","inputs":{"name":"qwen_image_inpaint_diffsynth_controlnet.safetensors"}},
		"15": {"class_type":"WanVideoModelLoader","inputs":{"model":"wan2.2_i2v_low_noise_14B_fp8.safetensors","base_precision":"bf16"}},
		"16": {"class_type":"IPAdapterModelLoader","inputs":{"ipadapter_file":"ip-adapter.safetensors"}},
		"17": {"class_type":"PrimitiveString", "inputs":{"value":"primitive_fed.safetensors"}},
		"18": {"class_type":"SomeDownloader",  "inputs":{"url":"https://example.invalid/fetched.safetensors"}}
	}`
	got, err := ScanGraph([]byte(graph))
	if err != nil {
		t.Fatal(err)
	}
	// Any string input whose value ends in a weight extension is a file the node must hold, whatever the key is called: a
	// loader names its file under `name` (ModelPatchLoader), `model` (a wrapper's loader), `ipadapter_file` or a primitive's
	// `value`, and a graph the scan misses is a graph a node without the file takes a lease, cold-starts ComfyUI and defers.
	// An output prefix (filename_prefix) is a name to write, and a URL is fetched, not held: neither is a file to hold.
	want := []string{
		"a.GGUF", "b.sft", "old.ckpt", "umt5_xxl_fp8.safetensors", "wan/wan2.2_i2v_high_noise_14B_fp8.safetensors", "wan_2.1_vae.pth", "lightx2v.pt",
		"qwen_image_inpaint_diffsynth_controlnet.safetensors", "wan2.2_i2v_low_noise_14B_fp8.safetensors", "ip-adapter.safetensors", "primitive_fed.safetensors",
	}
	// Order is by node id then input key (the graph's keys are unordered JSON), so a refusal names the same file on every run.
	if !sameSet(got.Models, want) {
		t.Fatalf("Models = %v, want the set %v", got.Models, want)
	}
	if again, _ := ScanGraph([]byte(graph)); !reflect.DeepEqual(got, again) {
		t.Errorf("a scan must be deterministic: %v then %v", got.Models, again.Models)
	}
	if len(got.Inputs) != 0 {
		t.Errorf("no input loader in this graph: %v", got.Inputs)
	}
}

// The price of reading every key: a text input that itself ends in a weight extension reads as a file name. That costs a
// re-placeable 503 naming it (the graph still runs on this machine), which is the cheaper side of the trade against a
// graph whose loader keys the scan does not know. The test pins it so the trade is a decision and not an accident.
func TestScanGraphReadsATextThatEndsInAWeightExtensionAsAFile(t *testing.T) {
	got, err := ScanGraph([]byte(`{"1":{"class_type":"CLIPTextEncode","inputs":{"text":"my_lora.safetensors"}}}`))
	if err != nil || !reflect.DeepEqual(got.Models, []string{"my_lora.safetensors"}) {
		t.Fatalf("got %+v, %v; want the text read as a file name", got, err)
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
		"7": {"class_type":"PreviewImage",    "inputs":{"images":["1",0]}},
		"8": {"class_type":"SomeCustomNode",  "inputs":{"image":"not-a-loader-input.png"}},
		"9": {"class_type":"LoadVideo",       "inputs":{"file":"driver.mp4"}},
		"10":{"class_type":"LoadAudio",       "inputs":{"audio":"voice.flac [temp]"}},
		"11":{"class_type":"LoadVideo",       "inputs":{"file":["9",0]}},
		"12":{"class_type":"LoadAudio",       "inputs":{"file":"wrong-key-for-audio.flac"}}
	}`
	got, err := ScanGraph([]byte(graph))
	if err != nil {
		t.Fatal(err)
	}
	// LoadVideo reads its `file` and LoadAudio its `audio`, annotated the same way: they are inputs a node must hold as well.
	// Node ids sort as strings, so "10" comes right after "1".
	want := []GraphInput{
		{Name: "ref.png", Dir: "input"}, {Name: "voice.flac", Dir: "temp"}, {Name: "mask.png", Dir: "input"}, {Name: "prev/step.png", Dir: "output"},
		{Name: "kept/in.png", Dir: "input"}, {Name: "driver.mp4", Dir: "input"},
	}
	if !reflect.DeepEqual(got.Inputs, want) {
		t.Fatalf("Inputs = %+v, want %+v", got.Inputs, want)
	}
}

func TestScanGraphToleratesWhatItDoesNotRead(t *testing.T) {
	for name, graph := range map[string]string{
		"a node that is not an object":                       `{"1": 7, "2": {"class_type":"X"}}`,
		"inputs that are not an object":                      `{"1": {"class_type":"UNETLoader","inputs":"x"}}`,
		"a node with no inputs":                              `{"1": {"class_type":"X"}}`,
		"a value that is a number":                           `{"1": {"class_type":"X","inputs":{"unet_name":3}}}`,
		"a name with no known extension":                     `{"1": {"class_type":"X","inputs":{"unet_name":"model.bin"}}}`,
		"an input with no name on a class that is no loader": `{"1": {"class_type":"X","inputs":{"":"x.png"}}}`,
	} {
		got, err := ScanGraph([]byte(graph))
		if err != nil || len(got.Models) != 0 || len(got.Inputs) != 0 {
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
	// Every name from the first to `\etc\x.safetensors` would resolve to a file that EXISTS if it were joined to a class
	// directory as written, so only the refusal to look it up keeps it missing. The last three (a drive path, an empty and a
	// blank name) have no file to point at on this machine; TestSafeRelRefusesWhatCannotBeAFileInAClassDirectory pins that
	// safeRel refuses them by itself.
	outside := tree(t, "secret.safetensors")
	dir := tree(t, "models/vae/ok.safetensors", "secret.safetensors", "models/vae/etc/x.safetensors")
	roots := ModelRoots(dir)
	names := []string{
		"../../secret.safetensors", // out of models/vae to the ComfyUI dir
		`..\..\secret.safetensors`,
		"vae/../../../secret.safetensors",
		filepath.ToSlash(filepath.Join(outside, "secret.safetensors")),
		"/etc/x.safetensors",
		`\etc\x.safetensors`,
		`C:\x.safetensors`,
		"",
		"   ",
	}
	if got := MissingModelFiles(roots, names); !reflect.DeepEqual(got, names) {
		t.Fatalf("every name outside the roots must be missing: got %v", got)
	}
	if got := MissingModelFiles(roots, []string{"ok.safetensors"}); len(got) != 0 {
		t.Fatalf("control: a plain name is held: %v", got)
	}
}

// safeRel is the only thing between a name a peer sent and a stat call, so it is tested on its own: ok for any of these would
// let a lookup read a path that is not one file name inside the class directory.
func TestSafeRelRefusesWhatCannotBeAFileInAClassDirectory(t *testing.T) {
	for _, n := range []string{
		"", "   ", "../x.safetensors", `..\x.safetensors`, "a/../../x.safetensors", "/x.safetensors", `\x.safetensors`,
		"//host/share/x.safetensors", `C:\x.safetensors`, "C:x.safetensors", "c:/x.safetensors", "a\x00b.safetensors",
	} {
		if got, ok := safeRel(n); ok {
			t.Errorf("safeRel(%q) = %q, true; want a refusal", n, got)
		}
	}
	for n, want := range map[string]string{
		"x.safetensors": "x.safetensors", "wan/high.safetensors": "wan/high.safetensors", `wan\high.safetensors`: "wan/high.safetensors",
		"a..b.safetensors": "a..b.safetensors", " my lora v2.safetensors ": "my lora v2.safetensors",
	} {
		if got, ok := safeRel(n); !ok || got != want {
			t.Errorf("safeRel(%q) = %q, %v; want %q, true", n, got, ok, want)
		}
	}
}

// dirLinks are the ways a class directory can be a link to a directory elsewhere (a models tree kept on a second volume and
// linked into place). os.ReadDir reports such an entry as not a directory, so enumerating class directories by IsDir alone
// drops it, and the node refuses graphs whose files it holds.
var dirLinks = map[string]func(link, target string) error{
	"symlink": os.Symlink,
	"junction": func(link, target string) error {
		if runtime.GOOS != "windows" {
			return os.ErrInvalid
		}
		return exec.Command("cmd", "/c", "mklink", "/J", link, target).Run()
	},
}

func TestAClassDirectoryThatIsALinkIsStillAClassDirectory(t *testing.T) {
	for kind, link := range dirLinks {
		t.Run(kind, func(t *testing.T) {
			store := tree(t, "ckpts/m.safetensors")
			dir := t.TempDir()
			models := filepath.Join(dir, "models")
			if err := os.MkdirAll(filepath.Join(models, "vae"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(models, "vae", "v.safetensors"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(models, "README.txt"), []byte("not a class"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := link(filepath.Join(models, "checkpoints"), filepath.Join(store, "ckpts")); err != nil {
				t.Skipf("this host cannot make a %s here: %v", kind, err)
			}
			roots := ModelRoots(dir)
			var classes []string
			for _, r := range roots {
				for _, cd := range classDirs(r) {
					classes = append(classes, cd[0])
				}
			}
			sort.Strings(classes)
			if !reflect.DeepEqual(classes, []string{"checkpoints", "vae"}) {
				t.Fatalf("class directories = %v, want checkpoints (the %s) and vae, and not the README", classes, kind)
			}
			if got := MissingModelFiles(roots, []string{"m.safetensors", "v.safetensors"}); len(got) != 0 {
				t.Fatalf("a file reached through a %s is held: missing %v", kind, got)
			}
		})
	}
}

func TestMissingModelFilesOnANodeWithNoModelsRoot(t *testing.T) {
	if got := MissingModelFiles(nil, []string{"a.safetensors"}); !reflect.DeepEqual(got, []string{"a.safetensors"}) {
		t.Fatalf("a node with no models root holds no model file: %v", got)
	}
}

func TestMissingInputFilesLooksInTheDirTheNameSays(t *testing.T) {
	dir := tree(t, "input/ref.png", "input/kept/in.png", "output/prev/step.png")
	got := MissingInputFiles(dir, []GraphInput{
		{Name: "ref.png", Dir: "input"},
		{Name: "kept/in.png", Dir: "input"},
		{Name: "prev/step.png", Dir: "output"},
		{Name: "step.png", Dir: "input"},         // exists only under output/prev
		{Name: "prev/step.png", Dir: "input"},    // exists only under output
		{Name: "../input/ref.png", Dir: "input"}, // climbs
		{Name: "x.png", Dir: "temp"},
	})
	want := []GraphInput{
		{Name: "step.png", Dir: "input"}, {Name: "prev/step.png", Dir: "input"}, {Name: "../input/ref.png", Dir: "input"}, {Name: "x.png", Dir: "temp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %+v, want %+v", got, want)
	}
	if got := MissingInputFiles("", []GraphInput{{Name: "ref.png", Dir: "input"}}); len(got) != 1 {
		t.Errorf("a node with no ComfyUI dir holds no input file: %v", got)
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

// A graph can name thousands of files, so the check lists each models root once per call and not once per name: the listing of
// a root is the dear part of a lookup (a directory read, plus a stat for every entry that is not itself a directory), and a
// name's own cost is then one stat per class directory. The answer is the one a name-by-name resolve gives.
func TestMissingModelFilesListsEachRootOnceNotOncePerName(t *testing.T) {
	dir := tree(t,
		"models/vae/v0.safetensors", "models/loras/l0.safetensors", "models/checkpoints/c0.ckpt", "models/diffusion_models/wan/d0.gguf",
		"models/text_encoders/t0.safetensors", "models/clip_vision/cv0.sft",
	)
	other := tree(t, "weights/diff/big.safetensors")
	yaml := "other:\n  base_path: " + filepath.ToSlash(other) + "\n  diffusion_models: weights/diff\n"
	if err := os.WriteFile(filepath.Join(dir, "extra_model_paths.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := ModelRoots(dir)
	if len(roots) != 2 {
		t.Fatalf("the fixture wants a models root and one extra root, got %d: %+v", len(roots), roots)
	}

	var names []string
	for i := 0; i < 400; i++ {
		names = append(names, fmt.Sprintf("absent-%03d.safetensors", i)) // the long list of a workflow this node was never meant to run
	}
	names = append(names, "v0.safetensors", "l0.safetensors", "c0.ckpt", "wan/d0.gguf", "t0.safetensors", "cv0.sft", "big.safetensors", "../v0.safetensors")

	// What a name-by-name resolve says, the way the check worked before: each name asks every root for its class directories.
	var classes []string
	for _, r := range roots {
		for _, cd := range classDirs(r) {
			classes = append(classes, cd[0])
		}
	}
	var want []string
	for _, n := range names {
		rel, ok := safeRel(n)
		if ok {
			_, ok = ResolveModelFile(roots, ModelFile{Name: rel, Classes: classes}, nil)
		}
		if !ok {
			want = append(want, n)
		}
	}

	var reads []string
	prev := readDir
	readDir = func(name string) ([]os.DirEntry, error) { reads = append(reads, name); return os.ReadDir(name) }
	t.Cleanup(func() { readDir = prev })

	got := MissingModelFiles(roots, names)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the answer changed: got %d missing, want %d\n got  %v\n want %v", len(got), len(want), got, want)
	}
	if len(got) != 401 {
		t.Fatalf("control: 400 absent names and the one that climbs are missing, the 7 held names are not; got %d", len(got))
	}
	// One models root lists its subdirectories (the extra root names its class directories in the yaml and lists nothing).
	if len(reads) != 1 {
		t.Errorf("a call lists each models root once; %d names caused %d directory reads", len(names), len(reads))
	}
}

// The cost of the check is bounded by what the refusal shows, not by the graph: a door open to any peer takes a graph of
// thousands of names, and a name that is missing costs a stat in every class directory. The search stops at the seventh missing
// name when six are shown, and says there was more.
func TestMissingModelFilesUpToStopsLookingPastTheLimit(t *testing.T) {
	dir := tree(t, "models/vae/v0.safetensors", "models/loras/l0.safetensors", "models/checkpoints/c0.ckpt", "models/unet/u0.gguf")
	roots := ModelRoots(dir)
	var classDirCount int
	for _, r := range roots {
		classDirCount += len(classDirs(r))
	}
	if classDirCount != 4 {
		t.Fatalf("the fixture has 4 class directories, got %d", classDirCount)
	}

	var names []string
	for i := 0; i < 5000; i++ {
		names = append(names, fmt.Sprintf("absent-%04d.safetensors", i))
	}
	names = append(names, "v0.safetensors") // held, but after the seventh missing name: never reached

	var stats, reads int
	prevStat, prevRead := statModel, readDir
	statModel = func(p string) (int64, bool) { stats++; return prevStat(p) }
	readDir = func(name string) ([]os.DirEntry, error) { reads++; return os.ReadDir(name) }
	t.Cleanup(func() { statModel, readDir = prevStat, prevRead })

	got, more := MissingModelFilesUpTo(roots, names, 6)
	if !more || !reflect.DeepEqual(got, names[:6]) {
		t.Fatalf("want the first six missing names and more=true, got %v more=%v", got, more)
	}
	// Seven names were looked up (six listed, the seventh proves there is more), each in every class directory.
	if want := 7 * classDirCount; stats != want {
		t.Errorf("%d names in the graph cost %d stats, want %d (seven names x %d class directories)", len(names), stats, want, classDirCount)
	}
	if reads != 1 {
		t.Errorf("the models root is listed once, got %d directory reads", reads)
	}

	// Exactly the limit missing is not "more", fewer is not either, and a limit of 0 asks for them all.
	for _, tc := range []struct {
		name  string
		names []string
		limit int
		want  []string
		more  bool
	}{
		{"exactly the limit", []string{"v0.safetensors", "x1.safetensors", "x2.safetensors"}, 2, []string{"x1.safetensors", "x2.safetensors"}, false},
		{"one over the limit", []string{"x1.safetensors", "v0.safetensors", "x2.safetensors", "x3.safetensors"}, 2, []string{"x1.safetensors", "x2.safetensors"}, true},
		{"below the limit", []string{"x1.safetensors"}, 2, []string{"x1.safetensors"}, false},
		{"none missing", []string{"v0.safetensors", "l0.safetensors"}, 2, nil, false},
		{"no limit", []string{"x1.safetensors", "x2.safetensors", "x3.safetensors"}, 0, []string{"x1.safetensors", "x2.safetensors", "x3.safetensors"}, false},
		{"an unsafe name is missing and counts", []string{"../v0.safetensors", "x1.safetensors", "x2.safetensors"}, 2, []string{"../v0.safetensors", "x1.safetensors"}, true},
	} {
		got, more := MissingModelFilesUpTo(roots, tc.names, tc.limit)
		if !reflect.DeepEqual(got, tc.want) || more != tc.more {
			t.Errorf("%s: got %v more=%v, want %v more=%v", tc.name, got, more, tc.want, tc.more)
		}
	}
}
