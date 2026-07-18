package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocsLint enforces the structural rules of the repo-local documentation
// system (see docs/README.md and docs/STYLE.md): the scaffold files exist,
// relative Markdown links resolve on disk, ADRs carry schema-valid frontmatter,
// and system/flow docs keep the sections agents navigate by.
//
// It checks structure, not meaning. Keeping docs factually in step with the
// code is a review duty (CONTRIBUTING.md), not something a test can decide.
func TestDocsLint(t *testing.T) {
	required := []string{
		"AGENTS.md",
		"docs/README.md",
		"docs/STYLE.md",
		"docs/glossary.md",
		"docs/templates/system.md",
		"docs/templates/flow.md",
		"docs/templates/adr.md",
		"docs/architecture/README.md",
		"docs/architecture/decisions/README.md",
	}
	for _, p := range required {
		if _, err := os.Stat(filepath.FromSlash(p)); err != nil {
			t.Errorf("required doc missing: %s", p)
		}
	}

	// Markdown inline links: ](target) or ](target#anchor). Reference-style
	// links and bare autolinks are intentionally out of scope.
	linkRe := regexp.MustCompile(`\]\(([^)#]+)(#[^)]*)?\)`)
	// \r?\n throughout so the gate is line-ending agnostic: a Windows checkout
	// with autocrlf rewrites these files to CRLF, and an LF-only anchor would
	// spuriously fail every ADR for that contributor.
	adrFrontRe := regexp.MustCompile(`(?s)\A---\r?\nstatus: (Proposed|Accepted|Superseded|Deprecated|Rejected)\r?\ndate: "\d{4}-\d{2}-\d{2}"\r?\n(superseded_by: \S+\r?\n)?---\r?\n`)

	err := filepath.Walk("docs", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Errorf("%s: %v", path, readErr)
			return nil
		}
		content := string(raw)
		slash := filepath.ToSlash(path)

		// Link checking covers the durable documentation surface only. Two
		// exemptions, both deliberate:
		//   - docs/templates/ carries placeholder links by design; templates are
		//     copied and filled in, not followed.
		//   - docs/superpowers/ is the dated process archive (specs, plans,
		//     evidence). Evidence ledgers are immutable records that may cite
		//     paths from an earlier checkout location, and plans legitimately
		//     reference files that do not exist until the plan is executed.
		//     Rewriting them to satisfy a linter would destroy the record.
		isTemplate := strings.HasPrefix(slash, "docs/templates/")
		isArchive := strings.HasPrefix(slash, "docs/superpowers/")

		// 1. Every relative link must resolve on disk.
		if !isTemplate && !isArchive {
			for _, m := range linkRe.FindAllStringSubmatch(content, -1) {
				target := strings.TrimSpace(m[1])
				if target == "" ||
					strings.Contains(target, "://") ||
					strings.HasPrefix(target, "mailto:") ||
					strings.HasPrefix(target, "<") {
					continue
				}
				resolved := filepath.Join(filepath.Dir(path), filepath.FromSlash(target))
				if _, statErr := os.Stat(resolved); statErr != nil {
					t.Errorf("%s: broken relative link %q", path, target)
				}
			}
		}

		// 2. ADRs (everything in decisions/ except its README) need valid frontmatter.
		if strings.HasPrefix(slash, "docs/architecture/decisions/") && filepath.Base(path) != "README.md" {
			if !adrFrontRe.MatchString(content) {
				t.Errorf("%s: missing or invalid ADR frontmatter (want status/date[/superseded_by])", path)
			}
			if strings.Contains(content, "status: Superseded") && !strings.Contains(content, "superseded_by:") {
				t.Errorf("%s: Superseded ADR missing superseded_by", path)
			}
		}

		// 3. System and flow docs must keep their navigational sections.
		if strings.HasPrefix(slash, "docs/systems/") || strings.HasPrefix(slash, "docs/flows/") {
			for _, h := range []string{"## Purpose", "## Source map"} {
				if !strings.Contains(content, h) {
					t.Errorf("%s: missing required section %q", path, h)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking docs/: %v", err)
	}
}
