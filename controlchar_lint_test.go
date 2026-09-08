package main

import (
	"fmt"
	"os"
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

	var offenders []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // an unreadable path is not this test's business
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !interesting[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for i, c := range b {
			if c == '\t' || c == '\n' || c == '\r' {
				continue
			}
			if c < 0x20 || c == 0x7f {
				line := 1 + strings.Count(string(b[:i]), "\n")
				offenders = append(offenders,
					fmt.Sprintf("%s:%s contains control byte 0x%02x", filepath.ToSlash(path), strconv.Itoa(line), c))
				return nil // one report per file is enough to act on
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("stray control characters found — a `\\b` written through a double-quoted "+
			"string becomes byte 0x08 and silently voids the regex containing it:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
