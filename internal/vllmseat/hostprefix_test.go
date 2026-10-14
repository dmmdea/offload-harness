package vllmseat

import (
	"errors"
	"io/fs"
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

// pathCause is the operating system's own reason for a failed stat: the part after the path
// in a *fs.PathError. The tests compute it rather than spell it, so they hold on every OS.
func pathCause(t *testing.T, p string) string {
	t.Helper()
	_, err := os.Stat(p)
	if err == nil {
		t.Fatalf("%s exists; the test needs it missing", p)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// "No vllm entry point" told an operator to build a venv in every case, including the ones
// where the venv was never the problem: a mistyped prefix, a distro this process cannot
// reach, a share it may not read. Detect threw the stat error away, so the three looked
// identical to a missing venv. The reason now carries what the operating system said, and a
// prefix that cannot be read is named as the prefix, not as a missing venv.
func TestDetectNamesAnUnreadableHostPrefixAndKeepsTheStatError(t *testing.T) {
	s := flagship()
	_, r := fakeDistro(t, s)
	r.HostPrefix = filepath.Join(t.TempDir(), "no-such-distro")
	ok, why := s.Detect(r)
	if ok {
		t.Fatal("detected a seat through a host prefix that does not exist")
	}
	for _, want := range []string{"host prefix", filepath.ToSlash(r.HostPrefix), pathCause(t, r.HostPrefix)} {
		if !strings.Contains(why, want) {
			t.Errorf("the reason must contain %q, got %q", want, why)
		}
	}
	if strings.Contains(why, "vllm entry point") {
		t.Errorf("an unreadable prefix is not a missing venv, yet the reason says so: %q", why)
	}
}

func TestDetectKeepsTheStatErrorOfTheVenvAndTheWeights(t *testing.T) {
	s := flagship()
	prefix, r := fakeDistro(t, s)

	// A venv that is not there: the reason says where and why the operating system refused.
	vllm := r.hostPath(filepath.Join(r.VenvDir, "bin", "vllm"))
	if err := os.Remove(vllm); err != nil {
		t.Fatal(err)
	}
	_, why := s.Detect(r)
	for _, want := range []string{"vllm entry point", pathCause(t, vllm)} {
		if !strings.Contains(why, want) {
			t.Errorf("a missing venv: the reason must contain %q, got %q", want, why)
		}
	}

	// An entry point that is a directory is a different fault from a missing one.
	if err := os.MkdirAll(vllm, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, why := s.Detect(r); !strings.Contains(why, "vllm entry point") || !strings.Contains(why, "directory") {
		t.Errorf("a directory where the entry point should be: the reason must say so, got %q", why)
	}
	if err := os.Remove(vllm); err != nil {
		t.Fatal(err)
	}
	putFile(t, vllm)

	// A pinned snapshot that is not there.
	s.ModelPath = "/hf/" + s.ModelRepo + "/snapshots/gone"
	_, why = s.Detect(r)
	gone := filepath.Join(prefix, filepath.FromSlash(s.ModelPath))
	for _, want := range []string{"model snapshot", pathCause(t, gone)} {
		if !strings.Contains(why, want) {
			t.Errorf("a missing snapshot: the reason must contain %q, got %q", want, why)
		}
	}
}

func putFile(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
