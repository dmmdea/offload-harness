package mcpserver

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// enumNode is a parameter with an enum, at any depth of a tool's input schema, or one with no enum keyword whose description opens by
// listing its values ("full | lightning8 | lightning4 — a matched triple").
type enumNode struct {
	path   string
	values []string
	desc   string
}

// valueListLead matches a description that opens with a list of identifiers joined by pipes, the way a parameter without an enum
// keyword names its choices: "full | lightning8 | lightning4 — a MATCHED steps+cfg+LoRA triple".
var valueListLead = regexp.MustCompile(`^\s*([A-Za-z0-9_.-]+(?:\s*\|\s*[A-Za-z0-9_.-]+)+)`)

// listedValues returns the values a description opens by listing, or nil.
func listedValues(desc string) []string {
	m := valueListLead.FindStringSubmatch(desc)
	if m == nil {
		return nil
	}
	var out []string
	for _, v := range strings.Split(m[1], "|") {
		out = append(out, strings.TrimSpace(v))
	}
	return out
}

// enumNodes returns every schema node that has an enum, and every one that opens its description by listing its values, keyed by its path
// ("#/properties/route"). The values are written the way a description writes them: a string as it is, a number in its shortest decimal
// form, and the empty string (a "not set" value no sentence can explain) is left out.
func enumNodes(schema any) map[string]enumNode {
	out := map[string]enumNode{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		n, ok := v.(map[string]any)
		if !ok {
			return
		}
		if raw, ok := n["enum"].([]any); ok {
			node := enumNode{path: path}
			node.desc, _ = n["description"].(string)
			for _, e := range raw {
				switch x := e.(type) {
				case string:
					if x != "" {
						node.values = append(node.values, x)
					}
				case float64:
					node.values = append(node.values, strconv.FormatFloat(x, 'f', -1, 64))
				}
			}
			out[path] = node
		} else if desc, _ := n["description"].(string); desc != "" {
			if vals := listedValues(desc); len(vals) > 0 {
				out[path] = enumNode{path: path, values: vals, desc: desc}
			}
		}
		if props, ok := n["properties"].(map[string]any); ok {
			for name, child := range props {
				walk(path+"/properties/"+name, child)
			}
		}
		if items, ok := n["items"]; ok {
			walk(path+"/items", items)
		}
	}
	walk("#", schema)
	return out
}

// enumTermEdge is what may precede a value for it to be a term of its own: not the tail of a longer word, number or identifier, and not
// the digit of a negative number or a version.
const enumTermEdge = `(?:^|[^A-Za-z0-9_.-])`

// enumConnectives are the words that join the items of a plain list ("build" or "general"); a quoted value followed by one of them is
// being listed, not explained.
var enumConnectives = map[string]bool{"or": true, "and": true, "nor": true, "vs": true, "versus": true}

// enumValueExplained reports whether a text explains the enum value v: it names the value as a term of its own (not inside a longer
// word, number or negative number) in one of the shapes a description explains a value in.
//
//   - the value, alone or as one member of an a|b|c list, then "=", ":" or "(": "value = meaning", "value: meaning", "value (meaning)",
//     "birds|insects|plants (MobileNet v2 iNat)", "lightning8 (default, ~4x faster)", "local:{...}". A quote or backtick around the value
//     is allowed.
//   - the value in quotes, then a word that is not a list connective: `Prefer "build" for ...`, `"general" advertises everything`.
//
// A bare mention in running prose ("route local or remote", "local pins it to this box") is not one of these shapes, and a bare 1 inside
// "-1 = ..." or "1,024" is not the value 1.
func enumValueExplained(desc, v string) bool {
	q := regexp.QuoteMeta(v)
	listed := regexp.MustCompile(enumTermEdge + `(?:[A-Za-z0-9_.-]+\|)*["'` + "`" + `]?` + q + `["'` + "`" + `]?(?:\|[A-Za-z0-9_.-]+)*\s*(?:=|:|\()`)
	if listed.MatchString(desc) {
		return true
	}
	quoted := regexp.MustCompile(enumTermEdge + `["'` + "`" + `]` + q + `["'` + "`" + `]\s+([A-Za-z]+)`)
	for _, m := range quoted.FindAllStringSubmatch(desc, -1) {
		if !enumConnectives[strings.ToLower(m[1])] {
			return true
		}
	}
	return false
}

// enumExplainedIn reports whether any of the texts explains v.
func enumExplainedIn(v string, texts ...string) bool {
	for _, t := range texts {
		if enumValueExplained(t, v) {
			return true
		}
	}
	return false
}

// droppedEnumValue names one enum value that compact is allowed to leave unexplained although the full description explains it, and
// why that is safe.
type droppedEnumValue struct {
	Tool, Path, Value, Reason string
}

// droppedEnumValues are the values the check found that compact may leave unexplained. Two groups.
//
// The transcribe route's own two lines (auto, local) are explained once in the kept tool-level route sentence, which names all three
// values: the default auto, local pins it to this box and remote forces a fleet node (the remote line stays too, because it carries the
// deferred:true protocol).
//
// The offload_status sections that name a block of the status result (local, fleet, media, remote, accelerators, pair, gpu_lease): the
// full description explains each one only as the first key of that block inside its "Returns {...}" result shape (`local:{endpoint,
// roster{...}}`), the section value is the block's own name, and the contents of a block are a result shape that compact drops as one.
// The kept sentences say what a caller needs from the blocks: trust the live roster over a written figure, read verdict before concluding
// anything from held, and what idle_agent_nodes above zero means.
var droppedEnumValues = []droppedEnumValue{
	{Tool: "offload_forecast", Path: "#/properties/route", Value: "local", Reason: "the kept tool-level route sentence says local pins the work to this box, which is all the parameter's own local line adds"},
	{Tool: "offload_transcribe", Path: "#/properties/route", Value: "auto", Reason: "the kept tool-level route sentence says route (default auto) places the work, on this box's whisper when it is free and on a fleet node's otherwise, and that the audio is sent as 16 kHz mono Opus"},
	{Tool: "offload_transcribe", Path: "#/properties/route", Value: "local", Reason: "the kept tool-level route sentence says local pins the work to this box, which is all the parameter's own local line adds"},
	{Tool: "offload_status", Path: "#/properties/section", Value: "local", Reason: "section local returns the local block, whose contents (the roster of seats and the model ids served now) are a result shape the dropped Returns sentence lists; the section value is the block's name"},
	{Tool: "offload_status", Path: "#/properties/section", Value: "fleet", Reason: "section fleet returns the fleet block, whose contents (the live delegation roster) are a result shape; the kept sentences say to trust it over a written figure and what idle_agent_nodes above zero means"},
	{Tool: "offload_status", Path: "#/properties/section", Value: "media", Reason: "section media returns the media block, this machine's configured generation engines, a result shape the dropped Returns sentence lists; the section value is the block's name"},
	{Tool: "offload_status", Path: "#/properties/section", Value: "remote", Reason: "section remote returns the remote block (the hosted-endpoint settings and whether a key is present), a result shape; the kept sentence on offload_nim says it is the only remote tool"},
	{Tool: "offload_status", Path: "#/properties/section", Value: "accelerators", Reason: "section accelerators returns the accelerators block, present only when this box lists an accelerator device, a result shape; the section value is the block's name"},
	{Tool: "offload_status", Path: "#/properties/section", Value: "pair", Reason: "section pair returns the pair block, present only when pair workloads are enabled, a result shape; the section value is the block's name"},
	{Tool: "offload_status", Path: "#/properties/section", Value: "gpu_lease", Reason: "section gpu_lease returns the gpu_lease block (held, verdict, activity, queue_with), a result shape; the kept sentence says to read verdict before concluding anything from held"},
}

func isDroppedEnumValue(tool, path, value string) bool {
	for _, d := range droppedEnumValues {
		if d.Tool == tool && d.Path == path && d.Value == value {
			return true
		}
	}
	return false
}

// enumExplanationLosses compares one tool's full and compact forms. For every enum value of every parameter (at any depth) that the full
// list explains in the parameter's own description or in the tool's description, it requires that the compact list explains it in one of
// the same two places, unless the value is on droppedEnumValues. It returns how many values the full list explains and one loss per value
// the compact list leaves unexplained, keyed "tool path value" with the message to report.
func enumExplanationLosses(ft, ct *mcp.Tool) (explained int, losses map[string]string) {
	losses = map[string]string{}
	fullNodes, compactNodes := enumNodes(ft.InputSchema), enumNodes(ct.InputSchema)
	paths := make([]string, 0, len(fullNodes))
	for p := range fullNodes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fn := fullNodes[p]
		cn, ok := compactNodes[p]
		if !ok {
			losses[fmt.Sprintf("%s %s gone", ft.Name, p)] = fmt.Sprintf("%s %s: the enum is gone from the compact schema", ft.Name, p)
			continue
		}
		for _, v := range fn.values {
			if !enumExplainedIn(v, fn.desc, ft.Description) {
				continue
			}
			explained++
			if enumExplainedIn(v, cn.desc, ct.Description) || isDroppedEnumValue(ft.Name, p, v) {
				continue
			}
			where := "the parameter's description"
			if !enumValueExplained(fn.desc, v) {
				where = "the tool's description"
			}
			losses[fmt.Sprintf("%s %s value %q", ft.Name, p, v)] = fmt.Sprintf("%s %s value %q is explained in %s of the full list and not in the compact one:\n full parameter: %s\n compact parameter: %s\n full tool: %s\n compact tool: %s", ft.Name, p, v, where, fn.desc, cn.desc, ft.Description, ct.Description)
		}
	}
	return explained, losses
}

// TestCompactExplainsEveryEnumValueTheFullListExplains: the compact schema keeps every enum value, but a value is only usable when the
// description says what it does. For every parameter of every tool on every surface, each enum value that the full list explains, in the
// parameter's own description or in the tool's (a value-then-meaning shape, a quoted value that the prose then speaks of, or a member of an
// a|b|c list that a parenthesis explains), is explained in the compact list too, unless it is on droppedEnumValues with a reason.
func TestCompactExplainsEveryEnumValueTheFullListExplains(t *testing.T) {
	explainedInFull, lost := 0, map[string]bool{}
	for _, surface := range gateSurfaces() {
		compact := map[string]*mcp.Tool{}
		for _, tool := range listTools(t, surface.cfg("compact")) {
			compact[tool.Name] = tool
		}
		for _, ft := range listTools(t, surface.cfg("full")) {
			n, losses := enumExplanationLosses(ft, compact[ft.Name])
			explainedInFull += n
			keys := make([]string, 0, len(losses))
			for k := range losses {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if !lost[k] {
					lost[k] = true
					t.Errorf("%s: %s", surface.name, losses[k])
				}
			}
		}
	}
	if explainedInFull < 40 {
		t.Fatalf("only %d enum values were found explained in a full list: the check is blind", explainedInFull)
	}
}

// TestEnumCheckReadsTheToolLevelDescriptionToo proves the tool's own description is read, not only the parameter's: a value that only
// the tool-level sentence explains, and that the compact list then leaves unexplained, is reported; one the compact tool description
// still explains, or that is on the dropped list, is not.
func TestEnumCheckReadsTheToolLevelDescriptionToo(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{"preset": map[string]any{"enum": []any{"lightning8", "full"}, "description": "which preset"}}}
	full := &mcp.Tool{Name: "synthetic_enum_tool", InputSchema: schema, Description: "Edit the image for the caller here. preset trades speed for fidelity there: lightning8 (default, ~4x faster) or full."}
	cut := &mcp.Tool{Name: full.Name, InputSchema: schema, Description: "Edit the image for the caller here."}
	if n, losses := enumExplanationLosses(full, cut); n != 1 || len(losses) != 1 {
		t.Fatalf("a value explained only in the tool-level sentence and cut from compact was not reported once (explained %d, losses %v)", n, losses)
	}
	if n, losses := enumExplanationLosses(full, full); n != 1 || len(losses) != 0 {
		t.Fatalf("an unchanged pair was reported (explained %d, losses %v)", n, losses)
	}
	moved := &mcp.Tool{Name: full.Name, InputSchema: map[string]any{"properties": map[string]any{"preset": map[string]any{"enum": []any{"lightning8", "full"}, "description": "lightning8 (default, ~4x faster)"}}}, Description: "Edit the image for the caller here."}
	if _, losses := enumExplanationLosses(full, moved); len(losses) != 0 {
		t.Fatalf("a value explained by the compact parameter description instead of the tool description was reported: %v", losses)
	}
	droppedEnumValues = append(droppedEnumValues, droppedEnumValue{Tool: full.Name, Path: "#/properties/preset", Value: "lightning8", Reason: "synthetic entry for this test, removed when it ends"})
	t.Cleanup(func() { droppedEnumValues = droppedEnumValues[:len(droppedEnumValues)-1] })
	if _, losses := enumExplanationLosses(full, cut); len(losses) != 0 {
		t.Fatalf("a value on the dropped list was reported: %v", losses)
	}
}

// TestDroppedEnumValuesAreLiveAndReasoned: an entry on the dropped-enum list names a value that is really explained in the full
// list (in the parameter's description or the tool's) and absent from the compact one, and says why that is safe.
func TestDroppedEnumValuesAreLiveAndReasoned(t *testing.T) {
	for i, d := range droppedEnumValues {
		if len(strings.TrimSpace(d.Reason)) < minExplicitReason {
			t.Errorf("dropped enum value %d (%s %s %q) needs a reason of at least %d characters", i, d.Tool, d.Path, d.Value, minExplicitReason)
		}
		live := false
		for _, surface := range gateSurfaces() {
			compact := map[string]*mcp.Tool{}
			for _, tool := range listTools(t, surface.cfg("compact")) {
				compact[tool.Name] = tool
			}
			for _, ft := range listTools(t, surface.cfg("full")) {
				if ft.Name != d.Tool {
					continue
				}
				fn, ok := enumNodes(ft.InputSchema)[d.Path]
				cn := enumNodes(compact[ft.Name].InputSchema)[d.Path]
				if ok && enumExplainedIn(d.Value, fn.desc, ft.Description) && !enumExplainedIn(d.Value, cn.desc, compact[ft.Name].Description) {
					live = true
				}
			}
		}
		if !live {
			t.Errorf("dropped enum value %d (%s %s %q) is not a value the full description explains and the compact one does not", i, d.Tool, d.Path, d.Value)
		}
	}
}

// TestEnumNodesReadAValueListThatHasNoEnumKeyword: a parameter whose schema has no enum but whose description opens with "a | b | c"
// is a choice among those values (offload_edit_image_generative's preset is one), so the enum check reads it.
func TestEnumNodesReadAValueListThatHasNoEnumKeyword(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{
		"preset": map[string]any{"type": "string", "description": "full | lightning8 | lightning4 — a MATCHED steps+cfg+LoRA triple (2511 binding only)"},
		"route":  map[string]any{"type": "string", "enum": []any{"", "local", "remote"}, "description": "local | remote would not be read twice"},
		"note":   map[string]any{"type": "string", "description": "the text to send | not a list of values because it opens with prose"},
		"one":    map[string]any{"type": "string", "description": "single value only"},
	}}
	nodes := enumNodes(schema)
	if got := nodes["#/properties/preset"].values; !slices.Equal(got, []string{"full", "lightning8", "lightning4"}) {
		t.Errorf("a description that opens with a value list gave %q", got)
	}
	if got := nodes["#/properties/route"].values; !slices.Equal(got, []string{"local", "remote"}) {
		t.Errorf("a parameter with an enum keyword gave %q, want its enum (the empty string left out)", got)
	}
	if _, ok := nodes["#/properties/note"]; ok {
		t.Errorf("a description that merely contains a pipe was read as a value list")
	}
	if _, ok := nodes["#/properties/one"]; ok {
		t.Errorf("a description with no list was read as a value list")
	}
}

func TestEnumValueExplainedReadsTheShapeOfAnExplanation(t *testing.T) {
	const buildProse = "Prefer \"build\" for reading and reasoning over a codebase; \"general\" advertises everything."
	const domainProse = "domain=imagenet (default; EfficientNet-EdgeTPU-S, 1000 ImageNet classes) or birds|insects|plants (MobileNet v2 iNat)."
	const priority = "scheduling band for every subtask (default 0 = production). -1 = SHEDDABLE: measurement traffic. 1 = urgent (claimed first on every node)."
	for _, tc := range []struct {
		desc, value string
		want        bool
	}{
		{priority, "0", true},
		{priority, "-1", true},
		{priority, "1", true},
		{"scheduling band (default 0 = production). -1 = SHEDDABLE: traffic.", "1", false}, // the 1 of -1 is not the value 1
		{"at most 1,024 steps", "1", false},
		{"measurement: the call measures a place; route local or remote.", "local", false}, // running prose
		{"privacy: the material may not leave this box", "privacy", true},
		{"auto (the DEFAULT): this box first", "auto", true},
		{"local pins it to this box", "local", false},
		{"remote: force a fleet node", "remote", true},
		{"autopilot: something else", "auto", false},
		// A quoted value that the prose then speaks of, and a member of an a|b|c list that a parenthesis explains.
		{buildProse, "build", true},
		{buildProse, "general", true},
		{"Prefer \"build\" or \"general\" for most work.", "build", false}, // listed, not explained
		{"Prefer \"build\" or \"general\"", "general", false},
		{domainProse, "imagenet", true},
		{domainProse, "birds", true},
		{domainProse, "insects", true},
		{domainProse, "plants", true},
		{domainProse, "mobilenet", false},
		{"preset trades speed for fidelity there: lightning8 (default, ~4x faster) or full.", "lightning8", true},
		{"preset trades speed for fidelity there: lightning8 (default, ~4x faster) or full.", "full", false},
		{"quality defaults to this machine's compose_quality (high = libx264 slow CRF 15).", "high", true},
		{"Returns {local:{endpoint, roster{workhorse}}, fleet:{delegation_enabled}, gpu_lease:{held, verdict}}", "gpu_lease", true},
		{"Returns {local:{endpoint, roster{workhorse}}, fleet:{delegation_enabled}, gpu_lease:{held, verdict}}", "lease", false},
		{"space=tinyclip (default) or siglip2 (stronger: 73.0 vs 67.8 top-1).", "tinyclip", true},
		{"space=tinyclip (default) or siglip2 (stronger: 73.0 vs 67.8 top-1).", "siglip2", true},
	} {
		if got := enumValueExplained(tc.desc, tc.value); got != tc.want {
			t.Errorf("enumValueExplained(%q, %q) = %v, want %v", tc.desc, tc.value, got, tc.want)
		}
	}
}
