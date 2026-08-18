package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Phase A: per-model seat endpoints (multi-node delegation, spec §S1) ---

// TestSeatEndpointsDefaultEmpty: seat endpoints are opt-in per box, exactly
// like Pipelines — a shared config must never point every node's seats at a
// base URL that only exists on one machine's tailnet view.
func TestSeatEndpointsDefaultEmpty(t *testing.T) {
	if m := Default().SeatEndpoints; len(m) != 0 {
		t.Errorf("SeatEndpoints default must be empty (every seat stays on Endpoint); got %v", m)
	}
}

// TestSeatEndpointsLoadValidation: every seat_endpoints value is vetted by the
// tailnet guard at LOAD time, so a public/LAN endpoint fails loudly at config
// load — never silently at the first overridden completion (the same doctrine
// as validatePipelines). The error must name the offending KEY: a config with
// several seats bound must not leave the operator guessing which one broke.
func TestSeatEndpointsLoadValidation(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr string // substring the Load error must contain; "" = must load cleanly
	}{
		{
			name: "tailnet endpoints load and round-trip",
			json: `{"tailnet_suffix":"tailnnnnnn.ts.net","seat_endpoints":{` +
				`"lenovo-e4b":"http://node-c:11436",` +
				`"qube-27b":"http://workstation.tailnnnnnn.ts.net:11436",` +
				`"local-loop":"http://127.0.0.1:11436",` +
				`"cgnat-literal":"http://100.64.0.1:18811"}}`,
		},
		{
			name:    "public FQDN fails naming the key",
			json:    `{"tailnet_suffix":"tailnnnnnn.ts.net","seat_endpoints":{"lenovo-e4b":"http://example.com"}}`,
			wantErr: `seat_endpoints["lenovo-e4b"]`,
		},
		{
			name:    "public IP literal fails naming the key",
			json:    `{"tailnet_suffix":"tailnnnnnn.ts.net","seat_endpoints":{"remote-seat":"http://8.8.8.8:80"}}`,
			wantErr: `seat_endpoints["remote-seat"]`,
		},
		{
			name:    "cloud API endpoint fails naming the key",
			json:    `{"tailnet_suffix":"tailnnnnnn.ts.net","seat_endpoints":{"gpt":"https://api.openai.com"}}`,
			wantErr: `seat_endpoints["gpt"]`,
		},
		{
			name:    "generic .ts.net outside the house suffix fails",
			json:    `{"tailnet_suffix":"tailnnnnnn.ts.net","seat_endpoints":{"spoof":"http://evil.ts.net:443"}}`,
			wantErr: `seat_endpoints["spoof"]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "cfg.json")
			if err := os.WriteFile(p, []byte(tc.json), 0o644); err != nil {
				t.Fatal(err)
			}
			c, err := Load(p)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected a clean load, got error: %v", err)
				}
				if got := c.SeatEndpoints["lenovo-e4b"]; got != "http://node-c:11436" {
					t.Fatalf(`SeatEndpoints["lenovo-e4b"] = %q, did not round-trip`, got)
				}
				if len(c.SeatEndpoints) != 4 {
					t.Fatalf("SeatEndpoints = %v, want all 4 entries", c.SeatEndpoints)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a load error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestSeatEndpointsMultiErrorDeterministic: with MORE THAN ONE bad value the
// error must name the entry visited FIRST in SORTED key order — never depend
// on Go's randomized map iteration (same determinism rule, and the same test
// shape, as TestValidatePipelinesMultiErrorDeterministic).
func TestSeatEndpointsMultiErrorDeterministic(t *testing.T) {
	body := `{"tailnet_suffix":"tailnnnnnn.ts.net","seat_endpoints":{` +
		`"zzz-second":"http://example.com",` +
		`"aaa-first":"http://example.org"}}`
	for i := 0; i < 5; i++ {
		p := filepath.Join(t.TempDir(), "cfg.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		if err == nil {
			t.Fatal("expected a load error")
		}
		if !strings.Contains(err.Error(), `seat_endpoints["aaa-first"]`) {
			t.Fatalf("run %d: error = %q, want it to name the sorted-first key aaa-first", i, err.Error())
		}
	}
}
