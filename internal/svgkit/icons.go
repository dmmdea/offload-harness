package svgkit

import (
	"fmt"
	"strings"
)

// IconSpec describes a stroke-based icon in a 24×24 viewBox scaled to Size.
type IconSpec struct {
	Name   string  `json:"name"`
	Size   int     `json:"size,omitempty"`
	Color  string  `json:"color,omitempty"`
	Stroke float64 `json:"stroke,omitempty"`
	Theme  Theme   `json:"theme,omitempty"`
}

// iconPaths holds the inner markup for each built-in icon, hand-authored in a
// 24×24 coordinate space (geometry kept within [1,23] for padding). Strokes use
// currentColor so the wrapper can recolor via the stroke attribute. Original
// geometry (Lucide-style, but authored here).
var iconPaths = map[string]string{
	"check": `<polyline points="4,12.5 9.5,18 20,6.5"/>`,

	"x": `<line x1="6" y1="6" x2="18" y2="18"/><line x1="18" y1="6" x2="6" y2="18"/>`,

	"search": `<circle cx="11" cy="11" r="7"/><line x1="16" y1="16" x2="21" y2="21"/>`,

	"shield": `<path d="M12 2 L20 5 V11 C20 16 16.5 20 12 22 C7.5 20 4 16 4 11 V5 Z"/>`,

	"alert": `<path d="M12 2 L22 21 H2 Z"/><line x1="12" y1="9" x2="12" y2="14"/><circle cx="12" cy="18" r="0.6"/>`,

	"info": `<circle cx="12" cy="12" r="10"/><line x1="12" y1="11" x2="12" y2="17"/><circle cx="12" cy="8" r="0.6"/>`,

	"star": `<polygon points="12,2 14.9,8.6 22,9.3 16.5,14 18.2,21 12,17.3 5.8,21 7.5,14 2,9.3 9.1,8.6"/>`,

	"flask": `<path d="M9 2 H15 M10 2 V9 L4 19 A1.5 1.5 0 0 0 5.3 21 H18.7 A1.5 1.5 0 0 0 20 19 L14 9 V2"/><line x1="7" y1="15" x2="17" y2="15"/>`,

	"beaker": `<path d="M6 2 H18 M7 2 V7 L4 20 A1.5 1.5 0 0 0 5.4 22 H18.6 A1.5 1.5 0 0 0 20 20 L17 7 V2"/><line x1="5.5" y1="14" x2="18.5" y2="14"/>`,

	"chart": `<polyline points="3,21 3,3"/><polyline points="3,21 21,21"/><rect x="6" y="13" width="3" height="6"/><rect x="11" y="9" width="3" height="10"/><rect x="16" y="5" width="3" height="14"/>`,

	"arrow-up": `<line x1="12" y1="20" x2="12" y2="4"/><polyline points="5,11 12,4 19,11"/>`,

	"arrow-down": `<line x1="12" y1="4" x2="12" y2="20"/><polyline points="5,13 12,20 19,13"/>`,

	"dollar": `<line x1="12" y1="2" x2="12" y2="22"/><path d="M17 6.5 C17 4.5 14.8 3.5 12 3.5 C9.2 3.5 7 4.7 7 7 C7 9.3 9.2 10.5 12 10.5 C14.8 10.5 17 11.7 17 14 C17 16.3 14.8 17.5 12 17.5 C9.2 17.5 7 16.5 7 14.5"/>`,
}

func (s IconSpec) render() (string, int, int, error) {
	body, ok := iconPaths[s.Name]
	if !ok {
		return "", 0, 0, fmt.Errorf("svgkit: unknown icon %q", s.Name)
	}
	th := s.Theme.withDefaults()
	size := s.Size
	if size <= 0 {
		size = 24
	}
	color := s.Color
	if color == "" {
		color = th.FG
	}
	stroke := s.Stroke
	if stroke <= 0 {
		stroke = 2
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 24 24" fill="none" stroke="%s" stroke-width="%s" stroke-linecap="round" stroke-linejoin="round" role="img">`,
		size, size, color, f(stroke))
	if th.BG != "" && s.Theme.BG != "" {
		// Only paint a background when the caller explicitly set one.
		fmt.Fprintf(&b, `<rect width="24" height="24" fill="%s" stroke="none"/>`, th.BG)
	}
	b.WriteString(body)
	b.WriteString(footer)
	return b.String(), size, size, nil
}
