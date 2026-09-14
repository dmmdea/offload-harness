package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoConflictMarkersInTrackedTree: a merge resolved by a script that failed
// half-way committed `<<<<<<< HEAD` into CHANGELOG.md on 2026-09-14 and every
// existing lint let it through. Markers at line start are never legitimate
// content in this tree (a doc that must SHOW one indents it).
func TestNoConflictMarkersInTrackedTree(t *testing.T) {
	files, _ := trackedFiles(t)
	marker := regexp.MustCompile(`(?m)^(<<<<<<< |=======$|>>>>>>> )`)
	var hits []string
	for _, native := range files {
		path := filepath.ToSlash(native)
		if strings.HasSuffix(path, "_lint_test.go") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			continue
		}
		if loc := marker.FindIndex(b); loc != nil {
			hits = append(hits, fmt.Sprintf("%s:%d", path, bytes.Count(b[:loc[0]], []byte("\n"))+1))
		}
	}
	if len(hits) > 0 {
		t.Fatalf("conflict markers committed:\n  %s", strings.Join(hits, "\n  "))
	}
}
