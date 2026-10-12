package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// comfy_dir is where the launch markers live. The lease readers use them to tie a ComfyUI
// instance to the lease it was launched under (plan P10), so Load installs the directory where
// every reader sees it, like the orphan grace and the lease directory.
func TestLoadInstallsTheComfyDirectoryTheLeaseReadersLookInForMarkers(t *testing.T) {
	t.Cleanup(func() { gpulease.SetComfyDir("") })
	dir := t.TempDir()
	comfy := filepath.Join(dir, "comfy")
	p := filepath.Join(dir, "config.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"state_dir": "` + filepath.ToSlash(dir) + `", "comfy_dir": "` + filepath.ToSlash(comfy) + `"}`)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	if got := gpulease.ComfyDir(); got != filepath.ToSlash(comfy) && got != comfy {
		t.Fatalf("the lease readers look for markers in %q, want the configured %q", got, comfy)
	}
	// A config that does not set it restores the default: the directory is a property of the
	// config in force, not of whichever loaded first.
	write(`{"state_dir": "` + filepath.ToSlash(dir) + `"}`)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	if got := gpulease.ComfyDir(); got != Default().ComfyDir {
		t.Fatalf("an unset comfy_dir must restore the default %q, got %q", Default().ComfyDir, got)
	}
}
