package svgkit

import (
	"fmt"
	"math"
	"strings"
)

// Peak is one Gaussian peak in a chromatogram trace.
type Peak struct {
	RT     float64 `json:"rt"`
	Height float64 `json:"height"`
	Width  float64 `json:"width,omitempty"`
	Label  string  `json:"label,omitempty"`
}

// ChromatogramSpec describes an HPLC-style trace.
type ChromatogramSpec struct {
	Peaks  []Peak  `json:"peaks"`
	XMax   float64 `json:"x_max,omitempty"`
	Width  int     `json:"width,omitempty"`
	Height int     `json:"height,omitempty"`
	XLabel string  `json:"x_label,omitempty"`
	YLabel string  `json:"y_label,omitempty"`
	Theme  Theme   `json:"theme,omitempty"`
}

// gauss evaluates the signal (sum of Gaussians) at x for the given peaks.
func gauss(x float64, peaks []Peak, defSigma float64) float64 {
	var y float64
	for _, p := range peaks {
		sigma := p.Width
		if sigma <= 0 {
			sigma = defSigma
		}
		if sigma <= 0 {
			continue
		}
		d := x - p.RT
		y += p.Height * math.Exp(-(d*d)/(2*sigma*sigma))
	}
	return y
}

func (s ChromatogramSpec) render() (string, int, int, error) {
	th := s.Theme.withDefaults()
	width := s.Width
	if width <= 0 {
		width = 640
	}
	height := s.Height
	if height <= 0 {
		height = 280
	}
	xLabel := s.XLabel
	if xLabel == "" {
		xLabel = "Retention time (min)"
	}
	yLabel := s.YLabel
	if yLabel == "" {
		yLabel = "Signal (mAU)"
	}

	const (
		mLeft   = 48.0
		mBottom = 36.0
		mTop    = 16.0
		mRight  = 16.0
	)
	fw, fh := float64(width), float64(height)
	plotX := mLeft
	plotY := mTop
	plotW := fw - mLeft - mRight
	plotH := fh - mTop - mBottom
	if plotW < 1 {
		plotW = 1
	}
	if plotH < 1 {
		plotH = 1
	}
	plotBottom := plotY + plotH

	// XMax defaults to max(RT)*1.1 (≥1).
	xMax := s.XMax
	if xMax <= 0 {
		for _, p := range s.Peaks {
			if p.RT > xMax {
				xMax = p.RT
			}
		}
		xMax *= 1.1
	}
	if xMax < 1 {
		xMax = 1
	}

	// Y scale 0 to max(Height)*1.15 (≥1).
	var yMax float64
	for _, p := range s.Peaks {
		if p.Height > yMax {
			yMax = p.Height
		}
	}
	yMax *= 1.15
	if yMax < 1 {
		yMax = 1
	}

	defSigma := xMax * 0.012

	// Coordinate mappers.
	sx := func(x float64) float64 { return plotX + (x/xMax)*plotW }
	sy := func(y float64) float64 { return plotBottom - (y/yMax)*plotH }

	var b strings.Builder
	b.WriteString(header(width, height, th.BG))

	// Axes (muted lines): y-axis (left) and x-axis (bottom).
	fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="1"/>`,
		f(plotX), f(plotY), f(plotX), f(plotBottom), th.Muted)
	fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="1"/>`,
		f(plotX), f(plotBottom), f(plotX+plotW), f(plotBottom), th.Muted)

	// X-axis ticks + labels (a few evenly spaced).
	const nTicks = 5
	tickFont := 10.0
	for i := 0; i <= nTicks; i++ {
		xv := xMax * float64(i) / nTicks
		px := sx(xv)
		fmt.Fprintf(&b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="1"/>`,
			f(px), f(plotBottom), f(px), f(plotBottom+4), th.Muted)
		fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" font-family="%s" font-size="%s" fill="%s">%s</text>`,
			f(px), f(plotBottom+16), esc(th.Font), f(tickFont), th.Muted, esc(f(xv)))
	}

	// The signal trace (sum of Gaussians) as a polyline.
	steps := int(plotW)
	if steps < 2 {
		steps = 2
	}
	var pts strings.Builder
	for i := 0; i <= steps; i++ {
		xv := xMax * float64(i) / float64(steps)
		yv := gauss(xv, s.Peaks, defSigma)
		if i > 0 {
			pts.WriteByte(' ')
		}
		fmt.Fprintf(&pts, "%s,%s", f(sx(xv)), f(sy(yv)))
	}
	fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>`,
		pts.String(), th.Accent)

	// Peak apex labels (Peak.Label or f(RT)).
	apexFont := 11.0
	for _, p := range s.Peaks {
		if p.RT < 0 || p.RT > xMax {
			continue
		}
		lab := p.Label
		if lab == "" {
			lab = f(p.RT)
		}
		apexY := sy(gauss(p.RT, s.Peaks, defSigma))
		fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" font-family="%s" font-size="%s" fill="%s">%s</text>`,
			f(sx(p.RT)), f(apexY-6), esc(th.Font), f(apexFont), th.FG, esc(lab))
	}

	// Axis titles (FG).
	titleFont := 12.0
	fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" font-family="%s" font-size="%s" fill="%s">%s</text>`,
		f(plotX+plotW/2), f(fh-4), esc(th.Font), f(titleFont), th.FG, esc(xLabel))
	fmt.Fprintf(&b, `<text x="%s" y="%s" text-anchor="middle" transform="rotate(-90 %s %s)" font-family="%s" font-size="%s" fill="%s">%s</text>`,
		f(12), f(plotY+plotH/2), f(12), f(plotY+plotH/2), esc(th.Font), f(titleFont), th.FG, esc(yLabel))

	b.WriteString(footer)
	return b.String(), width, height, nil
}
