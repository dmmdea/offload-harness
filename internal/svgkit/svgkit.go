// Package svgkit renders brand-agnostic, parametric SVG components (gauge,
// comparison-bar, chromatogram, icon) from JSON specs. Pure, deterministic, and
// dependency-free: every color/datum is caller-supplied (no brand tokens), and a
// given spec always yields byte-identical SVG. Used by the harness's
// generate_svg task for crisp, free, on-demand data-viz.
package svgkit

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Theme is the brand-agnostic palette + type for a component. Empty fields fall
// back to a neutral slate default (NO brand tokens).
type Theme struct {
	FG     string `json:"fg,omitempty"`
	BG     string `json:"bg,omitempty"`
	Accent string `json:"accent,omitempty"`
	Muted  string `json:"muted,omitempty"`
	Font   string `json:"font,omitempty"`
}

func (t Theme) withDefaults() Theme {
	d := Theme{
		FG:     "#1e293b",
		BG:     "#ffffff",
		Accent: "#0ea5e9",
		Muted:  "#94a3b8",
		Font:   "ui-sans-serif, system-ui, -apple-system, Segoe UI, Roboto, sans-serif",
	}
	if t.FG != "" {
		d.FG = t.FG
	}
	if t.BG != "" {
		d.BG = t.BG
	}
	if t.Accent != "" {
		d.Accent = t.Accent
	}
	if t.Muted != "" {
		d.Muted = t.Muted
	}
	if t.Font != "" {
		d.Font = t.Font
	}
	return d
}

var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")

// esc escapes caller text for safe inclusion in SVG text/attributes.
func esc(s string) string { return escaper.Replace(s) }

// f formats a float compactly: rounded to 2 decimals, no trailing zeros.
func f(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

// polar maps (center, radius, angle°) to a cartesian point. 0° = east; angle
// increases counter-clockwise in math convention, rendered into SVG's y-down
// space (so y is subtracted).
func polar(cx, cy, r, deg float64) (x, y float64) {
	rad := deg * math.Pi / 180
	return cx + r*math.Cos(rad), cy - r*math.Sin(rad)
}

// header opens an <svg> with an explicit size + viewBox; bg paints a background
// rect when non-empty (transparent otherwise).
func header(w, h int, bg string) string {
	s := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img">`, w, h, w, h)
	if bg != "" {
		s += fmt.Sprintf(`<rect width="%d" height="%d" fill="%s"/>`, w, h, bg)
	}
	return s
}

const footer = `</svg>`

// Render renders the named component from its JSON spec and returns the SVG
// markup plus pixel dimensions. Unknown kind or unparseable spec returns an error.
func Render(kind string, spec json.RawMessage) (svg string, w, h int, err error) {
	switch kind {
	case "gauge":
		var s GaugeSpec
		if err = json.Unmarshal(spec, &s); err != nil {
			return
		}
		return s.render()
	case "comparison-bar":
		var s ComparisonBarSpec
		if err = json.Unmarshal(spec, &s); err != nil {
			return
		}
		return s.render()
	case "chromatogram":
		var s ChromatogramSpec
		if err = json.Unmarshal(spec, &s); err != nil {
			return
		}
		return s.render()
	case "icon":
		var s IconSpec
		if err = json.Unmarshal(spec, &s); err != nil {
			return
		}
		return s.render()
	default:
		return "", 0, 0, fmt.Errorf("svgkit: unknown kind %q", kind)
	}
}
