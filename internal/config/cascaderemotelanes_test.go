package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- roast delta 7: busy-aware cascade remote lanes ---

// TestCascadeRemoteLanesDefaultEmpty: lanes are opt-in per box, exactly like
// SeatEndpoints — a shared config must never make every node's cascade probe a
// lane base that only exists on one machine's tailnet view.
func TestCascadeRemoteLanesDefaultEmpty(t *testing.T) {
	if m := Default().CascadeRemoteLanes; len(m) != 0 {
		t.Errorf("CascadeRemoteLanes default must be empty (cascade stays on Endpoint); got %v", m)
	}
}

// TestCascadeRemoteLanesLoadValidation: every cascade_remote_lanes value is
// vetted by the tailnet guard at LOAD time, so a public/LAN lane base fails
// loudly at config load — never silently at the first busy-hour reroute — and
// the error names the offending KEY (the same doctrine, and the same shared
// validator, as seat_endpoints).
func TestCascadeRemoteLanesLoadValidation(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr string // substring the Load error must contain; "" = must load cleanly
	}{
		{
			name: "tailnet lanes load and round-trip",
			json: `{"cascade_remote_lanes":{` +
				`"offload-e4b":"http://lenovo-m720q:11436",` +
				`"gemma4-e2b":"http://qube.tail38a707.ts.net:11436",` +
				`"local-loop":"http://127.0.0.1:11436",` +
				`"cgnat-literal":"http://100.127.9.110:18811"}}`,
		},
		{
			name:    "public FQDN fails naming the key",
			json:    `{"cascade_remote_lanes":{"offload-e4b":"http://example.com"}}`,
			wantErr: `cascade_remote_lanes["offload-e4b"]`,
		},
		{
			name:    "public IP literal fails naming the key",
			json:    `{"cascade_remote_lanes":{"lane-seat":"http://8.8.8.8:80"}}`,
			wantErr: `cascade_remote_lanes["lane-seat"]`,
		},
		{
			name:    "cloud API endpoint fails naming the key",
			json:    `{"cascade_remote_lanes":{"gpt":"https://api.openai.com"}}`,
			wantErr: `cascade_remote_lanes["gpt"]`,
		},
		{
			name:    "generic .ts.net outside the house suffix fails",
			json:    `{"cascade_remote_lanes":{"spoof":"http://evil.ts.net:443"}}`,
			wantErr: `cascade_remote_lanes["spoof"]`,
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
				if got := c.CascadeRemoteLanes["offload-e4b"]; got != "http://lenovo-m720q:11436" {
					t.Fatalf(`CascadeRemoteLanes["offload-e4b"] = %q, did not round-trip`, got)
				}
				if len(c.CascadeRemoteLanes) != 4 {
					t.Fatalf("CascadeRemoteLanes = %v, want all 4 entries", c.CascadeRemoteLanes)
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

// TestCascadeRemoteLanesMultiErrorDeterministic: with MORE THAN ONE bad value
// the error must name the entry visited FIRST in SORTED key order — never
// depend on Go's randomized map iteration (the same determinism rule, pinned
// on the shared validator through this second call site).
func TestCascadeRemoteLanesMultiErrorDeterministic(t *testing.T) {
	body := `{"cascade_remote_lanes":{` +
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
		if !strings.Contains(err.Error(), `cascade_remote_lanes["aaa-first"]`) {
			t.Fatalf("run %d: error = %q, want the sorted-first key aaa-first", i, err.Error())
		}
	}
}
