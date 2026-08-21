package pipeline

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// webpVP8LHeader is a 30-byte header-only VP8L WebP fixture (the same encoding
// imagegen.OutputSize and render/image-size.mjs read); nothing here decodes pixels.
func webpVP8LHeader(w, h int) []byte {
	b := make([]byte, 30)
	copy(b[0:4], "RIFF")
	copy(b[8:12], "WEBP")
	copy(b[12:16], "VP8L")
	b[20] = 0x2f
	bits := uint32(w-1) | uint32(h-1)<<14
	b[21], b[22], b[23], b[24] = byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24)
	return b
}

func newUpscalePipeline(cfg config.Config) *Pipeline {
	return New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), nil, nil)
}

// writePNG writes a w x h PNG and returns its path.
func writePNG(t *testing.T, path string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.White)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpscaleDeferReasons(t *testing.T) {
	dir := t.TempDir()
	img := writePNG(t, filepath.Join(dir, "a.png"), 4, 4)
	run := func(p *Pipeline, params map[string]any) core.Result {
		t.Helper()
		return p.Run(context.Background(), core.Request{Task: core.TaskUpscaleImage, Params: params})
	}

	// default config ships the script but no model → unconfigured, naming both model keys
	cfg := config.Default()
	res := run(newUpscalePipeline(cfg), map[string]any{"image": img})
	if res.OK || !strings.HasPrefix(res.Reason, "no upscale route configured") || !strings.Contains(res.Reason, "videogen_upscale_model") {
		t.Fatalf("want unconfigured defer naming the fallback key, got %+v", res)
	}
	// an empty script is its own reason (the two surfaces must not blame the model for a script gap)
	cfg = config.Default()
	cfg.UpscaleScript = ""
	cfg.UpscaleModel = "4x-UltraSharp.pth"
	res = run(newUpscalePipeline(cfg), map[string]any{"image": img})
	if res.OK || !strings.Contains(res.Reason, "upscale_script unset") {
		t.Fatalf("want script-unset defer, got %+v", res)
	}

	// the video route's upscaler binds stills too (EffectiveUpscaleModel fallback):
	// with it set the gate passes and the next check (missing image) is what defers
	cfg = config.Default()
	cfg.VideoGenUpscaleModel = "4x-UltraSharp.pth"
	p := newUpscalePipeline(cfg)
	res = run(p, nil)
	if res.OK || res.Reason != "upscale requires params.image" {
		t.Fatalf("want missing-image defer past the fallback-bound gate, got %+v", res)
	}
	res = run(p, map[string]any{"image": filepath.Join(dir, "missing.png")})
	if res.OK || !strings.HasPrefix(res.Reason, "upscale input not found") {
		t.Fatalf("want input-not-found defer, got %+v", res)
	}

	// every parameter gate defers BEFORE script resolution or GPU work: the runner here
	// does not exist, so reaching it would surface "script not found" instead
	cfg = config.Default()
	cfg.UpscaleModel = "4x-UltraSharp.pth"
	cfg.UpscaleScript = filepath.Join(dir, "definitely-missing-runner.mjs")
	p = newUpscalePipeline(cfg)
	for _, tc := range []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"half size", map[string]any{"image": img, "width": 3000}, "upscale width and height must be given together"},
		{"negative pair", map[string]any{"image": img, "width": -5, "height": -5}, "upscale width and height must be positive integers"},
		{"negative half", map[string]any{"image": img, "width": 3000, "height": -1}, "upscale width and height must be positive integers"},
		{"oversized", map[string]any{"image": img, "width": 20000, "height": 100}, "upscale width and height must be <= 16384 (ComfyUI limit), got 20000x100"},
		{"negative scale", map[string]any{"image": img, "scale": -2.0}, "upscale scale must be > 0"},
		{"zero scale (forwarded by the MCP handler)", map[string]any{"image": img, "scale": 0.0}, "upscale scale must be > 0"},
		{"int scale zero", map[string]any{"image": img, "scale": 0}, "upscale scale must be > 0"},
		{"bad method", map[string]any{"image": img, "method": "magic"}, "upscale method must be one of lanczos|bicubic|bilinear|area|nearest-exact, got magic"},
		{"absolute model path", map[string]any{"image": img, "model": `V:\models\4x.pth`}, `upscale model must be a name relative to ComfyUI's upscale_models/ (subfolders allowed), got V:\models\4x.pth`},
		{"parent-escaping model", map[string]any{"image": img, "model": `../checkpoints/4x.pth`}, `upscale model must be a name relative to ComfyUI's upscale_models/ (subfolders allowed), got ../checkpoints/4x.pth`},
		{"rooted model", map[string]any{"image": img, "model": `/4x.pth`}, `upscale model must be a name relative to ComfyUI's upscale_models/ (subfolders allowed), got /4x.pth`},
		{"scale past the ComfyUI limit", map[string]any{"image": img, "scale": 5000.0}, "upscale scale 5000 on a 4x4 source needs 20000x20000, above ComfyUI's 16384 limit"},
	} {
		res = run(p, tc.params)
		if res.OK || res.Reason != tc.want {
			t.Fatalf("%s: want %q, got %+v", tc.name, tc.want, res)
		}
	}
	// ...and with valid params the missing runner is the (actionable) reason
	res = run(p, map[string]any{"image": img, "scale": 2.0, "method": "bicubic"})
	if res.OK || !strings.Contains(res.Reason, "script not found at") {
		t.Fatalf("want script-not-found defer, got %+v", res)
	}
	// an int scale (CLI/json.Number shapes) passes the gate like a float
	res = run(p, map[string]any{"image": img, "scale": 2})
	if res.OK || !strings.Contains(res.Reason, "script not found at") {
		t.Fatalf("want int scale accepted (script-not-found), got %+v", res)
	}
	// a subfolder model name is how ComfyUI lists upscale_models/ESRGAN/4x.pth — accepted
	// on either separator, as is an odd-but-legal ".." inside a filename
	for _, name := range []string{"ESRGAN/4x-UltraSharp.pth", `ESRGAN\4x-UltraSharp.pth`, "4x..pth", "./4x.pth"} {
		res = run(p, map[string]any{"image": img, "model": name})
		if res.OK || !strings.Contains(res.Reason, "script not found at") {
			t.Fatalf("want model %q accepted (script-not-found), got %+v", name, res)
		}
	}
	// ...while a drive-relative name and a UNC path are still refused
	for _, name := range []string{"C:4x.pth", `\\server\share\4x.pth`, `ESRGAN\..\..\x.pth`} {
		res = run(p, map[string]any{"image": img, "model": name})
		if res.OK || !strings.HasPrefix(res.Reason, "upscale model must be a name relative to") {
			t.Fatalf("want model %q refused, got %+v", name, res)
		}
	}

	// a request model overrides an empty binding and passes the gate
	cfg = config.Default()
	cfg.UpscaleScript = filepath.Join(dir, "definitely-missing-runner.mjs")
	res = run(newUpscalePipeline(cfg), map[string]any{"image": img, "model": "RealESRGAN_x4plus.pth"})
	if res.OK || !strings.Contains(res.Reason, "script not found at") {
		t.Fatalf("want gate passed on request model (script-not-found), got %+v", res)
	}
}

// TestUpscaleVerifiesWrittenFile drives the real Go → node → file path with a stub
// runner that copies $UPSCALE_STUB_SRC to the out path (or writes junk), so the
// post-render verification is exercised at its real call site: a wrong size and an
// undecodable file must DEFER, a matching size must report width/height/factor.
func TestUpscaleVerifiesWrittenFile(t *testing.T) {
	dir := t.TempDir()
	src := writePNG(t, filepath.Join(dir, "src.png"), 4, 4)
	eight := writePNG(t, filepath.Join(dir, "eight.png"), 8, 8)
	junk := filepath.Join(dir, "junk.bin")
	if err := os.WriteFile(junk, []byte("not a png at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "stub-upscale.mjs")
	if err := os.WriteFile(stub, []byte(`import {copyFileSync} from "node:fs";
copyFileSync(process.env.UPSCALE_STUB_SRC, process.argv[2]);
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.UpscaleScript = stub
	cfg.UpscaleModel = "4x-UltraSharp.pth"
	cfg.MediaDir = filepath.Join(dir, "media")
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.GPULockPath = filepath.Join(dir, "gpu.lock")
	p := newUpscalePipeline(cfg)
	run := func(params map[string]any) core.Result {
		t.Helper()
		return p.Run(context.Background(), core.Request{Task: core.TaskUpscaleImage, Params: params})
	}

	// scale 3 on a 4x4 PNG source expects 12x12; the stub writes 8x8 → defer naming both,
	// and — since the runner measures PNG too — blaming the renderer, not a fallback
	t.Setenv("UPSCALE_STUB_SRC", eight)
	res := run(map[string]any{"image": src, "scale": 3.0, "out": filepath.Join(dir, "o1.png")})
	if res.OK || !strings.Contains(res.Reason, "upscale produced 8x8, expected 12x12 for scale 3 on a 4x4 png source (the runner pinned that size and the renderer did not honor it)") {
		t.Fatalf("want size-mismatch defer, got %+v", res)
	}
	// a pinned size that the runner did not honor defers the same way
	res = run(map[string]any{"image": src, "width": 16, "height": 16, "out": filepath.Join(dir, "o2.png")})
	if res.OK || !strings.Contains(res.Reason, "upscale produced 8x8, expected 16x16 for the pinned 16x16 (the renderer did not honor the requested size)") {
		t.Fatalf("want pinned-size mismatch defer, got %+v", res)
	}
	// a GIF source is measured by Go but NOT by the runner (image-size.mjs has no GIF),
	// so the same mismatch names the filename-factor fallback and the fix
	gifSrc := filepath.Join(dir, "src.gif")
	{
		f, err := os.Create(gifSrc)
		if err != nil {
			t.Fatal(err)
		}
		if err := gif.Encode(f, image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White}), nil); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	res = run(map[string]any{"image": gifSrc, "scale": 3.0, "out": filepath.Join(dir, "o1g.png")})
	if res.OK || !strings.Contains(res.Reason, "for scale 3 on a 4x4 gif source (the runner cannot measure this format and used the model's filename factor instead") {
		t.Fatalf("want GIF mismatch naming the fallback, got %+v", res)
	}
	// a non-uniform pinned size reports factor_x / factor_y instead of a single factor
	wide := writePNG(t, filepath.Join(dir, "wide.png"), 8, 2)
	t.Setenv("UPSCALE_STUB_SRC", wide)
	res = run(map[string]any{"image": src, "width": 8, "height": 2, "out": filepath.Join(dir, "o2w.png")})
	if !res.OK {
		t.Fatalf("want OK on a non-uniform pinned size, got %+v", res)
	}
	var nu map[string]any
	if err := json.Unmarshal(res.Data, &nu); err != nil {
		t.Fatal(err)
	}
	if _, has := nu["factor"]; has || nu["factor_x"] != 2.0 || nu["factor_y"] != 0.5 {
		t.Fatalf("non-uniform result = %v, want factor_x 2 factor_y 0.5 and no factor", nu)
	}
	// ...but per-axis rounding noise on a uniform scale is NOT non-uniform: a 3x5 source
	// at scale 2.33 renders 7x12 (2.33 vs 2.4 per axis) and still reports one factor
	odd := writePNG(t, filepath.Join(dir, "odd.png"), 3, 5)
	oddOut := writePNG(t, filepath.Join(dir, "odd-out.png"), 7, 12)
	t.Setenv("UPSCALE_STUB_SRC", oddOut)
	res = run(map[string]any{"image": odd, "scale": 2.33, "out": filepath.Join(dir, "o2o.png")})
	if !res.OK {
		t.Fatalf("want OK on the odd-size uniform scale, got %+v", res)
	}
	nu = nil
	if err := json.Unmarshal(res.Data, &nu); err != nil {
		t.Fatal(err)
	}
	if _, split := nu["factor_x"]; split || nu["factor"] != 2.33 {
		t.Fatalf("odd-size uniform result = %v, want a single factor 2.33", nu)
	}
	t.Setenv("UPSCALE_STUB_SRC", eight)
	// scale 2 → 8x8 expected, 8x8 written → OK with the size and the MEASURED factor
	res = run(map[string]any{"image": src, "scale": 2.0, "out": filepath.Join(dir, "o3.png")})
	if !res.OK {
		t.Fatalf("want OK on a matching size, got %+v", res)
	}
	var got map[string]any
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got["width"] != 8.0 || got["height"] != 8.0 || got["factor"] != 2.0 || got["model"] != "4x-UltraSharp.pth" {
		t.Fatalf("result = %v, want width 8 height 8 factor 2 model 4x-UltraSharp.pth", got)
	}
	// no size request: whatever the model wrote is reported, with the measured factor
	res = run(map[string]any{"image": src, "out": filepath.Join(dir, "o4.png")})
	if !res.OK {
		t.Fatalf("want OK with no size request, got %+v", res)
	}
	got = nil
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got["width"] != 8.0 || got["factor"] != 2.0 {
		t.Fatalf("no-size result = %v, want width 8 factor 2 (measured, not assumed)", got)
	}
	// a WebP source is measured on the Go side too (header-only fixture: the stub never
	// reads it), so the scale expectation and the factor hold for the third advertised format
	webp := filepath.Join(dir, "src.webp")
	if err := os.WriteFile(webp, webpVP8LHeader(4, 4), 0o644); err != nil {
		t.Fatal(err)
	}
	res = run(map[string]any{"image": webp, "scale": 3.0, "out": filepath.Join(dir, "o6.png")})
	if res.OK || !strings.Contains(res.Reason, "upscale produced 8x8, expected 12x12 for scale 3 on a 4x4 webp source (the runner pinned that size") {
		t.Fatalf("want WebP source measured → size-mismatch defer, got %+v", res)
	}
	res = run(map[string]any{"image": webp, "scale": 2.0, "out": filepath.Join(dir, "o7.png")})
	if !res.OK {
		t.Fatalf("want OK on a WebP source at scale 2, got %+v", res)
	}
	got = nil
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got["factor"] != 2.0 {
		t.Fatalf("WebP result = %v, want factor 2", got)
	}
	// an undecodable (but non-empty) file is a defer, never a size-less success
	t.Setenv("UPSCALE_STUB_SRC", junk)
	res = run(map[string]any{"image": src, "out": filepath.Join(dir, "o5.png")})
	if res.OK || !strings.HasPrefix(res.Reason, "upscale wrote an undecodable file at ") {
		t.Fatalf("want undecodable-file defer, got %+v", res)
	}
}
