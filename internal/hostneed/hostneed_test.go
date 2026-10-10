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
