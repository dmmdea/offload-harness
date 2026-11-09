package mediacap

import "math"

// The size a Qwen-Image-Edit 2511 edit comes out at, by the rule render/comfy-edit.mjs and render/wf-qwen-image-edit.mjs apply, so a
// caller that has to recognise a finished edit (a batch --resume) never carries a size of its own.
//
// The graph scales the source once with ComfyUI's ImageScaleToTotalPixels at resolution_steps 16 and denoises that image, so the
// scaler's output size is the edit's output size. The megapixel target is the configured one verbatim when it is above 0
// (gen_edit_megapixels, or a request's megapixels), else the source's own megapixels held inside [0.9, 2.0]. The scaler then takes
// scale = sqrt(total / (w * h)) and snaps each side to round(side * scale / 16) * 16, with Python's round (to even on a tie).
const (
	editMegapixelCap   = 2.0  // render/wf-qwen-image-edit.mjs EDIT_MEGAPIXEL_CAP
	editMegapixelFloor = 0.9  // EDIT_MEGAPIXEL_FLOOR
	editResolutionStep = 16.0 // EDIT_RESOLUTION_STEPS
)

// EditOutputSize is the width and height an edit of a source srcW x srcH comes out at when the working canvas is pinned to
// configured megapixels (0 = follow the source). ok is false when the source size is unknown.
func EditOutputSize(configured float64, srcW, srcH int) (w, h int, ok bool) {
	if srcW <= 0 || srcH <= 0 {
		return 0, 0, false
	}
	mp := configured
	if !(mp > 0) || math.IsNaN(mp) || math.IsInf(mp, 0) {
		mp = float64(srcW) * float64(srcH) / (1024 * 1024)
		mp = math.Min(math.Max(mp, editMegapixelFloor), editMegapixelCap)
	}
	scale := math.Sqrt(mp * 1024 * 1024 / (float64(srcW) * float64(srcH)))
	w = int(math.RoundToEven(float64(srcW)*scale/editResolutionStep) * editResolutionStep)
	h = int(math.RoundToEven(float64(srcH)*scale/editResolutionStep) * editResolutionStep)
	return w, h, w > 0 && h > 0
}
