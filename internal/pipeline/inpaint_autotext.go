package pipeline

// EXPERIMENTAL auto-text-mask chain for inpaint_image (--auto-text): vision-detect
// rendered-text regions → build a white-on-black mask via the mask_boxes edit op →
// hand the mask to the normal inpaint route. The vision boxes ride the existing
// free-text vision path (vqa machinery, this machine's vision model) with a
// strict-JSON instruction; ANY doubt — unparseable answer, no boxes, degenerate or
// absurd (>60% coverage) boxes — errors out so the caller defers with the manual
// mask_boxes workflow named. It never silently repaints unverified regions.
//
// NOTE (plan Task 9 gate): grounding reliability of the machine's vision model must
// be proven on real gibberish renders before trusting this path in production; the
// validation here rejects absurd output but cannot catch plausible-but-wrong boxes.

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"  // dimension probing for auto-text masks
	_ "image/jpeg" // dimension probing for auto-text masks
	_ "image/png"  // dimension probing for auto-text masks
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediaops"
)

// visionBox is one text bounding box reported by the vision model, in pixel
// coordinates of the probed image. Float-typed: models emit both ints and floats.
type visionBox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// parseVisionBoxes extracts a boxes array from the vision model's answer. It
// accepts a bare JSON array, a {"boxes":[...]} envelope, or either embedded in
// prose (models pad despite instructions). Anything else errors — never a silent
// empty result.
func parseVisionBoxes(answer string) ([]visionBox, error) {
	answer = strings.TrimSpace(answer)
	try := func(s string) ([]visionBox, bool) {
		var env struct {
			Boxes []visionBox `json:"boxes"`
		}
		if err := json.Unmarshal([]byte(s), &env); err == nil && env.Boxes != nil {
			return env.Boxes, true
		}
		var arr []visionBox
		if err := json.Unmarshal([]byte(s), &arr); err == nil {
			return arr, true
		}
		return nil, false
	}
	if b, ok := try(answer); ok {
		return b, nil
	}
	// embedded array: first '[' .. last ']'
	if i, j := strings.Index(answer, "["), strings.LastIndex(answer, "]"); i >= 0 && j > i {
		if b, ok := try(answer[i : j+1]); ok {
			return b, nil
		}
	}
	// embedded envelope: first '{' .. last '}'
	if i, j := strings.Index(answer, "{"), strings.LastIndex(answer, "}"); i >= 0 && j > i {
		if b, ok := try(answer[i : j+1]); ok {
			return b, nil
		}
	}
	return nil, fmt.Errorf("vision answer carries no parseable boxes JSON")
}

// validateTextBoxes clips the vision boxes to the image bounds, drops degenerate
// ones, and rejects an empty result or an absurd total coverage (>60% of the
// image — that is not "text regions", that is repainting the picture).
func validateTextBoxes(boxes []visionBox, w, h int) ([]mediaops.MaskBox, error) {
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid image dimensions %dx%d", w, h)
	}
	var out []mediaops.MaskBox
	area := 0
	for _, b := range boxes {
		x0, y0 := int(b.X), int(b.Y)
		x1, y1 := int(b.X+b.Width), int(b.Y+b.Height)
		if x0 < 0 {
			x0 = 0
		}
		if y0 < 0 {
			y0 = 0
		}
		if x1 > w {
			x1 = w
		}
		if y1 > h {
			y1 = h
		}
		if x1-x0 <= 0 || y1-y0 <= 0 {
			continue // degenerate or fully out of bounds
		}
		out = append(out, mediaops.MaskBox{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0})
		area += (x1 - x0) * (y1 - y0)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no usable text boxes detected")
	}
	if float64(area) > 0.6*float64(w)*float64(h) {
		return nil, fmt.Errorf("detected boxes cover >60%% of the image — refusing to repaint it wholesale")
	}
	return out, nil
}

// imageDims probes an image file's pixel dimensions (PNG/JPEG/GIF headers only).
func imageDims(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, fmt.Errorf("cannot read image dimensions of %s: %w", path, err)
	}
	return cfg.Width, cfg.Height, nil
}

// autoTextMask runs the vision→mask_boxes chain: detect rendered-text boxes on
// image, validate them, and render a feathered white-on-black mask file under
// MediaDir. Returns the mask path; any failure errors (the caller defers).
func (p *Pipeline) autoTextMask(ctx context.Context, imagePath string) (string, error) {
	w, h, err := imageDims(imagePath)
	if err != nil {
		return "", err
	}
	instr := fmt.Sprintf(
		"Return STRICT JSON only, no prose: {\"boxes\":[{\"x\":N,\"y\":N,\"width\":N,\"height\":N},...]} — "+
			"bounding boxes of every region containing rendered text (letters, numbers, gibberish glyphs), "+
			"in pixel coordinates of this %dx%d image. If there is no rendered text, return {\"boxes\":[]}.", w, h)
	res := p.Run(ctx, core.Request{
		Task:   core.TaskVQA,
		Image:  imagePath,
		Params: map[string]any{"question": instr},
	})
	if !res.OK {
		return "", fmt.Errorf("vision box detection deferred: %s", res.Reason)
	}
	var out struct {
		Answer string `json:"answer"`
	}
	answer := string(res.Data)
	if err := json.Unmarshal(res.Data, &out); err == nil && out.Answer != "" {
		answer = out.Answer
	}
	boxes, err := parseVisionBoxes(answer)
	if err != nil {
		return "", err
	}
	mb, err := validateTextBoxes(boxes, w, h)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(p.cfg.MediaDir, 0o755); err != nil {
		return "", err
	}
	maskOut := filepath.Join(p.cfg.MediaDir,
		"inpaint-autotext-"+sha256hex(fmt.Sprintf("%s|%v|%d", imagePath, mb, time.Now().UnixNano()))[:8]+".png")
	_, err = mediaops.RunEditImage(ctx, p.editConfig(), mediaops.EditRequest{
		Image: imagePath,
		Ops:   []mediaops.EditOp{{Op: "mask_boxes", Boxes: mb, Feather: 8}},
		Out:   maskOut,
	})
	if err != nil {
		return "", fmt.Errorf("mask render failed: %w", err)
	}
	return maskOut, nil
}
