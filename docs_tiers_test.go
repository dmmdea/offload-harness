package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/tierdocs"
)

// TestTierDocsAreCurrent: docs/tiers/ is GENERATED from setup/templates/profiles.json,
// so a tier added or retuned in the installer's table cannot ship with a page that
// still describes the old one. This is LO-17's config.example.json lesson applied to
// documentation — the drift it caught was prose that stayed confidently wrong for
// months.
//
// Line endings are normalized: a Windows checkout without .gitattributes rewrites the
// working tree to CRLF, which would otherwise fail this on one contributor's machine
// and pass on another's.
func TestTierDocsAreCurrent(t *testing.T) {
	want, err := tierdocs.Render(".")
	if err != nil {
		t.Fatalf("rendering tier docs: %v", err)
	}
	if len(want) < 2 {
		t.Fatalf("only %d page(s) rendered — the profile table did not parse", len(want))
	}

	var stale []string
	for path, body := range want {
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			stale = append(stale, path+" (missing)")
			continue
		}
		if norm(string(got)) != norm(body) {
			stale = append(stale, path)
		}
	}

	// A page for a tier that no longer exists is drift in the other direction.
	entries, err := os.ReadDir(filepath.Join("docs", "tiers"))
	if err != nil {
		t.Fatalf("reading docs/tiers: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join("docs", "tiers", e.Name())
		if _, ok := want[p]; !ok {
			stale = append(stale, p+" (orphan: no such tier in profiles.json)")
		}
	}

	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("tier docs are stale — run `go generate ./...`:\n  %s", strings.Join(stale, "\n  "))
	}
}

func norm(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
