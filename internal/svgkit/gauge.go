package svgkit

import (
	"fmt"
	"strings"
)

// GaugeZone colors the value arc up to the Upto threshold.
type GaugeZone struct {
	Upto  float64 `json:"upto"`
	Color string  `json:"color"`
}

// GaugeSpec describes a 270° radial dial, open at the bottom.
type GaugeSpec struct {
	Value float64     `json:"value"`
	Min   float64     `json:"min,omitempty"`
	Max   float64     `json:"max,omitempty"`
	Label string      `json:"label,omitempty"`
	Unit  string      `json:"unit,omitempty"`
	Size  int         `json:"size,omitempty"`
	Zones []GaugeZone `json:"zones,omitempty"`
	Theme Theme       `json:"theme,omitempty"`
}

// gaugeArc returns an SVG arc path "M…A…" sweeping the dial from fraction 0 to
// fraction t (t∈[0,1]). The dial spans 270°: angle(t) = 225 - 270*t degrees.
func gaugeArc(cx, cy, r, t float64) string {
	x0, y0 := polar(cx, cy, r, 225)
	x1, y1 := polar(cx, cy, r, 225-270*t)
	large := 0
	if t > 0.5 { // swept angle (270*t) exceeds 180° once t > 0.667; flag must flip
		// large-arc-flag is 1 when the arc spans more than 180°.
		if 270*t > 180 {
			large = 1
		}
	}
	// sweep-flag 1 = clockwise in SVG's y-down space, which matches our
	// decreasing-angle (down-left → top → down-right) traversal.
	return fmt.Sprintf("M %s %s A %s %s 0 %d 1 %s %s",
		f(x0), f(y0), f(r), f(r), large, f(x1), f(y1))
}

func (s GaugeSpec) render() (string, int, int, error) {
	th := s.Theme.withDefaults()
	size := s.Size
	if size <= 0 {
		size = 240
	}
	min, max := s.Min, s.Max
	if max == 0 && min == 0 {
		max = 100
	}
	if max <= min {
		max = min + 1
	}

	// Clamp value into [min,max] for the fraction; keep the original for display.
	v := s.Value
	if v < min {
		v = min
	}
	if v > max {
		v = max
	}
	t := (v - min) / (max - min)
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}

	fsize := float64(size)
	cx, cy := fsize/2, fsize/2
	stroke := fsize * 0.09
	r := fsize/2 - stroke/2 - fsize*0.04

	// Active color: first zone whose Upto >= Value, else accent.
	active := th.Accent
	for _, z := range s.Zones {
		if s.Value <= z.Upto && z.Color != "" {
			active = z.Color
			break
		}
	}

	var b strings.Builder
	b.WriteString(header(size, size, th.BG))

	// (1) full track arc 0→1 in muted.
	fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="%s" stroke-width="%s" stroke-linecap="round"/>`,
		gaugeArc(cx, cy, r, 1), th.Muted, f(stroke))

	// (2) value arc 0→t in the active color (omit when t==0 to avoid a stray dot).
	if t > 0 {
		fmt.Fprintf(&b, `<path d="%s" fill="none" stroke="%s" stroke-width="%s" stroke-linecap="round"/>`,
			gaugeArc(cx, cy, r, t), active, f(stroke))
	}

	// (3) centered value text + label below.
	valSize := fsize * 0.20
	fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" dominant-baseline="middle" font-family="%s" font-size="%s" font-weight="700" fill="%s">%s</text>`,
		f(cx), f(cy), esc(th.Font), f(valSize), th.FG, esc(f(s.Value)+s.Unit))
	if s.Label != "" {
		labSize := fsize * 0.085
		fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" dominant-baseline="middle" font-family="%s" font-size="%s" fill="%s">%s</text>`,
			f(cx), f(cy+valSize*0.95), esc(th.Font), f(labSize), th.Muted, esc(s.Label))
	}

	b.WriteString(footer)
	return b.String(), size, size, nil
}
