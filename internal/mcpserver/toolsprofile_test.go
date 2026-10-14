package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// fullSurfaceCfg is every optional tool surface on at once: the delegation pair, the browse lane and all three accelerators,
// so the invariants below are checked over every description the server can advertise, not only the default box's.
func fullSurfaceCfg(profile string) config.Config {
	c := browseCfg()
	c.AgentDelegationEnabled = true
	c.Accelerators = []string{"hailo-8l", "coral-edgetpu", "rknpu"}
	c.ToolsSchemaProfile = profile
	return c
}

// stripProse removes the keywords compact is allowed to drop, naively and independently of the production walker: a
// "description" whose value is a string (a PROPERTY named description holds an object, so it survives), and the dropped
// keywords. What is left is the structure a call depends on.
func stripProse(v any) any {
	switch n := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range n {
			if _, isStr := x.(string); k == "description" && isStr {
				continue
			}
			if k == "examples" || k == "example" || k == "title" || k == "$comment" {
				continue
			}
			out[k] = stripProse(x)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, x := range n {
			out[i] = stripProse(x)
		}
		return out
	}
	return v
}

// collectDescriptions returns every description string of a tool: its own and each schema description at any depth.
func collectDescriptions(tool *mcp.Tool) []string {
	out := []string{tool.Description}
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			for k, x := range n {
				if s, ok := x.(string); ok && k == "description" {
					out = append(out, s)
					continue
				}
				walk(x)
			}
		case []any:
			for _, x := range n {
				walk(x)
			}
		}
	}
	walk(tool.InputSchema)
	return out
}

// TestCompactKeepsEveryToolAndParameter: the compact list names every tool the full list names, in the same order, and each
// input schema is the full one minus prose: every parameter name, type, enum, items, required entry and bound survives at every
// depth, so a call valid against the full list is valid against the compact one.
func TestCompactKeepsEveryToolAndParameter(t *testing.T) {
	full := listTools(t, fullSurfaceCfg("full"))
	compact := listTools(t, fullSurfaceCfg("compact"))
	if len(full) < 30 {
		t.Fatalf("only %d tools in the full list: the test config lost an optional surface", len(full))
	}
	if len(compact) != len(full) {
		t.Fatalf("compact advertises %d tools, full %d", len(compact), len(full))
	}
	var fullBytes, compactBytes int
	for i := range full {
		f, c := full[i], compact[i]
		if f.Name != c.Name {
			t.Fatalf("tool %d: compact names %q, full %q: the order or the set changed", i, c.Name, f.Name)
		}
		if !reflect.DeepEqual(stripProse(f.InputSchema), stripProse(c.InputSchema)) {
			t.Errorf("%s: compact changed the schema beyond its prose\n full:    %v\n compact: %v", f.Name, stripProse(f.InputSchema), stripProse(c.InputSchema))
		}
		if strings.TrimSpace(c.Description) == "" {
			t.Errorf("%s: compact left no description", f.Name)
		}
		fb, _ := json.Marshal(f)
		cb, _ := json.Marshal(c)
		fullBytes += len(fb)
		compactBytes += len(cb)
	}
	if compactBytes*10 > fullBytes*8 {
		t.Errorf("compact is %d bytes against full %d: less than a fifth smaller, the profile has stopped doing its job", compactBytes, fullBytes)
	}
}

// TestCompactRequiredParametersSurvive names the contract of the previous test for the two fields a caller trips on: every
// required parameter of every tool, and an enum's values, are the same list under compact.
func TestCompactRequiredParametersSurvive(t *testing.T) {
	full := listTools(t, fullSurfaceCfg("full"))
	compact := listTools(t, fullSurfaceCfg("compact"))
	required := 0
	for i := range full {
		fs, _ := full[i].InputSchema.(map[string]any)
		cs, _ := compact[i].InputSchema.(map[string]any)
		if !reflect.DeepEqual(fs["required"], cs["required"]) {
			t.Errorf("%s: required %v became %v", full[i].Name, fs["required"], cs["required"])
		}
		if r, ok := fs["required"].([]any); ok {
			required += len(r)
		}
		fp, _ := fs["properties"].(map[string]any)
		cp, _ := cs["properties"].(map[string]any)
		for name, prop := range fp {
			pm, _ := prop.(map[string]any)
			cm, ok := cp[name].(map[string]any)
			if !ok {
				t.Errorf("%s: parameter %q is gone from the compact list", full[i].Name, name)
				continue
			}
			if !reflect.DeepEqual(pm["enum"], cm["enum"]) || !reflect.DeepEqual(pm["type"], cm["type"]) {
				t.Errorf("%s.%s: type/enum changed: %v/%v -> %v/%v", full[i].Name, name, pm["type"], pm["enum"], cm["type"], cm["enum"])
			}
		}
	}
	if required < 30 {
		t.Fatalf("only %d required parameters seen: the check is blind", required)
	}
}

// TestCompactNeverLosesASafetyWord: the property the profile promises, checked over every description the server advertises:
// a sentence that carries a safety word is kept whole and the cut only ever drops text that has none, so the number of safety
// words in a tool's descriptions is the same under both profiles.
func TestCompactNeverLosesASafetyWord(t *testing.T) {
	full := listTools(t, fullSurfaceCfg("full"))
	compact := listTools(t, fullSurfaceCfg("compact"))
	seen := 0
	for i := range full {
		count := func(tool *mcp.Tool) int {
			n := 0
			for _, d := range collectDescriptions(tool) {
				n += len(safetyWords.FindAllString(d, -1))
			}
			return n
		}
		f, c := count(full[i]), count(compact[i])
		seen += f
		if f != c {
			t.Errorf("%s: %d safety words in the full descriptions, %d in the compact ones", full[i].Name, f, c)
		}
	}
	if seen < 100 {
		t.Fatalf("only %d safety words across the whole list: the check is blind", seen)
	}
}

// TestCompactKeepsNamedSafetySentences pins the sentences a caller must read before it calls. Each pin is looked up in the FULL
// description first, so a reworded description makes the pin fail loudly instead of the test passing on nothing.
func TestCompactKeepsNamedSafetySentences(t *testing.T) {
	full := map[string]*mcp.Tool{}
	for _, tool := range listTools(t, fullSurfaceCfg("full")) {
		full[tool.Name] = tool
	}
	compact := map[string]*mcp.Tool{}
	for _, tool := range listTools(t, fullSurfaceCfg("compact")) {
		compact[tool.Name] = tool
	}
	pins := []struct{ tool, sentence string }{
		{"offload_compose_video", "html and project_dir are TRUSTED CODE ONLY: HyperFrames' Chrome runs without a sandbox, so never pass third-party pages."},
		{"offload_review_diff", "ADVISORY ONLY: this lane never gates a merge and never substitutes for the final does-it-actually-work check, which stays yours"},
		{"offload_review_diff", "never apply a finding unread"},
		{"agent_run", "It is READ-ONLY: it cannot write files, run commands, or touch the network."},
		{"agent_delegate", "the harness never applies it; that is what makes handing a 4B an edit safe"},
		{"offload_ask", "security or credential review"},
		{"offload_browse", "SAFETY: controls labelled publish / send / post / delete / pay / buy / checkout / subscribe / confirm / sign out"},
		{"offload_browse", "Never pass pages whose text should not leave this machine"},
		{"offload_generate_image", "do not lower steps/resolution to speed things up unless the caller explicitly asks for a draft"},
	}
	for _, p := range pins {
		f, ok := full[p.tool]
		if !ok {
			t.Errorf("%s is not in the full list", p.tool)
			continue
		}
		if !strings.Contains(f.Description, p.sentence) {
			t.Errorf("%s: the pinned sentence is not in the full description any more (reworded?): %q", p.tool, p.sentence)
			continue
		}
		if !strings.Contains(compact[p.tool].Description, p.sentence) {
			t.Errorf("%s: compact dropped a safety sentence: %q\n compact description: %s", p.tool, p.sentence, compact[p.tool].Description)
		}
	}
	// A safety sentence that lives in a PARAMETER description survives too.
	fp := paramDescription(t, full["agent_delegate"], "subtasks", "items", "properties", "write_root")
	cp := paramDescription(t, compact["agent_delegate"], "subtasks", "items", "properties", "write_root")
	const pin = "the harness never applies it"
	if !strings.Contains(fp, pin) {
		t.Fatalf("write_root no longer says %q: reword the pin (full: %q)", pin, fp)
	}
	if !strings.Contains(cp, pin) {
		t.Errorf("compact dropped the write_root safety sentence: %q", cp)
	}
	if len(cp) >= len(fp) {
		t.Errorf("compact did not shorten write_root: %d bytes against %d", len(cp), len(fp))
	}
}

// paramDescription follows a path of schema keys ("subtasks","items","properties","write_root"); the first name is a property
// of the top level, so "properties" is implied before it.
func paramDescription(t *testing.T, tool *mcp.Tool, path ...string) string {
	t.Helper()
	cur, _ := tool.InputSchema.(map[string]any)
	cur, _ = cur["properties"].(map[string]any)
	for _, k := range path {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("%s: no %q on the path %v", tool.Name, k, path)
		}
		cur = next
	}
	s, _ := cur["description"].(string)
	return s
}

// TestDefaultProfileIsFull: no key, "full" and the Default() value all advertise the same bytes, and compact does not. The
// compact middleware is installed only for compact, so the default list is the list before the key existed; the release notes
// record the one-off proof that the default bytes equal the previous release's.
func TestDefaultProfileIsFull(t *testing.T) {
	if got := config.Default().ToolsSchemaProfile; got != "full" {
		t.Fatalf("Default().ToolsSchemaProfile = %q, want \"full\"", got)
	}
	unset := config.Default()
	unset.ToolsSchemaProfile = ""
	explicit := config.Default()
	explicit.ToolsSchemaProfile = "full"
	compact := config.Default()
	compact.ToolsSchemaProfile = "compact"
	a, b, c := listToolsWire(t, config.Default()), listToolsWire(t, unset), listToolsWire(t, explicit)
	if !bytes.Equal(a, b) || !bytes.Equal(a, c) {
		t.Fatal("an unset profile, \"full\" and the default do not advertise the same bytes")
	}
	if bytes.Equal(a, listToolsWire(t, compact)) {
		t.Fatal("compact advertises the same bytes as full: the profile is not wired")
	}
	// A hand-built Config with a value the loader would have refused reads as full, never as a silent shrink.
	odd := config.Default()
	odd.ToolsSchemaProfile = "tiny"
	if !bytes.Equal(a, listToolsWire(t, odd)) {
		t.Fatal("an unknown profile on a hand-built Config changed the advertised list")
	}
}

// TestCompactDoesNotTouchTheRegistry: the list is rewritten on a copy. The registered tool keeps its prose, so a later tools/list
// under another profile (and the handlers' own view of the schema) is whole.
func TestCompactDoesNotTouchTheRegistry(t *testing.T) {
	cfg := fullSurfaceCfg("compact")
	srv := New(pipeline.New(cfg, nil, nil, nil)).buildServer("test")
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "pin", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	first, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first.Tools)
	b, _ := json.Marshal(second.Tools)
	if !bytes.Equal(a, b) {
		t.Fatal("two tools/list calls on one compact server answered differently: the middleware compacted its own output again or mutated the registry")
	}
	// compactTool on a registered-style tool leaves its argument alone.
	tool := &mcp.Tool{Name: "x", Description: "First sentence is long enough to stand. Second sentence is dropped.", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","description":"One thing here for you. And another thing too."}}}`)}
	before, _ := json.Marshal(tool)
	got := compactTool(tool)
	after, _ := json.Marshal(tool)
	if !bytes.Equal(before, after) {
		t.Fatal("compactTool modified the tool it was given")
	}
	if got.Description != "First sentence is long enough to stand." {
		t.Fatalf("description = %q", got.Description)
	}
}

// TestCompactProfileLeavesCallsAlone: the profile rewrites the advertised list only. A call that the full server refuses for a
// missing required parameter is refused identically under compact (the handlers read the full schema).
func TestCompactProfileLeavesCallsAlone(t *testing.T) {
	call := func(profile string) string {
		cfg := config.Default()
		cfg.ToolsSchemaProfile = profile
		srv := New(pipeline.New(cfg, nil, nil, nil)).buildServer("test")
		clientT, serverT := mcp.NewInMemoryTransports()
		ctx := context.Background()
		ss, err := srv.Connect(ctx, serverT, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer ss.Close()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "pin", Version: "1"}, nil).Connect(ctx, clientT, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "offload_summarize", Arguments: map[string]any{"text": 7}})
		if err != nil {
			return "protocol error: " + err.Error()
		}
		b, _ := json.Marshal(res)
		return string(b)
	}
	full, compact := call("full"), call("compact")
	if full != compact {
		t.Fatalf("a bad call is answered differently under compact\n full:    %s\n compact: %s", full, compact)
	}
	if !strings.Contains(full, "bad arguments") && !strings.Contains(full, "protocol error") {
		t.Fatalf("the probe call did not hit the argument check: %s", full)
	}
}

func TestSplitSentences(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"two sentences", "Run it. Then stop.", []string{"Run it.", "Then stop."}},
		{"version numbers do not split", "Added in 0.137.0 and again in 0.186.0. Done.", []string{"Added in 0.137.0 and again in 0.186.0.", "Done."}},
		{"e.g. and i.e. do not split", "Pass a seat (e.g. qwen3.5) or i.e. a name. Next.", []string{"Pass a seat (e.g. qwen3.5) or i.e. a name.", "Next."}},
		{"a bracket keeps its sentence together", "Use it (see the table. It lists all) now. Next.", []string{"Use it (see the table. It lists all) now.", "Next."}},
		{"lower-case identifier opens a sentence", "Read summary FIRST. results[].ran_on says where it ran.", []string{"Read summary FIRST.", "results[].ran_on says where it ran."}},
		{"a quoted argument does not split", "pass section:\"brief\" to size it. Then go.", []string{"pass section:\"brief\" to size it.", "Then go."}},
		{"ellipsis in a token does not split", "send waiter_token=mq-...: the ticket. Done.", []string{"send waiter_token=mq-...: the ticket.", "Done."}},
		{"no terminal period", "Only one clause here", []string{"Only one clause here"}},
		{"empty", "", nil},
	} {
		got := splitSentences(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: splitSentences(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestCompactDescription(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		lead     int
		want     string
	}{
		{"first sentence only", "Summarize the given text right now. Triggers: summarize, digest. Returns the gist.", 320, "Summarize the given text right now."},
		{"a safety sentence survives wherever it sits", "Summarize the given text right now. Triggers: summarize, digest. Never pass secrets here. Returns the gist.", 320, "Summarize the given text right now. Never pass secrets here."},
		{"a one-word opener grows to the next sentence", "OPTIONAL. The ticket from a queued answer. Extra detail follows here.", 320, "OPTIONAL. The ticket from a queued answer."},
		{"one sentence is untouched", "Just one sentence, with detail.", 320, "Just one sentence, with detail."},
		{"a long lead is cut at a clause boundary", "Do the first useful thing for the caller right here; then explain at length why that is so and what else follows from it in a very long tail.", 60, "Do the first useful thing for the caller right here."},
		{"a long lead whose tail has a safety word stays whole", "Do the first useful thing for the caller right here; then explain at length why you must never skip it, in a very long tail.", 60, "Do the first useful thing for the caller right here; then explain at length why you must never skip it, in a very long tail."},
		{"a lead with no clean boundary stays whole", "Do the first useful thing for the caller right here and then keep on going with no break at all in this run", 40, "Do the first useful thing for the caller right here and then keep on going with no break at all in this run"},
		{"a comma after a bracket is a boundary", "placement: auto (default; the local seat if idle), spread (deal across every node and keep the order), queue (durable).", 40, "placement: auto (default; the local seat if idle)."},
	} {
		if got := compactDescription(tc.in, tc.lead); got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
}

// TestCompactSchemaKeepsPropertyNamedDescription: a parameter that is itself called "description" is a property (an object), not
// the prose keyword, and is neither cut nor dropped; enum and default values are never walked as schemas.
func TestCompactSchemaKeepsPropertyNamedDescription(t *testing.T) {
	in := json.RawMessage(`{"type":"object","title":"T","properties":{"description":{"type":"string","description":"What this field is for, in full. More words follow here."},"mode":{"type":"string","enum":["a. B","c"],"default":"a. B","examples":["c"]}},"required":["description"]}`)
	got, ok := compactInputSchema(in).(json.RawMessage)
	if !ok {
		t.Fatalf("compactInputSchema returned %T", compactInputSchema(in))
	}
	want := `{"type":"object","properties":{"description":{"type":"string","description":"What this field is for, in full."},"mode":{"type":"string","enum":["a. B","c"],"default":"a. B"}},"required":["description"]}`
	if string(got) != want {
		t.Fatalf("\n got  %s\n want %s", got, want)
	}
}

// TestCompactSchemaUnparseableStaysAsItWas: the failure mode is a full schema, never a broken one.
func TestCompactSchemaUnparseableStaysAsItWas(t *testing.T) {
	bad := json.RawMessage(`{"type":"object","properties":`)
	if got := compactInputSchema(bad); !reflect.DeepEqual(got, any(bad)) {
		t.Fatalf("an unparseable schema came back changed: %v", got)
	}
	if got := compactInputSchema(nil); got != nil {
		t.Fatalf("nil schema came back as %v", got)
	}
}
