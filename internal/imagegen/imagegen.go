// Package imagegen generates an image from a text prompt by shelling out to the repo's
// render/comfy-generate.mjs (Node), which wraps the proven comfy-render.mjs with the
// GPU-lock + ComfyUI start/stop lifecycle. The render runs on the LOCAL ComfyUI (SDXL/
// RealVisXL) — free, no cloud. It is now a THIN caller of internal/gpugen, which owns
// the shared exec lifecycle (process-tree-kill on timeout, WaitDelay, defer
// freeComfyVRAM) so image/video/audio all get the SAME guards. Behavior is preserved
// byte-for-byte; only the lifecycle was lifted into gpugen.
package imagegen

import (
	"context"
	"strconv"
	"time"

	"github.com/dmmdea/local-offload/internal/gpugen"
)

// Generate runs `node <script> <out> <prompt> [--negative ..] [--width ..] ...` and
// returns out on success. node is the node executable ("" => "node"); script is the
// absolute path to comfy-generate.mjs; comfyDir is exported as COMFY_DIR for the script.
// params may carry negative (string) and width/height/steps/seed (int-ish). A non-zero
// exit, a timeout, or a missing/empty output file returns an error (the caller defers).
func Generate(ctx context.Context, node, script, comfyDir, out, prompt string, params map[string]any, timeout time.Duration) (string, error) {
	args := []string{out, prompt}
	if n, ok := params["negative"].(string); ok && n != "" {
		args = append(args, "--negative", n)
	}
	for _, k := range []string{"width", "height", "steps", "seed"} {
		if v := gpugen.AsInt(params[k]); v > 0 {
			args = append(args, "--"+k, strconv.Itoa(v))
		}
	}
	return gpugen.Generate(ctx, gpugen.Spec{
		Exe:     node,
		Script:  script,
		Args:    args,
		Env:     []string{"COMFY_DIR=" + comfyDir},
		Out:     out,
		Timeout: timeout,
	})
}
