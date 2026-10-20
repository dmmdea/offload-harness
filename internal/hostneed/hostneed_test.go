package hostneed

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/mediacap"
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

// The same rule for VIDEO: a videogen_families entry on the sdcpp engine renders through sd-cli, whose weights are not in a
// ComfyUI model tree, so the class default leaves it out, a family that is the box's default included. Were it counted it
// would be sized as the ComfyUI builder default of the runner family its name maps to (the sd.cpp entry carries none of the
// ComfyUI keys), an upper bound of weights sd-cli never loads. A ComfyUI video family beside it still decides.
func TestAnSdcppVideoFamilyIsNotCountedInTheClassDefault(t *testing.T) {
	dir, stat := comfyTree(t, map[string]float64{
		"wan_small_high.safetensors": 10, "wan_small_low.safetensors": 10, "wan_small_encoder.safetensors": 2,
	})
	// An sd.cpp family may carry a ComfyUI family's name; unscreened it would be sized as that family's builder defaults.
	sdcppLtx := config.VideoFamilyBinding{Engine: "sdcpp", SdcppModel: "ltx.gguf"}
	// 1. The box's default family is the sd.cpp one: nothing on this box is ComfyUI video, so nothing is declared.
	cfg := config.Config{
		ComfyDir:         dir,
		VideoGenScript:   "render/comfy-video.mjs",
		VideoGenFamily:   "ltx25",
		VideoGenFamilies: map[string]config.VideoFamilyBinding{"ltx25": sdcppLtx},
	}
	if n := ClassDefault(Facts{Cfg: cfg, VRAMGiB: 16, Stat: stat}); n.GiB != 0 || n.Source != SourceNone {
		t.Errorf("a default video family on the sdcpp engine is not counted, so this box declares nothing, got %v", n)
	}
	// 2. A named sd.cpp family beside a smaller ComfyUI default (Wan 2.2 on the flat keys, 10 + 10 + 2, more than the card
	// holds so it streams from RAM): the default decides, not the 34.35 GiB the sd.cpp family would be sized as.
	cfg.VideoGenFamily = ""
	cfg.VideoGenUnetHigh, cfg.VideoGenUnetLow, cfg.VideoGenTextEncoder = "wan_small_high.safetensors", "wan_small_low.safetensors", "wan_small_encoder.safetensors"
	n := ClassDefault(Facts{Cfg: cfg, VRAMGiB: 16, Stat: stat})
	if !near(n.GiB, 22) || strings.Contains(n.Detail, "ltx25") {
		t.Errorf("a named sdcpp video family is not counted: the ComfyUI default family decides (10 + 10 + 2 GiB), got %v", n)
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
// (fail closed on a CHOSEN figure, below the 44 GB the 2026-09-10 incident recorded), a negative figure is a typo and
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
	// record" (the incident that wrote the check recorded 44 GB, which is larger than 21). A line that says otherwise
	// tells an operator the warm-back is held to the worst case when it is held to less.
	detail := SeatNeed(config.Config{}).Detail
	for _, claim := range []string{"largest", "on record", "measured so far"} {
		if strings.Contains(detail, claim) {
			t.Errorf("the default seat figure is chosen, so it must not claim %q: %s", claim, detail)
		}
	}
	for _, want := range []string{"chosen", "44 GB", "agent_seat_host_ram_gib"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the default seat figure must say %q: %s", want, detail)
		}
	}
}

// The CLASS DEFAULT is the largest COMFYUI family a box binds, and a binding on the "sdcpp" engine is left out of it on purpose:
// its weights are not in a ComfyUI model tree, and "does it fit the card" means nothing on a unified-memory iGPU. So on a box
// that binds only sd.cpp (the shape of the small nodes) the class default is NOTHING. That is no longer what the sd.cpp lanes
// declare there (F60): they add the weights they park themselves, ForSdcpp (hostneed_sdcpp_test.go) wired in internal/pipeline
// (sdcpp_need_test.go). The docs say so (gpu-lease.md, media-generation.md); this pins the default so they change together.
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

// THE CARD-ONLY WAN PLACEMENT (comfy-video.mjs --wan-placement multicard, measured 2026-10-11). The two experts never sit on the
// cards together and the text encoder is gone before the first expert lands, so the set does not have to fit ONE card, and it does
// not have to fit as a sum either: what must fit is each card's own stage (the compute card's resident share + the film-size activations + the
// CUDA context, judged here against the larger card; the other card's donor blocks + the VAE and the context) on the TWO
// cards the lease holds. Measured (the film-size run): 15.18 + 10.86 GiB in use across the pair at the peak, 2.3 GiB of host growth, so the call declares 0.
const wanMulticardCmd = "node render/comfy-video.mjs o.mp4 s.png p --model wan --high-unet hi.safetensors --low-unet lo.safetensors --text-encoder te.safetensors --wan-placement multicard"

func wanFacts(t *testing.T, cards ...float64) Facts {
	dir, stat := comfyTree(t, map[string]float64{"hi.safetensors": 13.3, "lo.safetensors": 13.4, "te.safetensors": 6.3})
	f := Facts{Cfg: config.Config{ComfyDir: dir}, Stat: stat, CardsGiB: cards}
	for _, c := range cards {
		if c > f.VRAMGiB {
			f.VRAMGiB = c
		}
	}
	return f
}

func wanNeed(t *testing.T, cmd string, f Facts) Need {
	t.Helper()
	return Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields(cmd)}, f)
}

func TestAMulticardWanCallOnTwoLeasedCardsDeclaresNothing(t *testing.T) {
	n := wanNeed(t, wanMulticardCmd, wanFacts(t, 15.9, 15.9))
	if n.GiB != 0 || n.Source != SourceEstimate {
		t.Fatalf("a multicard Wan call whose stages fit the two leased cards streams nothing from RAM: %v", n)
	}
	for _, want := range []string{"multicard", "2 leased cards", "13.4", "fits"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail must say why (%q missing): %q", want, n.Detail)
		}
	}
	// the flag in its --name=value form is the same call
	if n := wanNeed(t, strings.Replace(wanMulticardCmd, "--wan-placement multicard", "--wan-placement=multicard", 1), wanFacts(t, 15.9, 15.9)); n.GiB != 0 {
		t.Fatalf("--wan-placement=multicard: %v", n)
	}
}

// The default placement is untouched: the same call without the flag, or with the single placement, on the same two cards.
func TestTheSingleWanPlacementIsSizedAsItAlwaysWas(t *testing.T) {
	for _, cmd := range []string{
		strings.Replace(wanMulticardCmd, " --wan-placement multicard", "", 1),
		strings.Replace(wanMulticardCmd, "--wan-placement multicard", "--wan-placement single", 1),
	} {
		n := wanNeed(t, cmd, wanFacts(t, 15.9, 15.9))
		if !near(n.GiB, 13.3+13.4+6.3) || !strings.Contains(n.Detail, "does not fit") {
			t.Fatalf("a call that does not ask for the card-only placement is sized against the ONE largest card: %v", n)
		}
	}
}

// The placement runs on exactly two cards. One card, three cards or an unreadable table are not that placement, so the
// ordinary (conservative) estimate stands.
func TestAMulticardWanCallThatDoesNotHoldTwoCardsKeepsTheOrdinaryEstimate(t *testing.T) {
	for name, cards := range map[string][]float64{"one card": {15.9}, "three cards": {15.9, 15.9, 15.9}, "no table": nil} {
		f := wanFacts(t, cards...)
		if len(cards) == 0 {
			f.VRAMGiB = 0
		}
		if n := wanNeed(t, wanMulticardCmd, f); !near(n.GiB, 13.3+13.4+6.3) {
			t.Errorf("%s: %v, want the ordinary 33.0", name, n)
		}
	}
}

// What must fit is each card's own stage: the compute card (judged here as the LARGER card; the runner may give the role to the faster of two cards
// within 1 percent of one size) holds its resident share of an expert + the film-size activations + the CUDA
// context and the other holds the donor blocks (the rest of the expert + the VAE and the CUDA context). Two 8 GiB cards cannot, two 24 GiB cards can, and
// the SUM of two unequal cards proves nothing: 24 + 8 GiB adds up to plenty, but the 8 GiB card cannot hold a 10.4 GiB donor share.
func TestAMulticardWanCallThatDoesNotFitTheLeasedPairIsSizedAsBefore(t *testing.T) {
	ordinary := func(name string, n Need) {
		t.Helper()
		if !near(n.GiB, 13.3+13.4+6.3) || !strings.Contains(n.Detail, "does not fit") {
			t.Fatalf("%s: %v, want the ordinary 33.0 that does not fit", name, n)
		}
	}
	ordinary("two 8 GiB cards", wanNeed(t, wanMulticardCmd, wanFacts(t, 8, 8)))
	if n := wanNeed(t, wanMulticardCmd, wanFacts(t, 24, 24)); n.GiB != 0 {
		t.Fatalf("two 24 GiB cards: %v, want 0", n)
	}
	// unequal cards: a big card and a small one that cannot hold the donor share (13.4 - 3 + 0.8 = 11.2 GiB beside the 1 GiB reserve)
	ordinary("24 + 8 GiB", wanNeed(t, wanMulticardCmd, wanFacts(t, 24, 8)))
	ordinary("8 + 24 GiB, the other order", wanNeed(t, wanMulticardCmd, wanFacts(t, 8, 24)))
	if n := wanNeed(t, wanMulticardCmd, wanFacts(t, 16, 13)); n.GiB != 0 {
		t.Fatalf("16 + 13 GiB: the 16 GiB card computes (3 + 12.2 + 0.5 = 15.7 of 16) and the 13 GiB card holds 11.2 of 12 usable: %v, want 0", n)
	}
	// the storage card's margin is the VAE and the context beside the donor blocks: 10.4 GiB of blocks fit a 11.7 GiB card (10.7 usable) only
	// without them, 11.2 GiB do not (the benchmark's storage card peaked at 11.1 GiB with 10.3 GiB of blocks)
	ordinary("16 + 11.7 GiB", wanNeed(t, wanMulticardCmd, wanFacts(t, 16, 11.7)))
	ordinary("16 + 12.1 GiB: 11.1 usable against the 11.2 the measured 0.8 GiB overhead asks for", wanNeed(t, wanMulticardCmd, wanFacts(t, 16, 12.1)))
	if n := wanNeed(t, wanMulticardCmd, wanFacts(t, 16, 12.5)); n.GiB != 0 {
		t.Fatalf("16 + 12.5 GiB (11.5 usable >= 11.2): %v, want 0", n)
	}
	// the compute card: resident 3 + 12.2 GiB of activations + 0.5 GiB of CUDA context must fit its size (15.7 GiB; the reserve is the storage card's)
	ordinary("2 x 14.5 GiB", wanNeed(t, wanMulticardCmd, wanFacts(t, 14.5, 14.5)))
	ordinary("2 x 15.1 GiB, which the 11.0 GiB figure and the reserve admitted (3 + 12.2 + 0.5 = 15.7 > 15.1)", wanNeed(t, wanMulticardCmd, wanFacts(t, 15.1, 15.1)))
	ordinary("2 x 15.6 GiB", wanNeed(t, wanMulticardCmd, wanFacts(t, 15.6, 15.6)))
	for _, size := range []float64{15.7, 15.8} {
		if n := wanNeed(t, wanMulticardCmd, wanFacts(t, size, size)); n.GiB != 0 {
			t.Fatalf("2 x %v GiB (3 + 12.2 + 0.5 = 15.7 fits): %v, want 0", size, n)
		}
	}
	// --reserve-vram is the runner's: a larger reserve takes more of the STORAGE card (the donor blocks and the VAE need 11.2 GiB of 15.9 less the reserve)
	ordinary("a 5 GiB reserve on 2 x 15.9 GiB (10.9 usable on the storage card)", wanNeed(t, wanMulticardCmd+" --reserve-vram 5", wanFacts(t, 15.9, 15.9)))
	if n := wanNeed(t, wanMulticardCmd+" --reserve-vram 4", wanFacts(t, 15.9, 15.9)); n.GiB != 0 {
		t.Fatalf("a 4 GiB reserve on 2 x 15.9 GiB (11.9 usable >= 11.2 on the storage card): %v, want 0", n)
	}
	// a card whose size is unknown (0) is no pair
	ordinary("a zero-size card", wanNeed(t, wanMulticardCmd, wanFacts(t, 0, 15.9)))
}

// The resident share is the call's --wan-resident-gb (default 3, the runner's own): a bigger share needs more of the compute card and
// less of the storage card.
func TestTheResidentShareIsReadFromTheCall(t *testing.T) {
	two := wanFacts(t, 15.9, 15.9)
	ordinary := func(name string, n Need) {
		t.Helper()
		if !near(n.GiB, 13.3+13.4+6.3) {
			t.Fatalf("%s: %v, want the ordinary 33.0", name, n)
		}
	}
	ordinary("a 6 GiB share (6 + 12.2 + 0.5 = 18.7 > 15.9 on the compute card)", wanNeed(t, wanMulticardCmd+" --wan-resident-gb 6", two))
	if n := wanNeed(t, wanMulticardCmd+" --wan-resident-gb 0", two); n.GiB != 0 {
		t.Fatalf("a 0 GiB share (everything on the storage card: 13.4 + 0.8 = 14.2 of 14.9 usable): %v, want 0", n)
	}
	if n := wanNeed(t, wanMulticardCmd+" --wan-resident-gb=3.2", two); n.GiB != 0 {
		t.Fatalf("the --name=value form is read as the flag: %v, want 0", n)
	}
	ordinary("the --name=value form with a 3.5 GiB share (3.5 + 12.2 + 0.5 = 16.2 > 15.9)", wanNeed(t, wanMulticardCmd+" --wan-resident-gb=3.5", two))
	// an unreadable or out-of-range value is the default (the runner refuses such a call before it starts)
	for _, bad := range []string{"x", "-1", "65", "NaN", "Inf"} {
		if n := wanNeed(t, wanMulticardCmd+" --wan-resident-gb "+bad, two); n.GiB != 0 {
			t.Fatalf("--wan-resident-gb %s: %v, want the default share (0 on two 15.9 GiB cards)", bad, n)
		}
	}
	// the storage card holds the rest of the expert: a 0 GiB share does not fit a 12 GiB card beside a 16 GiB one
	ordinary("a 0 GiB share on 16 + 12 GiB", wanNeed(t, wanMulticardCmd+" --wan-resident-gb 0", wanFacts(t, 16, 12)))
	if n := wanNeed(t, wanMulticardCmd, wanFacts(t, 16, 13)); n.GiB != 0 {
		t.Fatalf("the default 3 GiB share on 16 + 13 GiB (the 13 GiB card holds 11.2 GiB, 12 usable): %v, want 0", n)
	}
}

// THE FILM-SIZE RUN IS THE ACCEPTANCE CRITERION. On 2026-10-11 the film (720x1280x81, 20 steps, plain decode) ran on two RTX 5060 Ti 16 GB with
// --ram 0 and the runner's default share of 3 GiB: the compute card peaked at 15.18 GiB (3.0 resident + 12.2 beyond it), the storage card at 10.86 GiB,
// and the host grew by 2.3 GiB. The runner's planner (render/wan-multicard.mjs) admits a share up to the card less those 12.2 GiB less a 0.5 GiB context,
// 3.2 GiB on a 15.9 GiB card. This sizing must agree with both: a call the planner admits and that was measured to run declares 0, and one it refuses
// does not, or the lease sends the measured call to the 46.8 GiB class default, which the host-RAM brake then holds back for hours.
func TestTheMeasuredFilmSizeRunDeclaresNothingUpToThePlannersCeiling(t *testing.T) {
	two := wanFacts(t, 15.9, 15.9)
	ordinary := func(name string, n Need) {
		t.Helper()
		if !near(n.GiB, 13.3+13.4+6.3) || !strings.Contains(n.Detail, "does not fit") {
			t.Fatalf("%s: %v, want the ordinary 33.0 that does not fit", name, n)
		}
	}
	declaresNothing := func(name string, n Need) {
		t.Helper()
		if n.GiB != 0 || !strings.Contains(n.Detail, "card-only") {
			t.Fatalf("%s: %v, want the card-only 0 (the film-size run was measured to run with --ram 0)", name, n)
		}
	}
	declaresNothing("the runner's default share on 2 x 15.9 GiB", wanNeed(t, wanMulticardCmd, two))
	for _, share := range []string{"3", "3.0", "3.2"} {
		declaresNothing("a "+share+" GiB share on 2 x 15.9 GiB", wanNeed(t, wanMulticardCmd+" --wan-resident-gb "+share, two))
	}
	for _, share := range []string{"3.3", "3.5", "4"} {
		ordinary("a "+share+" GiB share, above the planner's 3.2 GiB ceiling", wanNeed(t, wanMulticardCmd+" --wan-resident-gb "+share, two))
	}
	// --reserve-vram is ComfyUI's model-load margin, not an activation ceiling, so the compute card's check does not use it: a 2 GiB reserve declares what the
	// default does. The film-size run itself ran at the runner's default reserve of 1.0; that a larger one leaves the compute card's peak where it was is assumed, not measured.
	declaresNothing("the measured share under a 2 GiB --reserve-vram", wanNeed(t, wanMulticardCmd+" --reserve-vram 2", two))
	// the real cards: 16,311 MiB is 15.93 GiB, so the ceiling is 3.22
	realCards := wanFacts(t, 16311.0/1024, 16311.0/1024)
	declaresNothing("a 3 GiB share on two 16,311 MiB cards", wanNeed(t, wanMulticardCmd, realCards))
	declaresNothing("a 3.22 GiB share on two 16,311 MiB cards", wanNeed(t, wanMulticardCmd+" --wan-resident-gb 3.22", realCards))
	ordinary("a 3.25 GiB share on two 16,311 MiB cards", wanNeed(t, wanMulticardCmd+" --wan-resident-gb 3.25", realCards))
}

// The two sides agree where it matters: the ceiling the runner's planner draws from ITS constants is the one this sizing draws from its own.
func TestTheComputeCardCeilingIsTheRunnersCeiling(t *testing.T) {
	activations := wanRunnerConst(t, "WAN_FILM_ACTIVATIONS_GIB")
	contextGiB := wanRunnerConst(t, "WAN_COMPUTE_CONTEXT_GIB")
	const card = 15.9
	ceiling := math.Round((card-activations-contextGiB)*10) / 10 // the planner's "at most" at film size, to a tenth, as it prints it
	if ceiling <= 0 {
		t.Fatalf("the runner's constants leave no room for a resident share on a %v GiB card: %v + %v", card, activations, contextGiB)
	}
	at := wanNeed(t, wanMulticardCmd+" --wan-resident-gb "+strconv.FormatFloat(ceiling, 'f', 1, 64), wanFacts(t, card, card))
	above := wanNeed(t, wanMulticardCmd+" --wan-resident-gb "+strconv.FormatFloat(ceiling+0.1, 'f', 1, 64), wanFacts(t, card, card))
	if at.GiB != 0 {
		t.Errorf("the runner admits a %.1f GiB share at film size on a %v GiB card (%v + %v), this sizing must declare 0 for it: %v", ceiling, card, activations, contextGiB, at)
	}
	if above.GiB == 0 {
		t.Errorf("the runner refuses a %.1f GiB share at film size on a %v GiB card, this sizing must not declare 0 for it: %v", ceiling+0.1, card, above)
	}
}

// The default share is the runner's: render/wf-wan22-i2v.mjs names it, and the two must stay one number (a different default here would
// size a call the runner then runs differently).
func TestTheResidentDefaultMirrorsTheRunner(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "render", "wf-wan22-i2v.mjs"))
	if err != nil {
		t.Skipf("the render folder is not beside this package: %v", err)
	}
	m := regexp.MustCompile(`WAN_RESIDENT_GB_DEFAULT = ([0-9.]+);`).FindSubmatch(src)
	if m == nil {
		t.Fatal("render/wf-wan22-i2v.mjs no longer defines WAN_RESIDENT_GB_DEFAULT; this package reads it")
	}
	got, _ := strconv.ParseFloat(string(m[1]), 64)
	if got != wanResidentDefaultGiB {
		t.Fatalf("the runner keeps %v GiB of each expert resident by default, this package assumes %v", got, wanResidentDefaultGiB)
	}
	if !regexp.MustCompile(`WAN_PLACEMENTS = Object\.freeze\(\["single", "multicard"\]\)`).Match(src) {
		t.Fatal("the runner's placements are no longer single|multicard; --wan-placement multicard is what this package sizes")
	}
}

// Only the Wan and LTX-2.5 graphs have this placement: the flag on another family (h3 here), or on a --graph file, is not read as it.
//
// The pair is two 24 GiB cards on purpose: the card-only stage maths (a share of the expert + 12.2 GiB of activations on one card, the rest of the
// expert + 0.8 GiB on the other) cannot fit a 20 GiB transformer on two 16 GiB cards, so there the figure is the ordinary one with or
// without the model guard in RenderCall.Estimate, and a test on such a pair passes with the guard deleted (the review of
// 2026-10-11 deleted it and nothing failed). On 24 GiB cards the unguarded code would declare 0 for a call that streams the whole set.
func TestTheMulticardFlagMeansNothingToAnotherFamily(t *testing.T) {
	f := ltxFacts(t, 24)
	f.CardsGiB = []float64{24, 24}
	ltx := func(flags string) Need {
		return Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields("node render/comfy-video.mjs o.mp4 s.png p " + flags)}, f)
	}
	a, b := ltx("--model h3"), ltx("--model h3 --wan-placement multicard")
	if a.GiB <= 0 {
		t.Fatalf("the premise of this test: h3 does not fit one 24 GiB card and streams from RAM, got %v", a)
	}
	if a.GiB != b.GiB || a.Detail != b.Detail {
		t.Fatalf("h3 with the flag = %v, without = %v", b, a)
	}
	for _, n := range []Need{a, b} {
		if strings.Contains(n.Detail, "card-only") {
			t.Fatalf("another family's declaration must never talk about the card-only placement: %v", n)
		}
	}
	// the Wan call on the same pair is the one that does fit it, so the two figures differ by the placement alone
	w := wanFacts(t, 24, 24)
	if n := wanNeed(t, wanMulticardCmd, w); n.GiB != 0 || !strings.Contains(n.Detail, "card-only") {
		t.Fatalf("the Wan call on two 24 GiB cards: %v, want the card-only 0", n)
	}
}

// The stage sizes are the runner's: render/wan-multicard.mjs checks the same two stages (the compute card and the storage card) against the
// cards it was given before it starts, and a different number on either side would make the lease declare for a placement the runner then
// refuses, or run one the lease sized against other figures. A test reads the runner's constants and fails when they drift.
func TestTheStageSizesMirrorTheRunner(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "render", "wan-multicard.mjs"))
	if err != nil {
		t.Skipf("the render folder is not beside this package: %v", err)
	}
	read := func(name string) float64 { return wanRunnerConstFrom(t, src, name) }
	if got := read("WAN_FILM_ACTIVATIONS_GIB"); got != wanActivationsGiB {
		t.Fatalf("the runner sizes the film's activations at %v GiB, this package at %v", got, wanActivationsGiB)
	}
	if got := read("WAN_COMPUTE_CONTEXT_GIB"); got != wanComputeContextGiB {
		t.Fatalf("the runner keeps %v GiB of the compute card for the CUDA context, this package %v", got, wanComputeContextGiB)
	}
	if got := read("WAN_STORAGE_OVERHEAD_GIB"); got != wanStorageOverheadGiB {
		t.Fatalf("the runner sizes the storage card's overhead at %v GiB, this package at %v", got, wanStorageOverheadGiB)
	}
	if got, want := read("WAN_EXPERT_GIB"), familySizes["wan22"][mediacap.RoleDiffusion][0]; got != want {
		t.Fatalf("the runner sizes an expert at %v GiB, this package's documented Wan 2.2 expert is %v", got, want)
	}
}

// wanRunnerConst is an exported constant of render/wan-multicard.mjs (`export const NAME = 1.5;`).
func wanRunnerConst(t *testing.T, name string) float64 {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "render", "wan-multicard.mjs"))
	if err != nil {
		t.Skipf("the render folder is not beside this package: %v", err)
	}
	return wanRunnerConstFrom(t, src, name)
}

func wanRunnerConstFrom(t *testing.T, src []byte, name string) float64 {
	t.Helper()
	m := regexp.MustCompile(`export const ` + name + ` = ([0-9.]+);`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("render/wan-multicard.mjs no longer exports %s; this package reads it", name)
	}
	v, _ := strconv.ParseFloat(string(m[1]), 64)
	return v
}

// ---- sage attention (--use-sage-attention in COMFY_EXTRA_ARGS) --------------------------------------------------------------
//
// The film session of 2026-10-12 ran the film clip with sage attention on a 16 GB card: 15,482 MiB in use at resident 0, 3.0 GiB more than the default
// attention's 12.2 GiB beyond the resident share. Neither the runner nor this sizing knew the attention mode, so a sage call with the default share of 3
// (3 + 12.2 + 0.5 = 15.7 of 15.9) was admitted by both and ran out of memory. COMFY_EXTRA_ARGS is read from the environment on both sides.

// extraArgsEnv is a Facts.Env that carries COMFY_EXTRA_ARGS and nothing else, so no test here reads the host's environment.
func extraArgsEnv(extra string) func(string) string {
	return func(k string) string {
		if k == "COMFY_EXTRA_ARGS" {
			return extra
		}
		return ""
	}
}

func TestASageAttentionWanCallIsSizedForSageActivations(t *testing.T) {
	sage := wanFacts(t, 15.9, 15.9)
	sage.Env = extraArgsEnv("--use-sage-attention")
	// the compute card holds the share + 15.2 GiB of sage activations + 0.5 GiB of context: 0.2 GiB of an expert at most on a 15.9 GiB card
	for _, share := range []string{"0", "0.2"} {
		n := wanNeed(t, wanMulticardCmd+" --wan-resident-gb "+share, sage)
		if n.GiB != 0 || n.Source != SourceEstimate || !strings.Contains(n.Detail, "card-only") || !strings.Contains(n.Detail, "sage attention") {
			t.Errorf("--wan-resident-gb %s under sage attention: %v, want the card-only 0 that says it was sized for sage attention", share, n)
		}
	}
	// the default share, and every share above the ceiling, is the ordinary set, and the detail says why and how to state the real figure
	for _, args := range []string{"", " --wan-resident-gb 3", " --wan-resident-gb 0.3", " --wan-resident-gb 2"} {
		n := wanNeed(t, wanMulticardCmd+args, sage)
		if !near(n.GiB, 13.3+13.4+6.3) || !strings.Contains(n.Detail, "does not fit") {
			t.Errorf("%q under sage attention: %v, want the ordinary 33.0 that does not fit (3 + 15.2 + 0.5 = 18.7 > 15.9)", args, n)
		}
		for _, want := range []string{"--use-sage-attention", "COMFY_EXTRA_ARGS", "15.2 GiB", "--ram 0"} {
			if !strings.Contains(n.Detail, want) {
				t.Errorf("%q under sage attention: the detail must say how to state the real figure (%q missing): %q", args, want, n.Detail)
			}
		}
	}
	// the controls: the same calls with no extra args, another attention flag, or an unrelated one, are the default sizing, where the default share declares 0
	for name, env := range map[string]func(string) string{
		"no environment": nil, "empty": extraArgsEnv(""), "flash attention": extraArgsEnv("--use-flash-attention"),
		"unrelated": extraArgsEnv("--x 1 --y"), "a longer flag": extraArgsEnv("--use-sage-attention-extra"),
	} {
		f := wanFacts(t, 15.9, 15.9)
		f.Env = env
		n := wanNeed(t, wanMulticardCmd, f)
		if n.GiB != 0 || strings.Contains(n.Detail, "sage") {
			t.Errorf("%s: %v, want the default sizing (the default share declares 0) with no word about sage", name, n)
		}
	}
	// the sizing reads no geometry: a measured partial share at a smaller clip is the ordinary set here unless --ram 0 states the figure (documented)
	if n := wanNeed(t, wanMulticardCmd+" --wan-resident-gb 2 --width 720 --height 1280 --frames 65", sage); !near(n.GiB, 13.3+13.4+6.3) {
		t.Errorf("a sage call at a smaller clip is still sized for the film's 15.2 GiB: %v, want the ordinary 33.0", n)
	}
}

// The hint on a sage call's ordinary detail ("--ram 0 states the real figure") answers one cause: the COMPUTE stage that the film's 15.2 GiB pushed out (a smaller
// clip's partial share would fit it). When the storage card is the one that cannot hold its stage, the same hint would blame the geometry-blind sizing and send the
// caller to an advice that changes nothing, so it is withheld; the ordinary set is declared in every case.
func TestTheSageHintIsForAMissedComputeStageOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		cards []float64
		hint  bool
	}{
		"the compute stage misses and the storage card fits (two 15.9 GiB cards)":              {[]float64{15.9, 15.9}, true},
		"the compute stage fits a 24 GiB card, the storage card (8 GiB) cannot hold its stage": {[]float64{24, 8}, false},
		"both stages miss: the 8 GiB storage card cannot hold its stage either":                {[]float64{15.9, 8}, false},
	} {
		f := wanFacts(t, tc.cards...)
		f.Env = extraArgsEnv("--use-sage-attention")
		n := wanNeed(t, wanMulticardCmd, f)
		if !near(n.GiB, 13.3+13.4+6.3) || !strings.Contains(n.Detail, "does not fit") {
			t.Errorf("%s: %v, want the ordinary 33.0 that does not fit", name, n)
		}
		if got := strings.Contains(n.Detail, "--ram 0"); got != tc.hint {
			t.Errorf("%s: the --ram 0 hint present = %v, want %v: %q", name, got, tc.hint, n.Detail)
		}
	}
}

// The sage figure is the runner's: render/wan-multicard.mjs WAN_FILM_ACTIVATIONS_SAGE_GIB, and a different number on either side would make the lease
// declare for a call the runner then refuses, or run one it sized against other figures.
func TestTheSageActivationsMirrorTheRunner(t *testing.T) {
	if got := wanRunnerConst(t, "WAN_FILM_ACTIVATIONS_SAGE_GIB"); got != wanActivationsSageGiB {
		t.Fatalf("the runner sizes the film's activations under sage attention at %v GiB, this package at %v", got, wanActivationsSageGiB)
	}
}

// The two sides agree where it matters: the ceiling the runner's planner draws from ITS sage constants is the one this sizing draws from its own.
func TestTheSageComputeCardCeilingIsTheRunnersCeiling(t *testing.T) {
	activations := wanRunnerConst(t, "WAN_FILM_ACTIVATIONS_SAGE_GIB")
	contextGiB := wanRunnerConst(t, "WAN_COMPUTE_CONTEXT_GIB")
	const card = 15.9
	ceiling := math.Round((card-activations-contextGiB)*10) / 10
	if ceiling < 0 {
		t.Fatalf("the runner's constants leave no room for a resident share on a %v GiB card: %v + %v", card, activations, contextGiB)
	}
	f := wanFacts(t, card, card)
	f.Env = extraArgsEnv("--use-sage-attention")
	at := wanNeed(t, wanMulticardCmd+" --wan-resident-gb "+strconv.FormatFloat(ceiling, 'f', 1, 64), f)
	above := wanNeed(t, wanMulticardCmd+" --wan-resident-gb "+strconv.FormatFloat(ceiling+0.1, 'f', 1, 64), f)
	if at.GiB != 0 {
		t.Errorf("the runner admits a %.1f GiB share at film size under sage on a %v GiB card (%v + %v), this sizing must declare 0 for it: %v", ceiling, card, activations, contextGiB, at)
	}
	if above.GiB == 0 {
		t.Errorf("the runner refuses a %.1f GiB share at film size under sage on a %v GiB card, this sizing must not declare 0 for it: %v", ceiling+0.1, card, above)
	}
}

// The token rule is the runner's (render/comfy-lifecycle.mjs isSageAttentionToken, and wanAttentionMode over a COMFY_EXTRA_ARGS value). ONE table, read by this test
// and by render/wan-multicard.test.mjs (render/testdata/sage-token-table.json), so the two languages cannot read one value two ways: a row changed there turns an answer in
// both, and a row added is checked by both. ComfyUI's parser accepts an unambiguous prefix of a flag (argparse allow_abbrev), so `--use-sa` IS sage attention and a missed
// prefix would size the call for the default; `--use-s` begins --use-split-cross-attention too and is not.
func TestTheSageTokenRuleMatchesTheRunner(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "render", "testdata", "sage-token-table.json"))
	if err != nil {
		if _, e := os.Stat(filepath.Join("..", "..", "render")); e != nil {
			t.Skipf("the render folder is not beside this package: %v", e)
		}
		t.Fatalf("the shared sage token table is missing: %v", err)
	}
	var table struct {
		Rows []struct {
			Extra string `json:"extra"`
			Sage  bool   `json:"sage"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("the shared sage token table does not parse: %v", err)
	}
	var yes, no int
	for _, row := range table.Rows {
		if row.Sage {
			yes++
		} else {
			no++
		}
	}
	if len(table.Rows) < 16 || yes == 0 || no == 0 {
		t.Fatalf("the shared sage token table looks empty or one-sided: %d rows, %d sage, %d not", len(table.Rows), yes, no)
	}
	for _, row := range table.Rows {
		if got := sageAttention(row.Extra); got != row.Sage {
			t.Errorf("sageAttention(%q) = %v, the shared table (and the runner) read it as %v", row.Extra, got, row.Sage)
		}
		f := wanFacts(t, 15.9, 15.9)
		f.Env = extraArgsEnv(row.Extra)
		// the default share declares 0 under the default attention and the ordinary set under sage, so the outcome shows which mode the value read as
		n := wanNeed(t, wanMulticardCmd, f)
		if got := n.GiB != 0; got != row.Sage {
			t.Errorf("COMFY_EXTRA_ARGS=%q: the sizing read it as sage = %v, want %v (%v)", row.Extra, got, row.Sage, n)
		}
	}
}

func TestLeasedCardsGiB(t *testing.T) {
	cards := []gpuprobe.Card{
		{UUID: "GPU-aaaa0000", VRAMTotalGiB: 16},
		{UUID: "GPU-bbbb0000", VRAMTotalGiB: 24},
		{UUID: "GPU-cccc0000", VRAMTotalGiB: 8},
	}
	if got := LeasedCardsGiB(cards, []string{cards[0].LeaseID(), cards[2].LeaseID()}); len(got) != 2 || got[0] != 16 || got[1] != 8 {
		t.Fatalf("the leased cards, in table order: %v, want [16 8]", got)
	}
	if got := LeasedCardsGiB(cards, nil); len(got) != 3 {
		t.Fatalf("no ids is every card: %v", got)
	}
	if got := LeasedCardsGiB(nil, nil); len(got) != 0 {
		t.Fatalf("no card table: %v, want none (unknown)", got)
	}
}
