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
//     load-bearing memory stack).
//
// This was extracted from internal/imagegen.Generate so video + audio get the SAME
// process-tree-kill (they previously had no Go wrapper → no kill on timeout). Pure
// os/exec + net/http, no deps. The output-file stat is the success gate (a child can
// exit 0 yet produce nothing). Behavior for the image path is preserved byte-for-byte.
package gpugen

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ResolveScript resolves a configured render-script path for a GPU-gen runner.
// A RELATIVE path (the shipped defaults are "render/*.mjs") is resolved against
// the EXECUTABLE's directory, NOT the process cwd: an MCP host (e.g. the
// ~/.claude.json registration) spawns the server with no meaningful cwd, so a
// cwd-relative default made node fail with MODULE_NOT_FOUND — an instant defer
// on every video/voice/music call. If the resolved file does not exist, the
// error reads "script not found at <absolute-path>": a distinct, actionable
// defer reason, unlike the generic runner failure.
func ResolveScript(script string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving executable path: %w", err)
	}
	return resolveScriptIn(script, filepath.Dir(exe))
}

// ResolveScriptIn is ResolveScript against a caller-supplied executable dir. It
// exists so capability reporting (internal/mediacap) answers "will this script
// resolve?" with THIS rule rather than a second copy of it that can drift.
func ResolveScriptIn(script, exeDir string) (string, error) {
	return resolveScriptIn(script, exeDir)
}

// resolveScriptIn is ResolveScript with an injectable exe dir (unit-testable).
func resolveScriptIn(script, exeDir string) (string, error) {
	p := script
	if !filepath.IsAbs(p) {
		p = filepath.Join(exeDir, p)
	}
	if fi, err := os.Stat(p); err != nil || fi.IsDir() {
		return "", fmt.Errorf("script not found at %s", p)
	}
	return p, nil
}

// Spec describes one GPU-gen invocation. The caller assembles Args (the runner's CLI
// flags) — gpugen owns only the cross-cutting lifecycle, not the per-task arg shape.
type Spec struct {
	// Exe is the executable ("" => "node"). Script is its first argument (the
	// render/*.mjs path, or for `node -e` the verb). Args are the remaining argv.
	Exe    string
	Script string
	Args   []string
	// Dir is the child's working directory ("" = inherit the current process's
	// cwd, the pre-existing behavior for every caller that leaves it unset).
	// Added for Task 6 (pipeline-job): an externally-provided pipeline CLI is
	// invoked with its OWN repo root as cwd (PipelineSpec.Workdir), unlike the
	// bundled render/*.mjs scripts, which run from wherever the harness resolves
	// them and never need a cwd override.
	Dir string
	// Env are extra "K=V" entries appended to the current environment (e.g.
	// COMFY_DIR, MEMORY_STACK, GPU_LOCK_WAIT_MS). nil = inherit only.
	Env []string
	// EnvExact, when true, makes Env the child's COMPLETE environment: nothing
	// is inherited from this process. The compose route (ADR 0059) sets it so an
	// external CLI's runner starts from an allowlist — no cloud key, lease token
	// (GPU_LEASE_*) or NODE_OPTIONS in the server's env can reach it. false keeps
	// the inherit-and-append behavior every other runner relies on.
	EnvExact bool
	// Out is the file the runner must produce; a missing/empty Out after a clean
	// exit is treated as failure (the caller defers). Required.
	Out string
	// Timeout bounds the whole invocation (cold-start + render + margin).
	Timeout time.Duration
	// ComfyAPI is the ComfyUI endpoint freeComfyVRAM hits after the run. "" =>
	// the COMFY_API env or the 127.0.0.1:8188 default. Set "" to inherit; a runner
	// with no ComfyUI (TTS) can leave it — /free on a dead endpoint is a no-op.
	// When set (a per-card instance) it is also exported to the runner as COMFY_API,
	// so the runner talks to, launches and frees the same instance the /free targets.
	ComfyAPI string
	// CardUUID is the GPU uuid the runner pins its ComfyUI instance to (exported as
	// COMFY_CARD_UUID; the runner sets CUDA_VISIBLE_DEVICES from it, never an index).
	// "" = not card-bound. It also blanks COMFY_CUDA_DEVICE: a uuid pin replaces the
	// legacy index, and the runner refuses a launch that carries both.
	CardUUID string
	// SkipFreeComfy, when true, suppresses the post-run ComfyUI /free (the TTS/voice
	// path never starts ComfyUI, so there is nothing to free). The killTree + output
	// stat still apply — the python worker still gets process-tree-killed on timeout.
	SkipFreeComfy bool
	// OwnProcessGroup, when true (non-Windows only; a no-op on Windows, where taskkill /T
	// already reaps the tree), starts the runner as the leader of its own process group, so a
	// timeout or a cancel signals the WHOLE group instead of the bare node process: SIGTERM
	// first, so the runner can kill its engine and remove its temp dirs, then SIGKILL after a
	// grace. The iGPU media runners set it (their engines are spawn-per-job native binaries that
	// would otherwise keep the GPU after the lease is released). false keeps every other lane's
	// kill exactly as it was.
	OwnProcessGroup bool
	// Footprint, when non-nil, turns on passive per-render VRAM peak sampling for
	// the fleet-node footprint store (added 2026-07-17): while the child runs,
	// SampleFunc is polled and the max observation is reported via OnFootprint —
	// on SUCCESS only (a crashed/phantom run's peak may be partial). nil keeps the
	// legacy CombinedOutput path byte-identical.
	Footprint *FootprintKey
	// SampleFunc returns the current VRAM usage in GiB attributable to the render
	// rooted at childPid. The CALLER composes what a sample means (PDH process-tree
	// sum, or a global-delta closure) — gpugen stays dependency-free and only tracks
	// the peak. nil = no sampling (OnFootprint never fires).
	SampleFunc func(childPid int) (float64, error)
	// OnFootprint receives the observed peak (GiB) after a SUCCESSFUL render whose
	// sampled peak was > 0. nil = observations are discarded.
	OnFootprint func(peakGiB float64)
	// HostSampleFunc returns the HOST memory (GiB) of the process tree rooted at childPid right now: its
	// private bytes (the commit-side figure) and its resident set (the physical-RAM figure). It is polled on
	// the same tick as SampleFunc, only while Footprint is set, and the peak of each is reported once via
	// OnHostFootprint on a SUCCESSFUL run (G3 of the P0 plan: the host-RAM guard sizes a render from what
	// renders were measured to hold, not only from the size of their model files). The caller composes what a
	// sample means (gpulease.TreeMemory); gpugen stays dependency-free. nil = no host sampling.
	HostSampleFunc func(childPid int) (privateGiB, residentGiB float64, err error)
	// OnHostFootprint receives the peak private and the peak resident memory (GiB) after a SUCCESSFUL render
	// whose sampled private peak was > 0. nil = discarded.
	OnHostFootprint func(privatePeakGiB, residentPeakGiB float64)
}

// FootprintKey identifies which footprint-store entry a sampled render belongs to
// (mirrors the fleet contract's model_footprints identity: family + quant + task).
type FootprintKey struct {
	Family string // e.g. "sdxl", "wan2.2", "whisper", "acestep"
	Quant  string // e.g. "bf16", "q8_0"; "" = node default
	Task   string // fleet task_type, e.g. "image-gen"
}

// Sampling bundles Spec's passive footprint-sampling hook fields so the thin
// wrappers (imagegen, rungraph) can thread them opaquely without growing three
// parameters each. nil = no sampling (the legacy Spec path, byte-identical).
type Sampling struct {
	Footprint   *FootprintKey
	SampleFunc  func(childPid int) (float64, error)
	OnFootprint func(peakGiB float64)
	// HostSampleFunc / OnHostFootprint: see Spec.
	HostSampleFunc  func(childPid int) (privateGiB, residentGiB float64, err error)
	OnHostFootprint func(privatePeakGiB, residentPeakGiB float64)
}

// ApplyTo copies s onto spec. nil-safe: a nil receiver is a no-op, so callers
// thread whatever the pipeline composed without a guard.
func (s *Sampling) ApplyTo(spec *Spec) {
	if s == nil {
		return
	}
	spec.Footprint = s.Footprint
	spec.SampleFunc = s.SampleFunc
	spec.OnFootprint = s.OnFootprint
	spec.HostSampleFunc = s.HostSampleFunc
	spec.OnHostFootprint = s.OnHostFootprint
}

// footprintSampleInterval is how often SampleFunc is polled during a sampled
// render. A var (not const) so tests can shorten it; 500ms is cheap for the PDH
// path (no process spawn) and plenty for multi-second GPU renders.
var footprintSampleInterval = 500 * time.Millisecond

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
	cmd.Dir = spec.Dir
	if spec.EnvExact {
		// A non-nil empty slice, never nil: os/exec treats a nil Env as "inherit".
		cmd.Env = append([]string{}, spec.Env...)
	} else {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	// Last, so nothing earlier in the env can shadow the instance this Spec names.
	cmd.Env = append(cmd.Env, instanceEnv(spec)...)
	// On timeout/cancel kill the WHOLE process tree (invariant 3): a bare kill on
	// Windows orphans the ComfyUI python grandchild and bypasses node's finally.
	cmd.Cancel = func() error { return killTree(cmd.Process) }
	cmd.WaitDelay = 10 * time.Second
	if spec.OwnProcessGroup {
		setProcessGroup(cmd)
	}
	// Belt-and-suspenders VRAM free (invariant 3, layer 2). Skipped for runners that
	// never launch ComfyUI (TTS) — there a /free is pointless, though harmless.
	if !spec.SkipFreeComfy {
		defer freeComfyVRAM(comfyAPIOf(spec))
	}

	// tw bounds BOTH capture sites below to tailWriterCap (Fix: SP3 follow-up
	// review) — previously cmd.CombinedOutput() (legacy path) and runSampled's
	// bytes.Buffer each accumulated the child's ENTIRE stdout+stderr in RAM;
	// tail(o, 400) truncated only at error-FORMAT time, never at capture time,
	// so a runaway child could balloon memory over the whole timeout_sec
	// window before that truncation ever ran.
	tw := newTailWriter(tailWriterCap)
	var (
		err               error
		peak              float64
		hostPriv, hostRes float64
	)
	if spec.Footprint == nil {
		// Legacy path — byte-identical to the pre-tailWriter behavior for any
		// output under the cap (which is every real case tail(o,400) cares about).
		err = runCombined(cmd, tw)
	} else {
		peak, hostPriv, hostRes, err = runSampledHost(cmd, tw, spec.SampleFunc, spec.HostSampleFunc)
	}
	if err != nil {
		// cctx.Err() is OUR OWN derived context, so this is authoritative regardless
		// of what the OS reports as the child's exit status. Without this check the
		// classification depended on the killed process's exit text containing
		// "timeout"/"deadline"/"killed"/"signal:" (ClassifyErr's patterns) — but
		// killTree's Windows path (taskkill /T /F) terminates via TerminateProcess,
		// which reports exit code 1, so cmd.Wait() returned a plain "exit status 1"
		// with none of those words in it. A cold ACE-Step music retry killed at
		// audiogen_timeout_sec surfaced that way (<node-e> remediation, 2026-09-23):
		// a real timeout, reported as a generic failure indistinguishable from any
		// other crash. Folding "deadline exceeded" into the error text here makes
		// EVERY gpugen caller's ClassifyErr(gerr) == "timeout" reliable, on every OS
		// and whatever exit code the kill happens to produce — not just audio.
		//
		// The class is TYPED (a *RunError), not left to ClassifyErr's substring match: the message
		// embeds the child's last 400 bytes, and an engine log that ends "...a living room, boom"
		// would otherwise read as "oom" through the "oom" in "room" / "boom".
		if cctx.Err() == context.DeadlineExceeded {
			return "", &RunError{Class: "timeout", err: fmt.Errorf("gpugen: %s timeout after %s (deadline exceeded, process tree killed): %w (%s)",
				baseName(spec.Script), spec.Timeout, err, tailDetail(tw))}
		}
		// A client cancel (the caller's own context) is the same event as a timeout for the class:
		// on an OwnProcessGroup lane the runner answers the SIGTERM with exit 143 ("exit status
		// 143"), which carries neither "killed" nor "signal:" and would read as "other", while
		// every other lane's cancel ends "signal: killed" and reads as a timeout. Typed for the same
		// reason as the deadline above ("...canceled...(the zoom lens)" must not read as oom).
		if cctx.Err() == context.Canceled {
			return "", &RunError{Class: "timeout", err: fmt.Errorf("gpugen: %s canceled (context canceled, process tree killed): %w (%s)",
				baseName(spec.Script), err, tailDetail(tw))}
		}
		// The iGPU runners end a typed failure with one short "IGPU_CLASS=<class>" line. The class
		// is read from the whole retained output, never from the 400-byte display tail: a real
		// GPU_RESET line is longer than that tail, so the token would be cut off with it.
		if cls := runnerClass(tw.Contents()); cls != "" {
			return "", &RunError{Class: cls, err: fmt.Errorf("gpugen: %s failed [%s%s]: %w (%s)",
				baseName(spec.Script), classTag, cls, err, typedDetail(tw))}
		}
		return "", fmt.Errorf("gpugen: %s failed: %w (%s)", baseName(spec.Script), err, tailDetail(tw))
	}
	if fi, statErr := os.Stat(spec.Out); statErr != nil || fi.Size() == 0 {
		return "", fmt.Errorf("gpugen: no output at %q (%s)", spec.Out, tailDetail(tw))
	}
	// SUCCESS only: a failed/phantom run's peak may be partial, so it never records.
	if spec.Footprint != nil && spec.OnFootprint != nil && peak > 0 {
		spec.OnFootprint(peak)
	}
	if spec.Footprint != nil && spec.OnHostFootprint != nil && hostPriv > 0 {
		spec.OnHostFootprint(hostPriv, hostRes)
	}
	return spec.Out, nil
}

// tailDetail formats a tailWriter's captured output for embedding in a gpugen
// error message: the last 400 bytes of whatever it retained (matching the
// pre-existing tail() truncation every error message already used — under
// the cap this is BYTE-IDENTICAL to the old cmd.CombinedOutput()/bytes.Buffer
// behavior), prefixed with a total-bytes marker on the rare path where the
// writer itself had to drop earlier data (total written > its capacity).
func tailDetail(tw *tailWriter) string {
	d := tail(tw.Contents(), 400)
	if tw.Truncated() {
		return fmt.Sprintf("truncated: total %d bytes; tail: %s", tw.Total(), d)
	}
	return d
}

// classTag is the label a typed failure carries in its message ("[class=gpu_reset]") for a reader.
const classTag = "class="

// RunError is a failure whose class was decided structurally, not by the words of its message: the
// class the runner itself reported (the IGPU_CLASS= line), or "timeout" for the deadline and the
// cancel gpugen itself observed. ClassifyErr returns Class for it, whatever words the rest of the
// message (which embeds the child's last output) happens to contain.
type RunError struct {
	Class string
	err   error
}

func (e *RunError) Error() string { return e.err.Error() }
func (e *RunError) Unwrap() error { return e.err }

// runnerClassLine matches the runner's class line: the whole line, at its start.
var runnerClassLine = regexp.MustCompile(`(?m)^IGPU_CLASS=([a-z_]+)[ \t\r]*$`)

// runnerClasses are the classes a runner may report. A line naming anything else is ignored, so
// engine output that happens to look like the marker cannot invent a class.
var runnerClasses = map[string]bool{
	"cpu_placement": true, "cpu_backend_refused": true, "gpu_reset": true, "token_cap_exceeded": true,
	"extra_args_refused": true, "illegal_instruction": true, "black_clip": true, "frozen_clip": true,
	"depth_frames_invalid": true, "model_incompatible": true, "binary_not_absolute": true,
	"out_dir_unwritable": true, "dead_air": true, "ffmpeg_unavailable": true, "unmeasurable": true,
	"engine_crashed": true, "oom": true, "device_invalid": true, "timeout": true,
}

// runnerClass is the class of the LAST valid IGPU_CLASS= line in the captured output, "" when
// there is none. The last one, because the runner prints its own after everything the engine said.
func runnerClass(out []byte) string {
	all := runnerClassLine.FindAllSubmatch(out, -1)
	for i := len(all) - 1; i >= 0; i-- {
		if c := string(all[i][1]); runnerClasses[c] {
			return c
		}
	}
	return ""
}

// failedLine matches a runner's final human line: "SDCPP VIDEO FAILED: GPU_RESET: ...".
var failedLine = regexp.MustCompile(`(?m)^[A-Z][A-Z ]* FAILED: .*$`)

// typedDetail is the detail of a typed runner failure: the runner's own "<NAME> FAILED: ..."
// line from its START (so the TOKEN: label survives, unlike in a tail cut), at most 600 bytes,
// else the display tail. A marker inside it is defanged so it cannot be read back as one.
func typedDetail(tw *tailWriter) string {
	out := tw.Contents()
	d := ""
	if loc := failedLine.FindAllIndex(out, -1); len(loc) > 0 {
		last := loc[len(loc)-1]
		d = strings.TrimRight(string(out[last[0]:last[1]]), "\r")
		if len(d) > 600 {
			d = d[:600] + "..."
		}
	}
	if d == "" {
		d = tailDetail(tw)
	}
	return strings.ReplaceAll(d, "IGPU_CLASS=", "IGPU_CLASS~")
}

// runCombined runs cmd with both stdout and stderr merged into w — the
// bounded-capture replacement for cmd.CombinedOutput() (which used its own
// unbounded internal buffer). This is the "legacy/CombinedOutput-style" call
// site the SP3 follow-up review named explicitly.
func runCombined(cmd *exec.Cmd, w io.Writer) error {
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

// runSampled runs cmd like runCombined (one merged stdout+stderr writer) but via
// Start/Wait so a sampler can poll sample(childPid) while the child is alive: one
// immediate sample (a fast child still gets observed), then every
// footprintSampleInterval. w receives the child's merged output (the bounded-
// capture replacement for this function's own former bytes.Buffer — the SP3
// follow-up review's second named call site). Returns the peak observation and
// the child's error. sample==nil degrades to a plain Start/Wait (peak 0).
func runSampled(cmd *exec.Cmd, w io.Writer, sample func(childPid int) (float64, error)) (float64, error) {
	peak, _, _, err := runSampledHost(cmd, w, sample, nil)
	return peak, err
}

// runSampledHost is runSampled that also samples the process tree's HOST memory on the same tick: it returns the
// peak VRAM sample and the peak private and peak resident memory the host sampler saw (each 0 when its sampler is
// nil or never answered). The two peaks are independent maxima, not a pair read at one instant.
func runSampledHost(cmd *exec.Cmd, w io.Writer, sample func(childPid int) (float64, error),
	hostSample func(childPid int) (privateGiB, residentGiB float64, err error)) (peak, hostPriv, hostResident float64, err error) {
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		return 0, 0, 0, err
	}
	var (
		done = make(chan struct{})
		wg   sync.WaitGroup
	)
	if sample != nil || hostSample != nil {
		pid := cmd.Process.Pid
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(footprintSampleInterval)
			defer t.Stop()
			for {
				if sample != nil {
					if g, serr := sample(pid); serr == nil && g > peak {
						peak = g
					}
				}
				if hostSample != nil {
					if p, r, serr := hostSample(pid); serr == nil {
						if p > hostPriv {
							hostPriv = p
						}
						if r > hostResident {
							hostResident = r
						}
					}
				}
				select {
				case <-done:
					return
				case <-t.C:
				}
			}
		}()
	}
	err = cmd.Wait()
	close(done)
	wg.Wait() // happens-before: the peaks are safely visible after the sampler exits
	return peak, hostPriv, hostResident, err
}

// KillTree is killTree for the lanes that spawn their own interactive child (the
// browse lane keeps a stdio conversation open, so it cannot go through Generate)
// and still need the same whole-tree kill on timeout/cancel.
func KillTree(p *os.Process) error { return killTree(p) }

// instanceEnv is the env a Spec's per-card ComfyUI instance adds to the child: its endpoint
// and its card, and a blank legacy index when a card is named. A Spec that names neither
// (every caller today) adds nothing.
func instanceEnv(spec Spec) []string {
	var env []string
	if spec.ComfyAPI != "" {
		env = append(env, "COMFY_API="+spec.ComfyAPI)
	}
	if spec.CardUUID != "" {
		env = append(env, "COMFY_CARD_UUID="+spec.CardUUID, "COMFY_CUDA_DEVICE=")
	}
	return env
}

// comfyAPIOf is the endpoint the post-run /free goes to: the one the Spec names, else the last
// non-empty COMFY_API in the env the runner is given (the routes that hand a per-card instance
// to their runner through Env, which is most of them, never set Spec.ComfyAPI; the runner talks
// to that instance, so that is the one to free), else the process's own.
func comfyAPIOf(spec Spec) string {
	if spec.ComfyAPI != "" {
		return spec.ComfyAPI
	}
	api := ""
	for _, kv := range spec.Env {
		if v, ok := strings.CutPrefix(kv, "COMFY_API="); ok && v != "" {
			api = v
		}
	}
	return comfyAPI(api)
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

// freeComfyTimeout bounds the post-run /free. It was one second, which a ComfyUI in the middle of a
// step does not always answer in; the request is only a flag the instance's worker acts on, so five
// seconds costs nothing when the instance is idle or gone (a refused connection returns at once) and
// is the difference when it is busy.
const freeComfyTimeout = 5 * time.Second

// freeComfyVRAM asks ComfyUI to unload models + free VRAM (zero-always-warm). Best-effort: it never
// fails the run (ComfyUI may already be gone, or never ours to free), but a request that reached an
// instance and did not succeed is said once on stderr, because an instance whose models were not freed
// is one the next family is loaded beside (render/comfy-family.mjs). One that is not listening is not
// news.
func freeComfyVRAM(api string) {
	cl := &http.Client{Timeout: freeComfyTimeout}
	req, err := http.NewRequest(http.MethodPost, api+"/free", strings.NewReader(`{"unload_models":true,"free_memory":true}`))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, derr := cl.Do(req)
	if derr != nil {
		if s := strings.ToLower(derr.Error()); !strings.Contains(s, "refused") && !strings.Contains(s, "no such host") {
			fmt.Fprintf(os.Stderr, "gpugen: POST %s/free did not succeed (%v); the instance may still hold its models\n", api, derr)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		fmt.Fprintf(os.Stderr, "gpugen: POST %s/free answered %s; the instance may still hold its models\n", api, resp.Status)
	}
}

// ClassifyErr maps a gen failure to a coarse class (oom|timeout|conn_refused|disk_full|other)
// for the ledger's ErrClass. Mirrors pipeline.classifyErr; nil => "".
func ClassifyErr(err error) string {
	if err == nil {
		return ""
	}
	// A class the runner reported itself (the IGPU_CLASS= line Generate read from the full output)
	// is exact and wins over any wording in the message.
	var re *RunError
	if errors.As(err, &re) && re.Class != "" {
		return re.Class
	}
	// A full volume (0.178.0): by the typed errno or the errno TOKEN in the text, never by its prose
	// (IsDiskFull). Before this class a batch that stopped on a full drive was recorded as "other".
	if IsDiskFull(err) {
		return "disk_full"
	}
	s := strings.ToLower(err.Error())
	switch {
	// The iGPU media runners' no-CPU guards (CT-49, render/igpu-engine.mjs): the engine's
	// log placed a model on the CPU, or the backend itself was a CPU one. Checked first so
	// the "killed" in a placement-abort message never reads as a timeout.
	case strings.Contains(s, "cpu_placement"):
		return "cpu_placement"
	case strings.Contains(s, "cpu_backend_refused"):
		return "cpu_backend_refused"
	// The GPU reset during the run (the amdgpu 2 s lockup timeout): never retried. Its message
	// names that timeout, so it must be classified before the "timeout" case below.
	case strings.Contains(s, "gpu_reset"):
		return "gpu_reset"
	case strings.Contains(s, "token_cap_exceeded"):
		return "token_cap_exceeded"
	case strings.Contains(s, "extra_args_refused"):
		return "extra_args_refused"
	case strings.Contains(s, "illegal_instruction"):
		return "illegal_instruction"
	// The iGPU runners' output gates and input refusals (render/igpu-qa.mjs, render/igpu-engine.mjs):
	// the engine exited 0 but the clip is black / frozen, the depth frames are not the RGB sd-cli
	// needs, sd-cli refused the model file, or the runner refused its own inputs. Never retried:
	// the same request fails the same way, and none of these is a timeout or an oom whatever words
	// the path in the message happens to contain.
	case strings.Contains(s, "black_clip"):
		return "black_clip"
	case strings.Contains(s, "frozen_clip"):
		return "frozen_clip"
	case strings.Contains(s, "depth_frames_invalid"):
		return "depth_frames_invalid"
	case strings.Contains(s, "model_incompatible"):
		return "model_incompatible"
	case strings.Contains(s, "binary_not_absolute"):
		return "binary_not_absolute"
	case strings.Contains(s, "out_dir_unwritable"):
		return "out_dir_unwritable"
	case strings.Contains(s, "device_invalid"):
		return "device_invalid"
	// The audio and video QA gates and the engine's own death: typed, so a path or an engine log
	// line in the message ("room", "timeout", "killed") cannot claim them for a looser class below.
	case strings.Contains(s, "dead_air"): // render/audio-qa.mjs's QA gate, F-35 follow-up 2026-09-23
		return "dead_air"
	case strings.Contains(s, "unmeasurable"): // the output could not be measured, so it was not checked
		return "unmeasurable"
	case strings.Contains(s, "ffmpeg_unavailable"): // render/comfy-music.mjs main(), F-38 fix 2026-09-24
		return "ffmpeg_unavailable"
	case strings.Contains(s, "engine_crashed"), crashedBySignal.MatchString(s):
		return "engine_crashed"
	// ggml_vulkan's allocation failure has neither "out of memory" nor "oom" in its text
	// ("Device memory allocation of size N failed ... vk::Device::allocateMemory: ErrorOutOfDeviceMemory");
	// ggml's own says "insufficient memory (attempted to allocate N MB)" and sd.cpp "alloc compute buffer failed".
	case strings.Contains(s, "out of memory") || strings.Contains(s, "out_of_memory") || strings.Contains(s, "cudamalloc") || strings.Contains(s, "oom") ||
		strings.Contains(s, "erroroutofdevicememory") || strings.Contains(s, "erroroutofhostmemory") || strings.Contains(s, "device memory allocation of size") ||
		strings.Contains(s, "insufficient memory") || strings.Contains(s, "alloc compute buffer failed"):
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

// crashedBySignal matches the wording of an engine that died of a crash signal ("sd-cli was
// killed by signal SIGSEGV"): the word "killed" in it is not a timeout. SIGKILL is not a crash
// (the OOM killer or a timeout) and SIGTERM / SIGINT / SIGHUP are a stop, so only the crash
// signals are listed.
var crashedBySignal = regexp.MustCompile(`killed by signal sig(?:segv|abrt|bus|fpe|trap|sys)`)

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
