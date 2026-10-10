package mediaops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A media op delivers its output through a staged sibling (deliver.go). The engine is the fake tool of
// faketool_test.go, so what is asserted is what ffmpeg is really told to write and what a caller finds on
// disk afterwards: the 2026-10-09 failure class, a full drive leaving a zero-byte or half-written file at
// the output path of an op that failed.

const previousClip = "PREVIOUS-GOOD-CLIP"

func mediaFixture(t *testing.T, mode string) (cfg MediaConfig, dir, record string) {
	t.Helper()
	exe, record := useFakeTool(t, mode)
	dir = t.TempDir()
	for _, n := range []string{"in.mp4", "a.mp4", "b.mp4", "voice.wav"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("INPUT-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return MediaConfig{FFmpeg: exe, Timeout: 60 * time.Second}, dir, record
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func noPartials(t *testing.T, dir string) {
	t.Helper()
	for _, n := range names(t, dir) {
		if strings.Contains(n, ".partial-") {
			t.Errorf("a staged file was left behind: %s (directory: %v)", n, names(t, dir))
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// the single-file ops, each built so that its output argument is the last one ffmpeg gets
func singleFileOps(dir, out string) map[string]MediaRequest {
	in := func(n string) string { return filepath.Join(dir, n) }
	return map[string]MediaRequest{
		"trim":      {Op: "trim", In: in("in.mp4"), Out: out, Start: "1", Duration: "2"},
		"concat":    {Op: "concat", Inputs: []string{in("a.mp4"), in("b.mp4")}, Out: out},
		"convert":   {Op: "convert", In: in("in.mp4"), Out: out},
		"mux_audio": {Op: "mux_audio", In: in("in.mp4"), Audio: in("voice.wav"), Out: out, Shortest: true},
	}
}

func TestRunMediaPointsFFmpegAtAStagedSiblingAndDeliversItOverTheOutput(t *testing.T) {
	cfg, dir, record := mediaFixture(t, "ok")
	out := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(out, []byte(previousClip), 0o644); err != nil {
		t.Fatal(err)
	}
	for op, req := range singleFileOps(dir, out) {
		before := len(calls(t, record))
		res, err := RunMedia(context.Background(), cfg, req)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if res.MediaPath != out {
			t.Errorf("%s: MediaPath = %q, want %q", op, res.MediaPath, out)
		}
		if got := read(t, out); got != "MEDIA-OK" {
			t.Errorf("%s: the output holds %q, want the engine's result", op, got)
		}
		argv := calls(t, record)[before]
		target := argv[len(argv)-1]
		if target == out {
			t.Errorf("%s: ffmpeg was pointed at the output path itself", op)
		}
		if filepath.Dir(target) != dir || !regexp.MustCompile(`^\.clip\.partial-\d+-\d+\.mp4$`).MatchString(filepath.Base(target)) {
			t.Errorf("%s: ffmpeg's output %q should be a hidden sibling of the output with the extension last", op, target)
		}
		noPartials(t, dir)
		if err := os.WriteFile(out, []byte(previousClip), 0o644); err != nil { // the next op starts from a previous clip again
			t.Fatal(err)
		}
	}
}

// 2026-10-09: the drive filled up in the middle of a write. The op fails, and what was at the output
// path before is exactly what is there after: not truncated, not replaced by the bytes that landed.
func TestRunMediaAFullDiskLeavesTheOutputAsItWas(t *testing.T) {
	cfg, dir, _ := mediaFixture(t, "full")
	out := filepath.Join(dir, "clip.mp4")
	for op, req := range singleFileOps(dir, out) {
		// a previous good clip survives
		if err := os.WriteFile(out, []byte(previousClip), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := RunMedia(context.Background(), cfg, req)
		if err == nil {
			t.Fatalf("%s: a run that hit a full disk must fail", op)
		}
		if !strings.Contains(err.Error(), "No space left on device") {
			t.Errorf("%s: the engine's reason is lost: %v", op, err)
		}
		if strings.Contains(err.Error(), ".partial-") || !strings.Contains(err.Error(), "clip.mp4") {
			t.Errorf("%s: the error should name the output, not the staged file: %v", op, err)
		}
		if got := read(t, out); got != previousClip {
			t.Errorf("%s: the previous clip was damaged: %q", op, got)
		}
		noPartials(t, dir)

		// and with nothing there before, nothing is there after: a half-written file looks finished
		if err := os.Remove(out); err != nil {
			t.Fatal(err)
		}
		if _, err := RunMedia(context.Background(), cfg, req); err == nil {
			t.Fatalf("%s: second run must fail too", op)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Errorf("%s: a failed op left a file at the output path (%v)", op, err)
		}
		noPartials(t, dir)
	}
}

func TestRunMediaAnEmptyOrMissingResultIsRefused(t *testing.T) {
	for _, mode := range []string{"empty", "none"} {
		cfg, dir, _ := mediaFixture(t, mode)
		out := filepath.Join(dir, "clip.mp4")
		if err := os.WriteFile(out, []byte(previousClip), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := RunMedia(context.Background(), cfg, MediaRequest{Op: "convert", In: filepath.Join(dir, "in.mp4"), Out: out})
		if err == nil || !strings.Contains(err.Error(), "produced no output at "+out) {
			t.Errorf("%s: want 'convert produced no output at %s', got %v", mode, out, err)
		}
		if got := read(t, out); got != previousClip {
			t.Errorf("%s: the previous clip was replaced by an empty result: %q", mode, got)
		}
		noPartials(t, dir)
	}
}

// ffmpeg refuses to write over its own input; with the engine pointed at a staged file that check can
// no longer fire, so the refusal is explicit and the source is never replaced.
func TestRunMediaRefusesAnOutputThatIsAlsoAnInput(t *testing.T) {
	cfg, dir, record := mediaFixture(t, "ok")
	in := filepath.Join(dir, "in.mp4")
	respelled := filepath.Join(dir, ".", "in.mp4")
	for name, req := range map[string]MediaRequest{
		"trim":      {Op: "trim", In: in, Out: respelled, Start: "1", Duration: "2"},
		"convert":   {Op: "convert", In: in, Out: in},
		"concat":    {Op: "concat", Inputs: []string{in, filepath.Join(dir, "a.mp4")}, Out: in},
		"mux_audio": {Op: "mux_audio", In: filepath.Join(dir, "a.mp4"), Audio: in, Out: in, Shortest: true},
	} {
		_, err := RunMedia(context.Background(), cfg, req)
		if err == nil || !strings.Contains(err.Error(), "is also an input") {
			t.Errorf("%s: want a refusal, got %v", name, err)
		}
		if got := read(t, in); got != "INPUT-in.mp4" {
			t.Errorf("%s: the input was replaced: %q", name, got)
		}
	}
	if n := len(calls(t, record)); n != 0 {
		t.Errorf("the engine must not even be started for a refused output, got %d calls", n)
	}
}

func TestDeliverFileRetriesATransientRenameThenDelivers(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "clip.mp4")
	waits := withRetryPause(t)
	real := os.Rename
	attempts := 0
	withRename(t, func(from, to string) error {
		attempts++
		if attempts <= 2 {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: fs.ErrPermission}
		}
		return real(from, to)
	})
	err := deliverFile(out, func(staged string) error { return os.WriteFile(staged, []byte("RESULT"), 0o644) })
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || len(*waits) != 2 || (*waits)[0] != 50*time.Millisecond || (*waits)[1] != 100*time.Millisecond {
		t.Errorf("attempts = %d, waits = %v; want 3 attempts after 50ms and 100ms", attempts, *waits)
	}
	if got := read(t, out); got != "RESULT" {
		t.Errorf("delivered %q", got)
	}
	noPartials(t, dir)
}

func TestDeliverFileGivesUpAfterTheBackoffAndKeepsThePreviousFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(out, []byte(previousClip), 0o644); err != nil {
		t.Fatal(err)
	}
	waits := withRetryPause(t)
	attempts := 0
	withRename(t, func(from, to string) error {
		attempts++
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EBUSY}
	})
	err := deliverFile(out, func(staged string) error { return os.WriteFile(staged, []byte("RESULT"), 0o644) })
	if err == nil || !errors.Is(err, syscall.EBUSY) || !strings.Contains(err.Error(), out) {
		t.Fatalf("want a delivery error that names the output and wraps EBUSY, got %v", err)
	}
	if attempts != 5 || len(*waits) != 4 {
		t.Errorf("attempts = %d, waits = %v; want one try and four retries", attempts, *waits)
	}
	if got := read(t, out); got != previousClip {
		t.Errorf("the previous clip was damaged: %q", got)
	}
	noPartials(t, dir)

	// a failure that is not transient is not retried
	attempts = 0
	withRename(t, func(from, to string) error {
		attempts++
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
	})
	if err := deliverFile(out, func(staged string) error { return os.WriteFile(staged, []byte("RESULT"), 0o644) }); err == nil || attempts != 1 {
		t.Errorf("EXDEV: err = %v after %d attempts, want an error after exactly one", err, attempts)
	}
	noPartials(t, dir)
}

func TestStagedSiblingKeepsTheExtensionLastAndStaysBesideTheOutput(t *testing.T) {
	out := filepath.Join("some", "dir", "clip.final.mp4")
	a, b := stagedSibling(out), stagedSibling(out)
	if a == b {
		t.Error("two calls must stage under different names")
	}
	if filepath.Dir(a) != filepath.Join("some", "dir") || !regexp.MustCompile(`^\.clip\.final\.partial-\d+-\d+\.mp4$`).MatchString(filepath.Base(a)) {
		t.Errorf("staged name %q", a)
	}
	if got := stagedSibling("bare"); !regexp.MustCompile(`^\.bare\.partial-\d+-\d+$`).MatchString(got) {
		t.Errorf("an output with no extension: %q", got)
	}
}

// ---- extract_frames ---------------------------------------------------------------------------------

func writeFrames(t *testing.T, dir string, contents map[string]string) {
	t.Helper()
	for n, c := range contents {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtractFramesLandInTheDestinationOnlyWhenTheRunSucceeded(t *testing.T) {
	cfg, dir, record := mediaFixture(t, "ok")
	dest := filepath.Join(dir, "shots")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFrames(t, dest, map[string]string{"frame_00001.png": "OLD-1", "notes.txt": "keep me"})
	res, err := RunMedia(context.Background(), cfg, MediaRequest{Op: "extract_frames", In: filepath.Join(dir, "in.mp4"), Out: dest, FPS: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 3 || len(res.Frames) != 3 {
		t.Errorf("frames = %v (count %d), want 3", res.Frames, res.Count)
	}
	if got := read(t, filepath.Join(dest, "frame_00001.png")); got != "FRAME-1" {
		t.Errorf("frame 1 should be the new one, got %q", got)
	}
	if got := read(t, filepath.Join(dest, "notes.txt")); got != "keep me" {
		t.Errorf("an unrelated file in the destination was touched: %q", got)
	}
	noPartials(t, dest)
	argv := calls(t, record)[0]
	pattern := argv[len(argv)-1]
	if filepath.Dir(filepath.Dir(pattern)) != dest || !strings.HasPrefix(filepath.Base(filepath.Dir(pattern)), ".frame.partial-") {
		t.Errorf("ffmpeg should write into a staging directory inside the destination, got %q", pattern)
	}
}

func TestExtractFramesAFailedRunLeavesTheDestinationAsItWas(t *testing.T) {
	cfg, dir, _ := mediaFixture(t, "full")
	dest := filepath.Join(dir, "shots")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFrames(t, dest, map[string]string{"frame_00001.png": "OLD-1", "frame_00002.png": "OLD-2"})
	_, err := RunMedia(context.Background(), cfg, MediaRequest{Op: "extract_frames", In: filepath.Join(dir, "in.mp4"), Out: dest, FPS: 1})
	if err == nil || !strings.Contains(err.Error(), "No space left on device") {
		t.Fatalf("a full disk must fail the op with the engine's reason, got %v", err)
	}
	if strings.Contains(err.Error(), ".partial-") {
		t.Errorf("the error names the staging directory: %v", err)
	}
	if got := read(t, filepath.Join(dest, "frame_00001.png")); got != "OLD-1" {
		t.Errorf("frame 1 was replaced by a frame of the failed run: %q", got)
	}
	if got := read(t, filepath.Join(dest, "frame_00002.png")); got != "OLD-2" {
		t.Errorf("frame 2 was damaged: %q", got)
	}
	if _, serr := os.Stat(filepath.Join(dest, "frame_00003.png")); !os.IsNotExist(serr) {
		t.Error("a frame of the failed run is in the destination")
	}
	noPartials(t, dest)
}

func TestExtractFramesRefusesAnEmptyFrameOrNoFrames(t *testing.T) {
	for _, mode := range []string{"empty", "none"} {
		cfg, dir, _ := mediaFixture(t, mode)
		dest := filepath.Join(dir, "shots")
		_, err := RunMedia(context.Background(), cfg, MediaRequest{Op: "extract_frames", In: filepath.Join(dir, "in.mp4"), Out: dest, FPS: 1})
		if err == nil || !strings.Contains(err.Error(), "extract_frames produced no frames") {
			t.Errorf("%s: want 'produced no frames', got %v", mode, err)
		}
		if ents := names(t, dest); len(ents) != 0 {
			t.Errorf("%s: nothing may be left in the destination, got %v", mode, ents)
		}
	}
}

func TestExtractFramesToAPatternUsesItsDirectory(t *testing.T) {
	cfg, dir, _ := mediaFixture(t, "ok")
	dest := filepath.Join(dir, "stills")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := RunMedia(context.Background(), cfg, MediaRequest{Op: "extract_frames", In: filepath.Join(dir, "in.mp4"), Out: filepath.Join(dest, "s_%04d.jpg"), FPS: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(names(t, dest)); got != "[s_0001.jpg s_0002.jpg s_0003.jpg]" {
		t.Errorf("destination holds %s (frames %v)", got, res.Frames)
	}
}

func TestDeliverFramesReportsHowManyLandedWhenARenameFails(t *testing.T) {
	dest := t.TempDir()
	real := os.Rename
	n := 0
	withRename(t, func(from, to string) error {
		n++
		if n == 2 {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
		}
		return real(from, to)
	})
	err := deliverFrames(filepath.Join(dest, "f_%02d.png"), func(pattern string) error {
		for i := 1; i <= 3; i++ {
			if err := os.WriteFile(fmt.Sprintf(pattern, i), []byte("x"), 0o644); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "1 of 3 landed") {
		t.Fatalf("want an error saying 1 of 3 frames landed, got %v", err)
	}
	noPartials(t, dest)
}

// ---- GIMP -------------------------------------------------------------------------------------------

// GIMP never writes the destination: it exports to a private temp raster that the PIL worker turns into
// the output, and the worker (render/edit_image.py) is what delivers it atomically. The fake pair makes
// that visible: the save target in the GIMP script and the "out" of the worker's request are different
// files in different directories, and the temp raster is gone afterwards.
func TestEditImageGIMPExportsToAPrivateRasterNeverTheDestination(t *testing.T) {
	exe, record := useFakeTool(t, "ok")
	dir := t.TempDir()
	xcf := filepath.Join(dir, "design.xcf")
	if err := os.WriteFile(xcf, []byte("XCF"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "design.png")
	if err := os.WriteFile(out, []byte(previousClip), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RunEditImage(context.Background(), EditConfig{Python: exe, GimpConsole: exe, Worker: "edit_image.py", Timeout: 60 * time.Second},
		EditRequest{Image: xcf, Ops: []EditOp{{Op: "flatten_design"}}, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if res.Engine != "gimp+pil" || len(res.Layers) != 1 {
		t.Errorf("result %+v: the GIMP path did not run", res)
	}
	if got := read(t, out); got != "WORKER-OUT" {
		t.Errorf("the destination should hold the worker's result, got %q", got)
	}
	var req struct {
		Image string `json:"image"`
		Out   string `json:"out"`
	}
	if err := json.Unmarshal([]byte(read(t, record+".worker")), &req); err != nil {
		t.Fatal(err)
	}
	if req.Out != out {
		t.Errorf("the worker was asked for %q, want the destination %q", req.Out, out)
	}
	if req.Image == out || filepath.Dir(req.Image) == dir || filepath.Base(req.Image) != "flat.png" {
		t.Errorf("GIMP must export to a private temp raster, not beside the destination; the worker's input is %q", req.Image)
	}
	if _, serr := os.Stat(req.Image); !os.IsNotExist(serr) {
		t.Errorf("the private raster %q was not cleaned up", req.Image)
	}
	for _, argv := range calls(t, record) {
		for _, a := range argv {
			if a == out {
				t.Errorf("an engine was handed the destination %q directly: %v", out, argv)
			}
		}
	}
}
