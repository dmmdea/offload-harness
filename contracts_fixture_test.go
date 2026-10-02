package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// contractFixtures are the three eight-subtask digest sets that carry this
// repository's own ADRs inline as their context. Nothing else loads them (the
// parallel-sessions gate script and the docs only name them), so before this
// test a malformed re-derivation of one — a dropped anchor, a context doc that
// no longer names a file, a subtask short of eight — surfaced only when a
// benchmark run misbehaved.
var contractFixtures = []string{
	"contracts/digest-8.json",
	"contracts/digest-8-grounded.json",
	"contracts/digest-adr-hard-8.json",
}

// TestContractFixturesAreWellFormed pins what contracts/README.md already says
// about the authoring rule, for the sets whose text is derived rather than
// written: eight subtasks, every acceptance string parses, none is UNGROUNDED
// (the intake lint's own verdict), every inline context doc is named after an
// ADR that exists, and no grounded regex alternative occurs in the goal that
// is supposed to be answered from the docs. The last property is the one a
// name-only substitution in the inline text can silently break: an anchor the
// substitution rewrote would stop matching the docs and fail right answers.
func TestContractFixturesAreWellFormed(t *testing.T) {
	adrDir := filepath.Join("docs", "architecture", "decisions")
	for _, rel := range contractFixtures {
		rel := rel
		t.Run(filepath.Base(rel), func(t *testing.T) {
			raw, err := os.ReadFile(rel)
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			var subtasks []core.AgentContract
			if err := json.Unmarshal(raw, &subtasks); err != nil {
				t.Fatalf("%s is not a JSON array of contracts: %v", rel, err)
			}
			if len(subtasks) != 8 {
				t.Fatalf("%s holds %d subtasks, want 8", rel, len(subtasks))
			}
			for i, c := range subtasks {
				where := func(msg string) string {
					return fmt.Sprintf("%s subtask %d: %s", rel, i, msg)
				}
				if strings.TrimSpace(c.Goal) == "" {
					t.Errorf("%s", where("empty goal"))
				}
				if len(c.Context) == 0 {
					t.Errorf("%s", where("no inline context docs"))
				}
				var docs strings.Builder
				for _, d := range c.Context {
					if _, err := os.Stat(filepath.Join(adrDir, d.Name)); err != nil {
						t.Errorf("%s", where("context doc "+d.Name+" is not a file under "+filepath.ToSlash(adrDir)))
					}
					if strings.TrimSpace(d.Text) == "" {
						t.Errorf("%s", where("context doc "+d.Name+" is empty"))
					}
					docs.WriteString(d.Text)
					docs.WriteByte('\n')
				}
				var schema map[string]any
				if err := json.Unmarshal(c.OutputSchema, &schema); err != nil {
					t.Errorf("%s", where("output_schema is not a JSON object: "+err.Error()))
				}
				if len(c.Acceptance) == 0 {
					t.Errorf("%s", where("no acceptance checks"))
				}
				for _, a := range c.Acceptance {
					chk, err := core.ParseAcceptanceCheck(a)
					if err != nil {
						t.Errorf("%s", where("acceptance does not parse: "+err.Error()))
						continue
					}
					if chk.Kind != core.AccRegex {
						continue
					}
					for _, alt := range regexAlternatives(chk.Arg) {
						re, err := regexp.Compile(alt)
						if err != nil {
							t.Errorf("%s", where("regex alternative "+alt+" does not compile: "+err.Error()))
							continue
						}
						if !re.MatchString(docs.String()) {
							t.Errorf("%s", where("regex alternative "+alt+" matches none of the inline docs"))
						}
						if re.MatchString(c.Goal) {
							t.Errorf("%s", where("regex alternative "+alt+" occurs in the goal, so echoing the question would satisfy it"))
						}
					}
				}
				for _, w := range delegate.LintAcceptance(c) {
					if strings.Contains(w, "UNGROUNDED") {
						t.Errorf("%s", where("intake lint: "+w))
					}
				}
			}
		})
	}
}

// regexAlternatives splits one `regex:` argument of the shape `(a|b|c)` into
// its alternatives. The fixtures use only that flat shape; anything else is
// returned whole so the caller still compiles and matches it.
func regexAlternatives(arg string) []string {
	if strings.HasPrefix(arg, "(") && strings.HasSuffix(arg, ")") {
		inner := arg[1 : len(arg)-1]
		if !strings.ContainsAny(inner, "()") {
			return strings.Split(inner, "|")
		}
	}
	return []string{arg}
}
