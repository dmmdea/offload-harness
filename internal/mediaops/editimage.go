package mediaops

import (
	"fmt"
	"regexp"
	"strings"
)

// renditionSuffixRe: a rendition suffix is a filename fragment — restrict to a
// filesystem-safe charset (review finding: separators/illegal chars reached disk).
var renditionSuffixRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// EditOp is one step of an offload_edit_image pipeline (spec §1). Ops are applied
// in order by render/edit_image.py (PIL); flatten_design runs first via GIMP.
type EditOp struct {
	Op string `json:"op"`
	// crop / composite / text placement
	X      int `json:"x,omitempty"`
	Y      int `json:"y,omitempty"`
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// resize
	KeepAspect *bool `json:"keep_aspect,omitempty"`
	// convert
	Format string `json:"format,omitempty"`
	// composite
	Overlay string  `json:"overlay,omitempty"`
	Opacity float64 `json:"opacity,omitempty"`
	// text
	Text   string `json:"text,omitempty"`
	Size   int    `json:"size,omitempty"`
	Color  string `json:"color,omitempty"`
	Font   string `json:"font,omitempty"`
	Anchor string `json:"anchor,omitempty"`
	// mask_boxes (white-on-black inpaint mask builder; REPLACES the working image)
	Boxes   []MaskBox `json:"boxes,omitempty"`
	Feather int       `json:"feather,omitempty"`
	Pad     int       `json:"pad,omitempty"`
	Invert  bool      `json:"invert,omitempty"`
	// perspective_composite (overlay reused; quad = 4 [x,y] dest corners UL,UR,LR,LL)
	Quad [][]float64 `json:"quad,omitempty"`
	// lut_cube (.cube 3D LUT; strength 0-1 blends graded over original, nil = 1.0)
	Path     string   `json:"path,omitempty"`
	Strength *float64 `json:"strength,omitempty"`
	// grade (tone/color: everything composes into ONE LUT per channel, single quantize)
	Levels        *GradeLevels `json:"levels,omitempty"`
	Curve         *GradeCurve  `json:"curve,omitempty"`
	WB            *GradeWB     `json:"wb,omitempty"`
	LuminanceOnly bool         `json:"luminance_only,omitempty"`
	// finish (delivery sharpening — MUST be the LAST op, after any resize:
	// sharpening before a resize is undone by resampling)
	Sharpen *FinishSharpen `json:"sharpen,omitempty"`
	Median  int            `json:"median,omitempty"`
	// instantiate_design (FIRST op only; GIMP layered-template factory —
	// layer name -> new text copy / replacement image path)
	SetText      map[string]string `json:"set_text,omitempty"`
	ReplaceImage map[string]string `json:"replace_image,omitempty"`
}

// FinishSharpen tunes the finish op's unsharp mask. ABSENT fields mean "worker
// default" (radius 1.2, percent 80, threshold 3 — post-AI-upscale web delivery).
// Pointers, not values: an EXPLICIT 0 (e.g. percent 0 = no visible sharpening,
// the Go-path way to get a median-only finish) must survive the struct
// round-trip — a value field with omitempty silently dropped it and the worker
// re-defaulted it (review finding 2026-07-17).
type FinishSharpen struct {
	Radius    *float64 `json:"radius,omitempty"`
	Percent   *int     `json:"percent,omitempty"`
	Threshold *int     `json:"threshold,omitempty"`
}

// GradeLevels is the levels sub-adjustment of a grade op. Zero values mean
// "worker default" (black 0, white 255, gamma 1.0) — the worker fills them in.
type GradeLevels struct {
	Black int     `json:"black,omitempty"`
	White int     `json:"white,omitempty"`
	Gamma float64 `json:"gamma,omitempty"`
}

// GradeCurve is a piecewise-linear tone curve through [in,out] control points (0-255).
type GradeCurve struct {
	Points [][]float64 `json:"points"`
}

// GradeWB is the white-balance sub-adjustment: mode "gray_world" (automatic) or
// "scale" with explicit per-channel multipliers (0 = worker default 1.0).
type GradeWB struct {
	Mode string  `json:"mode"`
	R    float64 `json:"r,omitempty"`
	G    float64 `json:"g,omitempty"`
	B    float64 `json:"b,omitempty"`
}

// MaskBox is one white rectangle of a mask_boxes op, in pixel coordinates of the
// working image (the same contract as offload_inpaint_image's mask: white = repaint).
type MaskBox struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

var editFormats = map[string]bool{"png": true, "jpg": true, "jpeg": true, "webp": true}

// ValidateOps checks a whole pipeline BEFORE any work starts, so a bad op defers
// with a message naming the offending index instead of failing mid-pipeline with a
// half-written file. Pure.
func ValidateOps(ops []EditOp) error {
	if len(ops) == 0 {
		return fmt.Errorf("ops: at least one operation is required")
	}
	for i, op := range ops {
		fail := func(msg string) error { return fmt.Errorf("ops[%d] %s: %s", i, op.Op, msg) }
		switch op.Op {
		case "flatten_design":
			if i != 0 {
				return fail("must be the first op (it opens the source design file)")
			}
		case "instantiate_design":
			if i != 0 {
				return fail("must be the first op (it opens the source template file)")
			}
			if len(op.SetText)+len(op.ReplaceImage) == 0 {
				return fail("requires at least one of set_text/replace_image (layer name -> value)")
			}
			for name := range op.SetText {
				if name == "" {
					return fail("set_text layer names must be non-empty")
				}
			}
			for name, path := range op.ReplaceImage {
				if name == "" {
					return fail("replace_image layer names must be non-empty")
				}
				if path == "" {
					return fail(fmt.Sprintf("replace_image[%q] path must be non-empty", name))
				}
			}
		case "crop":
			if op.Width <= 0 || op.Height <= 0 {
				return fail("width and height must be positive")
			}
			if op.X < 0 || op.Y < 0 {
				return fail("x and y must be non-negative")
			}
		case "resize":
			if op.Width <= 0 && op.Height <= 0 {
				return fail("at least one of width/height is required")
			}
		case "convert":
			if op.Format == "" {
				return fail("format is required (png|jpg|webp)")
			}
			if !editFormats[strings.ToLower(op.Format)] {
				return fail(fmt.Sprintf("unsupported format %q (png|jpg|webp)", op.Format))
			}
		case "composite":
			if op.Overlay == "" {
				return fail("overlay path is required")
			}
			if op.Opacity < 0 || op.Opacity > 1 {
				return fail("opacity must be within 0..1")
			}
		case "text":
			if op.Text == "" {
				return fail("text is required")
			}
			if op.Size < 0 {
				return fail("size must be positive")
			}
		case "mask_boxes":
			if len(op.Boxes) == 0 {
				return fail("requires a non-empty boxes array")
			}
			for j, b := range op.Boxes {
				if b.Width <= 0 || b.Height <= 0 {
					return fail(fmt.Sprintf("boxes[%d] width and height must be positive", j))
				}
				if b.X < 0 || b.Y < 0 {
					return fail(fmt.Sprintf("boxes[%d] x and y must be non-negative", j))
				}
			}
			if op.Feather < 0 {
				return fail("feather must be non-negative")
			}
			if op.Pad < 0 {
				return fail("pad must be non-negative")
			}
		case "grade":
			if op.Levels == nil && op.Curve == nil && op.WB == nil {
				return fail("requires at least one of levels/curve/wb")
			}
			if L := op.Levels; L != nil {
				white := L.White
				if white == 0 {
					white = 255 // worker default
				}
				if L.Black < 0 || L.Black > 254 {
					return fail("levels.black must be within 0..254")
				}
				if white < 1 || white > 255 {
					return fail("levels.white must be within 1..255")
				}
				if L.Black >= white {
					return fail("levels.black must be below levels.white")
				}
				if L.Gamma != 0 && (L.Gamma < 0.1 || L.Gamma > 10) {
					return fail("levels.gamma must be within 0.1..10")
				}
			}
			if c := op.Curve; c != nil {
				if len(c.Points) == 0 {
					return fail("curve.points must be a non-empty array of [in,out] pairs")
				}
				for j, p := range c.Points {
					if len(p) != 2 {
						return fail(fmt.Sprintf("curve.points[%d] must be an [in,out] pair", j))
					}
					if p[0] < 0 || p[0] > 255 || p[1] < 0 || p[1] > 255 {
						return fail(fmt.Sprintf("curve.points[%d] values must be within 0..255", j))
					}
				}
			}
			if wb := op.WB; wb != nil {
				switch wb.Mode {
				case "gray_world":
				case "scale":
					for _, s := range []float64{wb.R, wb.G, wb.B} {
						if s < 0 || s > 8 {
							return fail("wb scale factors must be within 0..8 (0 = default 1.0)")
						}
					}
				default:
					return fail(fmt.Sprintf("wb.mode must be gray_world or scale, got %q", wb.Mode))
				}
			}
		case "perspective_composite":
			if op.Overlay == "" {
				return fail("overlay path is required")
			}
			if len(op.Quad) != 4 {
				return fail("quad must be exactly 4 [x,y] corners (UL,UR,LR,LL winding)")
			}
			for j, p := range op.Quad {
				if len(p) != 2 {
					return fail(fmt.Sprintf("quad[%d] must be an [x,y] pair", j))
				}
			}
		case "finish":
			// finish must be the LAST op after any resize (sharpen-then-resample is
			// undone by the resample) — documented, not enforced: mask/rendition
			// chains may legitimately follow.
			if op.Median != 0 && op.Median != 3 && op.Median != 5 {
				return fail("median must be 3 or 5")
			}
			if sh := op.Sharpen; sh != nil {
				if sh.Radius != nil && (*sh.Radius < 0 || *sh.Radius > 10) {
					return fail("sharpen.radius must be within 0..10 (>3 already halos)")
				}
				if sh.Percent != nil && (*sh.Percent < 0 || *sh.Percent > 500) {
					return fail("sharpen.percent must be within 0..500")
				}
				if sh.Threshold != nil && (*sh.Threshold < 0 || *sh.Threshold > 255) {
					return fail("sharpen.threshold must be within 0..255")
				}
			}
		case "lut_cube":
			if op.Path == "" {
				return fail("path to a .cube LUT file is required")
			}
			if op.Strength != nil && (*op.Strength < 0 || *op.Strength > 1) {
				return fail("strength must be within 0..1")
			}
		default:
			return fmt.Errorf("ops[%d]: unknown op %q (flatten_design|instantiate_design|crop|resize|convert|composite|text|mask_boxes|grade|lut_cube|perspective_composite|finish)", i, op.Op)
		}
	}
	return nil
}

// Rendition is one entry of the optional export matrix: after the ops pipeline
// produces the master out, each rendition re-runs the worker with a
// resize+convert pair, writing <out-stem><suffix>.<format>.
type Rendition struct {
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Format string `json:"format"`
	Suffix string `json:"suffix"`
}

// ValidateRenditions checks the export matrix BEFORE any work starts. Pure.
// An empty set is valid (renditions are optional).
func ValidateRenditions(rs []Rendition) error {
	seen := map[string]bool{}
	for i, r := range rs {
		fail := func(msg string) error { return fmt.Errorf("renditions[%d]: %s", i, msg) }
		if r.Width < 0 || r.Height < 0 {
			return fail("width/height must not be negative")
		}
		if r.Width <= 0 && r.Height <= 0 {
			return fail("at least one of width/height must be positive")
		}
		if r.Format == "" {
			return fail("format is required (png|jpg|webp)")
		}
		if !editFormats[strings.ToLower(r.Format)] {
			return fail(fmt.Sprintf("unsupported format %q (png|jpg|webp)", r.Format))
		}
		if r.Suffix == "" {
			return fail("suffix is required (it names the output beside the master)")
		}
		// The suffix becomes part of a filename — keep it filesystem-safe (no path
		// separators, no Windows-illegal chars).
		if !renditionSuffixRe.MatchString(r.Suffix) {
			return fail(fmt.Sprintf("suffix %q must match [A-Za-z0-9._-]+", r.Suffix))
		}
		if seen[r.Suffix] {
			return fail(fmt.Sprintf("duplicate suffix %q — suffixes must be unique", r.Suffix))
		}
		seen[r.Suffix] = true
	}
	return nil
}

// renditionOut derives a rendition's output path from the master out path:
// <stem><suffix>.<ext> (jpeg normalizes to jpg).
func renditionOut(masterOut string, r Rendition) string {
	stem := strings.TrimSuffix(masterOut, pathExt(masterOut))
	ext := strings.ToLower(r.Format)
	if ext == "jpeg" {
		ext = "jpg"
	}
	return stem + r.Suffix + "." + ext
}

func pathExt(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		switch p[i] {
		case '.':
			return p[i:]
		case '/', '\\':
			return ""
		}
	}
	return ""
}

// UsesGimp reports whether the pipeline needs the GIMP engine (spec: only
// flatten_design and instantiate_design do, and validation pins both to ops[0]).
func UsesGimp(ops []EditOp) bool {
	return len(ops) > 0 && (ops[0].Op == "flatten_design" || ops[0].Op == "instantiate_design")
}
