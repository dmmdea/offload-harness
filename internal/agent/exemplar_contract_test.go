package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// allRegisteredSpecs assembles the MAXIMAL tool set — every constructor, every
// capability granted — and returns the specs by name.
//
// It lists the constructors explicitly rather than going through Build, which
// would need a full config and a live endpoint. The trade-off is deliberate: a
// NEW tool constructor that is not added here makes an exemplar referencing its
// tool fail as "not registered", which is the safe direction (loud, and the fix
// is one line here).
func allRegisteredSpecs(t *testing.T) map[string]ToolSpec {
	t.Helper()
	root := t.TempDir()
	pol := NewPolicy(false, nil)
	offload := func(context.Context, string, string, map[string]any) (string, error) { return "{}", nil }

	tools, err := ReadOnlyTools(root, offload)
	if err != nil {
		t.Fatalf("ReadOnlyTools: %v", err)
	}
	wtools, err := WriteTools(root, pol)
	if err != nil {
		t.Fatalf("WriteTools: %v", err)
	}
	tools = append(tools, wtools...)
	tools = append(tools, FetchTools(pol)...)
	tools = append(tools, SearchTools(pol)...)
	tools = append(tools, GitHubTools(pol, "test-token", "demo", root)...)
	tools = append(tools, ShellTools(pol, root, root)...)
	tools = append(tools, RunTools(pol, root, root)...)

	specs := make(map[string]ToolSpec, len(tools))
	for _, tl := range tools {
		specs[tl.Name] = tl.ToolSpec
	}
	return specs
}

// TestProfileExemplarArgsSatisfyTheToolSchema is the regression test for a defect
// that shipped and was measured to break real runs: the `edit` profile's ONLY
// search exemplar called search_files with {"query":...} while the tool requires
// {"pattern":...}, so the decoder rejected it with "search_files requires a
// pattern".
//
// That is worse than an ordinary bug. Per compaction.go, profile exemplars live
// in the never-compacted protected preamble, so a wrong call shape is taught to
// the planner for the ENTIRE run and cannot age out. The suite already validated
// exemplar STRUCTURE (complete tool cycles); nothing validated the ARGUMENTS
// against the schema the tool actually enforces.
func TestProfileExemplarArgsSatisfyTheToolSchema(t *testing.T) {
	specs := allRegisteredSpecs(t)
	compiler := jsonschema.NewCompiler()
	compiled := map[string]*jsonschema.Schema{} // a tool may appear in several exemplars

	for profName, p := range profileRegistry {
		for i, m := range p.Exemplars {
			for _, call := range m.ToolCalls {
				spec, ok := specs[call.Name]
				if !ok {
					t.Errorf("profile %q exemplar[%d]: tool %q is NOT REGISTERED — the planner is shown a call it can never make",
						profName, i, call.Name)
					continue
				}
				// The exemplar must also be a tool the profile actually grants,
				// or the preamble teaches a call the run will reject as unknown.
				if !profileGrants(p, call.Name) {
					t.Errorf("profile %q exemplar[%d]: calls %q, which the profile's own Tools list does not grant",
						profName, i, call.Name)
				}

				var args map[string]any
				if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
					t.Errorf("profile %q exemplar[%d] (%s): Args is not valid JSON: %v", profName, i, call.Name, err)
					continue
				}
				sch, ok := compiled[call.Name]
				if !ok {
					var err error
					sch, err = compileSpecSchema(compiler, call.Name, spec.Schema)
					if err != nil {
						t.Errorf("profile %q exemplar[%d] (%s): tool schema does not compile: %v", profName, i, call.Name, err)
						continue
					}
					compiled[call.Name] = sch
				}
				if err := sch.Validate(args); err != nil {
					t.Errorf("profile %q exemplar[%d]: %s(%s) VIOLATES its own tool schema: %v\n"+
						"  exemplars are never compacted, so this teaches a failing call for the whole run",
						profName, i, call.Name, call.Args, err)
				}
			}
		}
	}
}

// TestWithProfileDropsExemplarsForToolsTheLoopLacks: a profile's Tools list is a
// WISH, not a guarantee. The read-only MCP front door registers no run_shell, so
// applying `build` there used to inject exemplars demonstrating run_shell — a
// worked example of a call the planner cannot make, in the never-compacted
// preamble. Tools were narrowed; exemplars were copied wholesale.
func TestWithProfileDropsExemplarsForToolsTheLoopLacks(t *testing.T) {
	// A read-only loop: exactly what the MCP front door builds.
	l := NewLoop(&fakeClient{}, mkTools("list_dir", "read_file", "search_files", "update_plan"), 5)
	p, err := LookupProfile("build")
	if err != nil {
		t.Fatalf("LookupProfile(build): %v", err)
	}
	l.WithProfile(p)

	if len(p.Exemplars) == 0 {
		t.Fatal("the build profile has no exemplars; this test would prove nothing")
	}
	for i, m := range l.exemplars {
		for _, c := range m.ToolCalls {
			if _, ok := l.tools[c.Name]; !ok {
				t.Errorf("exemplar[%d] demonstrates %q, which this loop does not register", i, c.Name)
			}
		}
	}
	// And the surviving conversation must still be well-formed: every remaining
	// tool result answers a tool call that is still present. A dangling half is
	// rejected outright by strict --jinja templates.
	live := map[string]bool{}
	for _, m := range l.exemplars {
		for _, c := range m.ToolCalls {
			live[c.ID] = true
		}
	}
	for i, m := range l.exemplars {
		if m.Role == "tool" && !live[m.ToolCallID] {
			t.Errorf("exemplar[%d] is an ORPHAN tool result for call id %q — its assistant turn was dropped without it", i, m.ToolCallID)
		}
	}
}

// compileSpecSchema compiles a tool's raw JSON Schema under a synthetic URL.
func compileSpecSchema(c *jsonschema.Compiler, name string, raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	url := "mem://tool/" + name + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

// profileGrants reports whether a profile's declared Tools list includes name.
// A profile with no list (general) advertises everything.
func profileGrants(p Profile, name string) bool {
	if len(p.Tools) == 0 {
		return true
	}
	for _, n := range p.Tools {
		if n == name {
			return true
		}
	}
	return false
}
