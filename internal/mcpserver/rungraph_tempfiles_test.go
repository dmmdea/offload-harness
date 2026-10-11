package mcpserver

// offload_run_graph takes graph_json and manifest_json inline, and the runner reads files, so the door writes each to a temp file
// (run-graph-*.json) and threads its path on. Nothing removed them: every inline call left one or two files in the OS temp dir for
// good. The door now removes the files it made when its call returns, however it returns. A graph ticket the call made is not left
// holding them: it keeps its own copy of the bytes (internal/mediaremote/ticket.go writeBodies, held by
// TestAGraphTicketStillRunsOnThisMachinesLaneOnceTheDoorHasRemovedItsInlineFiles), which is what makes removing them safe.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

const (
	inlineGraph    = `{"1":{"class_type":"KSampler","inputs":{"seed":1}}}`
	inlineManifest = `{"models":[]}`
)

// freshTempDir points the OS temp dir (TMP and TEMP on Windows, TMPDIR elsewhere) at a new empty folder and returns it. The folder
// is made BEFORE the variables change, so t.TempDir's own removal still finds it, and a test that needs a folder of its own makes
// that one before calling this.
func freshTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(k, dir)
	}
	return dir
}

// tempEntries names what is in dir.
func tempEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// failingWrites makes createTemp hand back a file that cannot be written (opened for reading only) from the n-th call on, so the
// write fails with the file open and on disk, as a full disk would leave it.
func failingWrites(t *testing.T, fromCall int) {
	t.Helper()
	prev := createTemp
	calls := 0
	createTemp = func(dir, pattern string) (*os.File, error) {
		calls++
		f, err := os.CreateTemp(dir, pattern)
		if err != nil || calls < fromCall {
			return f, err
		}
		name := f.Name()
		if err := f.Close(); err != nil {
			return nil, err
		}
		return os.Open(name)
	}
	t.Cleanup(func() { createTemp = prev })
}

// inlineCall is the arguments of an offload_run_graph call that carries its graph and manifest inline, plus extra members.
func inlineCall(t *testing.T, extra map[string]any) string {
	t.Helper()
	args := map[string]any{"graph_json": inlineGraph, "manifest_json": inlineManifest}
	for k, v := range extra {
		args[k] = v
	}
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func answers(context.Context, core.Request) core.Result {
	return core.Result{OK: true, Data: json.RawMessage(`{}`)}
}

// Every way the door's call can end leaves its inline files removed: an answer, a deferral, a refusal before the runner is asked, a
// graph or a manifest that cannot be written (the one made before it goes too), and a runner that panics.
func TestRunGraphDoorRemovesItsInlineTempFilesOnEveryExit(t *testing.T) {
	for _, c := range []struct {
		name      string
		extra     map[string]any
		hook      func(context.Context, core.Request) core.Result
		failFrom  int // createTemp's n-th call and later cannot be written; 0 = none
		wantRuns  int
		wantPanic bool
	}{
		{"the runner answers", nil, answers, 0, 1, false},
		{"the runner defers", nil, func(context.Context, core.Request) core.Result {
			return core.Deferf("gpu busy: the card is held", "", core.Meta{ErrClass: core.ErrClassGPUQueued})
		}, 0, 1, false},
		{"the route refuses the call before the runner is asked (a malformed min_vram_gb)", map[string]any{"min_vram_gb": "sixteen"}, answers, 0, 0, false},
		{"the manifest cannot be written", nil, answers, 2, 0, false},
		{"the graph cannot be written", nil, answers, 1, 0, false},
		{"the runner panics", nil, func(context.Context, core.Request) core.Result { panic("the runner died") }, 0, 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := freshTempDir(t)
			if c.failFrom > 0 {
				failingWrites(t, c.failFrom)
			}
			s := New(nil)
			runs := 0
			s.runHook = func(ctx context.Context, r core.Request) core.Result {
				runs++
				return c.hook(ctx, r)
			}
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				if _, err := s.handleRunGraph(context.Background(), callReq(inlineCall(t, c.extra))); err != nil {
					t.Fatal(err)
				}
			}()
			if (recovered != nil) != c.wantPanic {
				t.Fatalf("panic = %v, want a panic: %v", recovered, c.wantPanic)
			}
			if runs != c.wantRuns {
				t.Fatalf("the runner ran %d times, want %d: the exit this arm is about was not the one taken", runs, c.wantRuns)
			}
			if got := tempEntries(t, tmp); len(got) != 0 {
				t.Fatalf("the door left %v in the OS temp dir", got)
			}
		})
	}
}

// Removal is not early: while the runner runs, graph_path and manifest_path are files that hold exactly the inline JSON.
func TestRunGraphDoorHandsTheRunnerTheInlineBytesWhileItRuns(t *testing.T) {
	tmp := freshTempDir(t)
	s := New(nil)
	var graph, manifest string
	s.runHook = func(_ context.Context, r core.Request) core.Result {
		read := func(key string) string {
			path, _ := r.Params[key].(string)
			if path == "" {
				return "(no path)"
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return "(unreadable: " + err.Error() + ")"
			}
			return string(b)
		}
		graph, manifest = read("graph_path"), read("manifest_path")
		return answers(context.Background(), r)
	}
	if _, err := s.handleRunGraph(context.Background(), callReq(inlineCall(t, nil))); err != nil {
		t.Fatal(err)
	}
	if graph != inlineGraph || manifest != inlineManifest {
		t.Fatalf("the runner read graph %q and manifest %q, want the inline JSON %q and %q", graph, manifest, inlineGraph, inlineManifest)
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("the door left %v after the runner returned", got)
	}

	// A call with no manifest threads an empty manifest_path and makes no manifest file.
	graph, manifest = "", ""
	if _, err := s.handleRunGraph(context.Background(), callReq(`{"graph_json":`+strconvQuote(inlineGraph)+`}`)); err != nil {
		t.Fatal(err)
	}
	if graph != inlineGraph || manifest != "(no path)" {
		t.Fatalf("graph-only call: the runner read graph %q and manifest %q", graph, manifest)
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("the door left %v after a graph-only call", got)
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// A file the caller named is the caller's: the door never removes it, whether it stands alone, comes with a graph_json that is then
// ignored, or sits beside an inline graph that the door does remove.
func TestRunGraphDoorNeverRemovesACallersOwnFiles(t *testing.T) {
	callerDir := t.TempDir()
	graph := filepath.Join(callerDir, "g.json")
	manifest := filepath.Join(callerDir, "m.json")
	body := map[string]string{graph: inlineGraph + "\n", manifest: inlineManifest + "\n"}
	for path, b := range body {
		if err := os.WriteFile(path, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name     string
		args     string
		wantG    string // graph_path the runner is handed
		wantM    string
		inlineIn bool // the call also carries inline JSON the door must remove
	}{
		{"both files named", `{"graph_path":` + strconvQuote(graph) + `,"manifest_path":` + strconvQuote(manifest) + `}`, graph, manifest, false},
		{"a graph_path given together with a graph_json", `{"graph_path":` + strconvQuote(graph) + `,"graph_json":"{\"other\":1}","manifest_path":` + strconvQuote(manifest) + `}`, graph, manifest, false},
		{"a named manifest beside an inline graph", `{"graph_json":` + strconvQuote(inlineGraph) + `,"manifest_path":` + strconvQuote(manifest) + `}`, "", manifest, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := freshTempDir(t)
			s := New(nil)
			var gotG, gotM string
			s.runHook = func(_ context.Context, r core.Request) core.Result {
				gotG, _ = r.Params["graph_path"].(string)
				gotM, _ = r.Params["manifest_path"].(string)
				return answers(context.Background(), r)
			}
			if _, err := s.handleRunGraph(context.Background(), callReq(c.args)); err != nil {
				t.Fatal(err)
			}
			if (c.wantG != "" && gotG != c.wantG) || gotM != c.wantM {
				t.Fatalf("the runner was handed graph %q and manifest %q, want the caller's paths %q and %q", gotG, gotM, c.wantG, c.wantM)
			}
			if c.inlineIn && (gotG == "" || gotG == graph) {
				t.Fatalf("test premise: the inline graph must have become a file of its own, got %q", gotG)
			}
			if got := tempEntries(t, tmp); len(got) != 0 {
				t.Errorf("the door left %v in the OS temp dir", got)
			}
			for path, want := range body {
				if b, err := os.ReadFile(path); err != nil || string(b) != want {
					t.Errorf("the door touched the caller's %s: %q (%v)", filepath.Base(path), b, err)
				}
			}
		})
	}
}

// materialize's cleanup removes the file THIS call made and nothing else: it is never nil, does nothing for a path the caller named
// or for no text at all, and may be run twice. A write that fails removes its own file before the error comes back (the file is
// still open at that moment; Windows does not delete an open file, so the order, close and then remove, is part of what this holds).
func TestMaterializeCleanupRemovesOnlyTheFileThisCallMade(t *testing.T) {
	callerDir := t.TempDir()
	named := filepath.Join(callerDir, "named.json")
	if err := os.WriteFile(named, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := freshTempDir(t)

	path, cleanup, err := materialize(named, inlineGraph, "run-graph-*.json")
	if err != nil || path != named || cleanup == nil {
		t.Fatalf("a named path is returned as it is with a cleanup: %q %v nil=%v", path, err, cleanup == nil)
	}
	cleanup()
	if b, rerr := os.ReadFile(named); rerr != nil || string(b) != "mine" {
		t.Fatalf("the cleanup of a named path touched the caller's file: %q (%v)", b, rerr)
	}

	path, cleanup, err = materialize("", "", "run-graph-*.json")
	if err != nil || path != "" || cleanup == nil {
		t.Fatalf("no path and no text is no file, with a cleanup: %q %v nil=%v", path, err, cleanup == nil)
	}
	cleanup()
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("no file was due, found %v", got)
	}

	path, cleanup, err = materialize("", inlineGraph, "run-graph-*.json")
	if err != nil || cleanup == nil {
		t.Fatalf("inline text becomes a file: %q %v nil=%v", path, err, cleanup == nil)
	}
	if b, rerr := os.ReadFile(path); rerr != nil || string(b) != inlineGraph {
		t.Fatalf("the file holds %q (%v), want the inline text", b, rerr)
	}
	cleanup()
	cleanup()
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("the cleanup left %v", got)
	}

	failingWrites(t, 1)
	path, cleanup, err = materialize("", inlineGraph, "run-graph-*.json")
	if err == nil || path != "" || cleanup == nil {
		t.Fatalf("a failed write returns an error, no path and a cleanup: %q %v nil=%v", path, err, cleanup == nil)
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("the file of a failed write was left: %v", got)
	}
}

// heldFile is what a delete gets when a scanner, or a process that has only just exited, still holds the file (a sharing violation on
// Windows).
var heldFile = errors.New("The process cannot access the file because it is being used by another process.")

// holdRemovals makes removeFile fail with heldFile for its first n calls and then delete for real (n < 0: it never deletes), puts the
// pause between attempts at a millisecond, and returns the number of calls made so far.
func holdRemovals(t *testing.T, n int) *int {
	t.Helper()
	prevRemove, prevPause := removeFile, removePause
	calls := 0
	removeFile = func(name string) error {
		calls++
		if n < 0 || calls <= n {
			return &fs.PathError{Op: "remove", Path: name, Err: heldFile}
		}
		return os.Remove(name)
	}
	removePause = time.Millisecond
	t.Cleanup(func() { removeFile, removePause = prevRemove, prevPause })
	return &calls
}

// logged captures what the package logs while the test runs.
func logged(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// A delete that is refused for a moment is waited out, as removeRunGraphEnvelope waits out a scanner holding the envelope: the
// cleanup tries again until the file goes.
func TestMaterializeCleanupWaitsOutAFileHeldForAMoment(t *testing.T) {
	tmp := freshTempDir(t)
	calls := holdRemovals(t, 3)
	_, cleanup, err := materialize("", inlineGraph, "run-graph-*.json")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("the cleanup gave up on a file that was held for three attempts and left %v", got)
	}
	if *calls != 4 {
		t.Fatalf("removeFile ran %d times, want 4 (three refused, one removed)", *calls)
	}
}

// A file that is held for good is named once, with the reason, after a bounded number of attempts: the leak is never silent, and the
// cleanup does not wait for ever.
func TestMaterializeCleanupNamesAFileItCannotRemove(t *testing.T) {
	tmp := freshTempDir(t)
	calls := holdRemovals(t, -1)
	out := logged(t)
	path, cleanup, err := materialize("", inlineGraph, "run-graph-*.json")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if *calls != removeAttempts {
		t.Fatalf("removeFile ran %d times, want %d (the attempts, then give up)", *calls, removeAttempts)
	}
	if got := tempEntries(t, tmp); len(got) != 1 {
		t.Fatalf("test premise: the file was held, so it must still be there, got %v", got)
	}
	line := out.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, path) || !strings.Contains(line, heldFile.Error()) {
		t.Fatalf("want one log line naming %q and the reason %q, got %q", path, heldFile, line)
	}
}

// A file that is already gone is not a failure and is not waited for: a cleanup may be run twice, and the second run says nothing.
func TestMaterializeCleanupIsQuietAboutAFileThatIsAlreadyGone(t *testing.T) {
	freshTempDir(t)
	calls := holdRemovals(t, 0)
	out := logged(t)
	path, cleanup, err := materialize("", inlineGraph, "run-graph-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if *calls != 1 || out.Len() != 0 {
		t.Fatalf("a missing file: removeFile ran %d times (want 1) and logged %q (want nothing)", *calls, out)
	}
}

// The file of a write that failed is removed through the same wait, so a hold on it does not leave it behind either.
func TestMaterializeRemovesTheFileOfAFailedWriteEvenWhenItIsHeldForAMoment(t *testing.T) {
	tmp := freshTempDir(t)
	failingWrites(t, 1)
	calls := holdRemovals(t, 2)
	path, cleanup, err := materialize("", inlineGraph, "run-graph-*.json")
	if err == nil || path != "" || cleanup == nil {
		t.Fatalf("a failed write returns an error, no path and a cleanup: %q %v nil=%v", path, err, cleanup == nil)
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("the file of a failed write was left: %v", got)
	}
	if *calls != 3 {
		t.Fatalf("removeFile ran %d times, want 3 (two refused, one removed)", *calls)
	}
}

// The real thing, with no seam: a handle that is still open when the cleanup runs, and closes a moment later. Windows refuses to
// delete a file that is open, so there the cleanup must wait for the handle; elsewhere the delete simply works.
func TestMaterializeCleanupOutwaitsAHandleThatIsClosedAMomentLater(t *testing.T) {
	tmp := freshTempDir(t)
	path, cleanup, err := materialize("", inlineGraph, "run-graph-*.json")
	if err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	time.AfterFunc(60*time.Millisecond, func() { _ = held.Close(); close(closed) })
	cleanup()
	<-closed
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("the cleanup returned while the file was held and left %v", got)
	}
}
