package mcpserver

// agent_delegate's route description is what a model reads to phrase its goal (the diagnosis' F12).
//
// It used to say that mechanical goals "take the smallest eligible seat so the roomier one stays free".
// That was the rule before W-11 (ADR 0050): since then mechanical work ranks by expected completion
// (queue wait, cold load and generation at the seat's measured rate), the window only breaking a tie, so
// a fast roomy seat takes it. A caller who believed the old text would phrase goals to steer work to a
// small seat that the engine no longer prefers.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func TestAgentDelegateRouteSchemaDescribesTheEtaFirstDeal(t *testing.T) {
	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	var description string
	for _, tool := range listTools(t, cfg) {
		if tool.Name != "agent_delegate" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("schema not JSON: %v", err)
		}
		description = schema.Properties["route"].Description
	}
	if description == "" {
		t.Fatal("agent_delegate has no route description (is it registered?)")
	}
	for _, want := range []string{"take the roomiest eligible seat", "expected to FINISH first", "only breaks a tie"} {
		if !strings.Contains(description, want) {
			t.Errorf("route description does not say %q", want)
		}
	}
	for _, stale := range []string{"smallest eligible seat", "so the roomier one stays free"} {
		if strings.Contains(description, stale) {
			t.Errorf("route description still says %q: mechanical work ranks by expected completion since ADR 0050", stale)
		}
	}
}
