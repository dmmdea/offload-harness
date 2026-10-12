package main

// gpu_doctor_installed_test.go: which build `gpu doctor` calls "the installed build" (D-36 fix round). The reference
// is read from the doctor's own executable file. Two cases that reference does not describe are said, not guessed:
// the doctor's own path is unresolved (nothing to read), and the doctor is not the harness binary PATH resolves (a
// worktree or dev build beside a deployed copy), where "older than the installed build" would send the operator to
// restart deployed processes for nothing.

import (
	"errors"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// When the doctor's own executable path cannot be resolved there is no installed file to read, and the report must
// blame that: not "the installed build carries no build marker", which would contradict the "could not resolve the
// running executable" note the same report already carries.
func TestGPUDoctorBlamesAnUnresolvedExecutablePathNotAMissingMarker(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	doctorExe = func() (string, error) { return "", errors.New("the executable path is unavailable") }
	old := writeFakeBinaryVersion(t, t.TempDir(), "local-offload-old.exe", "0.1.0")
	runningImagesFn = func(func(int, string) string) ([]gpulease.ProcImage, error) {
		return []gpulease.ProcImage{{PID: 4711, Path: old, Why: "harness process"}}, nil
	}
	if err := runGPUDoctor([]string{"--config", cfg}); err != nil {
		t.Fatalf("runGPUDoctor: %v\n%s", err, out)
	}
	text := out.String()
	if !strings.Contains(text, "could not resolve the running executable") {
		t.Fatalf("the unresolved path is noted:\n%s", text)
	}
	if strings.Contains(text, "carries no build marker") {
		t.Fatalf("an unresolved path is not a missing marker; one report must not give two causes for one fault:\n%s", text)
	}
	if !strings.Contains(text, "installed build version unknown") || !strings.Contains(text, "path could not be resolved") {
		t.Fatalf("the comparison note must name the unresolved path as its cause:\n%s", text)
	}
}

// The reference is the doctor's own file. Run from a build that is not the one PATH resolves (a worktree or dev
// binary, beside a deployed copy on PATH), it is not "the installed build", and calling the deployed processes older
// than it would send the operator to restart them for nothing: nothing is compared, and a note says why.
func TestGPUDoctorComparesNothingWhenItIsNotThePathCopy(t *testing.T) {
	cfg, _, out := doctorFixture(t) // the doctor runs from its own install dir, build 9.9.9
	deployed := t.TempDir()
	writeFakeBinaryVersion(t, deployed, "local-offload.exe", "9.9.7") // the copy PATH resolves: another file, same name
	doctorPathEnv = func() string { return deployed }
	pre := writeFakeBinaryVersion(t, t.TempDir(), "local-offload-running.exe", "9.9.8")
	runningImagesFn = func(func(int, string) string) ([]gpulease.ProcImage, error) {
		return []gpulease.ProcImage{{PID: 5150, Path: pre, Why: "harness process"}}, nil
	}
	for _, it := range doctorItems(t, cfg, out).Items {
		if it.Older {
			t.Errorf("%s was called older than a build that is not the installed one", it.Path)
		}
	}
	out.Reset()
	if err := runGPUDoctor([]string{"--config", cfg}); err != nil {
		t.Fatalf("runGPUDoctor: %v\n%s", err, out)
	}
	text := out.String()
	if strings.Contains(text, "older than the installed build") {
		t.Fatalf("no restart advice against a build that is not installed:\n%s", text)
	}
	if !strings.Contains(text, "not the harness binary on PATH") {
		t.Fatalf("the report must say why nothing was compared:\n%s", text)
	}
}

// Guard for the above: a doctor that IS the PATH copy (the normal case) still marks the older processes.
func TestGPUDoctorStillMarksWhenItIsThePathCopy(t *testing.T) {
	cfg, install, out := doctorFixture(t)
	doctorPathEnv = func() string { return install } // the doctor's own file is on PATH
	pre := writeFakeBinaryVersion(t, t.TempDir(), "local-offload-running.exe", "9.9.8")
	runningImagesFn = func(func(int, string) string) ([]gpulease.ProcImage, error) {
		return []gpulease.ProcImage{{PID: 5151, Path: pre, Why: "harness process"}}, nil
	}
	marked := 0
	for _, it := range doctorItems(t, cfg, out).Items {
		if it.Older {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("the PATH copy's own build is the installed one: want 1 process marked older, got %d", marked)
	}
}
