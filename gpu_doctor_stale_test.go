package main

// gpu_doctor_stale_test.go: `gpu doctor` marks the running harness processes whose image is older than the installed
// build (D-36). A rename-swap deploy replaces the installed binary while every process already running keeps the
// image it started from; the doctor already lists those images with the build version their marker states, and
// without a mark the operator has to compare the list by eye (13 older-build processes beside 10 current ones,
// 2026-10-11). The installed version is read from the installed FILE (the doctor's own executable path), not from
// the doctor's compiled-in version: a doctor that is itself a pre-swap process would otherwise compare the old
// build with itself and mark nothing.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// writeFakeBinaryVersion is writeFakeBinary with the version its build marker states ("" = no marker at all).
func writeFakeBinaryVersion(t *testing.T, dir, name, version string) string {
	t.Helper()
	body := []byte(strings.Repeat("\x00MZ-padding-", 200) + "\x00" + gpulease.FormatSignature + "\x00")
	if version != "" {
		body = append(body, []byte(gpulease.BuildMarkerPrefix+version+"\x00")...)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

type doctorJSON struct {
	Items []struct {
		Path    string `json:"path"`
		Version string `json:"version"`
		PIDs    []int  `json:"pids"`
		Older   bool   `json:"older_than_installed"`
	} `json:"items"`
}

func doctorItems(t *testing.T, cfg string, out interface{ Bytes() []byte }) doctorJSON {
	t.Helper()
	if err := runGPUDoctor([]string{"--config", cfg, "--json"}); err != nil {
		t.Fatalf("runGPUDoctor: %v", err)
	}
	var doc doctorJSON
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("--json must emit one JSON document: %v\n%s", err, out.Bytes())
	}
	return doc
}

func TestGPUDoctorMarksAProcessOlderThanTheInstalledBuild(t *testing.T) {
	cfg, _, out := doctorFixture(t) // the installed binary's marker states 9.9.9
	elsewhere := t.TempDir()
	older := writeFakeBinaryVersion(t, elsewhere, "local-offload-pre-swap.exe", "9.9.8")
	current := writeFakeBinaryVersion(t, elsewhere, "local-offload-current.exe", "9.9.9")
	runningImagesFn = func(func(int, string) string) ([]gpulease.ProcImage, error) {
		return []gpulease.ProcImage{
			{PID: 4242, Path: older, Why: "harness process"},
			{PID: 4243, Path: current, Why: "harness process"},
		}, nil
	}

	// The text report names the process and the installed build it is older than.
	if err := runGPUDoctor([]string{"--config", cfg}); err != nil {
		t.Fatalf("runGPUDoctor: %v\n%s", err, out)
	}
	text := out.String()
	if !strings.Contains(text, "older than the installed build 9.9.9") || !strings.Contains(text, "4242") {
		t.Fatalf("the report must mark the pre-swap process and say which build it is older than:\n%s", text)
	}
	if strings.Count(text, "older than the installed build 9.9.9") != 2 { // one on the row, one in the summary
		t.Errorf("exactly one process is older (a row line and a summary line); the report says it %d times:\n%s", strings.Count(text, "older than the installed build 9.9.9"), text)
	}

	// The JSON marks exactly that row.
	out.Reset()
	for _, it := range doctorItems(t, cfg, out).Items {
		want := it.Path == older
		if it.Older != want {
			t.Errorf("%s (build %s, pids %v): older_than_installed = %v, want %v", it.Path, it.Version, it.PIDs, it.Older, want)
		}
	}
}

// The installed version comes from the installed FILE: a doctor whose own compiled-in version is the old one still
// marks the older image, and one whose compiled-in version is newer than everything marks nothing it cannot prove.
func TestGPUDoctorReadsTheInstalledVersionFromTheInstalledFileNotItsOwnConstant(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	elsewhere := t.TempDir()
	// Older than the installed file (9.9.9) but NEWER than this binary's compiled-in version.
	pre := writeFakeBinaryVersion(t, elsewhere, "local-offload-pre-swap.exe", "9.9.8")
	runningImagesFn = func(func(int, string) string) ([]gpulease.ProcImage, error) {
		return []gpulease.ProcImage{{PID: 777, Path: pre, Why: "harness process"}}, nil
	}
	got := doctorItems(t, cfg, out)
	marked := 0
	for _, it := range got.Items {
		if it.Older {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("one process is older than the installed file's 9.9.9 whatever the doctor's own version is; %d marked", marked)
	}
}

// Guard: an image whose version cannot be compared (no marker: it predates the marker, or is a binary that never
// carried one) is shown as it always was and is never called older.
func TestGPUDoctorDoesNotCallAnImageWithoutAMarkerOlder(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	bare := writeFakeBinaryVersion(t, t.TempDir(), "local-agent.exe", "")
	runningImagesFn = func(func(int, string) string) ([]gpulease.ProcImage, error) {
		return []gpulease.ProcImage{{PID: 888, Path: bare, Why: "harness process"}}, nil
	}
	for _, it := range doctorItems(t, cfg, out).Items {
		if it.Older {
			t.Errorf("%s has no build marker and must not be marked older", it.Path)
		}
	}
}

// Guard: when the installed build's own version cannot be read, nothing is compared and the report says so.
func TestGPUDoctorSaysWhenTheInstalledVersionCannotBeRead(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	exe, _ := doctorExe()
	if err := os.WriteFile(exe, []byte(strings.Repeat("\x00MZ-padding-", 200)+"\x00"+gpulease.FormatSignature+"\x00"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := writeFakeBinaryVersion(t, t.TempDir(), "local-offload-old.exe", "0.1.0")
	runningImagesFn = func(func(int, string) string) ([]gpulease.ProcImage, error) {
		return []gpulease.ProcImage{{PID: 999, Path: old, Why: "harness process"}}, nil
	}
	if err := runGPUDoctor([]string{"--config", cfg}); err != nil {
		t.Fatalf("runGPUDoctor: %v\n%s", err, out)
	}
	if strings.Contains(out.String(), "older than the installed build") {
		t.Fatalf("nothing can be called older than an installed version that is unknown:\n%s", out)
	}
	if !strings.Contains(out.String(), "installed build version unknown") {
		t.Fatalf("the report must say the comparison was not made:\n%s", out)
	}
}
