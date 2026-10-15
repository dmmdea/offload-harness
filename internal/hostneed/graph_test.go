package hostneed

// A call that runs the caller's OWN graph (`--graph <file>`) is sized by the weight files that graph's loaders name when the lease
// can read the graph and size every one of them (ForGraph, SourceGraphLoaders), and by the media class default otherwise, exactly
// as before. The rule that matters is the second half: a size that is not known never shrinks a declaration.
//
// The model files here are sparse (os.Truncate sets the length, nothing is written) and a few MiB long, so no test holds a real
// weight; the sizes are compared in GiB the way the code sums them.

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const mib = 1 << 20

// sparseTree is a ComfyUI install whose model tree holds one sparse file per entry (a path under models/, its length in bytes),
// returned as the facts of a box that binds Krea 2, so the class default is the family's documented 32.8 GiB (24.5 + 8.3: none
// of its files is in the tree).
func sparseTree(t *testing.T, files map[string]int64) Facts {
	t.Helper()
	dir := t.TempDir()
	for rel, n := range files {
		p := filepath.Join(dir, "models", filepath.FromSlash(rel))
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
	cfg := config.Config{ComfyDir: dir, ImageGenScript: "render/comfy-generate.mjs", ImageGenFamily: "krea2", ImageGenCkpt: "krea2_turbo_bf16.safetensors"}
	return Facts{Cfg: cfg}
}

// writeGraph writes a graph document under a fresh temp dir and returns its path.
func writeGraph(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// krea2DocumentedGiB is what the family table says the bound Krea 2 family takes when none of its files is in the tree: 24.5 + 8.3.
const krea2DocumentedGiB = 24.5 + 8.3

func gibOf(n int64) float64 { return float64(n) / gib }

func sameGiB(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A qwen-image shaped graph: the checkpoint and the text encoder, plus the nodes that name no file.
const twoLoaderGraph = `{
	"1": {"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"qwen_image_fp8.safetensors"}},
	"2": {"class_type":"CLIPLoader","inputs":{"clip_name":"qwen_2.5_vl_7b.safetensors","type":"qwen_image"}},
	"3": {"class_type":"CLIPTextEncode","inputs":{"text":"a lighthouse","clip":["2",0]}},
	"4": {"class_type":"EmptyLatentImage","inputs":{"width":512,"height":512,"batch_size":1}},
	"5": {"class_type":"KSampler","inputs":{"model":["1",0],"seed":7,"steps":20}},
	"6": {"class_type":"SaveImage","inputs":{"filename_prefix":"out","images":["5",0]}}
}`

// (1) THE DEFECT. A graph that names two loader files the box holds declares their summed size, with the source that says it
// came from the graph's own loaders, and not the class default (here the bound family's 32.8 GiB): the incident was a 512x512
// graph that loads about 23 GiB declared as the 32.89 GiB of the largest family the box binds.
func TestAGraphNamingTwoLoaderFilesDeclaresTheirSum(t *testing.T) {
	const ckpt, clip = 6 * mib, 16 * mib
	f := sparseTree(t, map[string]int64{"checkpoints/qwen_image_fp8.safetensors": ckpt, "text_encoders/qwen_2.5_vl_7b.safetensors": clip})
	want := gibOf(ckpt) + gibOf(clip)
	if class := ClassDefault(f); !near(class.GiB, krea2DocumentedGiB) {
		t.Fatalf("the control: this box's class default is the bound family's documented 32.8 GiB, got %v", class)
	}

	graph := writeGraph(t, twoLoaderGraph)
	n, ok := ForGraphFile(graph, f)
	if !ok || n.Source != SourceGraphLoaders || !sameGiB(n.GiB, want) {
		t.Fatalf("ForGraphFile = %v %v, want %.9f GiB from %q", n, ok, want, SourceGraphLoaders)
	}
	for _, name := range []string{"qwen_image_fp8.safetensors", "qwen_2.5_vl_7b.safetensors", "loaders"} {
		if !strings.Contains(n.Detail, name) {
			t.Errorf("the detail names what the figure came from (%q): %q", name, n.Detail)
		}
	}
	if byBytes, ok := ForGraph([]byte(twoLoaderGraph), f); !ok || byBytes != n {
		t.Errorf("ForGraph (the bytes) = %v %v, want the same as the file's: %v", byBytes, ok, n)
	}

	// Through the order of the package comment: the helper's --graph reaches the same figure, on a media lease and on a text one.
	for _, class := range []gpulease.Class{gpulease.ClassMedia, gpulease.ClassText} {
		for _, args := range [][]string{graphCall("comfy-render.mjs", "--graph", graph), graphCall("comfy-render.mjs", "--graph="+graph)} {
			got := Resolve(Request{Class: class, Args: args}, f)
			if got != n {
				t.Errorf("Resolve(%s, %q) = %v, want %v", class, args, got, n)
			}
		}
	}
	// The helper that renders video takes the same road.
	if got := Resolve(Request{Class: gpulease.ClassMedia, Args: graphCall("comfy-video.mjs", "--graph", graph)}, f); got != n {
		t.Errorf("comfy-video --graph = %v, want %v", got, n)
	}
}

// The figure is counted in full whatever the card holds: a graph's placement and load order are its own, so the lease cannot say
// its files sit on a card (the family estimate may, it knows the family's recipe). Nor does a file counted twice by two nodes
// count twice, and a VAE or a LoRA is counted: the graph names it.
func TestAGraphIsCountedInFullDeduplicatedAndWithItsSmallFiles(t *testing.T) {
	const ckpt, vae, lora = 6 * mib, 1 * mib, 2 * mib
	f := sparseTree(t, map[string]int64{
		"checkpoints/ck.safetensors": ckpt, "vae/ae.safetensors": vae, "loras/sub/style.safetensors": lora,
	})
	f.VRAMGiB = 96 // a card these files would sit on, ten thousand times over
	graph := `{
		"1": {"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"ck.safetensors"}},
		"2": {"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"ck.safetensors"}},
		"3": {"class_type":"VAELoader","inputs":{"vae_name":"ae.safetensors"}},
		"4": {"class_type":"LoraLoader","inputs":{"lora_name":"sub/style.safetensors","strength_model":1}}
	}`
	n, ok := ForGraph([]byte(graph), f)
	want := gibOf(ckpt) + gibOf(vae) + gibOf(lora)
	if !ok || n.Source != SourceGraphLoaders || !sameGiB(n.GiB, want) {
		t.Fatalf("ForGraph = %v %v, want %.9f GiB (the checkpoint once, the VAE and the LoRA)", n, ok, want)
	}
}

// (2) ONE named file the lease cannot resolve and the figure is the class default, unchanged: the sizes of the others say nothing
// about the one it does not know.
func TestAGraphWithOneUnresolvedFileKeepsTheClassDefault(t *testing.T) {
	f := sparseTree(t, map[string]int64{"checkpoints/qwen_image_fp8.safetensors": 6 * mib}) // the text encoder is not in the tree
	graph := writeGraph(t, twoLoaderGraph)
	if n, ok := ForGraphFile(graph, f); ok {
		t.Fatalf("ForGraphFile sized a graph with a file it could not find: %v", n)
	}
	want := ClassDefault(f)
	if want.Source != SourceClassDefault || want.GiB < 30 {
		t.Fatalf("the control: the class default of this box is Krea 2's documented 32.8 GiB, got %v", want)
	}
	got := Resolve(Request{Class: gpulease.ClassMedia, Args: graphCall("comfy-render.mjs", "--graph", graph)}, f)
	if got != want {
		t.Errorf("a graph with one file the box does not hold = %v, want the class default exactly: %v", got, want)
	}
}

// A file of the same name that is empty, or a name that climbs out of the model tree, is not a size either.
func TestAGraphWhoseFileIsEmptyOrOutsideTheTreeKeepsTheClassDefault(t *testing.T) {
	f := sparseTree(t, map[string]int64{"checkpoints/ck.safetensors": 6 * mib, "checkpoints/empty.safetensors": 0})
	outside := filepath.Join(f.Cfg.ComfyDir, "outside.safetensors")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, second := range map[string]string{
		"an empty file":          "empty.safetensors",
		"a name that climbs out": "../../outside.safetensors",
	} {
		graph := `{"1":{"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"ck.safetensors"}},
			"2":{"class_type":"VAELoader","inputs":{"vae_name":"` + second + `"}}}`
		if n, ok := ForGraph([]byte(graph), f); ok {
			t.Errorf("%s: the graph was sized (%v) with a file that is not a size", name, n)
		}
	}
}

// (3) A document the lease cannot take for an API-format graph is the class default: not JSON, not an object, a UI-format
// workflow (its nodes are a list and its weights sit in widgets), an empty file, a directory, a missing file, a file too large to
// be a graph.
func TestAGraphThatCannotBeReadKeepsTheClassDefault(t *testing.T) {
	f := sparseTree(t, map[string]int64{"checkpoints/qwen_image_fp8.safetensors": 6 * mib, "text_encoders/qwen_2.5_vl_7b.safetensors": 16 * mib})
	want := ClassDefault(f)

	big := twoLoaderGraph + strings.Repeat(" ", maxGraphBytes) // valid JSON, and the two loaders resolve: only its length is wrong
	for name, body := range map[string]string{
		"not JSON":              "this is not a graph",
		"a JSON array":          `[{"class_type":"CheckpointLoaderSimple"}]`,
		"a JSON string":         `"qwen_image_fp8.safetensors"`,
		"a UI-format workflow":  `{"last_node_id":3,"nodes":[{"id":1,"type":"CheckpointLoaderSimple","widgets_values":["qwen_image_fp8.safetensors"]}],"links":[]}`,
		"an empty file":         "",
		"a graph over the size": big,
	} {
		p := writeGraph(t, body)
		if n, ok := ForGraphFile(p, f); ok {
			t.Errorf("%s: sized as %v", name, n)
		}
		got := Resolve(Request{Class: gpulease.ClassMedia, Args: graphCall("comfy-render.mjs", "--graph", p)}, f)
		if got != want {
			t.Errorf("%s: Resolve = %v, want the class default %v", name, got, want)
		}
	}
	if _, ok := ForGraphFile(t.TempDir(), f); ok {
		t.Error("a directory is not a graph")
	}
	if _, ok := ForGraphFile(filepath.Join(t.TempDir(), "absent.json"), f); ok {
		t.Error("a graph that does not exist is not sized")
	}
	if _, ok := ForGraphFile("", f); ok {
		t.Error("no path, no graph")
	}
	// the control: the same graph under the size cap IS sized
	if n, ok := ForGraph([]byte(twoLoaderGraph), f); !ok || n.Source != SourceGraphLoaders {
		t.Errorf("the control graph is sized, got %v %v", n, ok)
	}
}

// (4) A graph that names no weight file has nothing to sum: a figure of 0 would be a declaration that the graph needs no host
// RAM, which the lease does not know. It is the class default.
func TestAGraphThatNamesNoLoaderKeepsTheClassDefault(t *testing.T) {
	f := sparseTree(t, map[string]int64{"checkpoints/ck.safetensors": 6 * mib})
	want := ClassDefault(f)
	for name, graph := range map[string]string{
		"an empty object": `{}`,
		"nodes without a file": `{"1":{"class_type":"EmptyLatentImage","inputs":{"width":512}},
			"2":{"class_type":"SaveImage","inputs":{"filename_prefix":"model.safetensors","images":["1",0]}}}`,
		"only an input image":   `{"1":{"class_type":"LoadImage","inputs":{"image":"photo.png"}}}`,
		"a loader fed by a link": `{"1":{"class_type":"UNETLoader","inputs":{"unet_name":["9",0]}}}`,
		"a builtin vae":          `{"1":{"class_type":"VAELoader","inputs":{"vae_name":"builtin"}}}`,
	} {
		if n, ok := ForGraph([]byte(graph), f); ok {
			t.Errorf("%s: sized as %v", name, n)
		}
		p := writeGraph(t, graph)
		got := Resolve(Request{Class: gpulease.ClassMedia, Args: graphCall("comfy-render.mjs", "--graph", p)}, f)
		if got != want {
			t.Errorf("%s: Resolve = %v, want the class default %v", name, got, want)
		}
	}
}

// A box with no ComfyUI install holds no model: a graph that names a file cannot be sized there, and the class default (0, nothing
// bound) stands, as before.
func TestAGraphOnABoxWithNoComfyInstallKeepsTheClassDefault(t *testing.T) {
	f := Facts{}
	if n, ok := ForGraph([]byte(twoLoaderGraph), f); ok {
		t.Fatalf("a box with no model tree sized a graph: %v", n)
	}
	graph := writeGraph(t, twoLoaderGraph)
	got := Resolve(Request{Class: gpulease.ClassMedia, Args: graphCall("comfy-render.mjs", "--graph", graph)}, f)
	if got.Source != SourceNone || got.GiB != 0 {
		t.Errorf("Resolve = %v, want the class default of a box that binds nothing: 0, none", got)
	}
}

// The operator's --ram still wins over everything, and a call without --graph is still sized by its own family.
func TestAnExplicitRamStillBeatsAGraphsLoaders(t *testing.T) {
	f := sparseTree(t, map[string]int64{"checkpoints/qwen_image_fp8.safetensors": 6 * mib, "text_encoders/qwen_2.5_vl_7b.safetensors": 16 * mib})
	graph := writeGraph(t, twoLoaderGraph)
	ram := 3.5
	got := Resolve(Request{Explicit: &ram, Class: gpulease.ClassMedia, Args: graphCall("comfy-render.mjs", "--graph", graph)}, f)
	if got.Source != SourceExplicit || got.GiB != 3.5 {
		t.Errorf("Resolve = %v, want the explicit 3.5 GiB", got)
	}
}

// The sizes come through Facts.Stat when a caller gives one, as every other estimate has it, so a test of a caller can say the
// incident's files without writing them: the 6.76 GiB checkpoint and the 16.33 GiB text encoder of the measured graph are
// 23.09 GiB, where the class default of the box that binds Krea 2 is 32.8.
func TestTheIncidentsGraphIsItsOwnFilesNotTheLargestFamilyOfTheBox(t *testing.T) {
	dir, stat := comfyTree(t, map[string]float64{"qwen_image_fp8.safetensors": 6.76, "qwen_2.5_vl_7b.safetensors": 16.33})
	f := Facts{Cfg: config.Config{ComfyDir: dir, ImageGenScript: "render/comfy-generate.mjs", ImageGenFamily: "krea2", ImageGenCkpt: "krea2_turbo_bf16.safetensors"}, VRAMGiB: 16, Stat: stat}
	n, ok := ForGraph([]byte(twoLoaderGraph), f)
	if !ok || n.Source != SourceGraphLoaders || !near(n.GiB, 6.76+16.33) {
		t.Fatalf("ForGraph = %v %v, want %.2f GiB", n, ok, 6.76+16.33)
	}
	if class := ClassDefault(f); class.GiB <= n.GiB {
		t.Fatalf("the control: the class default (%v) is the larger figure the defect declared", class)
	}
}

// graphCall is the wrapped command of a render helper that runs the caller's own graph, one argument per element: a path with a
// space in it is one argument, as the shell would pass it.
func graphCall(helper string, graphArgs ...string) []string {
	return append(strings.Fields("node render/"+helper+" out"), graphArgs...)
}
