package pipeline

// What the run_graph lane declares for the host (graphRAM): the weight files the graph's own loaders name, by the family estimate's
// rule (hostneed.ForGraph), and the media class default when the graph cannot be read or one of its files cannot be sized. The
// incident: a 512x512 qwen-image graph of a 6.76 GiB checkpoint and a 16.33 GiB text encoder was declared the 33.1 GiB of the
// largest family the box binds (here the Wan 2.2 pair, 33.1 GiB) and the host-RAM brake held it, though the box could run it.

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

const incidentGraphSrc = `{
	"1": {"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"qwen_image_fp8.safetensors"}},
	"2": {"class_type":"CLIPLoader","inputs":{"clip_name":"qwen_2.5_vl_7b.safetensors","type":"qwen_image"}},
	"3": {"class_type":"EmptyLatentImage","inputs":{"width":512,"height":512,"batch_size":1}}
}`

// graphOnTheBox gives p a ComfyUI install whose model tree has the class directories, makes the two files the incident's graph
// names exist at their sizes (a stat table: a 16 GiB fixture is not an option), writes the graph and returns the run_graph params
// that name it. namedFile is the checkpoint the graph asks for: the incident's, or one the box does not hold.
func graphOnTheBox(t *testing.T, p *Pipeline, namedFile string) map[string]any {
	t.Helper()
	dir := t.TempDir()
	for _, class := range []string{"checkpoints", "text_encoders"} {
		if err := os.MkdirAll(filepath.Join(dir, "models", class), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p.cfg.ComfyDir = dir
	sizes := map[string]float64{"qwen_image_fp8.safetensors": 6.76, "qwen_2.5_vl_7b.safetensors": 16.33}
	t.Cleanup(hostneed.UseStat(func(path string) (int64, bool) {
		g, ok := sizes[filepath.Base(path)]
		return int64(g * (1 << 30)), ok
	}))
	graph := strings.ReplaceAll(incidentGraphSrc, "qwen_image_fp8.safetensors", namedFile)
	gp := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(gp, []byte(graph), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"graph_path": gp}
}

func krea2Pipeline() *Pipeline {
	cfg := config.Default()
	krea2Binding(&cfg)
	return &Pipeline{cfg: cfg}
}

// THE FIX. A graph lane whose graph names the incident's two files declares their 23.09 GiB, not the class default.
func TestTheGraphLaneDeclaresTheFilesItsGraphNamesNotTheClassDefault(t *testing.T) {
	p := krea2Pipeline()
	params := graphOnTheBox(t, p, "qwen_image_fp8.safetensors")
	cards := []gpuprobe.Card{{UUID: "GPU-00000000-0000-0000-0000-000000000001", VRAMTotalGiB: 16}}

	class := p.declaredRAM(context.Background(), wholeNeed("").declaresRAM(classDefaultRAM(p.cfg)), cards, nil)
	if class < 33 {
		t.Fatalf("the control: this box's class default is the largest family it binds (the Wan 2.2 pair, 33.1 GiB), got %.2f", class)
	}
	need := declaredNeed(nil, "").declaresRAM(graphRAM(p.cfg, params))
	if got := p.declaredRAM(context.Background(), need, cards, nil); math.Abs(got-(6.76+16.33)) > 0.01 {
		t.Fatalf("the graph lane declared %.2f GiB, want %.2f (the two files its graph names), not the class default %.2f", got, 6.76+16.33, class)
	}
	if n := need.RAM(16); n.Source != hostneed.SourceGraphLoaders {
		t.Errorf("the declaration says it came from %q, want %q: %v", n.Source, hostneed.SourceGraphLoaders, n)
	}
}

// The family estimate's rule: the same files on a card they fit stream nothing from RAM and declare 0.
func TestTheGraphLaneDeclaresZeroWhenTheGraphFitsTheCard(t *testing.T) {
	p := krea2Pipeline()
	params := graphOnTheBox(t, p, "qwen_image_fp8.safetensors")
	need := declaredNeed(nil, "").declaresRAM(graphRAM(p.cfg, params))
	if n := need.RAM(32); n.GiB != 0 || n.Source != hostneed.SourceGraphLoaders || !strings.Contains(n.Detail, "fits the 32.0 GiB card") {
		t.Errorf("on a 32 GiB card the 23.09 GiB graph fits: got %v, want 0 GiB with the fit wording", n)
	}
	if n := need.RAM(16); !strings.Contains(n.Detail, "does not fit the 16.0 GiB card, so the weights stream from RAM") {
		t.Errorf("on a 16 GiB card it does not fit and says so: %v", n)
	}
}

// Any doubt keeps the class default, unchanged: a file the box does not hold, a graph that is not there.
func TestTheGraphLaneKeepsTheClassDefaultWhenItCannotSizeTheGraph(t *testing.T) {
	p := krea2Pipeline()
	want := classDefaultRAM(p.cfg)(16)
	for name, params := range map[string]map[string]any{
		"a file the box does not hold": graphOnTheBox(t, p, "not_on_this_box.safetensors"),
		"a graph that is not there":    {"graph_path": filepath.Join(t.TempDir(), "absent.json")},
		"no graph_path at all":         {},
	} {
		if got := graphRAM(p.cfg, params)(16); got != want {
			t.Errorf("%s: declared %v, want the class default %v", name, got, want)
		}
	}
}
