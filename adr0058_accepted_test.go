package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ADR 0058 was Proposed until the operator accepted it on 2026-10-14, together with the third licence state `conditional` that the
// licence map and the run-template gate use. The record and its index row say so, and the acceptance is dated in the record itself.
func TestADR0058IsAccepted(t *testing.T) {
	const adr = "docs/architecture/decisions/0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md"
	raw, err := os.ReadFile(adr)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !regexp.MustCompile(`(?s)\A---\nstatus: Accepted\n`).MatchString(text) {
		t.Errorf("ADR 0058 frontmatter status is not Accepted")
	}
	for _, want := range []string{"2026-10-14", "operator accepted", "conditional"} {
		if !strings.Contains(text, want) {
			t.Errorf("ADR 0058 does not record the acceptance: no %q", want)
		}
	}
	index, err := os.ReadFile("docs/architecture/decisions/README.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile(`(?m)^\| \[0058\]\([^)]*\) \| (\w+) \|`).FindStringSubmatch(strings.ReplaceAll(string(index), "\r\n", "\n"))
	if row == nil || row[1] != "Accepted" {
		t.Errorf("the ADR index row of 0058 does not say Accepted: %v", row)
	}
}
