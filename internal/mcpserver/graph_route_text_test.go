package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// A whole-node graph is not a ticket in the release that carries both the ticket queue and the graph overflow (ADR 0083
// decision 12, ADR 0082 Amendment 1): the text a caller reads on the graph tool and on the route every media door shares
// must say so, and must still not carry the S1 sentence the image test forbids.
func TestTheGraphToolSaysItIsNotATicket(t *testing.T) {
	var (
		desc string
		raw  []byte
	)
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name == "offload_run_graph" {
			desc = tool.Description
			raw, _ = json.Marshal(tool.InputSchema)
		}
	}
	if raw == nil {
		t.Fatal("offload_run_graph is not advertised")
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	route := schema.Properties["route"].Description
	for _, want := range []string{"NOT a ticket", "does not wait in the cluster queue yet"} {
		if !strings.Contains(route, want) {
			t.Errorf("the route text must say %q: %s", want, route)
		}
	}
	if strings.Contains(route, "is never moved") {
		t.Errorf("the route text carries the S1 sentence: %s", route)
	}
	for _, want := range []string{"A graph is not a ticket", "does not wait in the cluster queue yet"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the graph tool must say %q: %s", want, desc)
		}
	}
}
