package mcpserver

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// enumNode is a parameter with an enum, at any depth of a tool's input schema.
type enumNode struct {
	path   string
	values []string
	desc   string
}

// enumNodes returns every schema node that has an enum, keyed by its path ("#/properties/route"). The values are written the way a
// description writes them: a string as it is, a number in its shortest decimal form, and the empty string (a "not set" value no
// sentence can explain) is left out.
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

// enumValueExplained reports whether a description explains the enum value v: it names the value as a term of its own (not inside a
// longer word, number or negative number) and then says what it means, as "value = meaning", "value: meaning" or "value (meaning)".
// A value that only appears in running prose ("route local or remote") is not explained by that, and a bare 1 inside "-1 = ..." or
// "1,024" is not the value 1.
func enumValueExplained(desc, v string) bool {
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(v) + `\s*(?:=|:|\()`)
	return re.MatchString(desc)
}

// droppedEnumValue names one enum value that compact is allowed to leave unexplained although the full description explains it, and
// why that is safe.
type droppedEnumValue struct {
	Tool, Path, Value, Reason string
}

// droppedEnumValues may be empty, and is: every value the check found was kept instead.
var droppedEnumValues = []droppedEnumValue{}

func isDroppedEnumValue(tool, path, value string) bool {
	for _, d := range droppedEnumValues {
		if d.Tool == tool && d.Path == path && d.Value == value {
			return true
		}
	}
	return false
}

// TestCompactExplainsEveryEnumValueTheFullListExplains: the compact schema keeps every enum value, but a value is only usable when the
// description says what it does. For every parameter of every tool on every surface, each enum value that the full description
// explains is explained in the compact description too, unless it is on droppedEnumValues with a reason.
func TestCompactExplainsEveryEnumValueTheFullListExplains(t *testing.T) {
	explainedInFull, lost := 0, map[string]bool{}
	for _, surface := range gateSurfaces() {
		compact := map[string]*mcp.Tool{}
		for _, tool := range listTools(t, surface.cfg("compact")) {
			compact[tool.Name] = tool
		}
		for _, ft := range listTools(t, surface.cfg("full")) {
			fullNodes, compactNodes := enumNodes(ft.InputSchema), enumNodes(compact[ft.Name].InputSchema)
			paths := make([]string, 0, len(fullNodes))
			for p := range fullNodes {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			for _, p := range paths {
				fn := fullNodes[p]
				cn, ok := compactNodes[p]
				if !ok {
					t.Errorf("%s: %s %s: the enum is gone from the compact schema", surface.name, ft.Name, p)
					continue
				}
				for _, v := range fn.values {
					if !enumValueExplained(fn.desc, v) {
						continue
					}
					explainedInFull++
					if enumValueExplained(cn.desc, v) || isDroppedEnumValue(ft.Name, p, v) {
						continue
					}
					key := fmt.Sprintf("%s %s value %q", ft.Name, p, v)
					if !lost[key] {
						lost[key] = true
						t.Errorf("%s is explained in the full description and not in the compact one:\n full:    %s\n compact: %s", key, fn.desc, cn.desc)
					}
				}
			}
		}
	}
	if explainedInFull < 40 {
		t.Fatalf("only %d enum values were found explained in a full description: the check is blind", explainedInFull)
	}
}

// TestDroppedEnumValuesAreLiveAndReasoned: an entry on the dropped-enum list names a value that is really explained in the full
// description and absent from the compact one, and says why that is safe.
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
				if ok && enumValueExplained(fn.desc, d.Value) && !enumValueExplained(cn.desc, d.Value) {
					live = true
				}
			}
		}
		if !live {
			t.Errorf("dropped enum value %d (%s %s %q) is not a value the full description explains and the compact one does not", i, d.Tool, d.Path, d.Value)
		}
	}
}

func TestEnumValueExplainedReadsTheShapeOfAnExplanation(t *testing.T) {
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
	} {
		if got := enumValueExplained(tc.desc, tc.value); got != tc.want {
			t.Errorf("enumValueExplained(%q, %q) = %v, want %v", tc.desc, tc.value, got, tc.want)
		}
	}
}
