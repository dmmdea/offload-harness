package fleetnode

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestInstalledInfoBackends: the advertised list is primary first, then only the
// alternates the install rendered; a manifest without a backend advertises nothing
// (nil → the health field is omitted, so pre-manifest nodes are byte-identical).
func TestInstalledInfoBackends(t *testing.T) {
	for _, tc := range []struct {
		label string
		info  InstalledInfo
		want  []string
	}{
		{"dual-route node", InstalledInfo{Profile: "amd-gcn", Backend: "vulkan", AltBackends: []string{"cpu"}}, []string{"vulkan", "cpu"}},
		{"single route", InstalledInfo{Profile: "ampere-16", Backend: "cuda"}, []string{"cuda"}},
		{"alt equal to primary is not repeated", InstalledInfo{Backend: "cpu", AltBackends: []string{"cpu"}}, []string{"cpu"}},
		{"no manifest backend → omitted", InstalledInfo{Profile: "amd-gcn"}, nil},
		{"zero value", InstalledInfo{}, nil},
	} {
		if got := tc.info.Backends(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Backends() = %v, want %v", tc.label, got, tc.want)
		}
	}
}

// TestHealthPayloadBackendsIsAdditive: the wire field appears only when set.
func TestHealthPayloadBackendsIsAdditive(t *testing.T) {
	b, _ := json.Marshal(healthPayload{NodeID: "n"})
	if string(b) == "" || strings.Contains(string(b), `"backends"`) {
		t.Fatalf("unset Backends must be omitted from the payload: %s", b)
	}
	b, _ = json.Marshal(healthPayload{NodeID: "n", Backends: []string{"vulkan", "cpu"}})
	if !strings.Contains(string(b), `"backends":["vulkan","cpu"]`) {
		t.Fatalf("Backends not serialised in order: %s", b)
	}
}
