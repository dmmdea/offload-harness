package imagegen

import (
	"context"
	"image"
	_ "image/jpeg" // DecodeConfig registration for OutputSize
	_ "image/png"  // DecodeConfig registration for OutputSize
	"os"
	"strconv"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpugen"
)

// UpscaleModel binds the ESRGAN-family upscale route: one ComfyUI upscale_models
// filename (4x-UltraSharp.pth, RealESRGAN_x4plus.pth, ...). The factor is the
// model's own; the runner rescales from it when a request asks for scale/size.
type UpscaleModel struct {
	Model string
}

// upscaleArgs assembles the comfy-upscale.mjs argv. Pure; unit-tested. A request
// model overrides the binding (the caller already checked one of the two exists);
// width/height are emitted only as a pair — the runner rejects a half-given size
// before taking the GPU slot, and the pipeline defers on it earlier still.
func upscaleArgs(out, image string, params map[string]any, m UpscaleModel) []string {
	args := []string{out, image}
	model := m.Model
	if s, ok := params["model"].(string); ok && s != "" {
		model = s
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if f, ok := params["scale"].(float64); ok && f > 0 {
		args = append(args, "--scale", strconv.FormatFloat(f, 'g', -1, 64))
	}
	w, h := gpugen.AsInt(params["width"]), gpugen.AsInt(params["height"])
	if w > 0 && h > 0 {
		args = append(args, "--width", strconv.Itoa(w), "--height", strconv.Itoa(h))
	}
	if s, ok := params["method"].(string); ok && s != "" {
		args = append(args, "--method", s)
	}
	return args
}

// Upscale enlarges image with an ESRGAN-family model on the LOCAL ComfyUI (free).
// Same lifecycle guards as Generate: gpugen tree-kill on timeout + deferred /free.
func Upscale(ctx context.Context, node, script, comfyDir, out, image string, params map[string]any, m UpscaleModel, timeout time.Duration, extraEnv ...string) (string, error) {
	env := []string{"COMFY_DIR=" + comfyDir}
	if timeout > 0 {
		env = append(env, "COMFY_WAIT_SEC="+strconv.Itoa(int(timeout/time.Second)))
	}
	return gpugen.Generate(ctx, gpugen.Spec{
		Exe:     node,
		Script:  script,
		Args:    upscaleArgs(out, image, params, m),
		Env:     append(env, extraEnv...),
		Out:     out,
		Timeout: timeout,
	})
}

// OutputSize reads a PNG/JPEG header and returns its pixel size; (0, 0) when the
// file is unreadable or not one of those formats — the caller omits the fields
// rather than reporting a guess.
func OutputSize(path string) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	c, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return c.Width, c.Height
}
