package svgkit

import (
	"strings"
	"testing"
)

func TestBarsRendersRowsAndScales(t *testing.T) {
	spec := ComparisonBarSpec{Items: []BarItem{{Label: "A", Value: 10}, {Label: "B", Value: 20}}, Unit: "u", Highlight: 1}
	svg, w, h, err := spec.render()
	if err != nil {
		t.Fatal(err)
	}
	if w <= 0 || h <= 0 {
		t.Fatalf("bad dims %dx%d", w, h)
	}
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatal("not well-formed")
	}
	for _, want := range []string{"A", "B", "10", "20"} {
		if !strings.Contains(svg, want) {
			t.Fatalf("missing %q", want)
		}
	}
	// B (value 20) bar must be wider than A (value 10): at least two filled rects.
	if strings.Count(svg, "<rect") < 3 { // bg/track/fills
		t.Fatal("expected track + fill rects per row")
	}
}

func TestBarsEmptyItemsNoPanic(t *testing.T) {
	if _, _, _, err := (ComparisonBarSpec{}).render(); err != nil {
		t.Fatal(err)
	}
}

func TestBarsDeterministic(t *testing.T) {
	s := ComparisonBarSpec{Items: []BarItem{{Label: "X", Value: 5}}}
	a, _, _, _ := s.render()
	b, _, _, _ := s.render()
	if a != b {
		t.Fatal("non-deterministic")
	}
}
