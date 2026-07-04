package svgkit

import (
	"strings"
	"testing"
)

func TestChromatogramTrace(t *testing.T) {
	spec := ChromatogramSpec{Peaks: []Peak{{RT: 2.5, Height: 80, Label: "API"}, {RT: 5.0, Height: 40}}}
	svg, w, h, err := spec.render()
	if err != nil {
		t.Fatal(err)
	}
	if w != 640 || h != 280 {
		t.Fatalf("default 640x280, got %dx%d", w, h)
	}
	if !strings.Contains(svg, "<polyline") {
		t.Fatal("want a polyline trace")
	}
	if !strings.Contains(svg, "Retention time") || !strings.Contains(svg, "API") {
		t.Fatal("axis title + peak label must appear")
	}
}

func TestChromatogramEmptyNoPanic(t *testing.T) {
	if _, _, _, err := (ChromatogramSpec{}).render(); err != nil {
		t.Fatal(err)
	}
}

func TestChromatogramDeterministic(t *testing.T) {
	s := ChromatogramSpec{Peaks: []Peak{{RT: 1, Height: 10}}}
	a, _, _, _ := s.render()
	b, _, _, _ := s.render()
	if a != b {
		t.Fatal("non-deterministic")
	}
}
