package mcpserver

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// notePathArg finds a "<something>_path" argument name in a forwarding note.
var notePathArg = regexp.MustCompile(`\b[a-z][a-z_]*_path\b`)

// TestFleetAcceleratorNoteNamesTheToolsOwnArgument: on the fleet surface every forwarder advertises its table row's description
// plus a "[FLEET: ...]" note, and the note says which of the caller's data travels with the job. It must name the argument the
// tool really takes: the row's required argument appears in the note, and no argument the tool does not have (a text tower has
// no image_path) is named.
func TestFleetAcceleratorNoteNamesTheToolsOwnArgument(t *testing.T) {
	forwarders := 0
	for _, id := range []string{"hailo-8l", "coral-edgetpu", "rknpu"} {
		byName := map[string]accelMCPTool{}
		for _, row := range accelMCPTools(id) {
			byName[row.name] = row
		}
		for _, tool := range listTools(t, deviceSurfaceCfg(id, true)("full")) {
			row, ok := byName[tool.Name]
			if !ok {
				continue // the delegation, browse and media tools
			}
			i := strings.Index(tool.Description, "[FLEET:")
			if i < 0 {
				t.Errorf("%s %s: a forwarder with no [FLEET: ...] note", id, tool.Name)
				continue
			}
			forwarders++
			note := tool.Description[i:]

			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			}
			if err := json.Unmarshal([]byte(row.schema), &schema); err != nil {
				t.Fatalf("%s %s: schema: %v", id, tool.Name, err)
			}
			if !slices.Contains(schema.Required, row.arg) {
				t.Fatalf("%s %s: the table row says it cannot run without %q but the schema requires %v", id, tool.Name, row.arg, schema.Required)
			}
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(row.arg) + `\b`).MatchString(note) {
				t.Errorf("%s %s: the note does not name the required argument %q: %s", id, tool.Name, row.arg, note)
			}
			for _, name := range notePathArg.FindAllString(note, -1) {
				if _, has := schema.Properties[name]; !has {
					t.Errorf("%s %s: the note names %q, which the tool does not take: %s", id, tool.Name, name, note)
				}
			}
		}
	}
	if forwarders < 18 {
		t.Fatalf("read %d forwarders, want all 18 (11 + 4 + 3): the check is blind", forwarders)
	}
}
