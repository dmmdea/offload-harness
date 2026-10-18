package fleetnode

import (
	"encoding/json"
	"strings"
	"testing"
)

// A composition's own html or project_dir is refused on the dispatch door whoever sends it, and the error the caller reads says
// why in terms of the rules as they are: the door needs the fleet bearer like every lane and renders the node's vetted
// templates; a whole project goes to a node that opted in to the project door, which also needs the bearer. It must not
// describe the door as open, and it must not say the bearer is limited to templates, which it is not on such a node.
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
		if strings.Contains(msg, "code of its own") {
			t.Errorf("%s: the refusal says the bearer does not run a caller's own code, which a node that opted in to the project door does: %q", name, msg)
		}
		if !strings.Contains(msg, ComposeProjectPath) || !strings.Contains(msg, "opted in") {
			t.Errorf("%s: the refusal does not point the caller at the project door of a node that opted in (%s): %q", name, ComposeProjectPath, msg)
		}
	}
}
