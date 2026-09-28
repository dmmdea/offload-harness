package core

import "testing"

func TestValidateBrowseHosts(t *testing.T) {
	for name, tc := range map[string]struct {
		allow bool
		hosts []string
		ok    bool
	}{
		"off, none":    {false, nil, true},
		"off, hosts":   {false, []string{"example.com"}, false},
		"on, none":     {true, nil, false},
		"on, bare":     {true, []string{"example.com", "pub.example.org"}, true},
		"on, scheme":   {true, []string{"https://example.com"}, false},
		"on, port":     {true, []string{"example.com:443"}, false},
		"on, wildcard": {true, []string{"*.example.com"}, false},
		"on, path":     {true, []string{"example.com/x"}, false},
		"on, blank":    {true, []string{" "}, false},
		"on, too many": {true, make([]string, 33), false},
	} {
		if err := ValidateBrowseHosts(tc.allow, tc.hosts); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", name, err, tc.ok)
		}
	}
	c := AgentContract{SchemaVersion: AgentWireSchemaVersion, Goal: "x", AllowBrowse: true}
	if err := c.Validate(); err == nil {
		t.Error("a contract with allow_browse and no hosts must fail validation")
	}
}
