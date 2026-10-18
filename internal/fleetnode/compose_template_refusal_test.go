package fleetnode

import (
	"encoding/json"
	"strings"
	"testing"
)

// A composition's own html or project_dir is refused over the fleet whoever sends it, and the error the caller reads says
// why in terms of the rules as they are: the door needs the fleet bearer like every lane, and a holder of the bearer renders
// the node's vetted templates, not code of its own. It must not describe the door as open.
func TestTheRefusalOfACompositionsOwnCodeDoesNotCallTheDoorOpen(t *testing.T) {
	for name, payload := range map[string]string{
		"html":        `{"html":"<p>x</p>"}`,
		"project_dir": `{"project_dir":"/opt/project"}`,
	} {
		_, _, err := buildComposeVideo(json.RawMessage(payload))
		if err == nil {
			t.Fatalf("%s: a composition's own code was accepted over the fleet", name)
		}
		msg := err.Error()
		if strings.Contains(msg, "token-gated") {
			t.Errorf("%s: the refusal describes the door as open: %q", name, msg)
		}
		if !strings.Contains(msg, "fleet bearer") {
			t.Errorf("%s: the refusal does not say the door needs the fleet bearer: %q", name, msg)
		}
		if !strings.Contains(msg, "vetted template") {
			t.Errorf("%s: the refusal does not point the caller at a vetted template: %q", name, msg)
		}
	}
}
