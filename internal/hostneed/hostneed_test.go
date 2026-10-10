package hostneed

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The sizes below are the incident's lane, read off the reference box's model tree: the bf16 Krea 2
// UNet (24.48 GiB) and the Qwen3-VL-4B bf16 text encoder (8.27 GiB), 32.75 GiB together.
const (
	krea2UnetGiB = 24.48
	krea2TEGiB   = 8.27
)

// comfyTree is an install whose model tree has the class directories and no weights: the sizes come
// from the Stat table, because a 24 GiB fixture file is not an option on a test machine.
func comfyTree(t *testing.T, sizes map[string]float64) (dir string, stat func(string) (int64, bool)) {
	t.Helper()
	dir = t.TempDir()
	for _, class := range []string{"checkpoints", "diffusion_models", "unet", "text_encoders", "clip", "vae", "loras"} {
		if err := os.MkdirAll(filepath.Join(dir, "models", class), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, func(p string) (int64, bool) {
		g, ok := sizes[filepath.Base(p)]
		return int64(g * gib), ok
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func krea2Facts(t *testing.T, vram float64) Facts {
	dir, stat := comfyTree(t, map[string]float64{
		"krea2_turbo_bf16.safetensors":              krea2UnetGiB,
		"qwen3vl_4b_bf16.safetensors":               krea2TEGiB,
		"qwen3vl_8b_bf16.safetensors":               16.33,
		"qwen_image_2.1_bf16.safetensors":           13.25,
		"qwen_image_edit_2511_fp8mixed.safetensors": 19.12,
		"qwen_2.5_vl_7b_fp8_scaled.safetensors":     8.74,
	})
	return Facts{Cfg: config.Config{ComfyDir: dir}, VRAMGiB: vram, Stat: stat}
}

const krea2Cmd = "node render/comfy-generate.mjs --batch jobs.jsonl --family krea2 --ckpt krea2_turbo_bf16.safetensors"

// THE ACCEPTANCE CASE. A krea2 bf16 call on a 16 GiB card streams from RAM, so the estimate is the
// UNet plus the text encoder, in full: 24.48 + 8.27 = 32.75 GiB.
func TestAKrea2Bf16CallOn16GiBIsTheUnetPlusTheTextEncoder(t *testing.T) {
	n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields(krea2Cmd)}, krea2Facts(t, 16))
	if !near(n.GiB, krea2UnetGiB+krea2TEGiB) || n.Source != SourceEstimate {
		t.Fatalf("estimate = %v, want %.2f GiB from the model files", n, krea2UnetGiB+krea2TEGiB)
	}
	if !strings.Contains(n.Detail, "does not fit") || !strings.Contains(n.Detail, "krea2") {
		t.Fatalf("the detail must say why: %q", n.Detail)
	}
}

// Overflow only: a set that fits the card declares nothing.
func TestAModelSetThatFitsTheCardDeclaresNothing(t *testing.T) {
	n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields(krea2Cmd)}, krea2Facts(t, 48))
	if n.GiB != 0 || n.Source != SourceEstimate || !strings.Contains(n.Detail, "fits") {
		t.Fatalf("a 32.75 GiB set on a 48 GiB card streams nothing from RAM, got %v", n)
	}
}

// The fit test is "weights + the runner's reserve-vram <= card": 15.2 GiB of weights on a 16 GiB
// card fits with a 0.5 GiB reserve and does not with the default 1.0 plus the context it needs.
func TestTheFitTestHonoursTheRunnersReserveVram(t *testing.T) {
	dir, stat := comfyTree(t, map[string]float64{"edge_ckpt.safetensors": 15.2})
	f := Facts{Cfg: config.Config{ComfyDir: dir}, VRAMGiB: 16, Stat: stat}
	base := "node render/comfy-render.mjs out.png prompt --family sdxl --ckpt edge_ckpt.safetensors"
	if n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields(base)}, f); !near(n.GiB, 15.2) {
		t.Fatalf("default reserve 1.0: 15.2 > 15.0 usable, so it streams: %v", n)
	}
	if n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields(base + " --reserve-vram 0.5")}, f); n.GiB != 0 {
		t.Fatalf("--reserve-vram 0.5: 15.2 <= 15.5 usable, so it fits: %v", n)
	}
}

// With no card to measure against, nothing is assumed to fit.
func TestAnUnknownCardAssumesNothingFits(t *testing.T) {
	n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields(krea2Cmd)}, krea2Facts(t, 0))
	if !near(n.GiB, krea2UnetGiB+krea2TEGiB) || !strings.Contains(n.Detail, "unknown card") {
		t.Fatalf("an unreadable card table counts the whole set, got %v", n)
	}
}

// A file whose size cannot be read takes the documented per-family size, and the source says so.
func TestAFileThatCannotBeSizedTakesTheDocumentedFamilySize(t *testing.T) {
	dir, stat := comfyTree(t, nil) // no sizes at all
	f := Facts{Cfg: config.Config{ComfyDir: dir}, VRAMGiB: 16, Stat: stat}
	n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields(krea2Cmd)}, f)
	if !near(n.GiB, 24.5+8.3) || n.Source != SourceFamilyDefault {
		t.Fatalf("krea2 with unreadable sizes = %v, want the documented 24.5 + 8.3 as a family default", n)
	}
	// Qwen-Image 2512 bf16 is the 38 GiB stream of the incident.
	q := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields("node render/comfy-generate.mjs --family qwen-image --ckpt qwen_image_2512_bf16.safetensors")}, f)
	if !near(q.GiB, 38.0+8.8) || q.Source != SourceFamilyDefault {
		t.Fatalf("qwen-image bf16 with unreadable sizes = %v, want the documented 38 + 8.8", q)
	}
}

// Order of precedence: the operator's --ram beats every estimate, and 0 is a value.
func TestAnExplicitRamBeatsTheEstimate(t *testing.T) {
	five, zero := 5.0, 0.0
	f := krea2Facts(t, 16)
	req := Request{Class: gpulease.ClassMedia, Args: strings.Fields(krea2Cmd)}
	req.Explicit = &five
	if n := Resolve(req, f); n.GiB != 5 || n.Source != SourceExplicit {
		t.Fatalf("--ram 5 must beat the 32.75 GiB estimate, got %v", n)
	}
	req.Explicit = &zero
	if n := Resolve(req, f); n.GiB != 0 || n.Source != SourceExplicit {
		t.Fatalf("--ram 0 means needs no host RAM, got %v", n)
	}
}

// Text and seat leases declare nothing, whatever the box binds.
func TestTextAndSeatLeasesDefaultToZero(t *testing.T) {
	f := krea2Facts(t, 16)
	f.Cfg.ImageGenScript, f.Cfg.ImageGenFamily, f.Cfg.ImageGenCkpt = "render/comfy-generate.mjs", "krea2", "krea2_turbo_bf16.safetensors"
	for _, class := range []gpulease.Class{gpulease.ClassText, gpulease.ClassSeat} {
		if n := Resolve(Request{Class: class, Args: []string{"python", "bench.py"}}, f); n.GiB != 0 || n.Source != SourceNone {
			t.Fatalf("a %s lease must declare 0, got %v", class, n)
		}
	}
}

// A media command the estimator does not recognise gets the class default: the largest family this
// box binds, from its own config, so it describes what the box can be asked to run.
func TestTheMediaClassDefaultIsTheLargestFamilyThisBoxBinds(t *testing.T) {
	f := krea2Facts(t, 16)
	f.Cfg.ImageGenScript, f.Cfg.ImageGenFamily, f.Cfg.ImageGenCkpt = "render/comfy-generate.mjs", "krea2", "krea2_turbo_bf16.safetensors"
	n := Resolve(Request{Class: gpulease.ClassMedia, Args: []string{"python", "my_render.py"}}, f)
	if !near(n.GiB, krea2UnetGiB+krea2TEGiB) || n.Source != SourceClassDefault || !strings.Contains(n.Detail, "image") {
		t.Fatalf("class default = %v, want the bound krea2 family's 32.75 GiB", n)
	}
	// Another route bound with a smaller family does not lower it: the default is the LARGEST.
	f.Cfg.GenEditScript, f.Cfg.GenEditFamily = "render/comfy-edit.mjs", config.FamilyQwenImage21
	f.Cfg.GenEditUnet, f.Cfg.GenEditCLIP = "qwen_image_2.1_bf16.safetensors", "qwen3vl_8b_bf16.safetensors"
	n = Resolve(Request{Class: gpulease.ClassMedia, Args: []string{"python", "my_render.py"}}, f)
	if !near(n.GiB, krea2UnetGiB+krea2TEGiB) || !strings.Contains(n.Detail, "image") {
		t.Fatalf("class default = %v, want the image family's 32.75 (the 2.1 edit's 29.58 is smaller)", n)
	}
	// A larger family on another route raises it, and the detail names that route.
	f.Cfg.ImageGenFamily, f.Cfg.ImageGenCkpt = "sdxl", "RealVisXL.safetensors" // fits the card: declares nothing
	f.Cfg.GenEditFamily, f.Cfg.GenEditUnet, f.Cfg.GenEditCLIP = "", "qwen_image_edit_2511_fp8mixed.safetensors", ""
	n = Resolve(Request{Class: gpulease.ClassMedia, Args: []string{"python", "my_render.py"}}, f)
	if !near(n.GiB, 19.12+8.74) || !strings.Contains(n.Detail, "edit") {
		t.Fatalf("class default = %v, want the 2511 edit's 27.86 as the largest bound family", n)
	}
	// A box that binds nothing declares nothing, rather than a fleet-wide number.
	empty := Resolve(Request{Class: gpulease.ClassMedia, Args: []string{"python", "x.py"}}, Facts{Cfg: config.Config{ComfyDir: t.TempDir()}, VRAMGiB: 16})
	if empty.GiB != 0 {
		t.Fatalf("a box that binds no render family declares 0, got %v", empty)
	}
	if none := Resolve(Request{Class: gpulease.ClassMedia}, Facts{VRAMGiB: 16}); none.GiB != 0 || none.Source != SourceNone {
		t.Fatalf("no ComfyUI install bound: nothing to size, got %v", none)
	}
}

// A named video family renders with ITS binding, so the class default sizes it from that binding and
// not from the box's flat videogen_* keys (which belong to the box's default family).
func TestTheClassDefaultSizesANamedVideoFamilyFromItsOwnBinding(t *testing.T) {
	dir, stat := comfyTree(t, map[string]float64{
		"ltx_named_transformer.safetensors": 30,
		"gemma_named_encoder.safetensors":   20,
	})
	cfg := config.Config{
		ComfyDir:       dir,
		VideoGenScript: "render/comfy-video.mjs",
		VideoGenFamilies: map[string]config.VideoFamilyBinding{
			"ltx25": {Transformer: "ltx_named_transformer.safetensors", TextEncoder: "gemma_named_encoder.safetensors"},
		},
	}
	n := ClassDefault(Facts{Cfg: cfg, VRAMGiB: 16, Stat: stat})
	if !near(n.GiB, 50) || !strings.Contains(n.Detail, "video family ltx25") {
		t.Fatalf("class default = %v, want the named ltx25 binding's 30 + 20 GiB (the wan22 default is 33.1)", n)
	}
}

// The edit helper: the UNet from --unet, the text encoder the 2511 graph defaults to.
func TestAnEditCallCountsItsUnetAndTheDefaultTextEncoder(t *testing.T) {
	f := krea2Facts(t, 16)
	n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields("node render/comfy-edit.mjs out.png in.png edit --unet qwen_image_edit_2511_fp8mixed.safetensors")}, f)
	if !near(n.GiB, 19.12+8.74) || n.Source != SourceEstimate {
		t.Fatalf("2511 edit = %v, want 19.12 + 8.74", n)
	}
	n = Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields("node render/comfy-edit.mjs out.png in.png edit --family qwen-image-2.1 --unet qwen_image_2.1_bf16.safetensors --clip qwen3vl_8b_bf16.safetensors --vae v.safetensors")}, f)
	if !near(n.GiB, 13.25+16.33) {
		t.Fatalf("2.1 edit = %v, want 13.25 + 16.33", n)
	}
}

// What the helper recognition accepts and what it must not swallow.
func TestParseRenderCall(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		ok    bool
		route Route
		flags map[string]string
	}{
		{"generate behind node", strings.Fields(krea2Cmd), true, RouteImage, map[string]string{"family": "krea2", "ckpt": "krea2_turbo_bf16.safetensors", "batch": "jobs.jsonl"}},
		{"windows path and equals form", []string{`C:\harness\render\comfy-edit.mjs`, "--unet=u.safetensors", "o.png"}, true, RouteEdit, map[string]string{"unet": "u.safetensors"}},
		{"a bool flag does not swallow the next flag", strings.Fields("node render/comfy-render.mjs o.png p --no-lock --family qwen-image --ckpt x.gguf"), true, RouteImage, map[string]string{"no-lock": "true", "family": "qwen-image", "ckpt": "x.gguf"}},
		{"video", strings.Fields("node render/comfy-video.mjs o.mp4 s.png prompt --model hunyuan --fast"), true, RouteVideo, map[string]string{"model": "hunyuan", "fast": "true"}},
		{"inpaint", strings.Fields("node render/comfy-inpaint.mjs --batch j.jsonl --family qwen --unet u.safetensors"), true, RouteInpaint, map[string]string{"family": "qwen", "unet": "u.safetensors"}},
		{"run-graph runs an arbitrary graph, so it is not recognised", strings.Fields("node render/comfy-run-graph.mjs --graph g.json"), false, "", nil},
		{"an unrelated command", strings.Fields("python bench.py --family krea2"), false, "", nil},
		{"no command", nil, false, "", nil},
	}
	for _, c := range cases {
		got, ok := ParseRenderCall(c.args)
		if ok != c.ok {
			t.Errorf("%s: recognised=%v, want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Route != c.route {
			t.Errorf("%s: route %s, want %s", c.name, got.Route, c.route)
		}
		for k, v := range c.flags {
			if got.Flags[k] != v {
				t.Errorf("%s: flag %s = %q, want %q (flags %v)", c.name, k, got.Flags[k], v, got.Flags)
			}
		}
	}
}

// A family the table does not know and whose files cannot be sized has no estimate: the media class
// default stands in rather than a guess.
func TestAnUnsizedFamilyFallsBackToTheClassDefault(t *testing.T) {
	f := krea2Facts(t, 16)
	f.Cfg.ImageGenScript, f.Cfg.ImageGenFamily, f.Cfg.ImageGenCkpt = "render/comfy-generate.mjs", "krea2", "krea2_turbo_bf16.safetensors"
	n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields("node render/comfy-video.mjs o.mp4 s.png prompt --model hunyuan")}, f)
	if n.Source != SourceClassDefault {
		t.Fatalf("hunyuan has no documented size and no bound files here: want the class default, got %v", n)
	}
}

func TestLargestCardGiB(t *testing.T) {
	cards := []gpuprobe.Card{
		{UUID: "GPU-aaaa0000", VRAMTotalGiB: 16},
		{UUID: "GPU-bbbb0000", VRAMTotalGiB: 24},
		{UUID: "GPU-cccc0000", VRAMTotalGiB: 8},
	}
	if got := LargestCardGiB(cards, nil); got != 24 {
		t.Fatalf("every card: %v, want 24", got)
	}
	if got := LargestCardGiB(cards, []string{cards[0].LeaseID(), cards[2].LeaseID()}); got != 16 {
		t.Fatalf("a job runs on ONE card, so the largest of the leased set, not the sum: %v, want 16", got)
	}
	if got := LargestCardGiB(nil, nil); got != 0 {
		t.Fatalf("no card table: %v, want 0 (unknown)", got)
	}
}

// THE LTX-2.5 FILES the video tests size, read off the reference box's model tree: the int8 transformer
// the builder defaults to (20.03 GiB), the int8 gemma text encoder (14.32) and the bf16 transformer a
// hero render opts into (39.13). Names are the builder's (mediacap's ltxDefaults) and the opt-in's.
const (
	ltxInt8Transformer = "ltx-2.5-22b-distilled-transformer-comfy-int8-convrot.safetensors"
	ltxBf16Transformer = "ltx-2.5-22b-distilled-transformer-bf16.safetensors"
	ltxGemma           = "gemma4-12b-with-proj-ltx-2.5-comfy-int8-convrot.safetensors"
)

func ltxFacts(t *testing.T, vram float64) Facts {
	dir, stat := comfyTree(t, map[string]float64{ltxInt8Transformer: 20.03, ltxBf16Transformer: 39.13, ltxGemma: 14.32})
	return Facts{Cfg: config.Config{ComfyDir: dir}, VRAMGiB: vram, Stat: stat}
}

// --graph POSTS THE CALLER'S OWN WORKFLOW: the helper's family flags are not read, so the call is as
// unknown as run-graph and takes the media class default, not the helper's default family (review of
// 2026-10-10: comfy-render --graph declared 0 because the default SDXL checkpoint fits the card, and
// comfy-video --graph declared Wan 2.2's 32.9 GiB for a graph of any model). Whatever the lease is called.
func TestAGraphCallIsSizedByTheClassDefaultNotItsDefaultFamily(t *testing.T) {
	f := krea2Facts(t, 16)
	f.Cfg.ImageGenScript, f.Cfg.ImageGenFamily, f.Cfg.ImageGenCkpt = "render/comfy-generate.mjs", "krea2", "krea2_turbo_bf16.safetensors"
	for _, c := range []struct {
		name  string
		class gpulease.Class
		cmd   string
	}{
		{"comfy-render --graph", gpulease.ClassMedia, "node render/comfy-render.mjs out.png --graph wf.json"},
		{"comfy-render --graph=", gpulease.ClassMedia, "node render/comfy-render.mjs out.png --graph=wf.json --api http://127.0.0.1:8190"},
		{"comfy-video --graph", gpulease.ClassMedia, "node render/comfy-video.mjs out.mp4 --graph wf.json"},
		{"a text lease wrapping a graph call", gpulease.ClassText, "node render/comfy-render.mjs out.png --graph wf.json"},
	} {
		n := Resolve(Request{Class: c.class, Args: strings.Fields(c.cmd)}, f)
		if n.Source != SourceClassDefault || !near(n.GiB, krea2UnetGiB+krea2TEGiB) {
			t.Errorf("%s = %v, want the class default (the bound krea2 family's 32.75 GiB)", c.name, n)
		}
	}
	// The same call without --graph is the helper's own family (the control).
	if n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields("node render/comfy-render.mjs out.png prompt --family sdxl --ckpt RealVisXL.safetensors")}, f); n.Source == SourceClassDefault {
		t.Errorf("a call without --graph is sized by its own family, got %v", n)
	}
}

// A recognised render helper whose weights cannot be sized is media work whatever the lease is called.
func TestAnUnsizableRenderHelperTakesTheClassDefaultOnATextLeaseToo(t *testing.T) {
	f := krea2Facts(t, 16)
	f.Cfg.ImageGenScript, f.Cfg.ImageGenFamily, f.Cfg.ImageGenCkpt = "render/comfy-generate.mjs", "krea2", "krea2_turbo_bf16.safetensors"
	n := Resolve(Request{Class: gpulease.ClassText, Args: strings.Fields("node render/comfy-video.mjs o.mp4 s.png prompt --model hunyuan")}, f)
	if n.Source != SourceClassDefault || !near(n.GiB, krea2UnetGiB+krea2TEGiB) {
		t.Fatalf("an unsizable helper call on a text lease = %v, want the class default", n)
	}
}

// A hand-run video helper loads what its FLAGS name and the builder's default for the rest. A bf16 LTX
// transformer is 39.13 GiB where the int8 default is 20.03, and the estimate used to size the default
// whatever the flag said (53.45 GiB declared as 34.34: 19 GiB under, more than twice the headroom).
func TestAVideoCallIsSizedFromTheFilesItsFlagsName(t *testing.T) {
	f := ltxFacts(t, 16)
	run := func(flags string) Need {
		return Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields("node render/comfy-video.mjs out.mp4 still.png prompt " + flags)}, f)
	}
	if n := run("--model ltx25"); !near(n.GiB, 20.03+14.32) || n.Source != SourceEstimate {
		t.Fatalf("ltx25 on its builder defaults = %v, want 20.03 + 14.32", n)
	}
	if n := run("--model ltx25 --transformer " + ltxBf16Transformer); !near(n.GiB, 39.13+14.32) || n.Source != SourceEstimate {
		t.Fatalf("ltx25 with a bf16 --transformer = %v, want 39.13 + 14.32 = 53.45", n)
	}
	// The machine's config binds the bf16 file, and the helper is run WITHOUT the flag the pipeline would
	// pass: the runner loads its builder default, so that is what is sized (the config reaches a runner
	// only as flags).
	f.Cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"ltx25": {Transformer: ltxBf16Transformer}}
	if n := run("--model ltx25"); !near(n.GiB, 20.03+14.32) {
		t.Fatalf("a flag the call does not pass is not the config's binding: %v, want the builder default 34.35", n)
	}
	// The Wan experts and the text encoder come from --high-unet, --low-unet and --text-encoder.
	dir, stat := comfyTree(t, map[string]float64{"hi.safetensors": 13.3, "lo.safetensors": 13.4, "te.safetensors": 6.3})
	wf := Facts{Cfg: config.Config{ComfyDir: dir}, VRAMGiB: 16, Stat: stat}
	n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields("node render/comfy-video.mjs o.mp4 s.png p --model wan --high-unet hi.safetensors --low-unet lo.safetensors --text-encoder te.safetensors")}, wf)
	if !near(n.GiB, 13.3+13.4+6.3) || n.Source != SourceEstimate {
		t.Fatalf("wan with its three flags = %v, want 33.0", n)
	}
}

// The pipeline's own estimate puts the request's `transformer` over the family's binding: ForVideoWith.
func TestForVideoWithPutsTheRequestsFilesOverTheBinding(t *testing.T) {
	f := ltxFacts(t, 16)
	cfg := f.Cfg
	cfg.VideoGenFamily = "ltx25"
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"ltx25": {Transformer: ltxInt8Transformer, TextEncoder: ltxGemma}}
	if n, ok := ForVideoWith(cfg, "ltx25", VideoOverrides{}, f); !ok || !near(n.GiB, 20.03+14.32) {
		t.Fatalf("the binding as it is = %v, want 34.35", n)
	}
	n, ok := ForVideoWith(cfg, "ltx25", VideoOverrides{Transformer: ltxBf16Transformer}, f)
	if !ok || !near(n.GiB, 39.13+14.32) {
		t.Fatalf("with the request's bf16 transformer = %v, want 53.45", n)
	}
	// ForVideo is the same estimate with nothing named over it.
	a, _ := ForVideo(cfg, "ltx25", f)
	b, _ := ForVideoWith(cfg, "ltx25", VideoOverrides{}, f)
	if a.GiB != b.GiB {
		t.Fatalf("ForVideo %v and ForVideoWith(nothing) %v must agree", a, b)
	}
	// An override that is only spaces names nothing.
	if n, _ := ForVideoWith(cfg, "ltx25", VideoOverrides{Transformer: "  "}, f); !near(n.GiB, 20.03+14.32) {
		t.Fatalf("a blank override keeps the binding, got %v", n)
	}
}

// The estimator's rules that a mutation check found nothing guarding (review, 2026-10-10): each of
// these was deleted in turn and every test stayed green.

// A role the family loads that the call did not name takes the documented size: a krea2 call with no
// --ckpt (and no COMFY_CKPT) still streams a 24.5 GiB UNet next to its text encoder.
func TestARoleTheCallDoesNotNameTakesTheDocumentedSize(t *testing.T) {
	f := krea2Facts(t, 16)
	n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields("node render/comfy-generate.mjs out.png prompt --family krea2")}, f)
	if !near(n.GiB, 24.5+krea2TEGiB) || n.Source != SourceFamilyDefault {
		t.Fatalf("krea2 with no --ckpt = %v, want the documented 24.5 UNet + the sized 8.27 text encoder as a family default", n)
	}
}

// COMFY_CKPT and COMFY_EDIT_UNET are what the runners read when the flag is absent, so they are what
// the estimate reads (Facts.Env), for image, inpaint and edit calls.
func TestTheRunnersEnvironmentNamesTheWeightsWhenTheFlagIsAbsent(t *testing.T) {
	dir, stat := comfyTree(t, map[string]float64{
		"krea2_turbo_bf16.safetensors":              krea2UnetGiB,
		"qwen3vl_4b_bf16.safetensors":               krea2TEGiB,
		"big_inpaint.safetensors":                   20,
		"qwen_image_edit_2511_fp8mixed.safetensors": 19.12,
		"qwen_2.5_vl_7b_fp8_scaled.safetensors":     8.74,
	})
	env := map[string]string{"COMFY_CKPT": "krea2_turbo_bf16.safetensors", "COMFY_EDIT_UNET": "qwen_image_edit_2511_fp8mixed.safetensors"}
	f := Facts{Cfg: config.Config{ComfyDir: dir}, VRAMGiB: 16, Stat: stat, Env: func(k string) string { return env[k] }}
	run := func(cmd string) Need {
		return Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields(cmd)}, f)
	}
	// image: the env ckpt is sized from its file (24.48 + 8.27 = 32.75), not the documented 24.5 + 8.3.
	if n := run("node render/comfy-generate.mjs out.png prompt --family krea2"); !near(n.GiB, krea2UnetGiB+krea2TEGiB) || n.Source != SourceEstimate {
		t.Errorf("image with COMFY_CKPT = %v, want 32.75 from the files", n)
	}
	// edit: 19.12 from the env UNet, not the documented 19.2.
	if n := run("node render/comfy-edit.mjs out.png in.png edit"); !near(n.GiB, 19.12+8.74) || n.Source != SourceEstimate {
		t.Errorf("edit with COMFY_EDIT_UNET = %v, want 19.12 + 8.74 from the files", n)
	}
	// inpaint: a 20 GiB checkpoint that does not fit the card, named only by the env.
	env["COMFY_CKPT"] = "big_inpaint.safetensors"
	if n := run("node render/comfy-inpaint.mjs out.png in.png mask.png prompt"); !near(n.GiB, 20) {
		t.Errorf("inpaint with COMFY_CKPT = %v, want 20", n)
	}
	// A flag beats the environment, as it does in the runner.
	if n := run("node render/comfy-inpaint.mjs out.png in.png mask.png prompt --ckpt other.safetensors"); near(n.GiB, 20) {
		t.Errorf("--ckpt beats COMFY_CKPT, got %v", n)
	}
}

// The DiffSynth inpaint patch rides a Qwen-Image DiT: --family qwen sizes the UNet and the Qwen2.5-VL
// encoder, not the SDXL checkpoint the other inpaint helper takes.
func TestAQwenInpaintCallIsSizedAsAQwenImageDiT(t *testing.T) {
	dir, stat := comfyTree(t, map[string]float64{"qwen_image_2512_bf16.safetensors": 38.1, "qwen_2.5_vl_7b_fp8_scaled.safetensors": 8.74})
	f := Facts{Cfg: config.Config{ComfyDir: dir}, VRAMGiB: 16, Stat: stat}
	n := Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields("node render/comfy-inpaint.mjs out.png in.png mask.png prompt --family qwen --unet qwen_image_2512_bf16.safetensors --clip qwen_2.5_vl_7b_fp8_scaled.safetensors")}, f)
	if !near(n.GiB, 38.1+8.74) || n.Source != SourceEstimate || !strings.Contains(n.Detail, "qwen-inpaint") {
		t.Fatalf("qwen inpaint = %v, want 38.1 + 8.74 sized as qwen-inpaint", n)
	}
	// With no --clip the documented encoder stands in; with no --unet the documented UNet does.
	n = Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields("node render/comfy-inpaint.mjs out.png in.png mask.png prompt --family qwen")}, Facts{Cfg: config.Config{ComfyDir: dir}, VRAMGiB: 16, Stat: stat})
	if !near(n.GiB, 19.2+8.8) || n.Source != SourceFamilyDefault {
		t.Fatalf("qwen inpaint with nothing named = %v, want the documented 19.2 + 8.8", n)
	}
}

// The helper is recognised by its file name in either path style and either case (Windows file names
// are not case-sensitive), and a bool flag never takes the next positional as its value.
func TestHelperRecognitionIgnoresCaseAndABoolFlagKeepsItsPositional(t *testing.T) {
	call, ok := ParseRenderCall([]string{"node", `D:\Harness\Render\COMFY-GENERATE.MJS`, "o.png", "p", "--family", "krea2"})
	if !ok || call.Script != "comfy-generate.mjs" || call.Flags["family"] != "krea2" {
		t.Fatalf("an upper-cased Windows path must be recognised: %+v ok=%v", call, ok)
	}
	call, ok = ParseRenderCall(strings.Fields("node render/comfy-render.mjs --keep-comfy out.png --family krea2"))
	if !ok || call.Flags["keep-comfy"] != "true" || call.Flags["family"] != "krea2" {
		t.Fatalf("--keep-comfy takes no value, so out.png is not it: %+v", call.Flags)
	}
}

// G4: the agent seat's host footprint. The node's own figure when it states one; an unstated seat is never 0
// (fail closed on a CHOSEN figure, below the 44 GiB the 2026-09-10 incident recorded), a negative figure is a typo and
// reads as unstated.
func TestSeatNeedIsTheConfiguredFigureElseTheFailClosedDefault(t *testing.T) {
	n := SeatNeed(config.Config{AgentSeatHostRAMGiB: 11.6})
	if n.GiB != 11.6 || n.Source != SourceSeat {
		t.Errorf("a configured seat declares its own figure: %+v", n)
	}
	for _, g := range []float64{0, -3} {
		n = SeatNeed(config.Config{AgentSeatHostRAMGiB: g})
		if n.GiB != DefaultSeatHostGiB || n.Source != SourceSeatDefault || n.GiB <= 0 {
			t.Errorf("agent_seat_host_ram_gib = %v must fall to the fail-closed default (never 0): %+v", g, n)
		}
	}
	if DefaultSeatHostGiB < 21 {
		t.Errorf("the default is the chosen fail-closed floor and must not shrink below 21 GiB: %v", DefaultSeatHostGiB)
	}
	// What the default says about itself: a figure the harness CHOSE, not one it measured, and not "the largest on
	// record" (the incident that wrote the check recorded 44 GiB, which is larger than 21). A line that says otherwise
	// tells an operator the warm-back is held to the worst case when it is held to less.
	detail := SeatNeed(config.Config{}).Detail
	for _, claim := range []string{"largest", "on record", "measured so far"} {
		if strings.Contains(detail, claim) {
			t.Errorf("the default seat figure is chosen, so it must not claim %q: %s", claim, detail)
		}
	}
	for _, want := range []string{"chosen", "44 GiB", "agent_seat_host_ram_gib"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the default seat figure must say %q: %s", want, detail)
		}
	}
}

// sd.cpp and the iGPU engines are not sized. The class default is the largest COMFYUI family a box binds, and a binding on the
// "sdcpp" engine is left out of it on purpose: its weights are not in a ComfyUI model tree, and "does it fit the card" means
// nothing on a unified-memory iGPU. So a box that binds only sd.cpp (the shape of the small nodes) declares NOTHING and its
// lanes are admitted whatever the host reads; the spill `--offload-to-cpu` parks in RAM there is outside the guard. The docs say
// so (gpu-lease.md, media-generation.md); this pins it so the docs and the code change together.
func TestAnSdcppOnlyBoxDeclaresNothing(t *testing.T) {
	f := krea2Facts(t, 16)
	// Bound to krea2's files on the sdcpp engine: were the engine not screened out these would size to 32.75 GiB.
	f.Cfg.ImageGenScript, f.Cfg.ImageGenEngine = "render/sdcpp-generate.mjs", "sdcpp"
	f.Cfg.ImageGenFamily, f.Cfg.ImageGenCkpt = "krea2", "krea2_turbo_bf16.safetensors"
	f.Cfg.AnimateGenScript, f.Cfg.AnimateGenEngine = "render/sdcpp-animate.mjs", "sdcpp"
	for name, cfg := range map[string]config.Config{"with a ComfyUI install": f.Cfg, "without one": func() config.Config { c := f.Cfg; c.ComfyDir = ""; return c }()} {
		g := f
		g.Cfg = cfg
		if n := ClassDefault(g); n.GiB != 0 || n.Source != SourceNone {
			t.Errorf("%s: a box that binds only sd.cpp declares nothing, got %v", name, n)
		}
		if n := Resolve(Request{Class: gpulease.ClassMedia, Args: []string{"python", "my_render.py"}}, g); n.GiB != 0 {
			t.Errorf("%s: an unrecognised media command on that box takes the class default, which is 0, got %v", name, n)
		}
	}
	// A ComfyUI family beside it still decides the default: the sd.cpp binding neither raises nor lowers it.
	f.Cfg.GenEditScript, f.Cfg.GenEditFamily, f.Cfg.GenEditUnet = "render/comfy-edit.mjs", "", "qwen_image_edit_2511_fp8mixed.safetensors"
	if n := ClassDefault(f); !near(n.GiB, 19.12+8.74) || !strings.Contains(n.Detail, "edit") {
		t.Errorf("a ComfyUI edit family beside the sd.cpp bindings decides the class default, got %v", n)
	}
}
