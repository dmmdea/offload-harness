package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// gpu_orphan_grace_min is how long an attended lease's owner may be gone before the lease
// reads as orphaned. Unset is 15 minutes; Load installs it where every reader sees it.
func TestLoadInstallsTheOrphanGrace(t *testing.T) {
	t.Cleanup(func() { gpulease.SetDefaultOrphanGrace(0) })
	if Default().GPUOrphanGraceMin != 0 || Default().GPUOrphanGrace() != gpulease.DefaultOrphanGrace {
		t.Fatalf("unset must mean the 15 minute default: %d %s", Default().GPUOrphanGraceMin, Default().GPUOrphanGrace())
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"state_dir": "` + filepath.ToSlash(dir) + `", "gpu_orphan_grace_min": 45}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.GPUOrphanGrace() != 45*time.Minute || gpulease.OrphanGrace() != 45*time.Minute {
		t.Fatalf("a configured grace must reach the lease readers: cfg %s, installed %s", c.GPUOrphanGrace(), gpulease.OrphanGrace())
	}
	// A later load of a config that does not set the key restores the default (the grace
	// is a property of the config in force, not of whichever loaded first).
	write(`{"state_dir": "` + filepath.ToSlash(dir) + `"}`)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	if gpulease.OrphanGrace() != gpulease.DefaultOrphanGrace {
		t.Fatalf("an unset key must restore the default, installed %s", gpulease.OrphanGrace())
	}
	// A negative value is nonsense and falls back to the default rather than disabling the
	// signal (nothing here acts on it; a bad value must not silence the surface).
	write(`{"state_dir": "` + filepath.ToSlash(dir) + `", "gpu_orphan_grace_min": -5}`)
	c, _ = Load(p)
	if c.GPUOrphanGrace() != gpulease.DefaultOrphanGrace {
		t.Fatalf("negative: %s", c.GPUOrphanGrace())
	}
}
