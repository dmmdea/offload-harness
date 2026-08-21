package imagegen

import (
	"context"
	"encoding/binary"
	"image"
	_ "image/jpeg" // DecodeConfig registration for OutputSize
	_ "image/png"  // DecodeConfig registration for OutputSize
	"io"
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

// OutputSize reads a PNG/JPEG/WebP header and returns its pixel size — the same three
// formats render/image-size.mjs measures, so the Go-side verification of an upscale
// sees every source the runner can pin a size for. (0, 0) when the file is unreadable
// or none of those formats; this never guesses a size it did not read.
func OutputSize(path string) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	if c, _, err := image.DecodeConfig(f); err == nil {
		return c.Width, c.Height
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, 0
	}
	head := make([]byte, 30)
	if n, _ := io.ReadFull(f, head); n < 30 {
		return 0, 0
	}
	return webpSize(head)
}

// webpSize mirrors image-size.mjs's webpSize: a RIFF/WEBP container, then one of the
// three payload headers with three different dimension encodings. (0, 0) for anything
// else — the stdlib has no WebP decoder and this route only needs the size.
func webpSize(b []byte) (int, int) {
	if len(b) < 30 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0
	}
	le24 := func(i int) int { return int(b[i]) | int(b[i+1])<<8 | int(b[i+2])<<16 }
	switch string(b[12:16]) {
	case "VP8X": // extended: 24-bit little-endian canvas dimensions, stored minus one
		return le24(24) + 1, le24(27) + 1
	case "VP8 ": // lossy: 3-byte frame tag, 3-byte sync code, then 14-bit dimensions
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0
		}
		return int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
	case "VP8L": // lossless: 0x2f signature, then 14 bits width-1 and 14 bits height-1
		if b[20] != 0x2f {
			return 0, 0
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1
	}
	return 0, 0
}
