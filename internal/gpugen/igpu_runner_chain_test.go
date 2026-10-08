package gpugen

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// goodSDHeader is the head of a healthy sd.cpp log: the device line and the Vulkan buffers the
// positive GPU guard needs.
var goodSDHeader = []string{
	"ggml_vulkan: Found 1 Vulkan devices:",
	"ggml_vulkan: 0 = AMD Radeon Graphics (RADV RENOIR) (radv) | uma: 1 | fp16: 1 | bf16: 0 | warp size: 64",
	"[VERBOSE] model_manager.cpp:490  - model manager prepared params backend buffers (4112.00 MB, 1264 tensors, 5 blocks, VRAM) on Vulkan0",
	"[VERBOSE] ggml_runner.cpp:1019 - wan compute buffer size: 512.00 MB(VRAM) on Vulkan0 (peak across 1 segment)",
}

// The failure the original finding reproduced: the REAL sdcpp-video.mjs replaying the captured
// device-lost log. Its final human line is longer than the 400-byte tail gpugen shows, with the
// GPU_RESET token at its START, so a classifier that reads the tail sees "lockup timeout" and says
// timeout. Generate reads the runner's class line from the whole output instead.
func TestRealVideoRunnerGPUResetIsTypedThroughGenerate(t *testing.T) {
	b := newIGPUBox(t)
	b.spec.Main = stubEngine{LogFile: fixturePath("sdcpp-vace-device-lost.log"), Exit: 1}
	out := filepath.Join(b.work, "clip.mp4")
	args := b.videoArgs(out, nil)

	final := b.rawFinal("sdcpp-video.mjs", args)
	if len(final) <= 420 {
		t.Fatalf("the premise is gone: the runner's GPU_RESET line is %d bytes, it must be longer than gpugen's 400-byte tail to pin the fix: %q", len(final), final)
	}
	if tailOnly := final[len(final)-400:]; strings.Contains(tailOnly, "GPU_RESET") {
		t.Fatalf("the premise is gone: the GPU_RESET token survives a 400-byte tail of %q", tailOnly)
	}
	if !strings.Contains(final[len(final)-400:], "lockup timeout") {
		t.Fatalf("the premise is gone: the tail no longer reads like a timeout: %q", final[len(final)-400:])
	}

	err := b.run("sdcpp-video.mjs", args, out)
	if err == nil {
		t.Fatal("a device-lost run must fail")
	}
	if got := ClassifyErr(err); got != "gpu_reset" {
		t.Fatalf("ClassifyErr = %q, want gpu_reset (the real %d-byte runner line); err: %v", got, len(final), err)
	}
	if !strings.Contains(err.Error(), "GPU_RESET:") {
		t.Errorf("the defer reason must carry the GPU_RESET: label, got: %v", err)
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Error("a reset run must deliver no file")
	}
}

type chainRow struct {
	name     string
	script   string
	spec     stubEngine
	args     func(b *igpuBox, out string) []string
	out      func(b *igpuBox) string
	want     string
	label    string // text the defer reason must carry ("" = none required)
	ffmpeg   bool   // measures a real clip or wav
	unixOnly bool   // needs a signal death
	timeout  time.Duration
}

func clipOut(b *igpuBox) string  { return filepath.Join(b.work, "nested", "clip.mp4") }
func audioOut(b *igpuBox) string { return filepath.Join(b.work, "nested", "audio.wav") }

func video(over map[string]string, extra ...string) func(b *igpuBox, out string) []string {
	return func(b *igpuBox, out string) []string { return b.videoArgs(out, over, extra...) }
}

func music(over map[string]string, extra ...string) func(b *igpuBox, out string) []string {
	return func(b *igpuBox, out string) []string { return b.audioArgs(out, "music", over, extra...) }
}

func chainRows(t *testing.T) []chainRow {
	t.Helper()
	longModel := strings.Repeat("a-long-model-directory/", 4) + "wan2.1_vace_1.3B_fp16.safetensors"
	vulkanDevice := goodSDHeader[:2]
	return []chainRow{
		{name: "cpu_placement/compute buffer on cpu", script: "sdcpp-video.mjs", want: "cpu_placement", label: "CPU_PLACEMENT:",
			spec: stubEngine{Log: append(append([]string{}, goodSDHeader...), "[VERBOSE] ggml_runner.cpp:1019 - t5 compute buffer size: 297.00 MB(RAM) on CPU (peak across 1 segment)"), Hang: true},
			args: video(nil), out: clipOut},
		{name: "cpu_placement/no gpu evidence at all", script: "sdcpp-video.mjs", want: "cpu_placement", label: "CPU_PLACEMENT:",
			spec: stubEngine{Log: []string{"sd.cpp: all done"}}, args: video(nil), out: clipOut},
		{name: "cpu_placement/the real audio.cpp host-prefill log", script: "audiocpp-generate.mjs", want: "cpu_placement", label: "CPU_PLACEMENT:",
			spec: stubEngine{LogFile: fixturePath("audiocpp-music-host-prefill.log"), Hang: true}, args: music(nil), out: audioOut},
		{name: "gpu_reset/the real device-lost log", script: "sdcpp-video.mjs", want: "gpu_reset", label: "GPU_RESET:",
			spec: stubEngine{LogFile: fixturePath("sdcpp-vace-device-lost.log"), Exit: 1}, args: video(nil), out: clipOut},
		{name: "illegal_instruction/exit 132", script: "audiocpp-generate.mjs", want: "illegal_instruction", label: "ILLEGAL_INSTRUCTION:",
			spec: stubEngine{Log: []string{"[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0"}, Exit: 132}, args: music(nil), out: audioOut},
		{name: "model_incompatible/a long model path", script: "sdcpp-video.mjs", want: "model_incompatible", label: "MODEL_INCOMPATIBLE:",
			spec: stubEngine{Log: append(append([]string{}, vulkanDevice...),
				"[ERROR] Diffusion model tensor 'model.diffusion_model.vace_patch_embedding.weight' not in model metadata",
				"[ERROR] model metadata validation failed"), Exit: 1},
			args: func(b *igpuBox, out string) []string {
				m := filepath.Join(b.work, longModel)
				_ = os.MkdirAll(filepath.Dir(m), 0o755)
				_ = os.WriteFile(m, []byte("x"), 0o644)
				return b.videoArgs(out, map[string]string{"--model": m})
			}, out: clipOut},
		{name: "oom/ggml insufficient memory, exit 1", script: "sdcpp-video.mjs", want: "oom", label: "OUT_OF_MEMORY:",
			spec: stubEngine{Log: append(append([]string{}, vulkanDevice...), "ggml_backend_alloc_ctx_tensors_from_buft: insufficient memory (attempted to allocate 5162.00 MB)"), Exit: 1},
			args: video(nil), out: clipOut},
		{name: "oom/sd.cpp alloc compute buffer failed, exit 1", script: "sdcpp-video.mjs", want: "oom", label: "OUT_OF_MEMORY:",
			spec: stubEngine{Log: append(append([]string{}, vulkanDevice...), "[ERROR] ggml_runner.cpp:991 - wan alloc compute buffer failed"), Exit: 1},
			args: video(nil), out: clipOut},
		{name: "oom/ggml insufficient memory then SIGABRT", script: "sdcpp-video.mjs", want: "oom", label: "OUT_OF_MEMORY:", unixOnly: true,
			spec: stubEngine{Log: append(append([]string{}, vulkanDevice...), "ggml_backend_alloc_ctx_tensors_from_buft: insufficient memory (attempted to allocate 5162.00 MB)"), Signal: "SIGABRT"},
			args: video(nil), out: clipOut},
		{name: "engine_crashed/SIGSEGV", script: "sdcpp-video.mjs", want: "engine_crashed", label: "ENGINE_CRASHED:", unixOnly: true,
			spec: stubEngine{Log: vulkanDevice, Signal: "SIGSEGV"}, args: video(nil), out: clipOut},
		{name: "engine_crashed/SIGABRT", script: "audiocpp-generate.mjs", want: "engine_crashed", label: "ENGINE_CRASHED:", unixOnly: true,
			spec: stubEngine{Log: []string{"[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0"}, Signal: "SIGABRT"}, args: music(nil), out: audioOut},
		{name: "token_cap_exceeded", script: "sdcpp-video.mjs", want: "token_cap_exceeded", label: "TOKEN_CAP_EXCEEDED:",
			spec: stubEngine{}, args: video(nil, "--max-tokens", "4", "--vae-stride", "16"), out: clipOut},
		{name: "extra_args_refused", script: "sdcpp-video.mjs", want: "extra_args_refused", label: "EXTRA_ARGS_REFUSED:",
			spec: stubEngine{}, args: video(nil, "--extra-args", `["--vae-on-cpu"]`), out: clipOut},
		{name: "cpu_backend_refused", script: "sdcpp-video.mjs", want: "cpu_backend_refused", label: "CPU_BACKEND_REFUSED:",
			spec: stubEngine{}, args: video(map[string]string{"--backend": "cpu"}), out: clipOut},
		{name: "binary_not_absolute", script: "sdcpp-video.mjs", want: "binary_not_absolute", label: "BINARY_NOT_ABSOLUTE:",
			spec: stubEngine{}, args: video(map[string]string{"--sd-bin": "sd-cli"}), out: clipOut},
		{name: "out_dir_unwritable", script: "sdcpp-video.mjs", want: "out_dir_unwritable", label: "OUT_DIR_UNWRITABLE:",
			spec: stubEngine{}, args: video(nil),
			out: func(b *igpuBox) string { return filepath.Join(b.file("blocker"), "clip.mp4") }},
		{name: "black_clip", script: "sdcpp-video.mjs", want: "black_clip", label: "BLACK_CLIP:", ffmpeg: true,
			spec: stubEngine{Log: goodSDHeader, Write: &stubWrite{Kind: "video", Black: true}}, args: video(nil), out: clipOut},
		{name: "frozen_clip", script: "sdcpp-video.mjs", want: "frozen_clip", label: "FROZEN_CLIP:", ffmpeg: true,
			spec: stubEngine{Log: goodSDHeader, Write: &stubWrite{Kind: "video", Frozen: true}},
			args: video(map[string]string{"--frames": "9"}), out: clipOut},
		{name: "dead_air/a silent render, engine log ends with room/boom/timeout words", script: "audiocpp-generate.mjs", want: "dead_air", label: "DEAD_AIR:", ffmpeg: true,
			spec: stubEngine{Log: []string{"[TIMING ts=1] ace_step.planner.weights.buffer_name Vulkan0", "[INFO] a bloom room boom, timeout killed"},
				Write: &stubWrite{Kind: "wav", Seconds: 4, Silent: true}}, args: music(nil), out: audioOut},
		{name: "timeout/the runner's own deadline", script: "sdcpp-video.mjs", want: "timeout", label: "timeout",
			spec: stubEngine{Log: goodSDHeader, Hang: true}, args: video(nil, "--timeout-sec", "3"), out: clipOut},
		{name: "timeout/gpugen's deadline kills the runner", script: "sdcpp-video.mjs", want: "timeout", timeout: 4 * time.Second,
			spec: stubEngine{Log: goodSDHeader, Hang: true}, args: video(nil), out: clipOut},
	}
}

// Every typed class the runners emit, through the REAL runner script, Generate and ClassifyErr: the
// class a ledger, a defer and a retry decision key on.
func TestEveryTypedRunnerClassSurvivesGenerate(t *testing.T) {
	for _, row := range chainRows(t) {
		row := row
		t.Run(row.name, func(t *testing.T) {
			if row.unixOnly && runtime.GOOS == "windows" {
				t.Skip("a signal death needs a POSIX host")
			}
			b := newIGPUBox(t)
			if row.ffmpeg {
				b.needFFmpeg()
			}
			if row.spec.Write != nil {
				w := *row.spec.Write
				w.FFmpeg = b.ffmpeg
				row.spec.Write = &w
			}
			b.spec.Main = row.spec
			out := row.out(b)
			timeout := row.timeout
			if timeout == 0 {
				timeout = 120 * time.Second
			}
			err := b.runFor(row.script, row.args(b, out), out, timeout)
			if err == nil {
				t.Fatal("the run must fail")
			}
			if got := ClassifyErr(err); got != row.want {
				t.Fatalf("ClassifyErr = %q, want %q\nerr: %v", got, row.want, err)
			}
			if row.label != "" && !strings.Contains(err.Error(), row.label) {
				t.Errorf("the defer reason must carry %q, got: %v", row.label, err)
			}
			if _, serr := os.Stat(out); serr == nil {
				t.Errorf("a failed run must deliver no file at %s", out)
			}
		})
	}
}
