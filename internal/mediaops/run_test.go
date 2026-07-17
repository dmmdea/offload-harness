package mediaops

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestToSeconds(t *testing.T) {
	cases := map[string]float64{"3": 3, "2.5": 2.5, "01:30": 90, "00:01:30.5": 90.5}
	for in, want := range cases {
		got, err := ToSeconds(in)
		if err != nil || got != want {
			t.Fatalf("ToSeconds(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "a", "1:2:3:4", "-5"} {
		if _, err := ToSeconds(bad); err == nil {
			t.Fatalf("ToSeconds(%q) must error", bad)
		}
	}
}

func TestResolveEditPython(t *testing.T) {
	// explicit-but-missing = absent (engine gating is existence-checked)
	if got := ResolveEditPython(`C:\nope\python.exe`, ""); got != "" {
		t.Fatalf("missing explicit python must resolve to absent, got %q", got)
	}
	// derived from a synthetic comfy dir
	dir := t.TempDir()
	venv := filepath.Join(dir, ".venv", "Scripts")
	if runtime.GOOS != "windows" {
		venv = filepath.Join(dir, ".venv", "bin")
	}
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "python.exe"
	if runtime.GOOS != "windows" {
		name = "python"
	}
	p := filepath.Join(venv, name)
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveEditPython("", dir); got != p {
		t.Fatalf("derived python = %q, want %q", got, p)
	}
	if got := ResolveEditPython("", filepath.Join(dir, "missing")); got != "" {
		t.Fatalf("no venv must resolve to absent, got %q", got)
	}
}

func TestRunEditImage_DeferClassErrors(t *testing.T) {
	ctx := context.Background()
	img := filepath.Join(t.TempDir(), "in.png")
	if err := os.WriteFile(img, []byte("not really a png but exists"), 0o644); err != nil {
		t.Fatal(err)
	}
	// PIL engine absent
	_, err := RunEditImage(ctx, EditConfig{Python: "", Worker: "w.py", Timeout: time.Second},
		EditRequest{Image: img, Out: "o.png", Ops: []EditOp{{Op: "resize", Width: 4}}})
	if !errors.Is(err, ErrEngineAbsent) {
		t.Fatalf("absent PIL must be ErrEngineAbsent, got %v", err)
	}
	// GIMP op with gimp absent
	_, err = RunEditImage(ctx, EditConfig{Python: "x", GimpConsole: "", Worker: "w.py", Timeout: time.Second},
		EditRequest{Image: filepath.Join(t.TempDir(), "d.xcf"), Out: "o.png", Ops: []EditOp{{Op: "flatten_design"}}})
	if err == nil {
		t.Fatal("flatten_design without input file must error")
	}
	// with the input existing:
	xcf := filepath.Join(t.TempDir(), "d.xcf")
	_ = os.WriteFile(xcf, []byte("x"), 0o644)
	_, err = RunEditImage(ctx, EditConfig{Python: "x", GimpConsole: "", Worker: "w.py", Timeout: time.Second},
		EditRequest{Image: xcf, Out: "o.png", Ops: []EditOp{{Op: "flatten_design"}}})
	if !errors.Is(err, ErrEngineAbsent) {
		t.Fatalf("absent GIMP must be ErrEngineAbsent, got %v", err)
	}
}

func TestRunMedia_EngineAbsent(t *testing.T) {
	_, err := RunMedia(context.Background(), MediaConfig{FFmpeg: ""}, MediaRequest{Op: "probe", In: "x.mp4"})
	if !errors.Is(err, ErrEngineAbsent) {
		t.Fatalf("unset ffmpeg must be ErrEngineAbsent, got %v", err)
	}
	_, err = RunMedia(context.Background(), MediaConfig{FFmpeg: `C:\nope\ffmpeg.exe`}, MediaRequest{Op: "probe", In: "x.mp4"})
	if !errors.Is(err, ErrEngineAbsent) {
		t.Fatalf("missing ffmpeg must be ErrEngineAbsent, got %v", err)
	}
}

// TestWorkerSelftest shells render/edit_image.py --selftest on the resolved edit
// python. Skips cleanly on boxes without a PIL python (CI parity with the render
// tests' hardware gating).
func TestWorkerSelftest(t *testing.T) {
	py := ResolveEditPython("", defaultComfyDirForTest())
	if py == "" {
		t.Skip("no PIL python resolvable on this box")
	}
	worker := filepath.Join("..", "..", "render", "edit_image.py")
	out, err := exec.Command(py, worker, "--selftest").CombinedOutput()
	if err != nil {
		t.Fatalf("worker selftest failed: %v\n%s", err, out)
	}
	if want := "SELFTEST PASS"; !containsStr(string(out), want) {
		t.Fatalf("selftest output missing %q: %s", want, out)
	}
}

// TestRunEditImage_Renditions runs the REAL worker (skips without a PIL python):
// one master + two renditions from a Go-generated PNG, checking the export loop
// writes every <stem><suffix>.<ext> and reports them in the result.
func TestRunEditImage_Renditions(t *testing.T) {
	py := ResolveEditPython("", defaultComfyDirForTest())
	if py == "" {
		t.Skip("no PIL python resolvable on this box")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "in.png")
	writeTestPNG(t, src, 64, 48)
	out := filepath.Join(dir, "master.png")
	worker, _ := filepath.Abs(filepath.Join("..", "..", "render", "edit_image.py"))
	res, err := RunEditImage(context.Background(),
		EditConfig{Python: py, Worker: worker, Timeout: 60 * time.Second},
		EditRequest{Image: src, Out: out, Ops: []EditOp{{Op: "resize", Width: 32}},
			Renditions: []Rendition{
				{Width: 16, Format: "jpg", Suffix: "-web"},
				{Width: 8, Format: "png", Suffix: "-ig"},
			}})
	if err != nil {
		t.Fatalf("RunEditImage with renditions failed: %v", err)
	}
	want := []string{filepath.Join(dir, "master-web.jpg"), filepath.Join(dir, "master-ig.png")}
	if len(res.Renditions) != 2 || res.Renditions[0] != want[0] || res.Renditions[1] != want[1] {
		t.Fatalf("renditions = %v, want %v", res.Renditions, want)
	}
	for _, p := range append(want, out) {
		if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
			t.Fatalf("missing/empty output %s: %v", p, err)
		}
	}
	// a bad rendition set fails fast, before any work
	_, err = RunEditImage(context.Background(),
		EditConfig{Python: py, Worker: worker, Timeout: 60 * time.Second},
		EditRequest{Image: src, Out: out, Ops: []EditOp{{Op: "resize", Width: 32}},
			Renditions: []Rendition{{Format: "png", Suffix: "-a"}}})
	if err == nil {
		t.Fatal("dimensionless rendition must fail validation")
	}
}

func writeTestPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 5), 96, 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func defaultComfyDirForTest() string {
	if v := os.Getenv("COMFY_DIR"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		return `C:\ComfyUI`
	}
	return ""
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
