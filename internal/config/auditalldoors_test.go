package config

import (
	"strings"
	"testing"
)

// TestConfigRejectsAnUnknownAuditAllDoorsMode (register SF-02): the key is a closed
// set, and a typo must fail at the config door naming the key, the value and the
// valid modes, not silently resolve to off (no trail) or to enforce (a new way for
// every agent door to fail).
func TestConfigRejectsAnUnknownAuditAllDoorsMode(t *testing.T) {
	_, err := Load(writeCfg(t, `{"model":"x","audit_all_doors":"on"}`))
	if err == nil {
		t.Fatal("an unknown audit_all_doors must refuse the load")
	}
	for _, want := range []string{"audit_all_doors", `"on"`, "off", "warn", "enforce"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}

// TestConfigAcceptsEveryAuditAllDoorsMode: the closed set loads and resolves, the
// unset key is off (today's behaviour), and a copy-pasted value is not demoted.
func TestConfigAcceptsEveryAuditAllDoorsMode(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"", "off"},
		{"off", "off"},
		{"warn", "warn"},
		{"enforce", "enforce"},
		{" Warn ", "warn"},
	} {
		body := `{"model":"x"}`
		if tc.raw != "" {
			body = `{"model":"x","audit_all_doors":"` + tc.raw + `"}`
		}
		c, err := Load(writeCfg(t, body))
		if err != nil {
			t.Fatalf("audit_all_doors %q: load failed: %v", tc.raw, err)
		}
		if got := c.AuditAllDoorsMode(); got != tc.want {
			t.Errorf("audit_all_doors %q resolves to %q, want %q", tc.raw, got, tc.want)
		}
	}
}
