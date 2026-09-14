package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestTrackedTreeCarriesNoOperatorIdentity is the repo-side half of the leak
// scan (the other half is the pre-push hook on the operator's machine, which
// CI never sees). This repository is PUBLIC. It ships an operator's tooling, so
// it keeps drifting toward the operator's machines: a home-directory path in a
// plan, a drive layout in a doc comment, a real tailnet address in a fixture, a
// second GitHub account's handle in vendored metadata. Each of those is a
// disclosure the moment it is pushed, and each one already happened at least
// once (2026-08: user paths; 2026-09-02: a pool path in a gate fixture;
// 2026-09-14: this test's first run found nine files).
//
// The rules name SHAPES, never the values they protect — a test that spelled
// out the username or the tailnet address would be the leak. Fixtures use the
// documentation forms the allowlists admit: `C:/Users/<user>/…`, `/home/<user>/…`,
// `D:/x/…`, tailnet examples in the lowest /22 of the CGNAT block (the pre-push
// scanner reads ANY such literal on an added line as a node: split the literal),
// `X:\My Drive\…` only in tests of the sync-root refusal.
func TestTrackedTreeCarriesNoOperatorIdentity(t *testing.T) {
	files, fromGit := trackedFiles(t)
	if !fromGit {
		t.Log("git ls-files unavailable: scanning the working tree")
	}
	type rule struct {
		name  string
		re    *regexp.Regexp
		allow func(path, match string) bool
	}
	placeholderUser := regexp.MustCompile(`(?i)^([a-z]|you|me|example|someone|user|operator|…|\.\.\.|<user>|<u>|<you>|<me>|<username>|%USERNAME%|\$env:USERNAME|\{user\}|__USER__\}?)$`)
	rules := []rule{
		{
			// A Windows profile path names an account on someone's machine.
			name: "windows profile path",
			re:   regexp.MustCompile(`[A-Za-z]:[\\/]+Users[\\/]+([^\\/"'\s\x60)]+)`),
			allow: func(_ string, m string) bool {
				sub := regexp.MustCompile(`[A-Za-z]:[\\/]+Users[\\/]+([^\\/"'\s\x60)]+)`).FindStringSubmatch(m)
				return len(sub) == 2 && placeholderUser.MatchString(sub[1])
			},
		},
		{
			// A Linux home path, same reason.
			name: "linux home path",
			re:   regexp.MustCompile(`/home/([A-Za-z0-9._<>{}$%-]+)`),
			allow: func(_ string, m string) bool {
				sub := regexp.MustCompile(`/home/([A-Za-z0-9._<>{}$%-]+)`).FindStringSubmatch(m)
				return len(sub) == 2 && (placeholderUser.MatchString(sub[1]) || sub[1] == "ubuntu" || sub[1] == "pi" || sub[1] == "$USER" || sub[1] == "${USER}")
			},
		},
		{
			// The operator's development drive layout (`<letter>:/Dev/…`) is a
			// machine fact; docs and examples use `D:/x/…`, `<repo>`, `<trees>`.
			name: "development drive path",
			re:   regexp.MustCompile(`(?i)[A-Za-z]:[\\/]+Dev[\\/]`),
		},
		{
			// A cloud-synced drive path with a drive letter is the operator's
			// mount. The refusal tests need the phrase, not the letter+workspace.
			name: "cloud-synced drive path",
			re:   regexp.MustCompile(`(?i)[A-Za-z]:[\\/]+My Drive[\\/]+[^\s\x60"'\\/]+`),
			allow: func(path, m string) bool {
				return strings.HasSuffix(path, "_test.go") && !strings.Contains(m, "Ecosystem")
			},
		},
		{
			// Tailnet addresses: the CGNAT block is fine to NAME; a literal
			// outside its lowest /22 is a real node.
			name: "tailnet address outside the documentation range",
			re:   regexp.MustCompile(`\b100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.([0-9]{1,3})\.[0-9]{1,3}\b`),
			allow: func(_ string, m string) bool {
				sub := regexp.MustCompile(`\b100\.(\d+)\.(\d+)\.\d+\b`).FindStringSubmatch(m)
				return len(sub) == 3 && sub[1] == "64" && (sub[2] == "0" || sub[2] == "1" || sub[2] == "2" || sub[2] == "3")
			},
		},
		{
			// A real MagicDNS zone (`tail<6 hex>.ts.net`); docs use `tailnnnnnn`.
			name: "tailnet zone",
			re:   regexp.MustCompile(`\btail[0-9a-f]{6}\.ts\.net\b`),
		},
		{
			name: "fleet token value",
			re:   regexp.MustCompile(`"fleet_auth_token"\s*:\s*"[0-9a-fA-F]{16,}"`),
		},
		{
			name:  "private key block",
			re:    regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
			allow: func(path, _ string) bool { return strings.HasPrefix(path, "internal/compeval/") },
		},
		{
			// A vendored tool's copyright header naming a bare handle that is
			// not this repository's owner is another account crossing into
			// this one. People keep their names; handles stay with the owner.
			name: "copyright header naming a foreign handle",
			re:   regexp.MustCompile(`Copyright 20[0-9]{2} ([a-z][a-z0-9-]{3,})\.`),
			allow: func(_ string, m string) bool {
				sub := regexp.MustCompile(`Copyright 20[0-9]{2} ([a-z][a-z0-9-]{3,})\.`).FindStringSubmatch(m)
				return len(sub) == 2 && sub[1] == repoOwner
			},
		},
	}
	binary := regexp.MustCompile(`(?i)\.(png|jpe?g|gif|webp|ico|woff2?|ttf|otf|pdf|mp3|wav|mp4|gguf|bin|exe|zip|db|pyc)$`)
	var findings []string
	for _, native := range files {
		path := filepath.ToSlash(native)
		if binary.MatchString(path) || path == "identity_lint_test.go" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			continue // binary by content
		}
		s := string(b)
		for _, r := range rules {
			for _, loc := range r.re.FindAllStringIndex(s, -1) {
				m := s[loc[0]:loc[1]]
				if r.allow != nil && r.allow(path, m) {
					continue
				}
				line := strings.Count(s[:loc[0]], "\n") + 1
				findings = append(findings, fmt.Sprintf("%s:%d: %s: %q", path, line, r.name, m))
			}
		}
	}
	// The vendored printed CLIs declare their owner in metadata; it must be
	// this repository's owner, never another account of the same person.
	for _, meta := range []string{"tools/llamaswap/.printing-press.json", "tools/comfyui/.printing-press.json"} {
		b, err := os.ReadFile(meta)
		if err != nil {
			continue
		}
		for _, key := range []string{`"owner"`, `"printer"`, `"handle"`} {
			re := regexp.MustCompile(key + `\s*:\s*"([^"]+)"`)
			for _, sub := range re.FindAllStringSubmatch(string(b), -1) {
				if sub[1] != repoOwner {
					findings = append(findings, fmt.Sprintf("%s: %s is %q, want the repository owner %q", meta, key, sub[1], repoOwner))
				}
			}
		}
	}
	for _, rel := range []string{"tools/llamaswap/.goreleaser.yaml", "tools/comfyui/.goreleaser.yaml"} {
		b, err := os.ReadFile(rel)
		if err != nil {
			continue
		}
		for _, sub := range regexp.MustCompile(`(?m)^\s*owner:\s*(\S+)`).FindAllStringSubmatch(string(b), -1) {
			if sub[1] != repoOwner {
				findings = append(findings, fmt.Sprintf("%s: owner is %q, want %q", rel, sub[1], repoOwner))
			}
		}
	}
	sort.Strings(findings)
	if len(findings) > 0 {
		const cap = 60
		shown := findings
		if len(shown) > cap {
			shown = shown[:cap]
		}
		t.Fatalf("%d operator-identity finding(s) in the tracked tree (this repository is public; use the placeholder forms the rules admit):\n  %s",
			len(findings), strings.Join(shown, "\n  "))
	}
}

// repoOwner is the GitHub account this repository belongs to, read from the
// module path so the test never hard-codes it apart from go.mod.
var repoOwner = func() string {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		return "dmmdea"
	}
	m := regexp.MustCompile(`(?m)^module\s+github\.com/([^/\s]+)/`).FindSubmatch(b)
	if m == nil {
		return "dmmdea"
	}
	return string(m[1])
}()

