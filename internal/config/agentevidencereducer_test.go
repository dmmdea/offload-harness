package config

import "testing"

// TestConfigAgentEvidenceReducerIsOffUnlessSet: the evidence-preserving reducer is a
// per-node opt-in. Unset it is off, and the key turns it on.
func TestConfigAgentEvidenceReducerIsOffUnlessSet(t *testing.T) {
	if Default().AgentEvidenceReducer {
		t.Fatal("agent_evidence_reducer must default to false")
	}
	cfg, err := Load(writeCfg(t, `{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentEvidenceReducer {
		t.Error("a config that does not mention agent_evidence_reducer turned it on")
	}
	cfg, err = Load(writeCfg(t, `{"model":"x","agent_evidence_reducer":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AgentEvidenceReducer {
		t.Error("agent_evidence_reducer:true did not load")
	}
}
