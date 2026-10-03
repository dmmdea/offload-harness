package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// gpu_max_term_min and gpu_max_total_min bound a lease's renewal term and its total life
// (plan P9). Unset is 6 h and 48 h; Load installs them where every acquirer and every holder's
// tick sees them, the way gpu_orphan_grace_min is installed.
func TestLoadInstallsTheTermLimits(t *testing.T) {
	t.Cleanup(func() { gpulease.SetDefaultTerms(0, 0) })
	d := Default()
	if d.GPUMaxTermMin != 0 || d.GPUMaxTotalMin != 0 || d.GPUMaxTerm() != gpulease.DefaultMaxTerm || d.GPUMaxTotal() != gpulease.DefaultMaxTotal {
		t.Fatalf("unset must mean 6 h and 48 h: %d %d %s %s", d.GPUMaxTermMin, d.GPUMaxTotalMin, d.GPUMaxTerm(), d.GPUMaxTotal())
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"state_dir": "` + filepath.ToSlash(dir) + `", "gpu_max_term_min": 120, "gpu_max_total_min": 600}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.GPUMaxTerm() != 2*time.Hour || c.GPUMaxTotal() != 10*time.Hour {
		t.Fatalf("configured limits: %s %s", c.GPUMaxTerm(), c.GPUMaxTotal())
	}
	if gpulease.MaxTerm() != 2*time.Hour || gpulease.MaxTotal() != 10*time.Hour {
		t.Fatalf("configured limits must reach the lease library: %s %s", gpulease.MaxTerm(), gpulease.MaxTotal())
	}
	// A later load of a config that sets neither restores the defaults.
	write(`{"state_dir": "` + filepath.ToSlash(dir) + `"}`)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	if gpulease.MaxTerm() != gpulease.DefaultMaxTerm || gpulease.MaxTotal() != gpulease.DefaultMaxTotal {
		t.Fatalf("an unset key must restore the default: %s %s", gpulease.MaxTerm(), gpulease.MaxTotal())
	}
	// A negative value is nonsense and falls back rather than disabling the bound.
	write(`{"state_dir": "` + filepath.ToSlash(dir) + `", "gpu_max_term_min": -5, "gpu_max_total_min": -1}`)
	c, _ = Load(p)
	if c.GPUMaxTerm() != gpulease.DefaultMaxTerm || c.GPUMaxTotal() != gpulease.DefaultMaxTotal {
		t.Fatalf("negative: %s %s", c.GPUMaxTerm(), c.GPUMaxTotal())
	}
}
