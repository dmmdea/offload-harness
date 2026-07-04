package svgkit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGaugeWellFormedAndSized(t *testing.T) {
	svg, w, h, err := (GaugeSpec{Value: 72, Label: "Purity", Unit: "%"}).render()
	if err != nil {
		t.Fatal(err)
	}
	if w != 240 || h != 240 {
		t.Fatalf("default size 240, got %dx%d", w, h)
	}
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatalf("not a well-formed svg: %.60s", svg)
	}
	if !strings.Contains(svg, "72") || !strings.Contains(svg, "Purity") {
		t.Fatal("value + label must appear in the svg")
	}
	if strings.Count(svg, "<path") < 2 {
		t.Fatal("want at least a track arc + a value arc")
	}
}

func TestGaugeDeterministic(t *testing.T) {
	a, _, _, _ := (GaugeSpec{Value: 50}).render()
	b, _, _, _ := (GaugeSpec{Value: 50}).render()
	if a != b {
		t.Fatal("same spec must render byte-identically")
	}
}

func TestGaugeUsesAccentByDefaultAndZoneColorWhenMatched(t *testing.T) {
	plain, _, _, _ := (GaugeSpec{Value: 50}).render()
	if !strings.Contains(plain, (Theme{}).withDefaults().Accent) {
		t.Fatal("no zones: value arc should use the accent color")
	}
	zoned, _, _, _ := (GaugeSpec{Value: 90, Zones: []GaugeZone{{Upto: 100, Color: "#dc2626"}}}).render()
	if !strings.Contains(zoned, "#dc2626") {
		t.Fatal("zone color for the matched band must appear")
	}
}

func TestGaugeClampsOutOfRange(t *testing.T) {
	// Value above Max must not panic and must still render.
	if _, _, _, err := (GaugeSpec{Value: 999, Max: 100}).render(); err != nil {
		t.Fatal(err)
	}
	via, _, _, err := Render("gauge", json.RawMessage(`{"value":33,"unit":"%"}`))
	if err != nil || !strings.Contains(via, "33") {
		t.Fatalf("Render dispatch for gauge failed: err=%v", err)
	}
}
