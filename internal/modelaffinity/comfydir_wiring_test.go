package modelaffinity

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// comfy_dir is where the launch markers live. The lease readers tie a ComfyUI instance to the lease it was launched
// under through them (GPU-I3), and config.Load already arms this package with the directory, so arming it here is what
// puts the same directory where the lease readers look: one call, one directory, no second wiring in config.
func TestSetComfyDirArmsTheDirectoryTheLeaseReadersLookInForMarkers(t *testing.T) {
	t.Cleanup(func() { SetComfyDir(""); gpulease.SetComfyDir("") })
	dir := t.TempDir()
	SetComfyDir("  " + dir + "  ")
	if got := gpulease.ComfyDir(); got != dir {
		t.Fatalf("the lease readers look for markers in %q, want the armed %q", got, dir)
	}
	SetComfyDir("")
	if got := gpulease.ComfyDir(); got != "" {
		t.Fatalf("an empty directory disarms the marker evidence, got %q", got)
	}
}
