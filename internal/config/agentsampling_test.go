package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAgentSamplingDefaultsToNothing (D-95b): a config that does not mention
// the key leaves the planner exactly as it was — no policy, so the client
// sends `temperature: 0` and no other sampling key.
func TestAgentSamplingDefaultsToNothing(t *testing.T) {
	c, err := Load(writeCfg(t, `{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AgentSampling.IsZero() || !c.AgentSamplingFinal.IsZero() {
		t.Fatalf("unset keys must parse to no policy: %+v / %+v", c.AgentSampling, c.AgentSamplingFinal)
	}
	if def := Default(); !def.AgentSampling.IsZero() || !def.AgentSamplingFinal.IsZero() {
		t.Fatal("Default() must carry no sampling policy")
	}
}

// TestAgentSamplingParsesEveryKnob: the five knobs land where the client reads
// them, and a knob the operator did NOT write stays unset (a zero value would
// be an opinion the seat never asked for).
func TestAgentSamplingParsesEveryKnob(t *testing.T) {
	c, err := Load(writeCfg(t, `{
	  "agent_sampling": {"temperature": 0.2},
	  "agent_sampling_final": {"temperature": 0.7, "top_p": 0.8, "top_k": 20, "presence_penalty": 1.5, "repetition_penalty": 1.05}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentSampling.Temperature == nil || *c.AgentSampling.Temperature != 0.2 {
		t.Fatalf("agent_sampling.temperature = %v", c.AgentSampling.Temperature)
	}
	if c.AgentSampling.TopP != nil || c.AgentSampling.TopK != nil {
		t.Fatalf("knobs the operator did not write must stay unset: %+v", c.AgentSampling)
	}
	f := c.AgentSamplingFinal
	if f.TopP == nil || *f.TopP != 0.8 || f.TopK == nil || *f.TopK != 20 ||
		f.PresencePenalty == nil || *f.PresencePenalty != 1.5 || f.RepetitionPenalty == nil || *f.RepetitionPenalty != 1.05 {
		t.Fatalf("agent_sampling_final = %+v", f)
	}
	if got, want := f.Summary(), "temperature=0.7 top_p=0.8 top_k=20 presence_penalty=1.5 repetition_penalty=1.05"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

// TestAgentSamplingRefusesOutOfRangeValuesAtTheDoor: a value no backend
// accepts must fail the LOAD, named by its key — never as a 400 in the middle
// of a delegated run on a remote node.
func TestAgentSamplingRefusesOutOfRangeValuesAtTheDoor(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{"agent_sampling":{"temperature":3}}`, "agent_sampling.temperature"},
		{`{"agent_sampling":{"top_p":0}}`, "agent_sampling.top_p"},
		{`{"agent_sampling_final":{"top_k":0}}`, "agent_sampling_final.top_k"},
		{`{"agent_sampling_final":{"presence_penalty":9}}`, "agent_sampling_final.presence_penalty"},
		{`{"agent_sampling_final":{"repetition_penalty":0}}`, "agent_sampling_final.repetition_penalty"},
	} {
		_, err := Load(writeCfg(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %s", c.body, err, c.want)
		}
	}
}
