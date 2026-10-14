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
	// The surface must still hold the optional lanes the test claims to cover: the browse lane, the delegation pair and one tool
	// of each accelerator. A bare count would pass at 31 tools with all of them gone.
	have := map[string]bool{}
	for _, tool := range full {
		have[tool.Name] = true
	}
	for _, name := range []string{"offload_browse", "agent_delegate", "offload_research", "offload_face_detect", "offload_classify_image", "offload_semantic_segment"} {
		if !have[name] {
			t.Fatalf("%s is not in the full list: the test config lost an optional surface (%d tools)", name, len(full))
		}
	}
	if len(full) < 45 {
		t.Fatalf("only %d tools in the full list, want at least 45: the test config lost an optional surface", len(full))
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
		{"offload_nim", "This is the ONLY cloud/remote tool on this server — every other offload_* tool runs on the LOCAL models (see offload_status for that roster)."},
		{"offload_nim", "It is OPT-IN (the hosted endpoint needs NVIDIA_API_KEY in the server env; a self-hosted NIM via base is keyless): use it deliberately for a stronger model than the local cascade, NOT for routine grunt work."},
		{"offload_generate_video", "The graph family comes from THIS machine's videogen_family binding (the 2x16 reference box is bound to LTX-2.5, which generates joint AAC audio); pass model ONLY to override that deliberately — no cloud, runs on the local GPU."},
		{"offload_generate_video", "QUALITY-FIRST DEFAULT: the native two-stage recipe (no distill LoRA, 20 steps, cfg 3.5, the model's official negative) — a render takes tens of minutes and that is intended; set fast=true ONLY when the caller explicitly wants a draft (8-step lightx2v distill — visibly weaker motion)."},
		{"agent_delegate", "Caps: 8 files, 64 KiB written, 192 KiB of diff, and no delete/shell/run/network — past a cap nothing is published at all."},
		{"agent_delegate", "Verify it with diff_touches:<path-prefix> and diff_max_files:<n>, which read the write set rather than the prose; neither says the change is CORRECT, so read the diff."},
		{"agent_delegate", "On any refusal before placement it returns deferred:true with a reason and you do the work yourself."},
		{"offload_ocr", "Returns {text} with the transcribed text in reading order; if it can't transcribe confidently it returns deferred:true and you should read the image yourself."},
		{"offload_summarize", "Returns {summary, bullets}; if it can't do it confidently it returns deferred:true and you should summarize it yourself."},
		{"offload_vqa", "Returns {answer}; if it can't answer confidently it returns deferred:true and you should look at the image yourself."},
		{"offload_transcribe", "If it can't transcribe confidently it returns deferred:true and you should handle the audio yourself."},
		{"offload_video_describe", "Returns {answer} (which notes what the relevant frames show); if it can't answer confidently it returns deferred:true and you should watch the video yourself."},
		{"offload_status", "Every offload_* tool except offload_nim and offload_browse runs LOCAL — the GPU roster or a listed accelerator (free, on-box, no cloud); offload_nim is the only remote MODEL surface, and offload_browse (opt-in, present only when configured) drives the operator's own local browser with typed choices from a loopback decision endpoint the operator runs."},
		{"offload_review_diff", "Equally, an EMPTY findings list means this reviewer found nothing; it is not a verification that the change works."},
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

// TestCompactKeepsNamedParameterSentences pins constraint sentences that live in a PARAMETER description, and the enum-value lines a
// kept one belongs with. Each pin is looked up in the full description first, so a rewording fails loudly.
func TestCompactKeepsNamedParameterSentences(t *testing.T) {
	full := map[string]*mcp.Tool{}
	for _, tool := range listTools(t, fullSurfaceCfg("full")) {
		full[tool.Name] = tool
	}
	compact := map[string]*mcp.Tool{}
	for _, tool := range listTools(t, fullSurfaceCfg("compact")) {
		compact[tool.Name] = tool
	}
	pins := []struct {
		tool     string
		path     []string
		sentence string
	}{
		{"agent_delegate", []string{"subtasks", "items", "properties", "write_root"}, "Grants create+overwrite only: no delete, no shell, no run, no network."},
		{"agent_delegate", []string{"subtasks", "items", "properties", "write_root"}, "Omit for read-only work, which is everything else"},
		{"agent_delegate", []string{"pin_reason"}, "privacy: the material may not leave this box (credentials, unreleased or brand-isolated data); route local only."},
		{"agent_delegate", []string{"pin_reason"}, "locality: the work needs something only this box has (a local service, files the contract cannot carry inline, this box's own seat as the thing under comparison); route local only."},
		{"agent_delegate", []string{"remotes"}, "A call can only narrow the fleet, never add a node: a URL outside delegate_remotes is refused and never dialled, and a box with no delegate_remotes accepts none."},
		{"agent_delegate", []string{"subtasks", "items", "properties", "output_schema"}, "REQUIRED for any remote placement"},
		{"agent_delegate", []string{"subtasks", "items", "properties", "allow_browse"}, "Admitted only with route local, this node's agent_allow_browse, a configured lane and browse_hosts; judged unattended (publish/send-class controls always refused), audited."},
		{"agent_run", []string{"thinking"}, "on: never send the kwarg"},
		{"agent_run", []string{"thinking"}, "off: every planner call renders in non-thinking mode (chat_template_kwargs enable_thinking:false) — use for grounded extraction on a thinking seat that spends its budget in the think block."},
		{"agent_run", []string{"judge"}, "Never gates anything."},
		{"offload_generate_video", []string{"fast"}, "The default is the native quality recipe — only set when the caller explicitly accepts draft quality"},
		{"offload_generate_image", []string{"waiter_token"}, "mq-...: the ticket the harness is holding for this request, which starts on the first matching lane that frees anywhere; do NOT call again to wait for it (a repeat of the identical request attaches to the same ticket), and send the SAME request with waiter_token=mq-... only to collect the result."},
	}
	for _, p := range pins {
		f, ok := full[p.tool]
		if !ok {
			t.Errorf("%s is not in the full list", p.tool)
			continue
		}
		fp := paramDescription(t, f, p.path...)
		cp := paramDescription(t, compact[p.tool], p.path...)
		if !strings.Contains(fp, p.sentence) {
			t.Errorf("%s %v: the pinned sentence is not in the full description any more (reworded?): %q", p.tool, p.path, p.sentence)
			continue
		}
		if !strings.Contains(cp, p.sentence) {
			t.Errorf("%s %v: compact dropped %q\n compact description: %s", p.tool, p.path, p.sentence, cp)
		}
	}
}

// TestCompactKeepsTheReferentOfAKeptSentence: a kept sentence that cannot be read without its neighbour comes with it. "Non-zero
// means a node is broken" is about summary.infrastructure, and "on: never send the kwarg" is one of three enum-value lines.
func TestCompactKeepsTheReferentOfAKeptSentence(t *testing.T) {
	compact := map[string]*mcp.Tool{}
	for _, tool := range listTools(t, fullSurfaceCfg("compact")) {
		compact[tool.Name] = tool
	}
	d := compact["agent_delegate"].Description
	const anaphor = "Non-zero means a node is broken or misconfigured"
	const referent = "summary.infrastructure counts the results whose story is a broken STACK"
	if !strings.Contains(d, anaphor) {
		t.Fatalf("agent_delegate compact no longer keeps %q", anaphor)
	}
	if !strings.Contains(d, referent) {
		t.Errorf("agent_delegate compact keeps %q without the sentence it refers to (%q)", anaphor, referent)
	}
	th := paramDescription(t, compact["agent_run"], "thinking")
	for _, line := range []string{"auto (default;", "off: every planner call", "on: never send the kwarg"} {
		if !strings.Contains(th, line) {
			t.Errorf("agent_run.thinking compact is missing the enum-value line %q: %s", line, th)
		}
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
		{"an ellipsis followed by a lower-case word does not split", "send the SAME request with waiter_token=mq-... only to collect the result. Next.", []string{"send the SAME request with waiter_token=mq-... only to collect the result.", "Next."}},
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

// TestStripProseKeepsPropertiesNamedLikeProseKeywords: stripProse is the independent half of the structure comparison, so it must not
// remove a PARAMETER that happens to be called title, example, examples or $comment: under a properties map those keys are names,
// not keywords. A compact regression that dropped such a parameter would otherwise compare equal.
func TestStripProseKeepsPropertiesNamedLikeProseKeywords(t *testing.T) {
	in := map[string]any{
		"type":  "object",
		"title": "T",
		"properties": map[string]any{
			"title":    map[string]any{"type": "string", "description": "d", "examples": []any{"x"}},
			"example":  map[string]any{"type": "string"},
			"examples": map[string]any{"type": "array", "items": map[string]any{"type": "string", "title": "i"}},
		},
	}
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":    map[string]any{"type": "string"},
			"example":  map[string]any{"type": "string"},
			"examples": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
	if got := stripProse(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("stripProse dropped a property name or kept prose\n got:  %v\n want: %v", got, want)
	}
}

// TestCompactSchemaKeepsPropertiesNamedLikeProseKeywords: the production walker has the same obligation.
func TestCompactSchemaKeepsPropertiesNamedLikeProseKeywords(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","description":"What this field is for, in full. More words follow here."},"example":{"type":"string"},"examples":{"type":"array","items":{"type":"string"}}},"required":["title"]}`)
	got, ok := compactInputSchema(in).(json.RawMessage)
	if !ok {
		t.Fatalf("compactInputSchema returned %T", compactInputSchema(in))
	}
	want := `{"type":"object","properties":{"title":{"type":"string","description":"What this field is for, in full."},"example":{"type":"string"},"examples":{"type":"array","items":{"type":"string"}}},"required":["title"]}`
	if string(got) != want {
		t.Fatalf("\n got  %s\n want %s", got, want)
	}
}
