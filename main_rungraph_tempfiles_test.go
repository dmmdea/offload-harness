package main

// `run-graph --graph-json` and `--manifest-json` hand the runner a file, so the CLI writes the inline JSON to a temp file
// (run-graph-*.json) and threads its path on. Nothing removed it: every inline call left one or two files in the OS temp dir for
// good. runGraphParams now returns the cleanup of the files THIS call made, and runRunGraph runs it however the call ends. A graph
// ticket the call made keeps its own copy of the bytes (internal/mediaremote/ticket.go writeBodies), so the files are not read
// after the call returns.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// An inline graph and manifest are files while the call runs (the runner reads them) and gone when its cleanup runs.
func TestRunGraphParamsInlineJSONIsRemovedByItsCleanup(t *testing.T) {
	tmp := freshTempDir(t)
	p, cleanup, err := runGraphParams([]string{"--graph-json", inlineGraph, "--manifest-json", inlineManifest})
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("the cleanup is never nil")
	}
	t.Cleanup(cleanup)
	graph, _ := p["graph_path"].(string)
	manifest, _ := p["manifest_path"].(string)
	for path, want := range map[string]string{graph: inlineGraph, manifest: inlineManifest} {
		if b, rerr := os.ReadFile(path); rerr != nil || string(b) != want {
			t.Fatalf("%s holds %q (%v) while the call runs, want the inline JSON %q: removal must not be early", path, b, rerr, want)
		}
	}
	if got := tempEntries(t, tmp); len(got) != 2 {
		t.Fatalf("test premise: the call made 2 temp files, found %v", got)
	}
	cleanup()
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("the cleanup left %v in the OS temp dir", got)
	}
	cleanup() // a second run finds nothing to remove and says nothing
}

// A file the caller named is the caller's: the cleanup never touches it, and a --graph given together with --graph-json is the file
// the caller named (no temp file is made for the inline text at all).
func TestRunGraphParamsLeavesACallersOwnFilesAlone(t *testing.T) {
	callerDir := t.TempDir()
	graph := filepath.Join(callerDir, "g.json")
	manifest := filepath.Join(callerDir, "m.json")
	for path, body := range map[string]string{graph: inlineGraph + "\n", manifest: inlineManifest + "\n"} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tmp := freshTempDir(t)
	p, cleanup, err := runGraphParams([]string{"--graph", graph, "--graph-json", `{"inline":"ignored"}`, "--manifest", manifest})
	if err != nil {
		t.Fatal(err)
	}
	if p["graph_path"] != graph || p["manifest_path"] != manifest {
		t.Fatalf("the caller's paths must be the ones threaded on: %v", p)
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("a temp file was made for a call that named its files: %v", got)
	}
	cleanup()
	for path, body := range map[string]string{graph: inlineGraph + "\n", manifest: inlineManifest + "\n"} {
		if b, rerr := os.ReadFile(path); rerr != nil || string(b) != body {
			t.Errorf("the cleanup touched the caller's %s: %q (%v)", filepath.Base(path), b, rerr)
		}
	}
}

// A manifest that cannot be written ends the call at once: the graph file it had already made goes with it, and the cleanup that
// comes back has nothing left to do.
func TestRunGraphParamsRemovesTheGraphFileWhenTheManifestCannotBeWritten(t *testing.T) {
	tmp := freshTempDir(t)
	failingWrites(t, 2)
	_, cleanup, err := runGraphParams([]string{"--graph-json", inlineGraph, "--manifest-json", inlineManifest})
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("want an error naming the manifest, got %v", err)
	}
	if cleanup == nil {
		t.Fatal("the cleanup is never nil, an error included")
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("a failed call left %v in the OS temp dir", got)
	}
	cleanup()
}

// A write that fails removes the file it had just made before it returns the error. The file is still open at the moment of the
// failure; Windows does not delete an open file, so the order (close, then remove) is part of what this holds.
func TestMaterializeArgRemovesItsOwnFileWhenTheWriteFails(t *testing.T) {
	tmp := freshTempDir(t)
	failingWrites(t, 1)
	path, cleanup, err := materializeArg("", inlineGraph, "run-graph-*.json")
	if err == nil {
		t.Fatal("the write was made to fail and the call said nothing")
	}
	if path != "" || cleanup == nil {
		t.Fatalf("an error returns no path and a usable cleanup, got path %q and a nil cleanup=%v", path, cleanup == nil)
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("the file of a failed write was left: %v", got)
	}
}

// run-graph exits before it runs anything when the pipeline cannot open (a media_dir that cannot be created); the temp files made for
// its inline JSON must go with it.
func TestRunGraphCLIRemovesItsTempFilesWhenThePipelineCannotOpen(t *testing.T) {
	home := t.TempDir()
	blocker := filepath.Join(home, "afile")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, "config.json")
	// A media_dir under a regular file cannot be created, which is what openPipeline's EnsureDirs refuses.
	cfgJSON := `{"endpoint":"http://127.0.0.1:1","state_dir":` + strconvQuote(filepath.ToSlash(home)) +
		`,"media_dir":` + strconvQuote(filepath.ToSlash(filepath.Join(blocker, "media"))) + `}`
	if err := os.WriteFile(cfg, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := freshTempDir(t)
	err := runRunGraph([]string{"--graph-json", inlineGraph, "--manifest-json", inlineManifest, "--config", cfg})
	if err == nil {
		t.Fatal("test premise: the pipeline must fail to open when media_dir sits under a file")
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("run-graph left %v in the OS temp dir after it failed to open the pipeline (%v)", got, err)
	}
}
