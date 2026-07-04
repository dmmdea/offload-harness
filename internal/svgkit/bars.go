package svgkit

import (
	"fmt"
	"strings"
)

// BarItem is one row in a comparison-bar chart.
type BarItem struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Color string  `json:"color,omitempty"`
}

// ComparisonBarSpec describes a horizontal bar chart. Highlight is the index of
// the item to emphasize (use a negative value, e.g. -1, for none).
type ComparisonBarSpec struct {
	Items     []BarItem `json:"items"`
	Max       float64   `json:"max,omitempty"`
	Width     int       `json:"width,omitempty"`
	BarHeight int       `json:"bar_height,omitempty"`
	Unit      string    `json:"unit,omitempty"`
	Highlight int       `json:"highlight,omitempty"`
	Theme     Theme     `json:"theme,omitempty"`
}

func (s ComparisonBarSpec) render() (string, int, int, error) {
	th := s.Theme.withDefaults()
	width := s.Width
	if width <= 0 {
		width = 480
	}
	barH := s.BarHeight
	if barH <= 0 {
		barH = 28
	}
	const (
		gap       = 12
		padTop    = 16
		padBottom = 16
		labelGut  = 130.0
		valueGut  = 56.0
	)

	n := len(s.Items)
	height := padTop + padBottom + n*(barH+gap) - gap
	if n == 0 {
		height = padTop + padBottom
	}

	// Max defaults to the largest value (≥1 to avoid div-by-zero).
	max := s.Max
	if max <= 0 {
		for _, it := range s.Items {
			if it.Value > max {
				max = it.Value
			}
		}
	}
	if max < 1 {
		max = 1
	}

	fw := float64(width)
	barAreaX := labelGut
	barAreaW := fw - labelGut - valueGut
	if barAreaW < 1 {
		barAreaW = 1
	}
	fbarH := float64(barH)
	labelFont := fbarH * 0.5
	radius := f(fbarH * 0.18)

	var b strings.Builder
	b.WriteString(header(width, height, th.BG))

	for i, it := range s.Items {
		rowY := float64(padTop + i*(barH+gap))
		cy := rowY + fbarH/2

		// Label in the left gutter, right-aligned so it sits flush before the bar
		// (text-anchor="end" grows leftward into the gutter, never clipped by the bar).
		fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="end" dominant-baseline="middle" font-family="%s" font-size="%s" fill="%s">%s</text>`,
			f(labelGut-12), f(cy), esc(th.Font), f(labelFont), th.FG, esc(it.Label))

		// Track rect (muted, low opacity) spanning the bar area.
		fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" rx="%s" fill="%s" opacity="0.18"/>`,
			f(barAreaX), f(rowY), f(barAreaW), f(fbarH), radius, th.Muted)

		// Filled rect ∝ value/max.
		frac := it.Value / max
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
		fillW := barAreaW * frac
		fill := it.Color
		if fill == "" {
			if i == s.Highlight {
				fill = th.FG
			} else {
				fill = th.Accent
			}
		}
		if fillW > 0 {
			fmt.Fprintf(&b, `<rect x="%s" y="%s" width="%s" height="%s" rx="%s" fill="%s"/>`,
				f(barAreaX), f(rowY), f(fillW), f(fbarH), radius, fill)
		}

		// Value text right after the bar.
		fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="start" dominant-baseline="middle" font-family="%s" font-size="%s" font-weight="600" fill="%s">%s</text>`,
			f(barAreaX+fillW+8), f(cy), esc(th.Font), f(labelFont), th.FG, esc(f(it.Value)+s.Unit))
	}

	b.WriteString(footer)
	return b.String(), width, height, nil
}
