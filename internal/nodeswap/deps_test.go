package nodeswap

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
