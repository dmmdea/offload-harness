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
	"go/ast"
	"go/parser"
	"go/token"
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
	weightQ359B,
}

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// gatedModelWeights maps each gated llama-swap model id to the weight filenames
// install_render.go stats for it. PER-MODEL, because the check has to be
// per-template: install_render.go's templateFor() selects exactly ONE embedded
// template per (goos, backend), so the contract is "every template that DEFINES
// this model must reference its weights", not "some template somewhere does".
var gatedModelWeights = map[string][]string{
	"gemma4-26b-a4b":   {weight26B},
	"qwen3.8-27b":      {weightQ38, mmprojQ38},
	"qwen3.5-4b-agent": {weightQ354B},
	"qwen3.5-9b-agent": {weightQ359B},
}

// TestGatedWeightFilenamesMatchTheShippedTemplates pins each filename against the
// serving templates — the contract llama-server actually reads.
//
// The first version of this test concatenated all seven templates into ONE corpus
// and asserted the name appeared somewhere in it, which meant renaming a weight in
// a SUBSET of templates stayed green — while the header claimed that exact mutation
// was closed. Since Q38/mmproj/Q354B appear in only 2 of 7 templates, the corpus
// form was near-useless. Now asserted per template that defines the model.
func TestGatedWeightFilenamesMatchTheShippedTemplates(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("setup", "templates", "llama-swap.*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 5 {
		t.Fatalf("expected the full shipped template set, globbed %d: %v", len(files), files)
	}

	checked := 0
	for _, f := range files {
		tmpl := repoFile(t, f)
		for model, weights := range gatedModelWeights {
			// Only templates that DEFINE the model owe its weights. The pattern
			// mirrors servingtmpl.definesModel: a two-space-indented bare key.
			if !strings.Contains(tmpl, "\n  "+model+":") {
				continue
			}
			for _, name := range weights {
				checked++
				if !strings.Contains(tmpl, name) {
					t.Errorf("%s DEFINES model %q but never references its weight %q — `install render` would stat a filename this template does not serve, print no warning, and emit an entry that fails only when called",
						filepath.Base(f), model, name)
				}
			}
		}
	}
	// Guard the guard: zero checks means every `definesModel` probe missed and the
	// loop asserted nothing.
	if checked == 0 {
		t.Fatal("no template/model pair was checked — the model-id patterns no longer match any template, so this test proves nothing")
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

// callsFunc reports whether goFile contains a real CALL to fn — parsed from the
// AST, not grepped.
//
// Text matching cannot do this job, and two successive attempts proved it:
//   - `strings.Contains(src, "fn(")` is satisfied by the func DEFINITION.
//   - the full argument list is satisfied by a COMMENTED-OUT call (measured: the
//     mutation `// MUTATION DELETED CALL: warnMissingGatedModels(...)` left the
//     text-based pin green, because the comment still contains the text).
//
// The AST excludes comments and string literals by construction, so this is the
// first version of the pin that can actually fail.
func callsFunc(t *testing.T, goFile, fn string) bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, goFile, nil, 0) // 0 = drop comments
	if err != nil {
		t.Fatalf("parse %s: %v", goFile, err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == fn {
			found = true
		}
		return true
	})
	return found
}

// TestGatedModelWarningIsWiredIntoProduction pins the CALL, not the body. A *To
// refactor makes the body testable and the call invisible; without this, deleting
// the production call leaves every test green while the warning stops existing.
func TestGatedModelWarningIsWiredIntoProduction(t *testing.T) {
	if !callsFunc(t, "install_render.go", "warnMissingGatedModels") {
		t.Error("install_render.go no longer CALLS warnMissingGatedModels — its body is still tested, so the suite stays green while the warning never fires. That is the silent-capability-loss shape the warning itself exists to catch")
	}
	// The wrapper must keep delegating to the injectable variant, or the tested
	// body and the shipped body are two different functions.
	if !callsFunc(t, "install_render.go", "warnMissingGatedModelsTo") {
		t.Error("install_render.go no longer delegates to warnMissingGatedModelsTo — the tests would then cover a body production does not run")
	}
}

// TestImageGenBindingTrapsAreWiredIntoProduction is the same pin for the config
// warner, whose body moved to warnMediaGenBindingTrapsTo in 0.73.0.
func TestImageGenBindingTrapsAreWiredIntoProduction(t *testing.T) {
	cfgFile := filepath.Join("internal", "config", "config.go")
	if !callsFunc(t, cfgFile, "warnImageGenBindingTraps") {
		t.Error("internal/config/config.go no longer CALLS warnImageGenBindingTraps at load — the pooled-seat warnings (image AND video) would silently stop firing with every test still green")
	}
	if !callsFunc(t, cfgFile, "warnMediaGenBindingTrapsTo") {
		t.Error("internal/config/config.go no longer delegates to warnMediaGenBindingTrapsTo — the tests would then cover a body production does not run")
	}
}
