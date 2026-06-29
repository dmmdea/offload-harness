// Package gpugen is the shared Go exec wrapper for every LOCAL GPU generation runner
// (image / video / audio). It shells out to a render/*.mjs (or a python TTS worker)
// that takes the single-slot GPU lock + drives ComfyUI/Chatterbox, and wraps that
// child with the hard-won lifecycle guards the bare runners lack:
//
//   - killTree on timeout/cancel — on Windows a bare node-kill ORPHANS the spawned
//     ComfyUI python grandchild (pinning ~8GB VRAM) and skips node's JS finally
//     (leaking the GPU lock); we taskkill the WHOLE process tree.
//   - WaitDelay — a short grace window after Cancel before the pipe is force-closed.
//   - defer freeComfyVRAM — belt-and-suspenders: however the child ended (clean exit,
//     error, or a timeout-kill that skipped its finally), force-drop any ComfyUI VRAM
//     so a render never leaves the GPU pinned (zero-always-warm; protects the
//     load-bearing CPU memory stack).
//
// This was extracted from internal/imagegen.Generate so video + audio get the SAME
// process-tree-kill (they previously had no Go wrapper → no kill on timeout). Pure
// os/exec + net/http, no deps. The output-file stat is the success gate (a child can
// exit 0 yet produce nothing). Behavior for the image path is preserved byte-for-byte.
package gpugen

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Spec describes one GPU-gen invocation. The caller assembles Args (the runner's CLI
// flags) — gpugen owns only the cross-cutting lifecycle, not the per-task arg shape.
type Spec struct {
	// Exe is the executable ("" => "node"). Script is its first argument (the
	// render/*.mjs path, or for `node -e` the verb). Args are the remaining argv.
	Exe    string
	Script string
	Args   []string
	// Env are extra "K=V" entries appended to the current environment (e.g.
	// COMFY_DIR, MEMORY_STACK, GPU_LOCK_WAIT_MS). nil = inherit only.
	Env []string
	// Out is the file the runner must produce; a missing/empty Out after a clean
	// exit is treated as failure (the caller defers). Required.
	Out string
	// Timeout bounds the whole invocation (cold-start + render + margin).
	Timeout time.Duration
	// ComfyAPI is the ComfyUI endpoint freeComfyVRAM hits after the run. "" =>
	// the COMFY_API env or the 127.0.0.1:8188 default. Set "" to inherit; a runner
	// with no ComfyUI (TTS) can leave it — /free on a dead endpoint is a no-op.
	ComfyAPI string
	// SkipFreeComfy, when true, suppresses the post-run ComfyUI /free (the TTS/voice
	// path never starts ComfyUI, so there is nothing to free). The killTree + output
	// stat still apply — the python worker still gets process-tree-killed on timeout.
	SkipFreeComfy bool
}

// Generate runs the spec's command, returning Out on success. A non-zero exit, a
// timeout (child + tree killed), or a missing/empty Out returns an error so the
// caller can map it to a clean defer. Never panics on a nil/absent process.
func Generate(ctx context.Context, spec Spec) (string, error) {
	exe := spec.Exe
	if exe == "" {
		exe = "node"
	}
	if spec.Script == "" {
		return "", fmt.Errorf("gpugen: no script configured")
	}
	if spec.Out == "" {
		return "", fmt.Errorf("gpugen: no output path configured")
	}
	cctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	args := append([]string{spec.Script}, spec.Args...)
	cmd := exec.CommandContext(cctx, exe, args...)
	cmd.Env = append(os.Environ(), spec.Env...)
	// On timeout/cancel kill the WHOLE process tree (invariant 3): a bare kill on
	// Windows orphans the ComfyUI python grandchild and bypasses node's finally.
	cmd.Cancel = func() error { return killTree(cmd.Process) }
	cmd.WaitDelay = 10 * time.Second
	// Belt-and-suspenders VRAM free (invariant 3, layer 2). Skipped for runners that
	// never launch ComfyUI (TTS) — there a /free is pointless, though harmless.
	if !spec.SkipFreeComfy {
		defer freeComfyVRAM(comfyAPI(spec.ComfyAPI))
	}

	o, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gpugen: %s failed: %w (%s)", baseName(spec.Script), err, tail(o, 400))
	}
	if fi, statErr := os.Stat(spec.Out); statErr != nil || fi.Size() == 0 {
		return "", fmt.Errorf("gpugen: no output at %q (%s)", spec.Out, tail(o, 400))
	}
	return spec.Out, nil
}

// killTree force-terminates p and ALL descendants. On Windows, killing the bare node
// process leaves the spawned ComfyUI python alive (no process-group semantics), so we
// taskkill the whole tree; elsewhere a direct kill is the best portable effort.
func killTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
		return nil
	}
	return p.Kill()
}

// comfyAPI resolves the ComfyUI endpoint: explicit override, else COMFY_API, else the
// 127.0.0.1:8188 default (matching the render/*.mjs scripts).
func comfyAPI(override string) string {
	if override != "" {
		return override
	}
	if v := os.Getenv("COMFY_API"); v != "" {
		return v
	}
	return "http://127.0.0.1:8188"
}

// freeComfyVRAM asks ComfyUI to unload models + free VRAM (zero-always-warm). Best-
// effort: a 1s timeout and any error are ignored (ComfyUI may already be gone, or
// never ours to free).
func freeComfyVRAM(api string) {
	cl := &http.Client{Timeout: 1 * time.Second}
	req, err := http.NewRequest(http.MethodPost, api+"/free", strings.NewReader(`{"unload_models":true,"free_memory":true}`))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, derr := cl.Do(req); derr == nil {
		_ = resp.Body.Close()
	}
}

// ClassifyErr maps a gen failure to a coarse class (oom|timeout|conn_refused|other)
// for the ledger's ErrClass. Mirrors pipeline.classifyErr; nil => "".
func ClassifyErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "out of memory") || strings.Contains(s, "cudamalloc") || strings.Contains(s, "oom"):
		return "oom"
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") || strings.Contains(s, "context canceled") || strings.Contains(s, "killed") || strings.Contains(s, "signal:"):
		return "timeout"
	case strings.Contains(s, "connection refused") || strings.Contains(s, "econnrefused") || strings.Contains(s, "no such host"):
		return "conn_refused"
	case strings.Contains(s, "llama-server 5"): // "llama-server 5xx: ..."
		return "http_5xx"
	default:
		return "other"
	}
}

// asInt coerces an any (int / int64 / float64) to int; 0 on miss. Shared so callers
// (imagegen, pipeline) can normalize param maps the same way.
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// AsInt is the exported coercion used by thin callers assembling Args.
func AsInt(v any) int { return asInt(v) }

// tail returns the last n bytes of b as a string (so a long stack trace is bounded).
func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// baseName returns the trailing path element of a script path for error messages.
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
