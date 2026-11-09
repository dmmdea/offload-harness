package config

import (
	"fmt"
	"strings"
)

// ImageDimGrid is the pixel grid this (effective) image binding's render graph snaps a width and a height to, and the smallest side it
// renders: step 0 means the graph takes the size as asked (the SDXL-shaped generic graph, and the stable-diffusion.cpp engine).
//
// It is the Go reading of the constants the graph builders own, kept in one place so the batch verb can ask for the size the render will
// produce: render/wf-qwen-image.mjs and render/wf-krea2.mjs and render/wf-flux3-klein.mjs floor to a multiple of 16 (minimum 64),
// render/wf-qwen-image-21.mjs to a multiple of 32 (minimum 256, QWEN_IMAGE_21_DIM_STEP / _MIN) and render/wf-hidream-o1.mjs to a multiple
// of 32 (minimum 64). A change to a builder's grid is a change here.
func (c Config) ImageDimGrid() (step, lo int) {
	if c.ImageGenEngine == "sdcpp" {
		return 0, 0
	}
	switch c.ImageGenFamily {
	case FamilyQwenImage21:
		return 32, 256
	case "hidream-o1", "hidream-o1-dev":
		return 32, 64
	case "qwen-image", "krea2", FamilyFlux3Klein:
		return 16, 64
	}
	return 0, 0
}

// SnapImageDims returns the width and height the render of this binding produces for the size asked: each side floored to the binding's
// grid (ImageDimGrid), a side of 0 (not named) left at 0. A binding with no grid returns the size unchanged. It is idempotent.
func (c Config) SnapImageDims(w, h int) (int, int) {
	step, lo := c.ImageDimGrid()
	if step == 0 {
		return w, h
	}
	snap := func(n int) int {
		if n <= 0 {
			return n
		}
		if n = n / step * step; n < lo {
			n = lo
		}
		return n
	}
	return snap(w), snap(h)
}

// WithImagePlacement is this config with a per-call image placement laid over the binding's own imagegen_placement: "" keeps the binding's,
// "single" is the one-card lease, "multicard" the card-only pair. Anything else is refused by name. The placement is not part of the
// recipe digest (internal/mediacap digestForm), so it never changes which nodes match.
func (c Config) WithImagePlacement(placement string) (Config, error) {
	switch p := strings.ToLower(strings.TrimSpace(placement)); p {
	case "":
		return c, nil
	case PlacementSingle, PlacementMulticard:
		c.ImageGenPlacement = p
		return c, nil
	default:
		return c, fmt.Errorf("unknown placement %q (valid: %q, %q)", placement, PlacementSingle, PlacementMulticard)
	}
}
