package fleetnode

// Register C-78: the startup sweep of BaseDir()/pipeline-jobs/ used to remove
// EVERY entry on the premise that fleet-serve is the only writer there. It is
// not: a delegator process (the MCP server, the delegate and research
// commands, the fleet smoke) materializes its own local runs in the same root
// (pipeline.RunAgentContract, agent-local-*), outlives fleet-serve restarts,
// and lost the context of every run in flight to each restart. The sweep now
// asks who owns a directory. These tests pin the decision table:
//
//	marked (jobdir.OwnerFile)    kept iff its owner is alive and the dir is
//	                             younger than jobdir.MaxRunLifetime
//	unmarked, agent-local-*      (a delegator older than the marker) kept iff
//	                             younger than jobdir.MaxRunLifetime
//	anything else                fleet-serve's own: removed, as before

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/jobdir"
)

// pastMaxRunLifetime is comfortably older than jobdir.MaxRunLifetime: the
// margin keeps a filesystem that reports directory times with some lag from
// flipping a verdict.
const pastMaxRunLifetime = jobdir.MaxRunLifetime + time.Hour

// TestSweepHelperExitNow is the child process deadOwnerPID starts: it exits at
// once, leaving a real process id that no longer belongs to anything. Outside
// that child (no env var) it does nothing.
func TestSweepHelperExitNow(t *testing.T) {
	if os.Getenv("FLEETNODE_SWEEP_HELPER") == "1" {
		os.Exit(0)
	}
}

// deadOwnerPID returns the id of a process that has already exited, the way a
// crashed delegator leaves one behind. The test binary re-runs itself (the same
// pattern as internal/gpuactivity) so no shell or platform tool is needed.
func deadOwnerPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSweepHelperExitNow$")
	cmd.Env = append(os.Environ(), "FLEETNODE_SWEEP_HELPER=1")
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper process: %v", err)
	}
	pid := cmd.Process.Pid
	if gpulease.PIDAlive(pid) {
		t.Skipf("pid %d was handed to another process before it could be probed", pid)
	}
	return pid
}

// jobDirSpec is one entry under pipeline-jobs/.
type jobDirSpec struct {
	name      string
	owner     int           // the pid in the owner marker; 0 = no marker
	rawMarker string        // the marker's exact text; wins over owner (a garbled one)
	age       time.Duration // how long ago the dir was last modified; 0 = just now
}

func docText(name string) string { return "context of " + name }

// sweepFixture is a config rooted in a temp dir and its pipeline-jobs/ root.
func sweepFixture(t *testing.T) (config.Config, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Home = t.TempDir()
	jobs := filepath.Join(cfg.BaseDir(), "pipeline-jobs")
	if err := os.MkdirAll(jobs, 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg, jobs
}

// makeJobDir lays a job dir out the way the writers do: context/doc.txt, the
// marker when there is one (this process's own through the real writer), and
// the age applied LAST, because creating entries in a directory updates its
// modification time.
func makeJobDir(t *testing.T, jobs string, s jobDirSpec) string {
	t.Helper()
	d := filepath.Join(jobs, s.name)
	if err := os.MkdirAll(filepath.Join(d, "context"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "context", "doc.txt"), []byte(docText(s.name)), 0o644); err != nil {
		t.Fatal(err)
	}
	switch {
	case s.rawMarker != "":
		if err := os.WriteFile(filepath.Join(d, jobdir.OwnerFile), []byte(s.rawMarker), 0o644); err != nil {
			t.Fatal(err)
		}
	case s.owner == os.Getpid():
		if err := jobdir.WriteOwner(d); err != nil {
			t.Fatal(err)
		}
	case s.owner > 0:
		if err := os.WriteFile(filepath.Join(d, jobdir.OwnerFile), []byte(strconv.Itoa(s.owner)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if s.age > 0 {
		when := time.Now().Add(-s.age)
		if err := os.Chtimes(d, when, when); err != nil {
			t.Fatalf("setup: aging %s: %v", s.name, err)
		}
		// Read it back the way the sweep does: an ageing that silently did
		// nothing would make every "old" case pass for the wrong reason.
		fi, err := os.Lstat(d)
		if err != nil {
			t.Fatal(err)
		}
		if got := time.Since(fi.ModTime()); got < s.age-time.Minute {
			t.Fatalf("setup: %s reads as %v old after Chtimes, want about %v", s.name, got, s.age)
		}
	}
	return d
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// wantDocIntact fails unless the dir's context doc still reads back: a seat
// reading its run's context is the whole point of keeping the dir.
func wantDocIntact(t *testing.T, jobs, name string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(jobs, name, "context", "doc.txt"))
	if err != nil {
		t.Errorf("%s: the context doc is gone: %v", name, err)
		return
	}
	if string(got) != docText(name) {
		t.Errorf("%s: the context doc reads %q, want %q", name, got, docText(name))
	}
}

// sweep runs the startup sweep once and returns what it reports. The effects on
// disk are asserted first by each test, so a failure names what was lost
// before the counts say how the sweep counted it.
func sweep(t *testing.T, cfg config.Config) (swept, kept int) {
	t.Helper()
	swept, kept, err := SweepOrphanedPipelineJobs(cfg)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return swept, kept
}

func wantCounts(t *testing.T, swept, kept, wantSwept, wantKept int) {
	t.Helper()
	if swept != wantSwept || kept != wantKept {
		t.Errorf("swept/kept = %d/%d, want %d/%d", swept, kept, wantSwept, wantKept)
	}
}

// The incident: a local run in flight in a live process, fleet-serve restarts.
// The run's dir, with its context doc, must be exactly as it was.
func TestSweepKeepsALiveLocalRunsDir(t *testing.T) {
	cfg, jobs := sweepFixture(t)
	d := makeJobDir(t, jobs, jobDirSpec{name: "agent-local-101", owner: os.Getpid()})

	swept, kept := sweep(t, cfg)

	if !exists(d) {
		t.Error("the dir of a run owned by a live process was removed")
	}
	wantDocIntact(t, jobs, "agent-local-101")
	wantCounts(t, swept, kept, 0, 1)
}

// A delegator older than the marker leaves none. Its run may be in flight, and
// nothing identifies its owner, so a young dir is kept.
func TestSweepKeepsAYoungUnmarkedLocalRunDir(t *testing.T) {
	cfg, jobs := sweepFixture(t)
	d := makeJobDir(t, jobs, jobDirSpec{name: "agent-local-202"})

	swept, kept := sweep(t, cfg)

	if !exists(d) {
		t.Error("a young unmarked agent-local dir was removed")
	}
	wantDocIntact(t, jobs, "agent-local-202")
	wantCounts(t, swept, kept, 0, 1)
}

// ...but not forever: past the longest a run can live it is a leak.
func TestSweepRemovesAnOldUnmarkedLocalRunDir(t *testing.T) {
	cfg, jobs := sweepFixture(t)
	d := makeJobDir(t, jobs, jobDirSpec{name: "agent-local-303", age: pastMaxRunLifetime})

	swept, kept := sweep(t, cfg)

	if exists(d) {
		t.Error("an unmarked agent-local dir older than MaxRunLifetime survived")
	}
	wantCounts(t, swept, kept, 1, 0)
}

// A crashed delegator's dir: the marker names a process that has exited.
func TestSweepRemovesADirWhoseOwnerExited(t *testing.T) {
	cfg, jobs := sweepFixture(t)
	d := makeJobDir(t, jobs, jobDirSpec{name: "agent-local-404", owner: deadOwnerPID(t)})

	swept, kept := sweep(t, cfg)

	if exists(d) {
		t.Error("the dir of a run whose owner exited survived")
	}
	wantCounts(t, swept, kept, 1, 0)
}

// A live owner does not keep a dir past MaxRunLifetime: a recycled process id
// must not pin a leak for good.
func TestSweepRemovesALiveOwnersDirPastMaxRunLifetime(t *testing.T) {
	cfg, jobs := sweepFixture(t)
	d := makeJobDir(t, jobs, jobDirSpec{name: "agent-local-505", owner: os.Getpid(), age: pastMaxRunLifetime})

	swept, kept := sweep(t, cfg)

	if exists(d) {
		t.Error("a marked dir older than MaxRunLifetime survived a live owner")
	}
	wantCounts(t, swept, kept, 1, 0)
}

// fleet-serve's own materializations carry no marker and are orphans at
// startup by definition, young or not, whatever their names resemble.
func TestSweepStillRemovesFleetServesOwnDirs(t *testing.T) {
	cfg, jobs := sweepFixture(t)
	names := []string{"agent-123", "accel-9", "web-1a2b3c4d"}
	for _, n := range names {
		makeJobDir(t, jobs, jobDirSpec{name: n})
	}

	swept, kept := sweep(t, cfg)

	for _, n := range names {
		if exists(filepath.Join(jobs, n)) {
			t.Errorf("%s: fleet-serve's own dir survived the startup sweep", n)
		}
	}
	wantCounts(t, swept, kept, len(names), 0)
}

// A marker that is not a usable id is no marker (jobdir.ReadOwner's rule). The
// sweep resolves that toward keeping when the name says a delegator made the
// dir (a marker caught mid-write), and toward removing for fleet-serve's own.
func TestSweepTreatsAGarbledMarkerAsUnmarked(t *testing.T) {
	cfg, jobs := sweepFixture(t)
	makeJobDir(t, jobs, jobDirSpec{name: "agent-local-606", rawMarker: "not-a-pid"})
	makeJobDir(t, jobs, jobDirSpec{name: "agent-7", rawMarker: "not-a-pid"})

	swept, kept := sweep(t, cfg)

	if !exists(filepath.Join(jobs, "agent-local-606")) {
		t.Error("a young agent-local dir with a garbled marker was removed")
	}
	if exists(filepath.Join(jobs, "agent-7")) {
		t.Error("fleet-serve's own dir with a garbled marker survived")
	}
	wantCounts(t, swept, kept, 1, 1)
}

// Every case at once, with the exact counts the startup log reports, and the
// kept dirs' docs intact.
func TestSweepCountsAreExact(t *testing.T) {
	cfg, jobs := sweepFixture(t)
	dead := deadOwnerPID(t)

	kept := []jobDirSpec{
		{name: "agent-local-11", owner: os.Getpid()},
		{name: "agent-local-12"},
		{name: "agent-local-13", rawMarker: "\n"},
	}
	swept := []jobDirSpec{
		{name: "agent-local-21", owner: dead},
		{name: "agent-local-22", owner: os.Getpid(), age: pastMaxRunLifetime},
		{name: "agent-local-23", age: pastMaxRunLifetime},
		{name: "agent-31"},
		{name: "accel-32"},
		{name: "web-33"},
	}
	for _, s := range append(append([]jobDirSpec{}, kept...), swept...) {
		makeJobDir(t, jobs, s)
	}
	// A stray file is fleet-serve's own debris too, and counts as swept.
	if err := os.WriteFile(filepath.Join(jobs, "stray.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	gotSwept, gotKept := sweep(t, cfg)

	for _, s := range kept {
		if !exists(filepath.Join(jobs, s.name)) {
			t.Errorf("%s: should have been kept", s.name)
			continue
		}
		wantDocIntact(t, jobs, s.name)
	}
	for _, s := range swept {
		if exists(filepath.Join(jobs, s.name)) {
			t.Errorf("%s: should have been swept", s.name)
		}
	}
	if exists(filepath.Join(jobs, "stray.tmp")) {
		t.Error("stray.tmp should have been swept")
	}
	if !exists(jobs) {
		t.Error("the pipeline-jobs root itself must survive")
	}
	wantCounts(t, gotSwept, gotKept, len(swept)+1, len(kept))
}

// A removal that fails is reported, is never counted as swept or as kept, and
// does not stop the rest of the sweep.
func TestSweepFailedRemovalIsNeitherSweptNorKept(t *testing.T) {
	cfg, jobs := sweepFixture(t)
	healthy := makeJobDir(t, jobs, jobDirSpec{name: "accel-42"})
	stuck := makeJobDir(t, jobs, jobDirSpec{name: "agent-41"})
	live := makeJobDir(t, jobs, jobDirSpec{name: "agent-local-43", owner: os.Getpid()})
	release := blockRemoval(t, stuck)
	defer release()

	swept, kept, err := SweepOrphanedPipelineJobs(cfg)

	if !exists(live) {
		t.Error("the live run's dir was removed")
	}
	if !exists(stuck) {
		t.Error("the setup did not actually block the removal")
	}
	if exists(healthy) {
		t.Error("a removable dir was left because another one failed")
	}
	if err == nil {
		t.Error("a failed removal must be reported as an error")
	}
	// The stuck dir is neither swept nor kept.
	wantCounts(t, swept, kept, 1, 1)
}

// The age rule is "older than jobdir.MaxRunLifetime", strictly: a dir exactly
// that old is still kept, one a second older is not, and a dir stamped in the
// future (a clock that stepped back) counts as young.
func TestJudgeAgeBoundary(t *testing.T) {
	_, jobs := sweepFixture(t)
	d := makeJobDir(t, jobs, jobDirSpec{name: "agent-local-707", owner: os.Getpid()})
	fi, err := os.Lstat(d)
	if err != nil {
		t.Fatal(err)
	}
	born := fi.ModTime()
	cases := []struct {
		name string
		now  time.Time
		want pipelineJobDirFate
	}{
		{"just made", born, pipelineJobDirInUse},
		{"exactly the maximum", born.Add(jobdir.MaxRunLifetime), pipelineJobDirInUse},
		{"a second past it", born.Add(jobdir.MaxRunLifetime + time.Second), pipelineJobDirOrphaned},
		{"stamped in the future", born.Add(-time.Hour), pipelineJobDirInUse},
	}
	for _, c := range cases {
		if got := judgePipelineJobDir(d, filepath.Base(d), c.now); got != c.want {
			t.Errorf("%s: fate = %d, want %d", c.name, got, c.want)
		}
	}
}

// An entry that disappears between the listing and the decision (its run ended
// and removed its own dir) is nothing to remove and nothing to keep.
func TestJudgeAnEntryThatVanishedIsNeitherOrphanedNorInUse(t *testing.T) {
	_, jobs := sweepFixture(t)
	gone := filepath.Join(jobs, "agent-local-808")
	if got := judgePipelineJobDir(gone, "agent-local-808", time.Now()); got != pipelineJobDirGone {
		t.Fatalf("fate of a vanished agent-local entry = %d, want %d (gone)", got, pipelineJobDirGone)
	}
}

// The marker decides, not the name: a marked dir is kept for a live, recent
// owner whatever it is called, and removed for a dead one. (Only the
// delegator's local runs write markers today; this pins the rule for whatever
// writes the next.)
func TestSweepMarkerBeatsTheName(t *testing.T) {
	cfg, jobs := sweepFixture(t)
	live := makeJobDir(t, jobs, jobDirSpec{name: "worker-909", owner: os.Getpid()})
	dead := makeJobDir(t, jobs, jobDirSpec{name: "worker-910", owner: deadOwnerPID(t)})

	swept, kept := sweep(t, cfg)

	if !exists(live) {
		t.Error("a marked dir with a live owner was removed because of its name")
	}
	if exists(dead) {
		t.Error("a marked dir with a dead owner survived because of its name")
	}
	wantCounts(t, swept, kept, 1, 1)
}
