package config

import (
	"strings"
	"testing"
)

// TestConfigAgentReadFloorIsAClosedSet (register SF-07): the three modes and the
// unset key load; anything else refuses the load naming the key, the value and the
// valid modes, never silently resolving to a floor the operator did not choose.
func TestConfigAgentReadFloorIsAClosedSet(t *testing.T) {
	for _, ok := range []string{"", "off", "warn", "enforce", " Enforce "} {
		body := `{"model":"x"}`
		if ok != "" {
			body = `{"model":"x","agent_read_floor":"` + ok + `"}`
		}
		if _, err := Load(writeCfg(t, body)); err != nil {
			t.Errorf("agent_read_floor %q refused: %v", ok, err)
		}
	}
	_, err := Load(writeCfg(t, `{"model":"x","agent_read_floor":"strict"}`))
	if err == nil {
		t.Fatal("an unknown agent_read_floor must refuse the load")
	}
	for _, want := range []string{"agent_read_floor", `"strict"`, "warn", "enforce", "off"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
