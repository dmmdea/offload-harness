package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A windows-wsl seat's venv and HF cache are DISTRO paths ("/opt/seat/vllm-env", "/hf"), so
// the prerequisite check must read them through Runtime.HostPrefix (normally
// \\wsl.localhost\<distro>). Two defects hid behind the field being set by no caller:
// Resolve listed the snapshots directory without mapping it, and hostPath tested for a leading
// "/" on a path that filepath.Join had already turned into backslashes on Windows, so the
// prefix was never applied there at all. Both made a correct --host-prefix report "build the
// venv" about a venv that was already there.

// fakeDistro lays down a distro filesystem under a temp dir: the venv's entry point and one
// HF snapshot of the seat's model, at the paths the ENGINE sees them at.
func fakeDistro(t *testing.T, s Spec) (prefix string, r Runtime) {
	t.Helper()
	prefix = t.TempDir()
	r = wslRT()
	r.HostPrefix = prefix
	vllm := filepath.Join(prefix, filepath.FromSlash(r.VenvDir), "bin", "vllm")
	if err := os.MkdirAll(filepath.Dir(vllm), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vllm, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(prefix, filepath.FromSlash(r.HFHome), filepath.FromSlash(s.ModelRepo), "snapshots", "abc")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	return prefix, r
}

func TestResolveReadsTheSnapshotsThroughTheHostPrefix(t *testing.T) {
	s := flagship()
	_, r := fakeDistro(t, s)
	got, err := s.Resolve(r)
	if err != nil {
		t.Fatalf("a snapshot that is in the distro must resolve through the host prefix: %v", err)
	}
	// The path that reaches the rendered files is the one bash INSIDE the distro opens.
	if want := "/hf/" + s.ModelRepo + "/snapshots/abc"; got.ModelPath != want {
		t.Errorf("ModelPath = %q, want the distro path %q (never the host path)", got.ModelPath, want)
	}
	// Without the prefix the same runtime must still look at the process's own filesystem,
	// where /hf does not exist: the prefix is the ONLY thing that moved the read.
	r.HostPrefix = ""
	if _, err := s.Resolve(r); err == nil {
		t.Error("resolved a distro path against the host filesystem with no host prefix")
	}
}

func TestDetectProbesTheVenvAndTheWeightsThroughTheHostPrefix(t *testing.T) {
	s := flagship()
	_, r := fakeDistro(t, s)
	if ok, why := s.Detect(r); !ok {
		t.Fatalf("a box whose distro holds the venv and the weights must detect: %s", why)
	}
	r.HostPrefix = ""
	ok, why := s.Detect(r)
	if ok {
		t.Fatal("detected a seat whose distro paths do not exist on this filesystem")
	}
	if !strings.Contains(why, "vllm entry point") {
		t.Errorf("the reason must name the missing prerequisite, got %q", why)
	}
}
