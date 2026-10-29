package main

import (
	"encoding/json"
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
// floors.json is skipped, and so is an action's own major tag (actions/checkout@v7,
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

// trackedSlashFiles is the git-tracked file list (trackedFiles in controlchar_lint_test.go)
// as slash paths. A tree with no git (a source tarball) skips the two tracked-file guards
// below: they guard the repository, not a build.
func trackedSlashFiles(t *testing.T) []string {
	t.Helper()
	files, fromGit := trackedFiles(t)
	if !fromGit {
		t.Skip("git ls-files unavailable: the tracked-file guards need a git checkout")
	}
	out := make([]string, 0, len(files))
	for _, p := range files {
		out = append(out, filepath.ToSlash(p))
	}
	return out
}

// TestTrackedPackageJSONCarryNoVersionPins closes the audit gap that let a ~ and a ^ range
// into integrations/opencode/package.json: the installer scan above only looks at three
// manifests and only at a bare x.y.z. Here EVERY tracked package.json is read as JSON and
// each value in dependencies, devDependencies, peerDependencies and optionalDependencies
// must be "latest", "*", "" or a non-registry spec (a path, a link, a workspace, a git or
// http(s) URL, a github: shorthand). An exact version, a ~ or ^ range, a comparator range
// and an x-range are refused. "engines" may keep a ">=" floor and is not read.
func TestTrackedPackageJSONCarryNoVersionPins(t *testing.T) {
	maps := []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"}
	nonRegistry := []string{"file:", "link:", "workspace:", "git+", "http:", "https:", "github:"}
	// A version-like value: optional operators, then a digit (1.2.3, ~1.2, ^1, >=1, v1) or an x-range.
	versionLike := regexp.MustCompile(`^[\s~^<>=v]*(\d|[xX](\.|$))`)

	var hits []string
	for _, rel := range trackedSlashFiles(t) {
		if filepath.Base(rel) != "package.json" || strings.Contains("/"+rel+"/", "/testdata/") {
			continue
		}
		raw, err := os.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: not valid JSON: %v", rel, err)
		}
		lines := strings.Split(string(raw), "\n")
		for _, mapName := range maps {
			body, ok := doc[mapName]
			if !ok {
				continue
			}
			var deps map[string]string
			if err := json.Unmarshal(body, &deps); err != nil {
				t.Fatalf("%s: %s is not a map of strings: %v", rel, mapName, err)
			}
			names := make([]string, 0, len(deps))
			for name := range deps {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				spec := strings.TrimSpace(deps[name])
				if spec == "" || spec == "latest" || spec == "*" {
					continue
				}
				isURL := false
				for _, p := range nonRegistry {
					if strings.HasPrefix(spec, p) {
						isURL = true
						break
					}
				}
				if isURL || !(versionLike.MatchString(spec) || strings.HasPrefix(spec, "npm:")) {
					continue // a dist-tag such as "next" is the publisher's own channel
				}
				lineNo := 1
				for i, l := range lines {
					if strings.Contains(l, `"`+name+`"`) {
						lineNo = i + 1
						break
					}
				}
				hits = append(hits, fmt.Sprintf("%s:%d: %q: %q  [%s: a version or range, not \"latest\"]", rel, lineNo, name, spec, mapName))
			}
		}
	}
	if len(hits) > 0 {
		t.Errorf("version pins in a tracked package.json (ADR 0099: floors never pins; a minimum belongs in setup/floors.json):\n%s", strings.Join(hits, "\n"))
	}
}

// TestInstallCommandsCarryNoVersionPins refuses an install command that names a version, in
// any tracked live text: *.md, *.mjs, *.js, non-test *.go, *.ps1, *.sh, *.py. The commands:
// pip install / uv pip install / uv add with <pkg>==<digits>; npm i|install|add <pkg>@<digit>;
// npx [--yes] <pkg>@<digit>; git clone with --branch or -b v<digit>; gh release download
// b<digits> or v<digit>. History (accepted ADRs, the changelog and its fragments, the frozen
// roadmap, testdata, runs, test files, this file) records what WAS installed and is skipped;
// a hit that is evidence rather than an instruction goes in installCmdAllowed with a reason.
func TestInstallCommandsCarryNoVersionPins(t *testing.T) {
	cmds := []*regexp.Regexp{
		regexp.MustCompile(`\b(?:pip3?\s+install|uv\s+pip\s+install|uv\s+add)\b.*[\w\]]==\d`),
		regexp.MustCompile(`\bnpm\s+(?:i|install|add)\b.*?\s(?:@[\w.-]+/)?[\w.-]+@\d`),
		regexp.MustCompile(`\bnpx\s+(?:(?:--yes|-y)\s+)*(?:@[\w.-]+/)?[\w.-]+@\d`),
		regexp.MustCompile(`\bgit\s+clone\b.*(?:--branch|-b)[ =]v\d`),
		regexp.MustCompile(`\bgh\s+release\s+download\s+(?:b\d+|v\d)`),
	}
	// {path, text the line must contain, why it is evidence or history and not an instruction}
	installCmdAllowed := [][3]string{}

	isHistory := func(rel string) bool {
		switch {
		case strings.HasPrefix(rel, "docs/architecture/decisions/"), strings.HasPrefix(rel, "changes/"),
			strings.HasPrefix(rel, "docs/changelog/"), rel == "CHANGELOG.md", rel == "no_version_pins_test.go":
			return true
		case strings.HasPrefix(rel, "docs/ROADMAP-") && strings.Contains(rel, "frozen"):
			return true
		}
		base := filepath.Base(rel)
		return strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, ".test.mjs") ||
			strings.HasSuffix(base, ".test.js") || strings.HasPrefix(base, "test_") ||
			strings.Contains("/"+rel+"/", "/testdata/")
	}
	liveExt := map[string]bool{".md": true, ".mjs": true, ".js": true, ".go": true, ".ps1": true, ".sh": true, ".py": true}

	var hits []string
	for _, rel := range trackedSlashFiles(t) {
		if !liveExt[filepath.Ext(rel)] || isHistory(rel) {
			continue
		}
		raw, err := os.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimRight(line, "\r")
			matched := false
			for _, re := range cmds {
				if re.MatchString(line) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			allowed := false
			for _, a := range installCmdAllowed {
				if a[0] == rel && strings.Contains(line, a[1]) {
					allowed = true
					break
				}
			}
			if allowed {
				continue
			}
			excerpt := strings.TrimSpace(line)
			if len(excerpt) > 120 {
				excerpt = excerpt[:120] + "..."
			}
			hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, i+1, excerpt))
		}
	}
	if len(hits) > 0 {
		t.Errorf("install commands that name a version (ADR 0099: floors never pins; install the newest, keep a minimum in setup/floors.json):\n%s", strings.Join(hits, "\n"))
	}
}
