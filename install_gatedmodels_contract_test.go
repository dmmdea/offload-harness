package main

// Contract-drift guards for the gated-model warning, closing two gaps a
// fresh-context review found in 0.73.0's version of this coverage.
//
// GAP 1 (the filename contract). install_gatedmodels_test.go duplicates the four
// gated weight filenames as consts and its header claims "if a rename lands on one
// side only, this test fails rather than silently checking a path nothing downloads
// to." That guarantee did NOT hold: both sides of every comparison were Go literals
// in this repo's Go code (the test consts vs install_render.go), and nothing read
// the REAL contracts. Verified by the reviewer: renaming a weight in two templates
// only left the whole suite green. The failure that gap allows is precisely the one
// warnMissingGatedModels exists to catch — `install render` stats the OLD filename,
// finds it (install.ps1 still downloads it), prints no warning, and emits a
// llama-swap entry pointing at a file nothing fetches. All four names match today,
// so this is a guarantee gap rather than live drift.
//
// GAP 2 (the call site). Both 0.73.0 refactors moved their body onto an injectable
// *To variant and left the PRODUCTION wiring unpinned: deleting
// warnMissingGatedModels(...) from install_render.go, or warnImageGenBindingTraps(c)
// from internal/config/config.go, left the entire suite green. Finding I-2 was "this
// function has zero tests"; covering the body while the call can vanish silently
// reproduces the finding one layer out.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gatedWeightFilenames are the same four literals install_render.go checks. They
// are asserted against the REAL contracts below rather than against each other.
var gatedWeightFilenames = []string{
	weight26B,
	weightQ38,
	mmprojQ38,
	weightQ354B,
}

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestGatedWeightFilenamesMatchTheShippedTemplates pins each filename against the
// serving templates — the contract llama-server actually reads. A re-quantised
// weight renamed in the template but not in install_render.go now fails here.
func TestGatedWeightFilenamesMatchTheShippedTemplates(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("setup", "templates", "llama-swap.*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 5 {
		t.Fatalf("expected the full shipped template set, globbed %d: %v", len(files), files)
	}
	var all strings.Builder
	for _, f := range files {
		all.WriteString(repoFile(t, f))
		all.WriteString("\n")
	}
	corpus := all.String()

	for _, name := range gatedWeightFilenames {
		if !strings.Contains(corpus, name) {
			t.Errorf("gated weight %q appears in NO serving template. Either the template was renamed without updating install_render.go (in which case `install render` will stat a filename nothing downloads to, emit no warning, and produce a llama-swap entry that fails only when called) or the checker names a weight no template serves", name)
		}
	}
}

// TestGatedWeightFilenamesMatchTheInstaller pins the same names against
// install.ps1's $PINNED table — the contract that decides what lands on disk. The
// warning compares on-disk presence, so a name the installer does not fetch can
// never be satisfied.
func TestGatedWeightFilenamesMatchTheInstaller(t *testing.T) {
	ps1 := repoFile(t, filepath.Join("setup", "install.ps1"))
	for _, name := range gatedWeightFilenames {
		if !strings.Contains(ps1, name) {
			t.Errorf("gated weight %q is not named in setup/install.ps1, so nothing downloads it — warnMissingGatedModels would warn forever and `install render` would emit an entry that can never resolve", name)
		}
	}
}

// TestGatedModelWarningIsWiredIntoProduction pins the CALL, not the body. A *To
// refactor makes the body testable and the call invisible; without this, deleting
// the production call leaves every test green while the warning stops existing.
func TestGatedModelWarningIsWiredIntoProduction(t *testing.T) {
	src := repoFile(t, "install_render.go")
	if !strings.Contains(src, "warnMissingGatedModels(") {
		t.Error("install_render.go no longer CALLS warnMissingGatedModels — its body is still tested, so the suite stays green while the warning never fires. That is the silent-capability-loss shape the warning itself exists to catch")
	}
	// The wrapper must keep delegating to the injectable variant, or the tested
	// body and the shipped body are two different functions.
	if !strings.Contains(src, "warnMissingGatedModelsTo(") {
		t.Error("install_render.go no longer delegates to warnMissingGatedModelsTo — the tests would then cover a body production does not run")
	}
}

// TestImageGenBindingTrapsAreWiredIntoProduction is the same pin for the config
// warner, whose body moved to warnMediaGenBindingTrapsTo in 0.73.0.
func TestImageGenBindingTrapsAreWiredIntoProduction(t *testing.T) {
	src := repoFile(t, filepath.Join("internal", "config", "config.go"))
	if !strings.Contains(src, "warnImageGenBindingTraps(c)") {
		t.Error("internal/config/config.go no longer CALLS warnImageGenBindingTraps at load — the pooled-seat warnings (image AND video) would silently stop firing with every test still green")
	}
	if !strings.Contains(src, "warnMediaGenBindingTrapsTo(") {
		t.Error("internal/config/config.go no longer delegates to warnMediaGenBindingTrapsTo — the tests would then cover a body production does not run")
	}
}
