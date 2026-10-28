package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestInstallersCarryNoVersionPins is the guard for ADR 0099: floors, never pins.
// The installers and the dev tooling resolve the newest upstream release at run time;
// a release tag, an exact version or a content digest written into them is a pin, and
// a pin silently freezes every install and re-hashes on every upgrade.
//
// It scans the installers (setup/install.ps1, setup/install.sh), the render runners
// and tables (render/*.mjs, render/*.json), install_*.go, scripts/openspec*,
// scripts/refresh-llamaswap-schema.sh, accelerators/** (no test files, no testdata),
// setup/hyperframes/package.json, setup/browse/pyproject.toml and the CI workflow, and
// fails, listing file:line:excerpt for every hit, on a llama.cpp or whisper.cpp build
// tag, an sd.cpp release tag, a llama-swap tag, a release-download URL, a 40- or
// 64-hex digest, an exact python requirement (==x.y), an exact x.y.z in a package.json
// dependencies map, or a go.mod `toolchain go1.` line. setup/floors.json is the one
// file allowed to carry a build number (a minimum, never a maximum); a line that names
// floors.json is skipped, and so is an action's own major tag (actions/checkout@v4,
// upstream's floating channel).
func TestInstallersCarryNoVersionPins(t *testing.T) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\bb\d{4,5}\b`),           // llama.cpp / whisper.cpp build tags
		regexp.MustCompile(`master-\d+-[0-9a-f]{7}`), // sd.cpp release tags
		regexp.MustCompile(`\bv\d{3}\b`),             // llama-swap tags
		regexp.MustCompile(`releases/download/`),     // a hard-wired asset URL
		regexp.MustCompile(`\b[0-9a-f]{64}\b`),       // sha256 digest
		regexp.MustCompile(`\b[0-9a-f]{40}\b`),       // git commit / sha1 digest
		regexp.MustCompile(`==\d+\.\d+`),             // an exact python requirement
		regexp.MustCompile(`toolchain go1\.`),        // a go.mod toolchain line
	}
	// "1.2.3" as the value of a dependency in a package.json dependencies map.
	exactDep := regexp.MustCompile(`^\s*"[^"]+"\s*:\s*"\d+\.\d+\.\d+[^"]*"\s*,?\s*$`)
	depsOpen := regexp.MustCompile(`"(dependencies|devDependencies|optionalDependencies|peerDependencies)"\s*:\s*\{`)
	actionMajor := regexp.MustCompile(`@v\d+\b`)

	files := map[string]bool{}
	add := func(rel string) { files[filepath.ToSlash(rel)] = true }
	for _, rel := range []string{
		"setup/install.ps1", "setup/install.sh",
		"scripts/refresh-llamaswap-schema.sh",
		"setup/hyperframes/package.json", "setup/browse/pyproject.toml",
		".github/workflows/ci.yml",
	} {
		add(rel)
	}
	globs := []string{"render/*.mjs", "render/*.json", "install_*.go", "scripts/openspec*"}
	for _, g := range globs {
		m, err := filepath.Glob(filepath.FromSlash(g))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range m {
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, werr error) error {
					if werr == nil && !d.IsDir() {
						add(q)
					}
					return nil
				})
				continue
			}
			add(p)
		}
	}
	_ = filepath.WalkDir("accelerators", func(q string, d fs.DirEntry, werr error) error {
		if werr == nil && !d.IsDir() {
			add(q)
		}
		return nil
	})

	skip := func(rel string) bool {
		base := filepath.Base(rel)
		switch {
		case strings.HasSuffix(base, "_test.go"), strings.HasSuffix(base, ".test.mjs"),
			strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
			base == "go.sum", base == "floors.json", strings.HasSuffix(base, "package-lock.json"):
			return true
		}
		return strings.Contains("/"+rel+"/", "/testdata/")
	}

	var names []string
	for rel := range files {
		if !skip(rel) {
			names = append(names, rel)
		}
	}
	sort.Strings(names)

	var hits []string
	for _, rel := range names {
		raw, err := os.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		isPackageJSON := filepath.Base(rel) == "package.json"
		inDeps := false
		for i, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimRight(line, "\r")
			if isPackageJSON {
				if depsOpen.MatchString(line) {
					inDeps = true
				} else if inDeps && strings.Contains(line, "}") {
					inDeps = false
				}
			}
			if strings.Contains(line, "floors.json") {
				continue
			}
			scan := actionMajor.ReplaceAllString(line, "@vN")
			excerpt := strings.TrimSpace(line)
			if len(excerpt) > 120 {
				excerpt = excerpt[:120] + "..."
			}
			if inDeps && exactDep.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s  [exact version in a dependencies map]", rel, i+1, excerpt))
				continue
			}
			for _, re := range patterns {
				if re.MatchString(scan) {
					hits = append(hits, fmt.Sprintf("%s:%d: %s  [%s]", rel, i+1, excerpt, re))
					break
				}
			}
		}
	}
	if len(hits) > 0 {
		t.Errorf("version pins in the installers and dev tooling (ADR 0099: floors never pins; a minimum belongs in setup/floors.json):\n%s", strings.Join(hits, "\n"))
	}
}
