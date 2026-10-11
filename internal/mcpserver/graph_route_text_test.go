package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// A whole-node graph is a ticket (ADR 0083 Amendment 1, which supersedes decision 12): the text a caller reads on the graph tool and
// on the route every media door shares must say so, tell the caller not to loop, advertise the window the call answers inside, and
// still not carry the S1 sentence the image test forbids.
func TestTheGraphToolAdvertisesTheTicketAndWaitS(t *testing.T) {
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
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if w := schema.Properties["wait_s"]; w.Type != "integer" || !strings.Contains(w.Description, "answers 'queued'") {
		t.Errorf("wait_s: %+v", w)
	}
	for _, want := range []string{"mq-", "do NOT call again", "collect the result", "tk-"} {
		if !strings.Contains(schema.Properties["waiter_token"].Description, want) {
			t.Errorf("waiter_token must say %q: %s", want, schema.Properties["waiter_token"].Description)
		}
	}
	route := schema.Properties["route"].Description
	for _, want := range []string{"TICKET", "ADR 0083", "mq-", "whole-node run_graph", "stays on this machine"} {
		if !strings.Contains(route, want) {
			t.Errorf("the route text must say %q: %s", want, route)
		}
	}
	for _, gone := range []string{"is never moved", "on THIS machine only", "NOT a ticket", "does not wait in the cluster queue yet"} {
		if strings.Contains(route, gone) || strings.Contains(desc, gone) {
			t.Errorf("the text still describes the one-shot path (%q): %s / %s", gone, route, desc)
		}
	}
	for _, want := range []string{"becomes a TICKET", "do not call again", "waiter_token=mq-", "not-capable", "stays on this machine", "keeps its own copy of the graph"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the graph tool must say %q: %s", want, desc)
		}
	}
}
