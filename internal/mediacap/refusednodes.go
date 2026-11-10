package mediacap

import (
	"encoding/json"
	"fmt"
)

// The node classes a run-graph door refuses by name.
//
// A graph is data, but a node pack can ship a class that executes one of its string inputs as code; once that pack is
// installed in the ComfyUI the node uses, a graph holding the class runs the caller's code on this node. Nothing
// legitimate submits such a class through the door, so a graph that holds one is refused with the class named, before a
// file is written, a job exists or a lease is taken. render/refused-node-classes.json is the runner's copy of the same table
// and the file a reviewer edits (the runner refuses on it too, before it starts ComfyUI, for a graph that reaches it another
// way, and for a template's converted graph); TestNodePackRowsMatchTheRunnersFile holds the two together.
//
// The match is exact and case-sensitive (see render/refused-node-classes.mjs): `class_type` of a node of an API-format
// graph, `type` of a node of a UI-format workflow (the entries of a "nodes" list), read anywhere in the document.

// RefusedNodeClass is one class the door refuses: the class, the pack that ships it, and what it does with caller input.
type RefusedNodeClass struct {
	Class string
	Pack  string
	Why   string
}

// refusedNodeClasses mirrors render/refused-node-classes.json, row for row.
var refusedNodeClasses = []RefusedNodeClass{
	{"ModelTest", "ComfyUI-Image-Filters", "runs its `code` STRING input through exec()"},
}

// RefusedNodeClasses returns the refused classes.
func RefusedNodeClasses() []RefusedNodeClass {
	return append([]RefusedNodeClass(nil), refusedNodeClasses...)
}

// NodeClassRefusedError is a graph's refusal: it holds a node class that executes caller-supplied code.
type NodeClassRefusedError struct {
	Row RefusedNodeClass
}

func (e *NodeClassRefusedError) Error() string {
	return fmt.Sprintf("NODE_CLASS_REFUSED: the graph holds a node of class %s (pack %s), which %s, so it would run the caller's code on this node; remove the %s node from the graph (render/refused-node-classes.json)", e.Row.Class, e.Row.Pack, e.Row.Why, e.Row.Class)
}

// CheckGraphNodeClasses refuses a run-graph graph that holds a refused node class, with a *NodeClassRefusedError naming the
// first listed class it holds. A document that is not JSON is not this check's to refuse (the caller has decoded it already).
func CheckGraphNodeClasses(graph []byte) error {
	var doc any
	if err := json.Unmarshal(graph, &doc); err != nil {
		return nil
	}
	found := map[string]bool{}
	collectNodeClasses(doc, found)
	for _, row := range refusedNodeClasses {
		if found[row.Class] {
			return &NodeClassRefusedError{Row: row}
		}
	}
	return nil
}

// collectNodeClasses is render/refused-node-classes.mjs graphNodeClasses: every class_type string of an object, and the type
// string of each object in a "nodes" list, anywhere in the document.
func collectNodeClasses(v any, found map[string]bool) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			collectNodeClasses(e, found)
		}
	case map[string]any:
		if s, ok := x["class_type"].(string); ok {
			found[s] = true
		}
		for k, e := range x {
			if list, ok := e.([]any); ok && k == "nodes" {
				for _, n := range list {
					if obj, ok := n.(map[string]any); ok {
						if s, ok := obj["type"].(string); ok {
							found[s] = true
						}
					}
				}
			}
			collectNodeClasses(e, found)
		}
	}
}
