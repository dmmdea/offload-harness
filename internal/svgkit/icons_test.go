package svgkit

import (
	"strings"
	"testing"
)

func TestIconKnownRenders(t *testing.T) {
	svg, w, h, err := (IconSpec{Name: "check", Color: "#22c55e"}).render()
	if err != nil {
		t.Fatal(err)
	}
	if w != 24 || h != 24 {
		t.Fatalf("default 24x24, got %dx%d", w, h)
	}
	if !strings.Contains(svg, "#22c55e") || !strings.Contains(svg, `viewBox="0 0 24 24"`) {
		t.Fatal("color + 24-grid viewBox expected")
	}
}

func TestIconUnknownErrors(t *testing.T) {
	if _, _, _, err := (IconSpec{Name: "not-an-icon"}).render(); err == nil {
		t.Fatal("unknown icon: want error")
	}
}

func TestIconScales(t *testing.T) {
	_, w, h, _ := (IconSpec{Name: "shield", Size: 48}).render()
	if w != 48 || h != 48 {
		t.Fatalf("size honored: got %dx%d", w, h)
	}
}

func TestIconCoverage(t *testing.T) {
	for _, n := range []string{"check", "x", "search", "shield", "alert", "info", "star", "flask", "beaker", "chart", "arrow-up", "arrow-down", "dollar"} {
		if _, _, _, err := (IconSpec{Name: n}).render(); err != nil {
			t.Fatalf("icon %q must render: %v", n, err)
		}
	}
}
