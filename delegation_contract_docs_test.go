package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/delegate"
)

// delegateContractPage is the one page that documents the file `local-offload delegate --contract` reads
// (register D-70). It cost three failed launches when the wire shape lived only in the flag help: the CLI
// wants a bare subtask object or an array of subtasks, not the {"subtasks": [...]} wrapper the MCP tool
// takes, and a wrapper file does not fail at parse time, it parses as ONE subtask with no goal.
const delegateContractPage = "docs/systems/delegate-contract-file.md"

// contractHelpFields reads the field list out of the --contract flag help in main.go, so the page is held
// to the text the CLI prints and not to a copy of it.
func contractHelpFields(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`fs\.String\("contract",\s*"",\s*"[^"]*\(fields: ([^)]*)\)`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("main.go: the --contract flag help no longer ends in a `(fields: ...)` list; this gate went blind")
	}
	fields := strings.Split(string(m[1]), ",")
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	if len(fields) < 5 {
		t.Fatalf("main.go: parsed only %d contract fields from the help: %q", len(fields), fields)
	}
	return fields
}

func TestDelegateContractFilePageNamesWhatTheCLIReads(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(delegateContractPage))
	if err != nil {
		t.Fatalf("the contract-file page is missing: %v", err)
	}
	page := strings.ReplaceAll(string(raw), "\r\n", "\n")

	for _, f := range contractHelpFields(t) {
		if !strings.Contains(page, "`"+f+"`") {
			t.Errorf("%s does not name the contract field `%s` that `delegate --help` lists", delegateContractPage, f)
		}
	}
	// The shape, the cap, the tolerant decode, the wrapper trap and the seat-id rule, each as the page states it.
	docNeeds(t, delegateContractPage, page,
		"bare",             // a bare subtask object
		"array",            // or an array of them
		"at most 8",        // the subtask cap
		"unknown fields",   // the tolerant decode
		`{"subtasks"`,      // the MCP wrapper the file must NOT use
		"goal is required", // what a wrapper file turns into
		"`agent_model`",    // the seat id rule
		"/v1/models",       // where the served id comes from
		"`infrastructure`", // the defer class a mistyped id earns
		"`lost_to_stack`",  // and the summary count it feeds
		"offload_status",   // the roster that shows the served id
	)
}

// TestContractFileWrapperIsNotUnwrapped pins the claim the page makes about the MCP wrapper: a file shaped
// {"subtasks": [{"goal": ...}]} parses without error (unknown fields are ignored) as ONE subtask whose goal is
// blank, and intake refuses it by name. Without this the page's warning is a sentence nothing holds to.
func TestContractFileWrapperIsNotUnwrapped(t *testing.T) {
	p := writeContract(t, `{"subtasks":[{"goal":"digest the docs","acceptance":["nonempty:answer"]}]}`)
	specs, err := parseContractFile(p)
	if err != nil {
		t.Fatalf("a wrapper file must still parse (unknown fields are ignored): %v", err)
	}
	if len(specs) != 1 || specs[0].Goal != "" {
		t.Fatalf("a wrapper file must parse as one subtask with no goal, got %d subtask(s), goal %q", len(specs), specs[0].Goal)
	}
	if _, err := delegate.PrepareContract(specs[0], t.TempDir()); err == nil || !strings.Contains(err.Error(), "goal is required") {
		t.Fatalf("intake must refuse the blank-goal subtask a wrapper file becomes with \"goal is required\", got %v", err)
	}
}
