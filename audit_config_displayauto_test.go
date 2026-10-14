package main

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The blackwell-3x16 seed ships its display layer with display_device "auto". audit-config compares the
// seed's `layers` value as a whole, so a node whose layers were merged from the table that named the
// display card by index reads DIFFERENT, and the command exits non-zero, until its display layer matches
// the seed's again. A node carrying today's seed audits clean. The root gate cannot see this on its own
// (it compares a seed with itself), which is why composite-tier.md has to say it.
func TestAuditConfigReportsLayersMergedFromTheTableThatNamedTheDisplayCard(t *testing.T) {
	home := t.TempDir()
	if out, err := auditNode(t, "blackwell-3x16", home, nil, nil); err != nil {
		t.Fatalf("a node carrying today's blackwell-3x16 seed audits clean, got %v\n%s", err, out)
	}

	out, err := auditNode(t, "blackwell-3x16", home, nil, func(live map[string]any) {
		layers, ok := live["layers"].([]config.LayerSpec)
		if !ok || len(layers) == 0 {
			t.Fatalf("the blackwell-3x16 seed carries layers, got %#v", live["layers"])
		}
		merged := make([]config.LayerSpec, len(layers))
		copy(merged, layers)
		found := false
		for i := range merged {
			if merged[i].Name == "display" {
				merged[i].DisplayDevice = "1" // what the table said before it said "auto"
				found = true
			}
		}
		if !found {
			t.Fatal("the seed carries no display layer")
		}
		live["layers"] = merged
	})
	if !errors.Is(err, errConfigDrift) {
		t.Fatalf("layers merged from the table that said \"1\" must read as drift (errConfigDrift), got %v\n%s", err, out)
	}
	if !regexp.MustCompile(`DIFFERENT\s+layers\b`).MatchString(out) {
		t.Errorf("the drift must name layers as DIFFERENT:\n%s", out)
	}
	if !strings.Contains(out, "1 seed-owned key(s) drifted") {
		t.Errorf("layers is the only key that differs:\n%s", out)
	}
}
