package imagegen

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUpscaleArgs(t *testing.T) {
	m := UpscaleModel{Model: "4x-UltraSharp.pth"}
	// binding only → positional out/image + the bound model, nothing else
	got := upscaleArgs("o.png", "i.png", nil, m)
	if want := []string{"o.png", "i.png", "--model", "4x-UltraSharp.pth"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("binding-only args = %v, want %v", got, want)
	}
	// request model overrides the binding; scale/method flow; width+height only as a pair
	got = upscaleArgs("o.png", "i.png", map[string]any{
		"model": "RealESRGAN_x4plus.pth", "scale": 2.0, "method": "bicubic", "width": 3000.0, "height": 2000.0,
	}, m)
	want := []string{"o.png", "i.png", "--model", "RealESRGAN_x4plus.pth", "--scale", "2", "--width", "3000", "--height", "2000", "--method", "bicubic"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("full args = %v, want %v", got, want)
	}
	// width/height arrive as int from the MCP handler and the CLI (the float64 case above
	// is the JSON-decoded shape) — both must forward
	got = upscaleArgs("o.png", "i.png", map[string]any{"width": 3000, "height": 2000}, m)
	want = []string{"o.png", "i.png", "--model", "4x-UltraSharp.pth", "--width", "3000", "--height", "2000"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("int-typed size args = %v, want %v", got, want)
	}
	// a half-given size is NOT forwarded (the pipeline defers on it; the runner would exit 2)
	got = upscaleArgs("o.png", "i.png", map[string]any{"width": 3000.0}, m)
	for _, a := range got {
		if a == "--width" || a == "--height" {
			t.Fatalf("half-given size must not be forwarded: %v", got)
		}
	}
	// non-positive scale and empty model override are ignored
	got = upscaleArgs("o.png", "i.png", map[string]any{"scale": 0.0, "model": ""}, m)
	if want := []string{"o.png", "i.png", "--model", "4x-UltraSharp.pth"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ignored-params args = %v, want %v", got, want)
	}
}

func TestOutputSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.png")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 12, 7))
	img.Set(0, 0, color.White)
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if w, h := OutputSize(p); w != 12 || h != 7 {
		t.Fatalf("OutputSize = %dx%d, want 12x7", w, h)
	}
	if w, h := OutputSize(filepath.Join(dir, "missing.png")); w != 0 || h != 0 {
		t.Fatalf("missing file must report 0x0, got %dx%d", w, h)
	}
	junk := filepath.Join(dir, "junk.png")
	if err := os.WriteFile(junk, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w, h := OutputSize(junk); w != 0 || h != 0 {
		t.Fatalf("undecodable file must report 0x0, got %dx%d", w, h)
	}
	// WebP: the three container payloads the runner's image-size.mjs reads, header-only
	// fixtures (OutputSize never decodes pixels). Sizes chosen so every encoding's
	// "minus one" / 14-bit / 24-bit rule is exercised.
	for name, fx := range map[string]struct {
		bytes []byte
		w, h  int
	}{
		"VP8X 1024x768": {WebPHeader("VP8X", 1024, 768), 1024, 768},
		"VP8 640x480":   {WebPHeader("VP8 ", 640, 480), 640, 480},
		"VP8L 300x200":  {WebPHeader("VP8L", 300, 200), 300, 200},
	} {
		p := filepath.Join(dir, name+".webp")
		if err := os.WriteFile(p, fx.bytes, 0o644); err != nil {
			t.Fatal(err)
		}
		if w, h := OutputSize(p); w != fx.w || h != fx.h {
			t.Fatalf("%s: OutputSize = %dx%d, want %dx%d", name, w, h, fx.w, fx.h)
		}
	}
	// a RIFF that is not WEBP, and a VP8 payload without its sync code, report 0x0
	bad := WebPHeader("VP8 ", 640, 480)
	bad[24] = 0
	for name, b := range map[string][]byte{"no sync": bad, "not webp": append([]byte("RIFF\x00\x00\x00\x00WAVE"), make([]byte, 20)...)} {
		p := filepath.Join(dir, name+".webp")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if w, h := OutputSize(p); w != 0 || h != 0 {
			t.Fatalf("%s must report 0x0, got %dx%d", name, w, h)
		}
	}
}

func TestSourceFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.png")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 3, 3))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := SourceFormat(p); got != "png" {
		t.Fatalf("png: SourceFormat = %q", got)
	}
	w := filepath.Join(dir, "a.webp")
	if err := os.WriteFile(w, WebPHeader("VP8L", 5, 7), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SourceFormat(w); got != "webp" {
		t.Fatalf("webp: SourceFormat = %q", got)
	}
	j := filepath.Join(dir, "junk.bin")
	if err := os.WriteFile(j, []byte("definitely not an image header at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SourceFormat(j); got != "" {
		t.Fatalf("junk: SourceFormat = %q, want empty", got)
	}
	if got := SourceFormat(filepath.Join(dir, "missing.png")); got != "" {
		t.Fatalf("missing: SourceFormat = %q, want empty", got)
	}
}

// TestOutputSizeRealWebP reads REAL encoder output (opt-in: UPSCALE_REAL_WEBP_DIR
// holds files named like real-37x23-lossy.webp) so the header reader and the
// header-only fixtures above cannot share one mistake. Skipped when unset.
func TestOutputSizeRealWebP(t *testing.T) {
	dir := os.Getenv("UPSCALE_REAL_WEBP_DIR")
	if dir == "" {
		t.Skip("UPSCALE_REAL_WEBP_DIR unset")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		var w, h int
		var kind string
		if _, err := fmt.Sscanf(e.Name(), "real-%dx%d-%s", &w, &h, &kind); err != nil {
			continue
		}
		gw, gh := OutputSize(filepath.Join(dir, e.Name()))
		if gw != w || gh != h {
			t.Errorf("%s: OutputSize = %dx%d, want %dx%d", e.Name(), gw, gh, w, h)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("no real-WxH-*.webp files in %s", dir)
	}
	t.Logf("%d real WebP files measured correctly", n)
}

// WebPHeader builds a 30-byte RIFF/WEBP header carrying the given size in the named
// payload's encoding (test fixture).
func WebPHeader(kind string, w, h int) []byte {
	b := make([]byte, 30)
	copy(b[0:4], "RIFF")
	copy(b[8:12], "WEBP")
	copy(b[12:16], kind)
	switch kind {
	case "VP8X":
		for i, v := range []int{w - 1, h - 1} {
			off := 24 + 3*i
			b[off], b[off+1], b[off+2] = byte(v), byte(v>>8), byte(v>>16)
		}
	case "VP8 ":
		b[23], b[24], b[25] = 0x9d, 0x01, 0x2a
		b[26], b[27] = byte(w), byte(w>>8)
		b[28], b[29] = byte(h), byte(h>>8)
	case "VP8L":
		b[20] = 0x2f
		bits := uint32(w-1) | uint32(h-1)<<14
		b[21], b[22], b[23], b[24] = byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24)
	}
	return b
}
