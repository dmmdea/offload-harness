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
	"github.com/dmmdea/offload-harness/internal/mediacap"
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

// On a card that is not known the figure is counted in full (nothing is assumed to fit). A file named by two nodes does not
// count twice, and a VAE or a LoRA is counted: the graph names it.
func TestAGraphIsCountedInFullDeduplicatedAndWithItsSmallFiles(t *testing.T) {
	const ckpt, vae, lora = 6 * mib, 1 * mib, 2 * mib
	f := sparseTree(t, map[string]int64{
		"checkpoints/ck.safetensors": ckpt, "vae/ae.safetensors": vae, "loras/sub/style.safetensors": lora,
	})
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

// (4) A graph that names no weight file has nothing to sum, and a figure of 0 would declare that it needs no host RAM, which the lease
// does not know: an API-format graph with no loader node at all is the measured ComfyUI process floor (3 GiB, 4 GiB of commit stated), and
// any doubt (no node, a loader that names no file, a loader fed by a link) is the class default.
func TestAGraphThatNamesNoLoaderKeepsTheClassDefault(t *testing.T) {
	f := sparseTree(t, map[string]int64{"checkpoints/ck.safetensors": 6 * mib})
	want := ClassDefault(f)
	for name, graph := range map[string]string{
		"an empty object": `{}`,
		"a loader fed by a link": `{"1":{"class_type":"UNETLoader","inputs":{"unet_name":["9",0]}}}`,
		"a builtin vae":          `{"1":{"class_type":"VAELoader","inputs":{"vae_name":"builtin"}}}`,
		"a weight under another extension": `{"1":{"class_type":"SaveImage","inputs":{"filename_prefix":"x","thing":"w.onnx"}}}`,
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
	// the floor: nodes that name no weight file, run as a render helper's --graph or as render/comfy-run-graph.mjs --graph
	for name, graph := range map[string]string{
		"nodes without a file": `{"1":{"class_type":"EmptyLatentImage","inputs":{"width":512}},
			"2":{"class_type":"SaveImage","inputs":{"filename_prefix":"model.safetensors","images":["1",0]}}}`,
		"only an input image": `{"1":{"class_type":"LoadImage","inputs":{"image":"photo.png"}}}`,
		"a small gif graph":   `{"1":{"class_type":"VHS_LoadImages","inputs":{"directory":"in"}},"2":{"class_type":"VHS_VideoCombine","inputs":{"images":["1",0],"format":"image/gif"}}}`,
	} {
		p := writeGraph(t, graph)
		for _, args := range [][]string{graphCall("comfy-render.mjs", "--graph", p), graphCall("comfy-run-graph.mjs", "--graph", p, "--manifest", "m.json")} {
			got := Resolve(Request{Class: gpulease.ClassMedia, Args: args}, f)
			if got.GiB != 3 || got.CommitGiB != 4 || !got.CommitStated || got.Source != SourceMeasured {
				t.Errorf("%s (%s): Resolve = %v, want the process floor of 3 GiB with 4 GiB of commit stated", name, args[1], got)
			}
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

// A graph is sized by the family estimate's own rule (fitToCard): the set is counted in full when together it does not fit the
// card (the largest card of the lease, less the runner's reserve), and a set that fits streams nothing from RAM and declares 0,
// with the same wording. The incident's 6.76 + 16.33 GiB graph is 23.09 GiB.
func TestAGraphThatFitsTheCardDeclaresZeroAndOneThatDoesNotDeclaresItsSum(t *testing.T) {
	dir, stat := comfyTree(t, map[string]float64{"qwen_image_fp8.safetensors": 6.76, "qwen_2.5_vl_7b.safetensors": 16.33})
	f := Facts{Cfg: config.Config{ComfyDir: dir}, Stat: stat}

	f.VRAMGiB = 32 // 31 usable: 23.09 fits
	n, ok := ForGraph([]byte(twoLoaderGraph), f)
	if !ok || n.Source != SourceGraphLoaders || n.GiB != 0 {
		t.Fatalf("on a 32 GiB card the graph fits: got %v %v, want 0 GiB from %q", n, ok, SourceGraphLoaders)
	}
	for _, want := range []string{"fits the 32.0 GiB card", "no weights stream from RAM", "qwen_image_fp8.safetensors"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail says %q, got %q", want, n.Detail)
		}
	}

	f.VRAMGiB = 16 // 15 usable: 23.09 does not
	n, ok = ForGraph([]byte(twoLoaderGraph), f)
	if !ok || n.Source != SourceGraphLoaders || !near(n.GiB, 6.76+16.33) {
		t.Fatalf("on a 16 GiB card the graph does not fit: got %v %v, want %.2f GiB", n, ok, 6.76+16.33)
	}
	for _, want := range []string{"does not fit the 16.0 GiB card", "so the weights stream from RAM"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail says %q, got %q", want, n.Detail)
		}
	}

	f.VRAMGiB = 0 // an unknown card counts everything
	if n, _ := ForGraph([]byte(twoLoaderGraph), f); !near(n.GiB, 6.76+16.33) {
		t.Errorf("an unknown card counts the sum, got %v", n)
	}
}

// The edge is the family estimate's: a set that EQUALS the usable VRAM fits, and the reserve is the runner's (--reserve-vram, else 1.0).
func TestAGraphFitsTheCardExactlyAtItsUsableVRAM(t *testing.T) {
	dir, stat := comfyTree(t, map[string]float64{"qwen_image_fp8.safetensors": 6, "qwen_2.5_vl_7b.safetensors": 10})
	f := Facts{Cfg: config.Config{ComfyDir: dir}, Stat: stat, VRAMGiB: 17} // 17 - 1.0 reserve = 16 = the set
	if n, ok := ForGraph([]byte(twoLoaderGraph), f); !ok || n.GiB != 0 {
		t.Errorf("a 16 GiB set on 16 GiB usable fits: got %v %v", n, ok)
	}
	f.VRAMGiB = 16.9
	if n, ok := ForGraph([]byte(twoLoaderGraph), f); !ok || !near(n.GiB, 16) {
		t.Errorf("a 16 GiB set on 15.9 GiB usable does not fit: got %v %v", n, ok)
	}
	f.VRAMGiB, f.ReserveVRAMGiB = 17, 2 // the runner holds back 2 GiB: 15 usable
	if n, ok := ForGraph([]byte(twoLoaderGraph), f); !ok || !near(n.GiB, 16) {
		t.Errorf("with --reserve-vram 2 the set does not fit: got %v %v", n, ok)
	}
}

// The graph and the family estimate give one verdict on the same files: there is one rule, not a copy of it.
func TestAGraphAndTheFamilyEstimateAgreeOnTheSameFiles(t *testing.T) {
	dir, stat := comfyTree(t, map[string]float64{"a.safetensors": 6, "b.safetensors": 10})
	graph := []byte(`{"1":{"class_type":"UNETLoader","inputs":{"unet_name":"a.safetensors"}},"2":{"class_type":"CLIPLoader","inputs":{"clip_name":"b.safetensors"}}}`)
	files := []mediacap.ModelFile{
		{Label: "unet", Name: "a.safetensors", Classes: []string{"diffusion_models", "unet"}, Role: mediacap.RoleDiffusion},
		{Label: "clip", Name: "b.safetensors", Classes: []string{"text_encoders", "clip"}, Role: mediacap.RoleTextEncoder},
	}
	for _, vram := range []float64{0, 12, 16.9, 17, 40} {
		f := Facts{Cfg: config.Config{ComfyDir: dir}, Stat: stat, VRAMGiB: vram}
		g, gok := ForGraph(graph, f)
		e, eok := estimate("x", files, f)
		if !gok || !eok || g.GiB != e.GiB {
			t.Errorf("on a %.1f GiB card the graph says %v %v and the family estimate %v %v: they must agree", vram, g.GiB, gok, e.GiB, eok)
		}
	}
}

// --reserve-vram is the runner's own margin, and a graph call is fitted with it like every other helper call: a 25 GiB card holds
// the incident's 23.09 GiB set with the default 1.0 GiB held back (24 usable), and not with --reserve-vram 6 (19 usable), where
// 4 GiB would stream from RAM undeclared.
func TestAGraphIsFittedWithTheReserveVRAMTheCallStates(t *testing.T) {
	dir, stat := comfyTree(t, map[string]float64{"qwen_image_fp8.safetensors": 6.76, "qwen_2.5_vl_7b.safetensors": 16.33})
	f := Facts{Cfg: config.Config{ComfyDir: dir}, Stat: stat, VRAMGiB: 25}
	graph := writeGraph(t, twoLoaderGraph)
	for _, c := range []struct {
		name string
		args []string
		want float64
	}{
		{"no flag: 24 GiB usable, the set fits", graphCall("comfy-render.mjs", "--graph", graph), 0},
		{"--reserve-vram 6: 19 usable, it does not", graphCall("comfy-render.mjs", "--graph", graph, "--reserve-vram", "6"), 6.76 + 16.33},
		{"--reserve-vram=6 and --graph= forms", graphCall("comfy-render.mjs", "--graph="+graph, "--reserve-vram=6"), 6.76 + 16.33},
		{"a reserve that is not a number is the default", graphCall("comfy-render.mjs", "--graph", graph, "--reserve-vram", "lots"), 0},
		{"a reserve of 0 is the default", graphCall("comfy-render.mjs", "--graph", graph, "--reserve-vram", "0"), 0},
	} {
		n := Resolve(Request{Class: gpulease.ClassMedia, Args: c.args}, f)
		if n.Source != SourceGraphLoaders || !near(n.GiB, c.want) {
			t.Errorf("%s: %v, want %.2f GiB from %q", c.name, n, c.want, SourceGraphLoaders)
		}
	}
}

// The doubt check. ScanGraph lists only the top-level string inputs that end in one of six weight extensions, so a graph can name a
// weight it does not list: a file of another extension, or one inside a nested widget object. Sizing the files it DID list would
// declare a partial sum, and on a card they fit that is 0 where the class default stood. Any weight-like name in the whole
// document that was not sized keeps the class default.
func TestAGraphNamingAWeightItCouldNotSizeKeepsTheClassDefault(t *testing.T) {
	f := sparseTree(t, map[string]int64{"vae/ae.safetensors": mib, "checkpoints/big.bin": 6 * mib, "loras/style.safetensors": mib})
	f.VRAMGiB = 24 // the sized part fits this card (a partial sum would be 0); the class default (32.8 GiB) does not
	want := ClassDefault(f)
	for name, graph := range map[string]string{
		"a .bin weight beside a VAE": `{"1":{"class_type":"VAELoader","inputs":{"vae_name":"ae.safetensors"}},
			"2":{"class_type":"SomeLoader","inputs":{"model":"big.bin"}}}`,
		"a weight inside a nested widget object": `{"1":{"class_type":"VAELoader","inputs":{"vae_name":"ae.safetensors"}},
			"2":{"class_type":"PowerLoraLoader","inputs":{"lora_1":{"on":true,"lora":"style.safetensors","strength":1}}}}`,
		"a weight in a list": `{"1":{"class_type":"VAELoader","inputs":{"vae_name":"ae.safetensors"}},
			"2":{"class_type":"MultiLoader","inputs":{"models":["a.pt2","b.pkl"]}}}`,
		"an onnx file": `{"1":{"class_type":"VAELoader","inputs":{"vae_name":"ae.safetensors"}},
			"2":{"class_type":"Detector","inputs":{"detector":"face.ONNX"}}}`,
	} {
		if n, ok := ForGraph([]byte(graph), f); ok {
			t.Errorf("%s: sized as %v, want no figure (the class default)", name, n)
		}
		got := Resolve(Request{Class: gpulease.ClassMedia, Args: graphCall("comfy-render.mjs", "--graph", writeGraph(t, graph))}, f)
		if got != want {
			t.Errorf("%s: Resolve = %v, want the class default %v", name, got, want)
		}
	}
	// the controls: a nested name that is also a sized top-level input is not a doubt, and an output prefix or a URL is not a file
	ok := `{"1":{"class_type":"VAELoader","inputs":{"vae_name":"ae.safetensors"}},
		"2":{"class_type":"Note","inputs":{"meta":{"uses":"ae.safetensors"}}},
		"3":{"class_type":"SaveImage","inputs":{"filename_prefix":"out.bin"}},
		"4":{"class_type":"Fetch","inputs":{"url":"https://example.invalid/m.bin"}}}`
	if n, good := ForGraph([]byte(ok), f); !good || n.Source != SourceGraphLoaders {
		t.Errorf("the control graph is sized, got %v %v", n, good)
	}
}
