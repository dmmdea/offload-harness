package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// doctorFixture builds a host: a lease root (state_dir), an install dir with an aware
// binary and an aware Node reader, and seams so the doctor sees only that. It returns
// the config path, the install dir and the output buffer.
func doctorFixture(t *testing.T) (cfg, install string, out *bytes.Buffer) {
	t.Helper()
	cfg, _ = leaseFixture(t)
	install = t.TempDir()
	exe := writeFakeBinary(t, install, "local-offload.exe", true)
	if err := os.MkdirAll(filepath.Join(install, "render"), 0o755); err != nil {
		t.Fatal(err)
	}
	reader := "// " + gpulease.FormatSignature + "\nexport const x = 1\n"
	if err := os.WriteFile(filepath.Join(install, "render", "gpu-lock.mjs"), []byte(reader), 0o644); err != nil {
		t.Fatal(err)
	}
	out = &bytes.Buffer{}
	oldExe, oldPath, oldImgs, oldOut := doctorExe, doctorPathEnv, runningImagesFn, doctorOut
	doctorExe = func() (string, error) { return exe, nil }
	doctorPathEnv = func() string { return "" }
	runningImagesFn = func(func(int, string) string) ([]gpulease.ProcImage, error) { return nil, nil }
	doctorOut = out
	t.Cleanup(func() { doctorExe, doctorPathEnv, runningImagesFn, doctorOut = oldExe, oldPath, oldImgs, oldOut })
	return cfg, install, out
}

func writeFakeBinary(t *testing.T, dir, name string, aware bool) string {
	t.Helper()
	body := []byte(strings.Repeat("\x00MZ-padding-", 200))
	if aware {
		body = append(body, []byte("\x00"+gpulease.FormatSignature+"\x00offload-build-version=9.9.9\x00")...)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustOpenLease(t *testing.T, cfgPath string) *gpulease.Manager {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("config", cfgPath, "")
	m, err := openLease(fs)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// writerEnabledWithConfigOn rewrites the host's config with gpu_card_scoped_leases on
// (same state root) and reports whether the CLI then enables the writer.
func writerEnabledWithConfigOn(t *testing.T, cfgPath string) bool {
	t.Helper()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["gpu_card_scoped_leases"] = true
	b, _ := json.Marshal(m)
	on := filepath.Join(filepath.Dir(cfgPath), "config-on.json")
	if err := os.WriteFile(on, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return mustOpenLease(t, on).CardScoped()
}

func markerResult(t *testing.T, cfg string) string {
	t.Helper()
	m := mustOpenLease(t, cfg)
	return m.ReaderAuditResult()
}

func TestGPUDoctorGreenHostWritesTheMarkerOnlyWhenAsked(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	if err := runGPUDoctor([]string{"--config", cfg}); err != nil {
		t.Fatalf("a clean host audits green: %v\n%s", err, out)
	}
	if got := markerResult(t, cfg); got != "absent" {
		t.Fatalf("without --write-audit nothing may be written, marker is %q", got)
	}
	if !strings.Contains(out.String(), "GREEN") || !strings.Contains(out.String(), "--write-audit") {
		t.Fatalf("the output must give the verdict and the way to record it:\n%s", out)
	}
	out.Reset()
	if err := runGPUDoctor([]string{"--config", cfg, "--write-audit"}); err != nil {
		t.Fatal(err)
	}
	if got := markerResult(t, cfg); got != "green" {
		t.Fatalf("--write-audit on a green host writes the green marker, got %q", got)
	}
	// That marker is what lets the writer switch on.
	if !writerEnabledWithConfigOn(t, cfg) {
		t.Fatal("with config on and a green marker the writer must be enabled")
	}
}

func TestGPUDoctorRedHostFailsAndRevokesTheMarker(t *testing.T) {
	cfg, install, out := doctorFixture(t)
	if err := runGPUDoctor([]string{"--config", cfg, "--write-audit"}); err != nil {
		t.Fatal(err)
	}
	if markerResult(t, cfg) != "green" {
		t.Fatal("setup: the marker should be green")
	}
	old := writeFakeBinary(t, install, "local-offload-wrapper.exe", false)
	out.Reset()
	err := runGPUDoctor([]string{"--config", cfg, "--write-audit"})
	if err == nil {
		t.Fatalf("a host with a binary that predates the fence must fail:\n%s", out)
	}
	if !strings.Contains(out.String(), old) || !strings.Contains(out.String(), "NOT GREEN") {
		t.Fatalf("the offender must be named:\n%s", out)
	}
	if got := markerResult(t, cfg); got != "red" {
		t.Fatalf("the red result must revoke the earlier green marker, got %q", got)
	}
}

func TestGPUDoctorAuditsRunningImagesOffTheScanRoots(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	elsewhere := t.TempDir()
	stale := writeFakeBinary(t, elsewhere, "local-offload-fleet.exe", false)
	runningImagesFn = func(want func(int, string) string) ([]gpulease.ProcImage, error) {
		if want(4321, "local-offload-fleet.exe") == "" {
			t.Error("a process with a harness image name must be wanted")
		}
		if want(1, "explorer.exe") != "" {
			t.Error("an unrelated process must not be audited")
		}
		return []gpulease.ProcImage{{PID: 4321, Path: stale, Why: "harness process"}}, nil
	}
	if err := runGPUDoctor([]string{"--config", cfg}); err == nil {
		t.Fatalf("a running old image must fail the audit:\n%s", out)
	}
	if !strings.Contains(out.String(), "4321") {
		t.Fatalf("the pid must be shown:\n%s", out)
	}
}

// A lease holder or waiter is audited whatever its image is called: a wrapper copy
// named for a media repository would otherwise never be looked at.
func TestGPUDoctorWantsLeaseHoldersAndWaitersWhateverTheirImageIsCalled(t *testing.T) {
	cfg, _, _ := doctorFixture(t)
	m := mustOpenLease(t, cfg)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "holder", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	asked := false
	runningImagesFn = func(want func(int, string) string) ([]gpulease.ProcImage, error) {
		asked = true
		if want(os.Getpid(), "film-wrapper.exe") != "lease holder" {
			t.Error("the lease holder's image must be wanted whatever it is called")
		}
		if want(os.Getpid()+100000, "film-wrapper.exe") != "" {
			t.Error("a process that holds nothing and is not a harness binary must not be wanted")
		}
		return nil, nil
	}
	if err := runGPUDoctor([]string{"--config", cfg}); err != nil {
		t.Fatal(err)
	}
	if !asked {
		t.Fatal("the process table was never read")
	}
}

func TestGPUDoctorScanFlagAddsRootsAndJSONIsParseable(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	media := t.TempDir()
	if err := os.WriteFile(filepath.Join(media, "gpu-lock.mjs"), []byte("export const checkInheritedLease = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runGPUDoctor([]string{"--config", cfg, "--scan", media, "--json"})
	if err == nil {
		t.Fatal("an un-fenced Node reader in a scanned media repository must fail the audit")
	}
	var doc struct {
		Green bool `json:"green"`
		Items []struct {
			Path  string `json:"path"`
			Kind  string `json:"kind"`
			Aware bool   `json:"aware"`
		} `json:"items"`
		Marker string `json:"marker"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &doc); jerr != nil {
		t.Fatalf("--json must emit one JSON document: %v\n%s", jerr, out)
	}
	if doc.Green || doc.Marker != "absent" {
		t.Fatalf("%+v", doc)
	}
	found := false
	for _, it := range doc.Items {
		if strings.HasPrefix(it.Path, media) && it.Kind == "node-reader" && !it.Aware {
			found = true
		}
	}
	if !found {
		t.Fatalf("the media repository's reader must be listed as not aware: %s", out)
	}
}

func TestGPUDoctorListsBinariesFoundOnPATH(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	pathDir := t.TempDir()
	stale := writeFakeBinary(t, pathDir, "local-offload.exe", false)
	doctorPathEnv = func() string { return pathDir }
	if err := runGPUDoctor([]string{"--config", cfg}); err == nil || !strings.Contains(out.String(), stale) {
		t.Fatalf("a stale copy on PATH must be found and fail the audit: %v\n%s", err, out)
	}
}

// The render tree lives apart from the binary in an install: the directories of the
// configured render scripts are scanned for gpu-lock.mjs without being told.
func TestGPUDoctorScansTheConfiguredRenderScriptDirectories(t *testing.T) {
	cfg, _, out := doctorFixture(t)
	renderDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderDir, "gpu-lock.mjs"), []byte("export const checkInheritedLease = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(cfg)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["videogen_script"] = filepath.Join(renderDir, "comfy-video.mjs")
	b, _ := json.Marshal(m)
	if err := os.WriteFile(cfg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runGPUDoctor([]string{"--config", cfg}); err == nil || !strings.Contains(out.String(), filepath.Join(renderDir, "gpu-lock.mjs")) {
		t.Fatalf("an un-fenced reader beside a configured render script must fail the audit: %v\n%s", err, out)
	}
}
