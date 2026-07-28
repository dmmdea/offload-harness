package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWritableDirProvesItByWriting: the Linux node's failure was a lease directory
// that existed and was readable but not writable by the running user. Anything
// short of an actual write reports it as fine.
func TestWritableDirProvesItByWriting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gpu", "lease")
	got := WritableDir(dir)
	if got.Status != Pass {
		t.Fatalf("a fresh temp dir must pass: %+v", got)
	}
	if !strings.Contains(got.Detail, Identity()) {
		t.Errorf("the detail must name the identity that succeeded: %q", got.Detail)
	}
	// The probe must not survive the check.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".acceptance-") {
			t.Errorf("probe file left behind: %s", e.Name())
		}
	}
}

// TestWritableDirFailsWhenItCannotBeCreated: a path that cannot exist must fail
// loudly here rather than at the moment a job takes the GPU.
func TestWritableDirFailsWhenItCannotBeCreated(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := WritableDir(filepath.Join(blocker, "gpu", "lease"))
	if got.Status != Fail {
		t.Fatalf("a lease dir under a regular file must FAIL, got %+v", got)
	}
	if !strings.Contains(got.Detail, "job") {
		t.Errorf("the failure must say what it costs at job time: %q", got.Detail)
	}
}

// TestWritableDirRejectsAnEmptyPath: an unresolved lease dir is a failure, not a
// silently skipped check.
func TestWritableDirRejectsAnEmptyPath(t *testing.T) {
	if got := WritableDir("  "); got.Status != Fail {
		t.Fatalf("an unresolved lease dir must FAIL, got %+v", got)
	}
}

// TestRunnableExecutesRatherThanStats is the Windows failure in miniature: a uv
// trampoline stats fine for every account and runs only for its owner, so the
// check has to actually execute the thing.
func TestRunnableExecutesRatherThanStats(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-here")
	got := Runnable(context.Background(), "python", missing, "--version")
	if got.Status != Fail {
		t.Fatalf("an unexecutable binary must FAIL, got %+v", got)
	}
	if !strings.Contains(got.Detail, Identity()) {
		t.Errorf("the failure must name the identity that could not run it: %q", got.Detail)
	}
}

// TestRunnableSkipsWhatIsNotBound: a machine that never bound GIMP is a
// legitimate machine, not a broken one — the same rule the media verdicts use.
func TestRunnableSkipsWhatIsNotBound(t *testing.T) {
	got := Runnable(context.Background(), "gimp", "")
	if got.Status != Skip {
		t.Fatalf("an unbound binary must SKIP, got %+v", got)
	}
}

// TestReportReadyFlipsOnlyOnFailure: SKIP must never make a node look unready, or
// every minimal install would be quarantined.
func TestReportReadyFlipsOnlyOnFailure(t *testing.T) {
	r := New()
	if !r.Ready {
		t.Fatal("a fresh report starts ready")
	}
	r.Add(Check{"a", Pass, ""}, Check{"b", Skip, ""})
	if !r.Ready {
		t.Fatalf("pass + skip must stay ready: %+v", r)
	}
	r.Add(Check{"c", Fail, "boom"})
	if r.Ready {
		t.Fatal("a failure must make the node not ready")
	}
	if f := r.Failures(); len(f) != 1 || f[0].Name != "c" {
		t.Errorf("Failures() must return exactly the failed checks, got %+v", f)
	}
}
