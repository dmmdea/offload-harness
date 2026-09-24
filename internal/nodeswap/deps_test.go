package nodeswap

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := hashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hello world"), measured: printf 'hello world' | sha256sum.
	const want = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if len(got) != 64 {
		t.Fatalf("hashFile returned %q (len %d), want a 64-char hex sha256", got, len(got))
	}
	if got != want {
		t.Errorf("hashFile(%q) = %s, want %s", p, got, want)
	}
}

func TestHashFile_MissingFile(t *testing.T) {
	if _, err := hashFile(filepath.Join(t.TempDir(), "nope.bin")); err == nil {
		t.Fatal("expected an error hashing a missing file")
	}
}

func TestReadHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "qube", "harness_version": "0.140.8", "jobs_running": 0, "jobs_queued": 2,
		})
	}))
	defer srv.Close()
	h, err := readHealth(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !h.OK || h.NodeID != "qube" || h.Version != "0.140.8" || h.QueuedJobs != 2 {
		t.Errorf("readHealth = %+v, unexpected", h)
	}
}

func TestReadHealth_FallsBackToQueueDepthOnOlderNodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"node_id": "old-node", "harness_version": "0.99.0", "queue_depth": 3})
	}))
	defer srv.Close()
	h, err := readHealth(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if h.RunningJobs != 3 {
		t.Errorf("RunningJobs = %d, want the queue_depth fallback (3)", h.RunningJobs)
	}
}

func TestReadHealth_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("stale VRAM snapshot"))
	}))
	defer srv.Close()
	if _, err := readHealth(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error on a non-200 /fleet/health response")
	}
}

func buildTestTarGz(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	tgz := filepath.Join(dir, "render.tar.gz")
	f, err := os.Create(tgz)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: root + "/" + name, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return tgz
}

func TestExtractTarGz(t *testing.T) {
	tgz := buildTestTarGz(t, "render", map[string]string{
		"comfy-video.mjs":  "content-a",
		"lib/audio-qa.mjs": "content-b",
	})
	dest := t.TempDir()
	n, err := extractTarGz(tgz, dest)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("extracted %d files, want 2", n)
	}
	got, err := os.ReadFile(filepath.Join(dest, "comfy-video.mjs"))
	if err != nil || string(got) != "content-a" {
		t.Errorf("comfy-video.mjs = %q, %v", got, err)
	}
	got2, err := os.ReadFile(filepath.Join(dest, "lib", "audio-qa.mjs"))
	if err != nil || string(got2) != "content-b" {
		t.Errorf("lib/audio-qa.mjs = %q, %v", got2, err)
	}
}

func TestExtractTarGz_RejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	tgz := filepath.Join(dir, "evil.tar.gz")
	f, err := os.Create(tgz)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	// A top-level entry named "render/../../evil.txt" should be rejected
	// once the leading "render/" is stripped down to "../../evil.txt".
	body := []byte("gotcha")
	hdr := &tar.Header{Name: "render/../../evil.txt", Mode: 0o644, Size: int64(len(body))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	f.Close()

	dest := t.TempDir()
	if _, err := extractTarGz(tgz, dest); err == nil {
		t.Fatal("expected extractTarGz to refuse a path that escapes the destination directory")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil.txt")); err == nil {
		t.Fatal("a path-escaping entry was actually written outside the destination")
	}
}

func TestExtractTarGz_EmptyTarballIsAnError(t *testing.T) {
	dir := t.TempDir()
	tgz := filepath.Join(dir, "empty.tar.gz")
	f, _ := os.Create(tgz)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.Close()
	gz.Close()
	f.Close()
	if _, err := extractTarGz(tgz, t.TempDir()); err == nil {
		t.Fatal("expected an error extracting a tarball with no regular files")
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	if fileExists(p) {
		t.Fatal("fileExists true for a path that was never created")
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !fileExists(p) {
		t.Fatal("fileExists false for a path that was just created")
	}
}

func TestRenameFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	dst := filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := renameFile(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "payload" {
		t.Errorf("dst content = %q, %v", got, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should no longer exist after rename")
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	dst := filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "payload" {
		t.Errorf("dst content = %q, %v", got, err)
	}
	// A copy, never a move: installNewBinary (nodeswap.go) still needs the
	// original staged binary at its own path afterward.
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source should still exist after copyFile (it is a copy, not a move): %v", err)
	}
}

func TestCopyFile_MissingSource(t *testing.T) {
	dir := t.TempDir()
	if err := copyFile(filepath.Join(dir, "nope"), filepath.Join(dir, "dst")); err == nil {
		t.Fatal("expected an error copying a missing source file")
	}
}

func TestCopyFile_RefusesToOverwriteAnExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	dst := filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err == nil {
		t.Fatal("expected copyFile to refuse overwriting an existing destination (O_EXCL)")
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "already here" {
		t.Errorf("destination was overwritten despite the refusal: %q", got)
	}
}

// TestIsCrossDeviceRenameErr_EXDEV pins the Linux/macOS half: a real
// os.Rename across a mount boundary returns *os.LinkError wrapping
// syscall.EXDEV directly, and isCrossDeviceRenameErr must recognize that
// shape on every OS this package ships to (the check runs unconditionally,
// before the Windows-only errno branch).
func TestIsCrossDeviceRenameErr_EXDEV(t *testing.T) {
	err := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EXDEV}
	if !isCrossDeviceRenameErr(err) {
		t.Fatal("expected EXDEV to be detected as a cross-device rename error")
	}
}

// TestIsCrossDeviceRenameErr_WindowsErrno17 pins the Windows half: measured
// on this dev machine (a real cross-drive os.Rename, C:\ staged against a
// D:\ target), the failure decodes to syscall.Errno(17)
// (ERROR_NOT_SAME_DEVICE), NOT syscall.EXDEV — see the isCrossDeviceRenameErr
// doc comment (deps.go) for the measured detail. Skipped off Windows: errno
// 17 has no special meaning on any other OS, and the function's own
// runtime.GOOS guard means this branch is dead code there anyway.
func TestIsCrossDeviceRenameErr_WindowsErrno17(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("ERROR_NOT_SAME_DEVICE (errno 17) is a Windows-only failure shape")
	}
	err := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.Errno(17)}
	if !isCrossDeviceRenameErr(err) {
		t.Fatal("expected Windows errno 17 (ERROR_NOT_SAME_DEVICE) to be detected as cross-device")
	}
}

// TestIsCrossDeviceRenameErr_RealCrossDriveRename is the live end-to-end
// check on Windows: an actual os.Rename from one drive letter to another,
// through renameFile (not a synthetic error), must be classified as
// cross-device. Skipped when a second drive letter isn't available to test
// against (any CI runner, and any single-drive dev box).
func TestIsCrossDeviceRenameErr_RealCrossDriveRename(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive letters are a Windows concept")
	}
	other := os.Getenv("NODESWAP_TEST_OTHER_DRIVE_TEMP") // e.g. D:\tmp, set only for manual local runs
	if other == "" {
		t.Skip("NODESWAP_TEST_OTHER_DRIVE_TEMP not set — no known second drive to rename across in this environment")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(other, "nodeswap-crossdrive-test")
	defer os.Remove(dst)
	err := renameFile(src, dst)
	if err == nil {
		t.Skip("rename unexpectedly succeeded — src and the configured other-drive path are on the same volume")
	}
	if !isCrossDeviceRenameErr(err) {
		t.Fatalf("expected a real cross-drive rename failure to be classified as cross-device, got: %v", err)
	}
}

// TestIsCrossDeviceRenameErr_OrdinaryErrorIsNotCrossDevice guards the other
// direction: installNewBinary (nodeswap.go) must only take the copy fallback
// for THIS specific failure shape — an ordinary error (a missing file, a
// permission error, anything else) must never be misdetected as
// cross-device and silently routed into a copy it was never meant to take.
func TestIsCrossDeviceRenameErr_OrdinaryErrorIsNotCrossDevice(t *testing.T) {
	if isCrossDeviceRenameErr(errors.New("permission denied")) {
		t.Fatal("an ordinary error must not be misdetected as cross-device")
	}
	if isCrossDeviceRenameErr(&os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.Errno(5)}) {
		t.Fatal("an unrelated errno (5 = ERROR_ACCESS_DENIED on Windows) must not be misdetected as cross-device")
	}
}
