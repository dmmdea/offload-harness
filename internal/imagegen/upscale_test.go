package imagegen

import (
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
}
