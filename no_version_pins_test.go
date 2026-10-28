package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestInstallersCarryNoVersionPins is the guard for ADR 0099: floors, never pins.
// The installers resolve the newest upstream release at run time; a release tag
// or a content digest written into them is a pin, and a pin silently freezes
// every install and re-hashes on every upgrade.
//
// It scans setup/install.ps1 and setup/install.sh and fails, listing
// file:line:excerpt for every hit, on a llama.cpp or whisper.cpp build tag, an
// sd.cpp release tag, a llama-swap tag, a release-download URL, or a 40- or
// 64-hex digest. setup/floors.json is the one file allowed to carry a build
// number (a minimum, never a maximum); a line that names floors.json is skipped.
//
// Scope is the two installers today. PR C widens it to render/, install_*.go,
// scripts/openspec* and accelerators/** (non-test, non-testdata).
func TestInstallersCarryNoVersionPins(t *testing.T) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\bb\d{4,5}\b`),           // llama.cpp / whisper.cpp build tags
		regexp.MustCompile(`master-\d+-[0-9a-f]{7}`), // sd.cpp release tags
		regexp.MustCompile(`\bv\d{3}\b`),             // llama-swap tags
		regexp.MustCompile(`releases/download/`),     // a hard-wired asset URL
		regexp.MustCompile(`\b[0-9a-f]{64}\b`),       // sha256 digest
		regexp.MustCompile(`\b[0-9a-f]{40}\b`),       // git commit / sha1 digest
	}
	var hits []string
	for _, rel := range []string{"setup/install.ps1", "setup/install.sh"} {
		raw, err := os.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "floors.json") {
				continue
			}
			for _, re := range patterns {
				if loc := re.FindStringIndex(line); loc != nil {
					excerpt := strings.TrimSpace(line)
					if len(excerpt) > 120 {
						excerpt = excerpt[:120] + "..."
					}
					hits = append(hits, fmt.Sprintf("%s:%d: %s  [%s]", rel, i+1, excerpt, re))
					break
				}
			}
		}
	}
	if len(hits) > 0 {
		t.Errorf("version pins in the installers (ADR 0099: floors never pins; a minimum belongs in setup/floors.json):\n%s", strings.Join(hits, "\n"))
	}
}
