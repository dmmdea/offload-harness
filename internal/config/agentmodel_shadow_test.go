package config

import (
	"bytes"
	"strings"
	"testing"
)

// C-95: on a box that declares layers the placement table picks the agent seat
// from them, and agent_model only applies when a decision names no seat. A
// config COPY that repoints endpoint + agent_model at a scratch engine but keeps
// the original `layers` therefore still sends every run to the layer seat, and the
// only symptom was a roster defer naming a seat the operator never wrote in the
// copy. AgentModelShadowNote names that precedence so the CLI can say it.
func TestAgentModelShadowNote(t *testing.T) {
	plain := Config{AgentModel: "copy-seat"}
	if got := plain.AgentModelShadowNote(); got != "" {
		t.Fatalf("a plain box has no layers to shadow agent_model, got %q", got)
	}

	unset := CompositeFixture()
	if got := unset.AgentModelShadowNote(); got != "" {
		t.Fatalf("an unset agent_model has nothing to shadow, got %q", got)
	}

	agrees := CompositeFixture()
	agrees.AgentModel = "agent-pool" // a seat the pair layer declares
	if got := agrees.AgentModelShadowNote(); got != "" {
		t.Fatalf("an agent_model that IS a layer seat agrees with placement, got %q", got)
	}

	twin := CompositeFixture()
	twin.AgentModel = "gemma-4-e4b-display" // named only in a router seat's model_map
	if got := twin.AgentModelShadowNote(); got != "" {
		t.Fatalf("a model_map twin is a layer seat too, got %q", got)
	}

	copyCfg := CompositeFixture()
	copyCfg.AgentModel = "copy-seat"
	note := copyCfg.AgentModelShadowNote()
	for _, want := range []string{`agent_model "copy-seat"`, "layers", "agent-pool", "placement"} {
		if !strings.Contains(note, want) {
			t.Fatalf("the shadow note must name %q, got %q", want, note)
		}
	}
}

// WarnOnShadowedAgentModel prints the note with the config file that supplied the
// layers, and stays silent for a config the note does not apply to.
func TestWarnOnShadowedAgentModel(t *testing.T) {
	cfg := CompositeFixture()
	cfg.AgentModel = "copy-seat"
	var buf bytes.Buffer
	if !WarnOnShadowedAgentModel(Source{Path: "/tmp/copy.json"}, cfg, &buf) {
		t.Fatal("a composite config whose agent_model is no layer seat must warn")
	}
	out := buf.String()
	if !strings.Contains(out, "/tmp/copy.json") || !strings.Contains(out, `agent_model "copy-seat"`) {
		t.Fatalf("the warning must name the file and the key, got %q", out)
	}

	buf.Reset()
	cfg.AgentModel = "agent-pool"
	if WarnOnShadowedAgentModel(Source{Path: "/tmp/copy.json"}, cfg, &buf) || buf.Len() != 0 {
		t.Fatalf("an agreeing config must stay silent, got %q", buf.String())
	}
}
