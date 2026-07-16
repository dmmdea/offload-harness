package mediaops

import (
	"fmt"
	"strings"
)

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
		default:
			return fmt.Errorf("ops[%d]: unknown op %q (flatten_design|crop|resize|convert|composite|text)", i, op.Op)
		}
	}
	return nil
}

// UsesGimp reports whether the pipeline needs the GIMP engine (spec: only
// flatten_design does, and validation pins it to ops[0]).
func UsesGimp(ops []EditOp) bool {
	return len(ops) > 0 && ops[0].Op == "flatten_design"
}
