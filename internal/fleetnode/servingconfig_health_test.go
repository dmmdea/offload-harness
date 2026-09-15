package fleetnode_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// TestHealthPublishesServingConfigProvenance covers the K-02 health half: two
// NEW KEYS on the EXISTING /fleet/health payload -- no new route, no new bind.
//
// The absence case is the one with teeth. Both keys are omitempty, and that is
// load-bearing rather than tidy: a node that was never told which config it
// serves must publish NOTHING, so a fleet reader can tell "this node does not
// report" from "this node reports MATCH". Publishing an empty string, or a
// default state, would make an unconfigured node indistinguishable from a
// healthy one -- the same silent-absence failure the harness has already paid
// for elsewhere.
func TestHealthPublishesServingConfigProvenance(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reporter  func() (string, string)
		wantKeys  bool
		wantSHA   string
		wantState string
	}{
		{name: "unconfigured", reporter: nil, wantKeys: false},
		{name: "configured-but-unreadable", reporter: func() (string, string) { return "", "" }, wantKeys: false},
		{
			name:     "match",
			reporter: func() (string, string) { return "abc123", "MATCH" },
			wantKeys: true, wantSHA: "abc123", wantState: "MATCH",
		},
		{
			name:     "stale",
			reporter: func() (string, string) { return "def456", "STALE" },
			wantKeys: true, wantSHA: "def456", wantState: "STALE",
		},
		{
			// An UNSTAMPED config has no spec hash to publish, but the STATE is
			// exactly the finding a fleet sweep is looking for, so it must still
			// reach the wire on its own.
			name:     "unstamped-publishes-the-state-without-a-hash",
			reporter: func() (string, string) { return "", "UNSTAMPED" },
			wantKeys: true, wantSHA: "", wantState: "UNSTAMPED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{ImageGenScript: "C:/x/comfy-generate.mjs"}
			jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
			t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
			srv := fleetnode.New(nopRunner{}, jobs, fleetnode.Options{
				NodeID:  "prov-node",
				Version: "test",
				Snapshot: func() (fleetnode.Snapshot, bool) {
					return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
				},
				Cfg:           cfg,
				ServingConfig: tc.reporter,
			})
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			resp, err := http.Get(ts.URL + "/fleet/health")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			// Decoded as a free-form map on purpose: the question is which KEYS
			// are on the wire, and a typed struct would answer with zero values
			// for keys that never arrived.
			var raw map[string]any
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatalf("health is not JSON: %v\n%s", err, body)
			}
			_, gotSHA := raw["serving_config_spec_sha256"]
			state, gotState := raw["serving_config_state"]
			if !tc.wantKeys {
				if gotSHA || gotState {
					t.Fatalf("an unreporting node published provenance keys: sha=%v state=%v", raw["serving_config_spec_sha256"], raw["serving_config_state"])
				}
				return
			}
			if !gotState {
				t.Fatal("serving_config_state is missing from health")
			}
			if state != tc.wantState {
				t.Errorf("serving_config_state = %v, want %q", state, tc.wantState)
			}
			if tc.wantSHA == "" {
				if gotSHA {
					t.Errorf("an empty spec hash must be omitted, got %v", raw["serving_config_spec_sha256"])
				}
				return
			}
			if raw["serving_config_spec_sha256"] != tc.wantSHA {
				t.Errorf("serving_config_spec_sha256 = %v, want %q", raw["serving_config_spec_sha256"], tc.wantSHA)
			}
			// The pre-existing payload must be untouched: these are additive keys
			// on an endpoint every delegator already decodes.
			if raw["harness_version"] != "test" || raw["node_id"] != "prov-node" {
				t.Errorf("the existing health payload changed shape: %s", body)
			}
		})
	}
}
