package mcpserver

import (
	"reflect"
	"strings"
	"testing"
)

// TestCompactKeepsTheRunTemplateRules: offload_run_template is the tool a caller reaches for to run a workflow template, and four of
// its sentences are the rules it is run under, none of which the first sentence says: what a run is allowed for and what a refusal
// looks like (a paid-API template is a deferral with its class, never a cloud fallback; a licence that is not permissive is refused with
// its class), that it never downloads a model, that it stays on this machine's lane (a template that reads input files is never sent
// to the fleet), and that a prompt embedding the harness cannot verify is refused under a class of its own (embedding_unverified), which is
// not a missing file. A compact list that cut them would leave a caller to guess why a call was refused and to retry it elsewhere. Each pin
// is looked up in the full description first, so a rewording fails loudly.
func TestCompactKeepsTheRunTemplateRules(t *testing.T) {
	var full, compact string
	for _, tool := range listTools(t, fullSurfaceCfg("full")) {
		if tool.Name == "offload_run_template" {
			full = tool.Description
		}
	}
	for _, tool := range listTools(t, fullSurfaceCfg("compact")) {
		if tool.Name == "offload_run_template" {
			compact = tool.Description
		}
	}
	if full == "" || compact == "" {
		t.Fatal("offload_run_template is not in the tools list (full or compact)")
	}
	for _, pin := range []string{
		"returns deferred:true with class api_paid and never falls back to cloud",
		"is refused with its class",
		"It never downloads a model",
		"is never sent to the fleet",
		"embedding_unverified",
	} {
		if !strings.Contains(full, pin) {
			t.Errorf("the pinned phrase is not in the full description any more (reworded?): %q", pin)
			continue
		}
		if !strings.Contains(compact, pin) {
			t.Errorf("compact dropped a rule of the run-template tool: %q", pin)
		}
	}
}

// The action enum is the tool's whole shape: compact must keep every value, and the first sentence must still say what each one does.
func TestCompactKeepsTheRunTemplateActions(t *testing.T) {
	for _, profile := range []string{"full", "compact"} {
		for _, tool := range listTools(t, fullSurfaceCfg(profile)) {
			if tool.Name != "offload_run_template" {
				continue
			}
			props, _ := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
			action, _ := props["action"].(map[string]any)
			if got := asStrings(action["enum"]); !reflect.DeepEqual(got, []string{"list", "get", "readiness", "run"}) {
				t.Errorf("%s: action enum = %v", profile, got)
			}
			for _, word := range []string{"list (", "get (", "readiness (", "run ("} {
				if !strings.Contains(tool.Description, word) {
					t.Errorf("%s: the description no longer says what %q does", profile, word)
				}
			}
			return
		}
		t.Errorf("%s: offload_run_template is not in the tools list", profile)
	}
}
