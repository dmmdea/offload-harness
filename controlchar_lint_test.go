package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A C0 control character inside a source or config file is never intentional here,
// and twice now one has silently disabled a gate.
//
// The mechanism: someone writes a regex word boundary — `\b` — and a patch script
// with a DOUBLE-quoted Python/JS string interprets it as an escape, writing byte 0x08
// into the file instead of the two characters backslash-b. The regex then matches a
// literal backspace, which no config ever contains, so the assertion becomes
// unfalsifiable and reports PASS forever.
//
// It has bitten this repo at least three times, all recorded in CHANGELOG.md:
//   - `\bnone\b` became `<0x08>none<0x08>` in a tool-refusal matcher;
//   - `\bq354\b` became `<0x08>q354<0x08>` in setup/render.tests.ps1;
//   - `CUDA_VISIBLE_DEVICES=1\b` became `CUDA_VISIBLE_DEVICES=1<0x08>` in the ONE
//     assertion guarding the triple-Blackwell placement law ("no seat may be pinned
//     to the display card"). It passed on every run while the shipped tier pinned its
//     vision seat to exactly that card.
//
// Tabs, newlines and carriage returns are legitimate; nothing else in this class is.
func TestNoStrayControlCharacters(t *testing.T) {
	// Extensions whose contents drive behaviour or document it. Binary assets and
	// vendored trees are excluded by the walk below.
	interesting := map[string]bool{
		".go": true, ".ps1": true, ".psm1": true, ".sh": true, ".bash": true,
		".yaml": true, ".yml": true, ".json": true, ".md": true, ".txt": true,
		".py": true, ".mjs": true, ".js": true, ".toml": true,
	}
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "build": true,
		"testdata": true, "dist": true,
	}

	// Lint the files git TRACKS, not everything on disk.
	//
	// The walk used to descend into anything not on a hardcoded skip list, which meant a
	// gitignored tree in the working directory failed the whole suite: a VoiceStudio
	// `.tts-venv/` in the repo root put vendored gradio and transformers files in front
	// of the lint, and `go test ./...` — the repo's own documented gate — reported FAIL
	// on a clean checkout of main for a reason that had nothing to do with the repo.
	//
	// Tracked-only is also the honest scope. This test exists to catch a `\b` mangled
	// into byte 0x08 in OUR sources; a control byte inside a vendored minified bundle is
	// normal and nothing here would act on it. Files git does not track cannot carry that
	// defect into a commit.
	//
	// If git is unavailable (a source tarball), fall back to the walk so the gate still
	// runs rather than silently passing on an empty file list — a lint that quietly
	// checks nothing is worse than one that over-reports.
	files, viaGit := trackedFiles(t)

	var offenders []string
	check := func(path string) {
		if !interesting[strings.ToLower(filepath.Ext(path))] {
			return
		}
		for _, part := range strings.Split(filepath.ToSlash(path), "/") {
			if skipDirs[part] {
				return
			}
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		for i, c := range b {
			if c == '\t' || c == '\n' || c == '\r' {
				continue
			}
			if c < 0x20 || c == 0x7f {
				line := 1 + strings.Count(string(b[:i]), "\n")
				offenders = append(offenders,
					fmt.Sprintf("%s:%s contains control byte 0x%02x", filepath.ToSlash(path), strconv.Itoa(line), c))
				return // one report per file is enough to act on
			}
		}
	}

	for _, f := range files {
		check(f)
	}
	// A lint that inspected nothing must say so rather than report a clean bill.
	if len(files) == 0 {
		t.Fatal("no files to lint — the gate went blind")
	}
	t.Logf("linted %d files (source: %s)", len(files),
		map[bool]string{true: "git ls-files", false: "filesystem walk"}[viaGit])
	if len(offenders) > 0 {
		t.Errorf("stray control characters found — a `\\b` written through a double-quoted "+
			"string becomes byte 0x08 and silently voids the regex containing it:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// trackedFiles lists the paths git tracks, falling back to a filesystem walk when git
// is unavailable (a source tarball, or a checkout with no .git).
//
// The bool says WHICH source was used, and the caller logs it. A fallback that looked
// identical to the real thing would hide the case where the lint silently widened back
// to everything on disk — which is the behaviour this replaced.
func trackedFiles(t *testing.T) ([]string, bool) {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err == nil {
		var files []string
		for _, p := range strings.Split(string(out), "\x00") {
			if p == "" {
				continue
			}
			if fi, err := os.Stat(p); err != nil || fi.IsDir() {
				continue // a deleted-but-still-indexed path, or a submodule
			}
			files = append(files, filepath.FromSlash(p))
		}
		if len(files) > 0 {
			return files, true
		}
	}
	var files []string
	_ = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files, false
}
