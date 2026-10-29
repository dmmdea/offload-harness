package pipeline

// What a TEMPLATE run declares for the host (graphRAM, the template branch). A template request carries `template` and `manifest_path`
// and no graph_path, so the graph reader found nothing to read and the lane declared the media class default: the largest family the
// box binds (33.1 GiB on a box that binds a Wan 2.2 pair) for a template whose files are 23 GiB, held by the host-RAM brake though the
// box could run it, and on a box that binds no ComfyUI render family, 0 (source none), so a big template got no host-RAM check at all.
// The runtime manifest names the model files the final graph loads (an override sets values and never retargets a file or names an embedding, within the limit docs/systems/mcp-server.md states for text that string nodes assemble), as
// `models/<class>/<name>`; those are what the lane sizes, by the family estimate's rule.

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

const (
	tplName = "image_qwen_image"
	tplCkpt = "models/checkpoints/qwen_image_fp8.safetensors"   // 6.76 GiB
	tplClip = "models/text_encoders/qwen_2.5_vl_7b.safetensors" // 16.33 GiB
	tplSum  = 6.76 + 16.33
)

// templateSizes are the files the box "holds" and their sizes, by base name (a stat table: a 16 GiB fixture is not an option).
var templateSizes = map[string]float64{
	"qwen_image_fp8.safetensors": 6.76,
	"qwen_2.5_vl_7b.safetensors": 16.33,
	"style.safetensors":          0.5,
	"pytorch_model.bin":          2.0,
}

// templateOnTheBox gives p a ComfyUI install whose model tree has the class directories the manifest names, makes the files in
// templateSizes exist at their sizes, writes a runtime manifest that names the given model paths (the shape render/template-manifest.mjs
// emits: `path` as `models/<class>/<name>`, no source) and returns the run_graph params of a template run that names it. A path whose
// base name is not in templateSizes is a file the box does not hold.
func templateOnTheBox(t *testing.T, p *Pipeline, paths ...string) map[string]any {
	t.Helper()
	dir := t.TempDir()
	models := make([]map[string]any, 0, len(paths))
	for _, mp := range paths {
		if parts := strings.SplitN(filepath.ToSlash(mp), "/", 3); len(parts) == 3 && parts[1] != "" {
			if err := os.MkdirAll(filepath.Join(dir, "models", parts[1]), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		models = append(models, map[string]any{"path": mp, "source_url": "", "sha256": nil, "trust": "tofu"})
	}
	p.cfg.ComfyDir = dir
	t.Cleanup(hostneed.UseStat(func(path string) (int64, bool) {
		g, ok := templateSizes[filepath.Base(path)]
		return int64(g * (1 << 30)), ok
	}))
	return writeTemplateManifest(t, map[string]any{"schema_version": 1, "workflow": tplName, "comfyui_min_version": "", "node_packs": []any{}, "models": models})
}

func writeTemplateManifest(t *testing.T, manifest any) map[string]any {
	t.Helper()
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return templateParamsFor(t, string(body))
}

func templateParamsFor(t *testing.T, body string) map[string]any {
	t.Helper()
	mp := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(mp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"template": tplName, "manifest_path": mp}
}

// declaredOn is what the lane declares on one 16 GiB card (the fixture's), through the real declaration path.
func declaredOn(p *Pipeline, params map[string]any) (float64, hostneed.Need) {
	cards := []gpuprobe.Card{{UUID: "GPU-00000000-0000-0000-0000-000000000001", VRAMTotalGiB: 16}}
	need := declaredNeed(nil, "").declaresRAM(graphRAM(p.cfg, params))
	return p.declaredRAM(context.Background(), need, cards, nil), need.RAM(16)
}

// admittedOrHeld waits for the lane's verdict: the runner started (admitted), or the call came back (held or refused).
func admittedOrHeld(f *admitFixture, ch <-chan core.Result) (started bool, r core.Result) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.started()) >= 1 {
			return true, core.Result{}
		}
		select {
		case r = <-ch:
			return false, r
		case <-time.After(20 * time.Millisecond):
		}
	}
	f.t.Fatal("the call neither started a runner nor returned")
	return false, core.Result{}
}

// (1) THE DEFECT, through the real run-graph path (runRunGraph): on a host with room for the template's 23.09 GiB and not for the
// 33.1 GiB class default, the template is admitted and its runner starts.
func TestTheRunGraphLaneIsAdmittedOnTheFilesItsTemplateNames(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: krea2Binding})
	f.useHost(t, func() gpuprobe.HostMemory { return shortHost(65) })
	params := templateOnTheBox(t, f.p, tplCkpt, tplClip)
	params["graph_path"] = "" // the fixture's graph() puts a graph file in; a template run names none
	ch := f.graph(params)
	started, r := admittedOrHeld(f, ch)
	if !started {
		t.Fatalf("the template lane was not admitted on the files its manifest names: ok=%v %s", r.OK, r.Reason)
	}
	f.letRunnersGo()
	if r := f.await(ch); !r.OK {
		t.Fatalf("the template run did not complete: %+v", r)
	}
	g, _ := declaredOn(f.p, params)
	t.Logf("declared %.2f GiB (the class default of this box is %.2f)", g, classDefaultRAM(f.p.cfg)(16).GiB)
}

// (2) Any doubt keeps the class default, on the same host that admits the template above: a file the box does not hold, one among
// files it does (a partial sum is a figure that shrinks), a manifest that cannot be read, one that names nothing, one whose path is
// not models/<class>/<name>, a template with no manifest at all.
func TestTheRunGraphLaneHoldsATemplateItCannotSizeAtTheClassDefault(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder, mutate: krea2Binding})
	f.useHost(t, func() gpuprobe.HostMemory { return shortHost(65) })
	params := templateOnTheBox(t, f.p, tplCkpt, "models/text_encoders/not_on_this_box.safetensors")
	params["graph_path"] = ""
	r := f.await(f.graph(params))
	if r.OK || !strings.Contains(r.Reason, "waiting for host RAM") {
		t.Fatalf("want a hold on host RAM for the class default, got ok=%v: %s", r.OK, r.Reason)
	}
}

func TestATemplateTheLaneCannotSizeKeepsTheClassDefault(t *testing.T) {
	p := krea2Pipeline()
	// the box: its model tree has the class directories of every path below, and holds the two files of the good manifest
	templateOnTheBox(t, p, tplCkpt, tplClip, "models/loras/not_on_this_box.safetensors")
	good := templateParamsFor(t, `{"models":[{"path":"`+tplCkpt+`"},{"path":"`+tplClip+`"}]}`)
	want := classDefaultRAM(p.cfg)(16)
	if want.GiB < 33 {
		t.Fatalf("the control: the class default of this box is the largest family it binds (33.1 GiB), got %v", want)
	}
	tpl := func(body string) map[string]any { return templateParamsFor(t, body) }
	for name, params := range map[string]map[string]any{
		"a file among them the box does not hold": tpl(`{"models":[{"path":"` + tplCkpt + `"},{"path":"models/loras/not_on_this_box.safetensors"}]}`),
		"a manifest that is not JSON":             tpl(`not json`),
		"a manifest with no models key":           tpl(`{"workflow":"x"}`),
		"a model whose path is null":              tpl(`{"models":[{"path":null},{"path":"` + tplCkpt + `"}]}`),
		"a path that is not models/class/name":    tpl(`{"models":[{"path":"checkpoints/qwen_image_fp8.safetensors"}]}`),
		"a path that leaves its class directory":  tpl(`{"models":[{"path":"models/checkpoints/../../x.safetensors"}]}`),
		"a manifest file that is not there":       {"template": tplName, "manifest_path": filepath.Join(t.TempDir(), "absent.json")},
		"a template with no manifest_path":        {"template": tplName},
	} {
		if got := graphRAM(p.cfg, params)(16); got != want {
			t.Errorf("%s: declared %v, want the class default %v", name, got, want)
		}
	}
	// a manifest whose models list is present and empty loads no weight file: the ComfyUI process floor, its commit stated
	if got := graphRAM(p.cfg, tpl(`{"models":[]}`))(16); got.GiB != 3 || got.CommitGiB != 4 || !got.CommitStated || got.Source != hostneed.SourceMeasured {
		t.Errorf("a manifest that names no model: declared %v, want the process floor of 3 GiB with 4 GiB of commit stated", got)
	}
	if got := graphRAM(p.cfg, good)(16); got == want {
		t.Errorf("the control: a manifest of two files the box holds must not be the class default %v", want)
	}
}

// (3) The lane probe agrees with the admission for a template (graphLaneCases carries the rows).

// (4) A box that binds no ComfyUI render family (an sd.cpp-only node, here a ComfyUI install with no render script) has a class
// default of 0, source none, and that rule is deliberate for a call whose weights nobody can name (TestAnSdcppOnlyBoxDeclaresNothing).
// A template NAMES its weights in its manifest, so its files are declared on that box too, as a graph's are: the brake is a check
// of the call's own files, not of the families the box binds.
func TestATemplateOnABoxThatBindsNoRenderFamilyDeclaresItsFiles(t *testing.T) {
	p := &Pipeline{cfg: config.Config{}}
	params := templateOnTheBox(t, p, tplCkpt, tplClip)
	if c := classDefaultRAM(p.cfg)(16); c.Source != hostneed.SourceNone || c.GiB != 0 {
		t.Fatalf("the control: this box binds no render family, so its class default is nothing, got %v", c)
	}
	got, need := declaredOn(p, params)
	t.Logf("declared %.2f GiB (%s)", got, need.Source)
	if math.Abs(got-tplSum) > 0.01 || need.Source != hostneed.SourceGraphLoaders {
		t.Fatalf("a template of 23.09 GiB of files on a box that binds no render family declared %.2f GiB from %q, want %.2f from %q: %v",
			got, need.Source, tplSum, hostneed.SourceGraphLoaders, need)
	}
	// ...and a template whose files it cannot size keeps the box's class default, which on this box is nothing: the rule is unchanged.
	unsized := templateParamsFor(t, `{"models":[{"path":"models/loras/not_on_this_box.safetensors"}]}`)
	if n := graphRAM(p.cfg, unsized)(16); n.Source != hostneed.SourceNone || n.GiB != 0 {
		t.Errorf("an unsized template on that box: %v, want the class default (none)", n)
	}
}

// (5) What the graph path's files get, a template's get too: the call's reserve_vram fits the set (the runner holds it back; a value
// it would not take is the default), a nested model path and a .bin (a weight the graph scan does not list, but a manifest does) are
// counted, and one missing file keeps the class default instead of declaring the sum of the others.
func TestTheTemplateLaneFitsItsFilesWithTheReserveVRAMOfTheCall(t *testing.T) {
	p := krea2Pipeline()
	params := templateOnTheBox(t, p, tplCkpt, tplClip)
	for _, c := range []struct {
		reserve string
		want    float64
	}{{"", 0}, {"6", tplSum}, {"lots", 0}, {"0", 0}, {"-3", 0}} {
		pr := map[string]any{"template": params["template"], "manifest_path": params["manifest_path"]}
		if c.reserve != "" {
			pr["reserve_vram"] = c.reserve
		}
		if n := graphRAM(p.cfg, pr)(25); n.Source != hostneed.SourceGraphLoaders || math.Abs(n.GiB-c.want) > 0.01 {
			t.Errorf("reserve_vram %q on a 25 GiB card: %v, want %.2f GiB", c.reserve, n, c.want)
		}
	}
}

func TestTheTemplateLaneCountsANestedPathAndADotBinAndDoesNotCountAFileTwice(t *testing.T) {
	p := krea2Pipeline()
	params := templateOnTheBox(t, p, tplCkpt, "models/loras/sub/dir/style.safetensors", "models/diffusers/qwen/pytorch_model.bin", tplCkpt)
	want := 6.76 + 0.5 + 2.0 // 9.26 GiB: it does not fit an 8 GiB card, so the set is declared in full (on 16 GiB it fits and declares 0)
	need := graphRAM(p.cfg, params)(8)
	t.Logf("declared %.2f GiB (%s)", need.GiB, need.Source)
	if need.Source != hostneed.SourceGraphLoaders || math.Abs(need.GiB-want) > 0.01 {
		t.Errorf("a nested path and a .bin: declared %.2f GiB from %q, want %.2f (each file once) from %q: %v", need.GiB, need.Source, want, hostneed.SourceGraphLoaders, need)
	}
}

func TestTheTemplateLaneKeepsTheClassDefaultWhenADotBinOrANestedFileIsMissing(t *testing.T) {
	p := krea2Pipeline()
	// the box: class directories for every path below; pytorch_model.bin and style.safetensors are held, the other names are not
	templateOnTheBox(t, p, tplCkpt, "models/diffusers/qwen/pytorch_model.bin", "models/loras/sub/dir/style.safetensors")
	want := classDefaultRAM(p.cfg)(16)
	for name, paths := range map[string][]string{
		"a .bin the box does not hold": {tplCkpt, "models/diffusers/qwen/other_model.bin"},
		"a nested file the box lacks":  {tplCkpt, "models/loras/sub/dir/absent.safetensors"},
	} {
		models := make([]map[string]any, 0, len(paths))
		for _, mp := range paths {
			models = append(models, map[string]any{"path": mp})
		}
		params := writeTemplateManifest(t, map[string]any{"models": models})
		if got := graphRAM(p.cfg, params)(16); got != want {
			t.Errorf("%s: declared %v, want the class default %v", name, got, want)
		}
	}
}
