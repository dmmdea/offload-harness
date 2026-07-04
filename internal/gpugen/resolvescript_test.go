package gpugen

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveScriptIn_RelativeResolvesAgainstExeDir: the shipped relative
// default ("render/x.mjs") resolves against the EXECUTABLE dir, not the cwd —
// an MCP host spawns the server with no meaningful cwd (LO-2).
func TestResolveScriptIn_RelativeResolvesAgainstExeDir(t *testing.T) {
	exeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(exeDir, "render"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(exeDir, "render", "comfy-video.mjs")
	if err := os.WriteFile(want, []byte("// stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveScriptIn("render/comfy-video.mjs", exeDir)
	if err != nil {
		t.Fatalf("resolveScriptIn: %v", err)
	}
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
}

// TestResolveScriptIn_MissingFileErrorsWithAbsolutePath: a missing script must
// produce the DISTINCT "script not found at <absolute-path>" reason (not a
// generic runner failure), so the operator sees exactly what path was tried.
func TestResolveScriptIn_MissingFileErrorsWithAbsolutePath(t *testing.T) {
	exeDir := t.TempDir()
	_, err := resolveScriptIn("render/nope.mjs", exeDir)
	if err == nil {
		t.Fatal("missing script must error")
	}
	want := "script not found at " + filepath.Join(exeDir, "render", "nope.mjs")
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if !filepath.IsAbs(filepath.Join(exeDir, "render", "nope.mjs")) {
		t.Fatal("reported path must be absolute")
	}
}

// TestResolveScriptIn_AbsoluteExistingKept: an absolute existing path passes
// through unchanged; an absolute missing one errors with the same distinct reason.
func TestResolveScriptIn_AbsoluteExistingKept(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "tts.mjs")
	if err := os.WriteFile(abs, []byte("// stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveScriptIn(abs, filepath.Join(dir, "elsewhere"))
	if err != nil || got != abs {
		t.Fatalf("absolute existing: got (%q, %v), want (%q, nil)", got, err, abs)
	}
	missing := filepath.Join(dir, "gone.mjs")
	if _, err := resolveScriptIn(missing, dir); err == nil || err.Error() != "script not found at "+missing {
		t.Fatalf("absolute missing: err = %v, want script not found at %s", err, missing)
	}
}

// TestResolveScriptIn_DirectoryIsNotAScript: a directory at the resolved path
// is still "not found" — node cannot execute a directory.
func TestResolveScriptIn_DirectoryIsNotAScript(t *testing.T) {
	exeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(exeDir, "render", "comfy-video.mjs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveScriptIn("render/comfy-video.mjs", exeDir); err == nil {
		t.Fatal("a directory must not resolve as a script")
	}
}
