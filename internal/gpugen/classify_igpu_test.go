package gpugen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The classes the iGPU runners report, as ClassifyErr names them. Each row is a message whose
// WORDS would send a looser class's substring match somewhere else.
func TestClassifyErrIGPUWordingTable(t *testing.T) {
	cases := []struct{ msg, want string }{
		// TST1 / SIL7: a crash signal is not a timeout, whatever "killed" says
		{"sd-cli was killed by signal SIGSEGV", "engine_crashed"},
		{"sd-cli was killed by signal SIGABRT", "engine_crashed"},
		{"audiocpp_cli was killed by signal SIGBUS", "engine_crashed"},
		// each of the six crash signals on its own: the regexp lists them one by one
		{"sd-cli was killed by signal SIGFPE", "engine_crashed"},
		{"sd-cli was killed by signal SIGTRAP", "engine_crashed"},
		{"audiocpp_cli was killed by signal SIGSYS", "engine_crashed"},
		{"ENGINE_CRASHED: sd-cli died of signal SIGSEGV: an engine crash, not a timeout", "engine_crashed"},
		// ... but a stop or a kill from outside still is one
		{"sd-cli was killed by signal SIGTERM", "timeout"},
		{"sd-cli was killed by signal SIGHUP", "timeout"},
		// SIL7: ggml / sd.cpp memory exhaustion is oom, with or without a signal after it
		{"ggml_backend_alloc_ctx_tensors_from_buft: insufficient memory (attempted to allocate 5162.00 MB)", "oom"},
		{"[ERROR] ggml_runner.cpp:991 - wan alloc compute buffer failed", "oom"},
		{"OUT_OF_MEMORY: sd-cli ran out of memory (exit 1): insufficient memory", "oom"},
		{"SDCPP VIDEO FAILED: OUT_OF_MEMORY: sd-cli ran out of memory (died of signal SIGABRT)", "oom"},
		// TST17: each Vulkan allocation shape on its own
		{"vk::Device::allocateMemory: ErrorOutOfHostMemory", "oom"},
		{"vk::Device::allocateMemory: ErrorOutOfDeviceMemory", "oom"},
		{"ggml_vulkan: Device memory allocation of size 5368709120 failed.", "oom"},
		// TST2: the QA classes beat the oom / timeout words of a path or an engine log line
		{"[TIMING ts=9] a bloom room boom\nAUDIOCPP FAILED: DEAD_AIR: trailing silence 3.45s > 1s", "dead_air"},
		{"AUDIOCPP FAILED: FFMPEG_UNAVAILABLE: ffmpeg/ffprobe could not be resolved (/zoom/bin)", "ffmpeg_unavailable"},
		// SIL11 / SIL2: a measurement that could not be taken is its own class
		{"SDCPP VIDEO FAILED: UNMEASURABLE: ffmpeg could not read a duration from /work/room/clip.mp4", "unmeasurable"},
		{"AUDIOCPP FAILED: UNMEASURABLE: the dead-air gate could not measure /work/timeout/a.wav", "unmeasurable"},
		// SIL13: a bad device index is a config error, not a backend refusal
		{"AUDIOCPP FAILED: DEVICE_INVALID: --device \"abc\" is not a device index", "device_invalid"},
	}
	for _, c := range cases {
		if got := ClassifyErr(errString(c.msg)); got != c.want {
			t.Errorf("ClassifyErr(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}

// runnerClass reads the LAST valid class line from anywhere in the output, ignores unknown
// classes and lines that are not the whole line, and a forged line in the engine's output cannot
// outrank the runner's own (printed after it).
func TestRunnerClassReadsTheLastValidLineAnywhere(t *testing.T) {
	long := strings.Repeat("x", 300000)
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"none", "SDCPP VIDEO FAILED: nothing typed\n", ""},
		{"one", "a\nIGPU_CLASS=gpu_reset\n", "gpu_reset"},
		{"crlf", "a\r\nIGPU_CLASS=oom\r\n", "oom"},
		{"last wins", "IGPU_CLASS=black_clip\nengine said hi\nIGPU_CLASS=cpu_placement\n", "cpu_placement"},
		{"unknown class ignored", "IGPU_CLASS=gpu_reset\nIGPU_CLASS=not_a_class\n", "gpu_reset"},
		{"not the whole line", "prompt: IGPU_CLASS=oom\nIGPU_CLASS=oom tail\n", ""},
		{"deep in a long log", "IGPU_CLASS=dead_air\n" + long[:1000] + "\n", "dead_air"},
	}
	for _, c := range cases {
		if got := runnerClass([]byte(c.out)); got != c.want {
			t.Errorf("%s: runnerClass = %q, want %q", c.name, got, c.want)
		}
	}
}

// A RunError's class wins over wording; a plain error still goes through the wording table; the
// class survives wrapping.
func TestClassifyErrPrefersTheRunnersOwnClass(t *testing.T) {
	inner := errors.New("gpugen: x failed: exit status 1 (lockup timeout, killed, room)")
	var e error = &RunError{Class: "gpu_reset", err: inner}
	if got := ClassifyErr(e); got != "gpu_reset" {
		t.Errorf("RunError class = %q, want gpu_reset", got)
	}
	wrapped := errors.Join(errors.New("lane"), e)
	if got := ClassifyErr(wrapped); got != "gpu_reset" {
		t.Errorf("wrapped RunError class = %q, want gpu_reset", got)
	}
	if got := ClassifyErr(inner); got != "oom" {
		t.Errorf("the wording of the same text = %q, want oom (\"room\" is the baseline the class line overrides)", got)
	}
	if !errors.Is(e, inner) {
		t.Error("RunError must unwrap to the process error")
	}
}

// Generate classifies a typed failure from the class line even when the long human line pushed
// the token out of the 400-byte display tail, and keeps the TOKEN: label in the reason.
func TestGenerateReadsTheClassLineFromOutsideTheTail(t *testing.T) {
	requireNode(t)
	js := `
const pad = "y".repeat(900);
console.error("SDCPP VIDEO FAILED: GPU_RESET: the GPU reset during the run (" + pad + ") lockup timeout");
console.error("some later engine chatter " + pad);
console.error("IGPU_CLASS=gpu_reset");
process.exit(1);`
	_, err := Generate(context.Background(), Spec{Exe: "node", Script: "-e", Args: []string{js}, Out: filepath.Join(t.TempDir(), "x"), Timeout: 20 * time.Second, SkipFreeComfy: true})
	if err == nil {
		t.Fatal("must fail")
	}
	if got := ClassifyErr(err); got != "gpu_reset" {
		t.Fatalf("class = %q, want gpu_reset: %v", got, err)
	}
	if !strings.Contains(err.Error(), "GPU_RESET:") {
		t.Errorf("the reason lost its label: %v", err)
	}
	if len(err.Error()) > 1500 {
		t.Errorf("the reason must stay bounded, got %d bytes", len(err.Error()))
	}
	if strings.Count(err.Error(), "IGPU_CLASS=") != 0 {
		t.Errorf("the reason must not carry a raw class line a reader could mistake for one: %v", err)
	}
}

// SIL10: a client cancel on a lane whose runner answers SIGTERM with exit 143 (the iGPU runners)
// classifies as a timeout, the same as the cancel of every other media lane.
func TestClientCancelOfARunnerThatExits143IsATimeout(t *testing.T) {
	requireNode(t)
	js := `process.on("SIGTERM", () => process.exit(143)); console.error("started"); setInterval(() => {}, 1000);`
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1500 * time.Millisecond)
		cancel()
	}()
	_, err := Generate(ctx, Spec{Exe: "node", Script: "-e", Args: []string{js}, Out: filepath.Join(t.TempDir(), "x"), Timeout: time.Minute, SkipFreeComfy: true, OwnProcessGroup: true})
	if err == nil {
		t.Fatal("a cancelled run must fail")
	}
	if got := ClassifyErr(err); got != "timeout" {
		t.Fatalf("a client cancel classified as %q, want timeout: %v", got, err)
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Errorf("the reason should say the run was cancelled: %v", err)
	}
}

// endingCtx is a context whose end (DeadlineExceeded or Canceled) arrives at a moment the test chooses:
// when the child has printed its tail. A real deadline would have to be sized against node's start-up
// time, which a loaded host stretches.
type endingCtx struct {
	context.Context
	done chan struct{}
	err  error
}

func (c *endingCtx) Done() <-chan struct{} { return c.done }
func (c *endingCtx) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// F3: the deadline and the cancel gpugen itself observes are TYPED timeouts. The message embeds the
// child's last 400 bytes, and ClassifyErr's substring match reads the "oom" inside "room", "boom" and
// "zoom" as an out-of-memory failure, so a run killed at its deadline while its log said "a living
// room" was filed as oom, and retried as one.
func TestDeadlineAndCancelAreTypedTimeoutsWhateverTheTailSays(t *testing.T) {
	requireNode(t)
	const tailText = "engine log: a living room, a boom, the zoom lens"
	for _, c := range []struct {
		name string
		end  error
		word string // what the human message says about how it ended
	}{
		{"deadline", context.DeadlineExceeded, "deadline exceeded"},
		{"cancel", context.Canceled, "context canceled"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			sentinel := filepath.Join(t.TempDir(), "printed")
			// the sentinel is written once the tail has gone out to the pipe
			js := fmt.Sprintf(`process.stderr.write(%q + "\n", () => require("fs").writeFileSync(%q, "x")); setInterval(() => {}, 1000);`, tailText, sentinel)
			ctx := &endingCtx{Context: context.Background(), done: make(chan struct{}), err: c.end}
			go func() {
				for i := 0; i < 1200; i++ {
					if _, err := os.Stat(sentinel); err == nil {
						break
					}
					time.Sleep(25 * time.Millisecond)
				}
				close(ctx.done)
			}()
			_, err := Generate(ctx, Spec{Exe: "node", Script: "-e", Args: []string{js}, Out: filepath.Join(t.TempDir(), "x"), Timeout: time.Minute, SkipFreeComfy: true, OwnProcessGroup: true})
			if err == nil {
				t.Fatal("a run ended by its context must fail")
			}
			if !strings.Contains(err.Error(), "a living room") || !strings.Contains(err.Error(), c.word) {
				t.Fatalf("the premise is gone: the message must carry the child's tail and say %q: %v", c.word, err)
			}
			if got := ClassifyErr(errors.New(err.Error())); got != "oom" {
				t.Fatalf("the premise is gone: the same words as a plain error must read as oom (the wording is what the type overrides), got %q", got)
			}
			var re *RunError
			if !errors.As(err, &re) || re.Class != "timeout" {
				t.Errorf("want a *RunError with Class timeout, got %T %v", err, err)
			}
			if got := ClassifyErr(err); got != "timeout" {
				t.Errorf("ClassifyErr = %q, want timeout: %v", got, err)
			}
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Errorf("the process error must stay in the chain (%%w): %v", err)
			}
		})
	}
}
