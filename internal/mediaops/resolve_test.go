package mediaops

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestResolveBinary_Empty: an unset binding never resolves.
func TestResolveBinary_Empty(t *testing.T) {
	if p, ok := ResolveBinary(""); ok || p != "" {
		t.Fatalf("ResolveBinary(\"\") = (%q, %v), want (\"\", false)", p, ok)
	}
}

// TestResolveBinary_ExplicitPath: a real file at a path with a separator is
// stat'd directly, no PATH search needed.
func TestResolveBinary_ExplicitPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "custom-ffmpeg")
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := ResolveBinary(p)
	if !ok || got != p {
		t.Fatalf("ResolveBinary(%q) = (%q, %v), want (%q, true)", p, got, ok, p)
	}
}

// TestResolveBinary_ExplicitPathMissing: an explicit path that does not exist
// never falls back to a PATH search (F-38 test #95's guarantee, unchanged).
func TestResolveBinary_ExplicitPathMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "ffmpeg.exe")
	if p, ok := ResolveBinary(missing); ok {
		t.Fatalf("ResolveBinary(%q) = (%q, true), want not found", missing, p)
	}
}

// TestResolveBinary_BareNameOnPath is the regression test for F-38: a bare
// command name (the shipped "ffmpeg" config default) that is NOT a file in the
// current directory but IS on PATH must resolve, matching what exec.Command
// would actually run — a bare os.Stat check alone (the pre-fix behavior in both
// internal/mediaops.RunMedia and mediacap.binaryPresent's precursor) always
// missed this case.
func TestResolveBinary_BareNameOnPath(t *testing.T) {
	dir := t.TempDir()
	name := "fake-ffmpeg-f38"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	stub := filepath.Join(dir, name)
	if err := os.WriteFile(stub, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	bare := "fake-ffmpeg-f38"
	got, ok := ResolveBinary(bare)
	if !ok {
		t.Fatalf("ResolveBinary(%q) with %q on PATH must resolve, got not-found", bare, dir)
	}
	if abs, err := filepath.Abs(got); err != nil || abs != stub {
		t.Fatalf("ResolveBinary(%q) = %q, want the PATH-resolved stub %q", bare, got, stub)
	}
}

// TestResolveBinary_BareNameNotOnPath: a bare name that resolves nowhere stays
// not-found — the whole point of F-38's later "fail loudly" fix depends on this
// staying false rather than silently guessing.
func TestResolveBinary_BareNameNotOnPath(t *testing.T) {
	if p, ok := ResolveBinary("definitely-not-a-real-binary-f38-xyz"); ok {
		t.Fatalf("ResolveBinary(unknown bare name) = (%q, true), want not found", p)
	}
}
