package mediaops

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The tests below run this package's real code against a FAKE engine: the test binary itself,
// re-executed (the usual helper-process trick). When MEDIAOPS_FAKE_TOOL is set the binary does not
// run tests; it behaves as ffmpeg, gimp-console or the PIL worker, decided by the argv it is given,
// and does what the mode says. Nothing here needs ffmpeg, GIMP or PIL.
//
//	ok      write the output (the last argument) and exit 0
//	full    write a few bytes, say "No space left on device" and exit 1 (what a full drive does)
//	empty   create the output with no bytes and exit 0
//	none    write nothing and exit 0
func TestMain(m *testing.M) {
	if os.Getenv("MEDIAOPS_FAKE_TOOL") != "" {
		os.Exit(runFakeTool(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// useFakeTool makes the test binary the engine for the calling test; it returns its path and the file
// that records every invocation's argv as one JSON array per line.
func useFakeTool(t *testing.T, mode string) (exe, record string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	record = filepath.Join(t.TempDir(), "calls.jsonl")
	t.Setenv("MEDIAOPS_FAKE_TOOL", mode)
	t.Setenv("MEDIAOPS_FAKE_RECORD", record)
	return exe, record
}

// calls reads the recorded argvs.
func calls(t *testing.T, record string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(record)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatalf("bad record line %q: %v", line, err)
		}
		out = append(out, argv)
	}
	return out
}

func runFakeTool(args []string) int {
	mode := os.Getenv("MEDIAOPS_FAKE_TOOL")
	if rec := os.Getenv("MEDIAOPS_FAKE_RECORD"); rec != "" {
		if f, err := os.OpenFile(rec, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			b, _ := json.Marshal(args)
			fmt.Fprintln(f, string(b))
			f.Close()
		}
	}
	for _, a := range args {
		if a == "--batch-interpreter=plug-in-script-fu-eval" {
			return fakeGimp(args)
		}
	}
	if len(args) == 1 && strings.HasSuffix(args[0], ".py") {
		return fakeWorker()
	}
	return fakeFFmpeg(args, mode)
}

// fakeFFmpeg writes to its last argument, a file or a numbered pattern like the real one.
func fakeFFmpeg(args []string, mode string) int {
	out := args[len(args)-1]
	if strings.Contains(out, "%") {
		return fakeFrames(out, mode)
	}
	switch mode {
	case "ok":
		_ = os.WriteFile(out, []byte("MEDIA-OK"), 0o644)
		return 0
	case "full":
		_ = os.WriteFile(out, []byte("ME"), 0o644)
		fmt.Fprintf(os.Stderr, "Error writing trailer of %s: No space left on device\n", out)
		return 1
	case "empty":
		_ = os.WriteFile(out, nil, 0o644)
		return 0
	}
	return 0 // none
}

func fakeFrames(pattern, mode string) int {
	frame := func(n int) string { return fmt.Sprintf(pattern, n) }
	switch mode {
	case "ok":
		for n := 1; n <= 3; n++ {
			_ = os.WriteFile(frame(n), []byte(fmt.Sprintf("FRAME-%d", n)), 0o644)
		}
		return 0
	case "full":
		_ = os.WriteFile(frame(1), []byte("FRAME-1"), 0o644)
		_ = os.WriteFile(frame(2), []byte("F"), 0o644)
		fmt.Fprintf(os.Stderr, "Error writing %s: No space left on device\n", frame(2))
		return 1
	case "empty":
		_ = os.WriteFile(frame(1), nil, 0o644)
		return 0
	}
	return 0 // none
}

var (
	loadRe   = regexp.MustCompile(`\(load "([^"]+)"\)`)
	saveRe   = regexp.MustCompile(`gimp-file-save RUN-NONINTERACTIVE image "([^"]+)"`)
	layersRe = regexp.MustCompile(`open-output-file "([^"]+)"`)
)

// fakeGimp does what the batch script does: write the raster at the save target and the layer sidecar.
func fakeGimp(args []string) int {
	for _, a := range args {
		m := loadRe.FindStringSubmatch(a)
		if m == nil {
			continue
		}
		script, err := os.ReadFile(m[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake gimp: cannot read the script:", err)
			return 1
		}
		if d := saveRe.FindSubmatch(script); d != nil {
			_ = os.WriteFile(string(d[1]), []byte("GIMP-RASTER"), 0o644)
		}
		if l := layersRe.FindSubmatch(script); l != nil {
			_ = os.WriteFile(string(l[1]), []byte("LAYER:Background|visible\n"), 0o644)
		}
		return 0
	}
	return 1
}

// fakeWorker is render/edit_image.py's contract: a JSON request on stdin, the image at "out", a JSON line on stdout.
func fakeWorker() int {
	raw, _ := io.ReadAll(os.Stdin)
	var req struct {
		Image string          `json:"image"`
		Out   string          `json:"out"`
		Ops   json.RawMessage `json:"ops"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		fmt.Fprintln(os.Stderr, "fake worker: bad request:", err)
		return 2
	}
	if rec := os.Getenv("MEDIAOPS_FAKE_RECORD"); rec != "" {
		_ = os.WriteFile(rec+".worker", raw, 0o644)
	}
	if err := os.WriteFile(req.Out, []byte("WORKER-OUT"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fake worker:", err)
		return 3
	}
	fmt.Printf(`{"out":%q,"width":1,"height":1,"ops_applied":1}`+"\n", req.Out)
	return 0
}

// the seams of deliver.go, swapped for one test
func withRename(t *testing.T, fn func(from, to string) error) {
	t.Helper()
	old := renameFile
	renameFile = fn
	t.Cleanup(func() { renameFile = old })
}

func withRetryPause(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	old := retryPause
	retryPause = func(d time.Duration) { waits = append(waits, d) }
	t.Cleanup(func() { retryPause = old })
	return &waits
}
