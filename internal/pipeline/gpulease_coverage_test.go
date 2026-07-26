package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// leaseProbe is what the stub runner reports back about the environment it was spawned
// with. Empty fields mean the pipeline spawned a GPU job WITHOUT a lease.
type leaseProbe struct {
	Dir   string `json:"dir"`
	Epoch string `json:"epoch"`
	Class string `json:"class"`
}

// writeLeaseProbeStub writes a node stub that records the GPU_LEASE_* env it inherited
// to $LEASE_PROBE, then exits 0.
//
// It deliberately does NOT satisfy any runner's output contract. The assertion here is
// about the CHILD'S ENVIRONMENT, so whether the pipeline subsequently reports ok or a
// defer is beside the point — which lets ONE stub serve every runner shape (positional
// out path, `--result` envelope, batch results file). Per-task hand-written stubs are
// the same shape of work that let two call sites be missed in the first place.
func writeLeaseProbeStub(t *testing.T, dir string) string {
	t.Helper()
	stub := filepath.Join(dir, "leaseprobe.mjs")
	if err := os.WriteFile(stub, []byte(`import {writeFileSync} from "node:fs";
const probe = process.env.LEASE_PROBE;
if (probe) {
  writeFileSync(probe, JSON.stringify({
    dir: process.env.GPU_LEASE_DIR || "",
    epoch: process.env.GPU_LEASE_EPOCH || "",
    class: process.env.GPU_LEASE_CLASS || "",
  }));
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return stub
}

// gpuLeaseCase is one task that touches the GPU and therefore must run under a lease.
// runner names the render/*.mjs it routes to, which the completeness guard below cross-
// checks against the runners that actually call withGpuSlot.
type gpuLeaseCase struct {
	name   string
	runner string
	setup  func(t *testing.T, cfg *config.Config, stub, dir string)
	invoke func(t *testing.T, p *Pipeline, dir string)
}

func gpuLeaseCases() []gpuLeaseCase {
	genReq := func(task core.TaskType, params map[string]any) func(*testing.T, *Pipeline, string) {
		return func(t *testing.T, p *Pipeline, _ string) {
			t.Helper()
			p.Run(context.Background(), core.Request{Task: task, Input: "a calm ocean at dawn", Params: params})
		}
	}
	return []gpuLeaseCase{
		{
			name:   "generate_image (comfy)",
			runner: "comfy-generate.mjs",
			setup: func(_ *testing.T, cfg *config.Config, stub, _ string) {
				cfg.ImageGenScript = stub
			},
			invoke: genReq(core.TaskGenerateImage, nil),
		},
		{
			name:   "generate_image (sdcpp)",
			runner: "sdcpp-generate.mjs",
			setup: func(_ *testing.T, cfg *config.Config, stub, _ string) {
				cfg.ImageGenEngine = "sdcpp"
				cfg.SdcppScript = stub
				cfg.SdcppBin = "sd.exe"
				cfg.SdcppModel = "model.gguf"
			},
			invoke: genReq(core.TaskGenerateImage, nil),
		},
		{
			name:   "inpaint_image",
			runner: "comfy-inpaint.mjs",
			setup: func(t *testing.T, cfg *config.Config, stub, dir string) {
				t.Helper()
				cfg.InpaintScript = stub
				cfg.InpaintCkpt = "sdxl.safetensors"
				for _, n := range []string{"a.png", "m.png"} {
					if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			},
			invoke: func(t *testing.T, p *Pipeline, dir string) {
				t.Helper()
				p.Run(context.Background(), core.Request{Task: core.TaskInpaintImage, Input: "clean it",
					Params: map[string]any{"image": filepath.Join(dir, "a.png"), "mask": filepath.Join(dir, "m.png")}})
			},
		},
		{
			name:   "image batch",
			runner: "comfy-generate.mjs",
			setup: func(_ *testing.T, cfg *config.Config, stub, _ string) {
				cfg.ImageGenScript = stub
			},
			invoke: func(t *testing.T, p *Pipeline, _ string) {
				t.Helper()
				_, _ = p.RunImageBatch(context.Background(), []ImageBatchJob{{Prompt: "a red bike"}})
			},
		},
		{
			name:   "run_graph",
			runner: "comfy-run-graph.mjs",
			setup: func(t *testing.T, cfg *config.Config, stub, dir string) {
				t.Helper()
				cfg.RunGraphScript = stub
				if err := os.WriteFile(filepath.Join(dir, "g.json"), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			invoke: func(t *testing.T, p *Pipeline, dir string) {
				t.Helper()
				p.Run(context.Background(), core.Request{Task: core.TaskRunGraph,
					Params: map[string]any{"graph_path": filepath.Join(dir, "g.json"), "out_dir": dir}})
			},
		},
		{
			name:   "generate_video",
			runner: "comfy-video.mjs",
			setup: func(_ *testing.T, cfg *config.Config, stub, _ string) {
				cfg.VideoGenScript = stub
			},
			invoke: genReq(core.TaskGenerateVideo, nil),
		},
		{
			name:   "generate_audio (voice)",
			runner: "tts.mjs",
			setup: func(_ *testing.T, cfg *config.Config, stub, _ string) {
				cfg.VoiceGenScript = stub
			},
			invoke: genReq(core.TaskGenerateAudio, map[string]any{"kind": "voice"}),
		},
		{
			name:   "generate_audio (music)",
			runner: "comfy-music.mjs",
			setup: func(_ *testing.T, cfg *config.Config, stub, _ string) {
				cfg.MusicGenScript = stub
			},
			invoke: genReq(core.TaskGenerateAudio, map[string]any{"kind": "music"}),
		},
	}
}

// TestEveryGPUTaskRunsUnderALease is the regression test for the two call sites that
// were missed: video and audio built their child env from genEnv, which threads no
// GPU_LEASE_*, so their runners hit the "no lease ⇒ refuse" path and every video, TTS
// and music generation failed.
//
// It is a TABLE over GPU tasks on purpose. Five call sites were wired by hand and two
// were forgotten; hand-writing one test per task reproduces exactly that failure mode,
// whereas a table plus the completeness guard below makes an unleased task impossible
// to add quietly.
func TestEveryGPUTaskRunsUnderALease(t *testing.T) {
	requireNodePipeline(t)
	for _, tc := range gpuLeaseCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			probePath := filepath.Join(dir, "probe.json")
			t.Setenv("LEASE_PROBE", probePath)

			cfg := config.Default()
			cfg.MediaDir = dir
			stub := writeLeaseProbeStub(t, dir)
			tc.setup(t, &cfg, stub, dir)

			p := &Pipeline{cfg: cfg}
			tc.invoke(t, p, dir)

			raw, err := os.ReadFile(probePath)
			if err != nil {
				t.Fatalf("the runner never ran: %v. Either this task defers before spawning "+
					"(fix the case setup) or it could not take a lease", err)
			}
			var got leaseProbe
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("probe unreadable: %v (%s)", err, raw)
			}
			if got.Dir == "" || got.Epoch == "" || got.Class == "" {
				t.Fatalf("spawned WITHOUT a GPU lease: %+v. gpu-lock.mjs refuses a GPU job with "+
					"no lease, so this task is broken end to end", got)
			}
			if got.Class != "media" {
				t.Errorf("GPU_LEASE_CLASS = %q, want media (only a media holder may unload models)", got.Class)
			}
			if got.Epoch == "0" {
				t.Errorf("GPU_LEASE_EPOCH = 0, which is not a fencing token")
			}
		})
	}
}

// TestEveryGpuSlotRunnerIsCoveredByTheLeaseTable is the completeness half: the table
// above can only catch a missing lease for tasks it lists, so this asserts the table
// lists every render runner that takes a GPU slot. Adding a new withGpuSlot runner
// without a table entry fails HERE, at the point the omission is made, rather than in
// production the way video and audio did.
func TestEveryGpuSlotRunnerIsCoveredByTheLeaseTable(t *testing.T) {
	root := repoRootForLeaseTest(t)
	entries, err := os.ReadDir(filepath.Join(root, "render"))
	if err != nil {
		t.Skipf("render/ not reachable from the test binary: %v", err)
	}
	gpuRunners := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".mjs") || strings.HasSuffix(name, ".test.mjs") || name == "gpu-lock.mjs" {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(root, "render", name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		if callsWithGpuSlot(string(b)) {
			gpuRunners[name] = true
		}
	}
	if len(gpuRunners) == 0 {
		t.Fatal("found no withGpuSlot runners; the scan is broken, not the code")
	}
	covered := map[string]bool{}
	for _, tc := range gpuLeaseCases() {
		covered[tc.runner] = true
	}
	for name := range gpuRunners {
		if !covered[name] {
			t.Errorf("render/%s takes a GPU slot but no case in gpuLeaseCases() covers it; "+
				"a task routing to it would spawn unleased and fail at gpu-lock.mjs", name)
		}
	}
	for name := range covered {
		if !gpuRunners[name] {
			t.Errorf("gpuLeaseCases() names render/%s, which no longer calls withGpuSlot; "+
				"the table is describing a runner that no longer exists", name)
		}
	}
}

// TestAmbientLeaseIsInheritedNotReacquired covers the `gpu reserve --class media --
// local-offload …` flow: the reserve verb holds the lease and runs the harness as its
// CHILD. Acquiring again inside that child is a self-deadlock — it would queue behind
// its own parent for the whole wait window and then defer.
func TestAmbientLeaseIsInheritedNotReacquired(t *testing.T) {
	requireNodePipeline(t)
	m, err := gpulease.OpenAt("", "")
	if err != nil {
		t.Fatalf("open lease manager: %v", err)
	}
	held, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "reserve --class media", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}
	defer func() { _ = held.Release() }()

	dir := t.TempDir()
	probePath := filepath.Join(dir, "probe.json")
	t.Setenv("LEASE_PROBE", probePath)
	t.Setenv("GPU_LEASE_DIR", held.Dir())
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(held.Epoch(), 10))
	t.Setenv("GPU_LEASE_CLASS", string(held.Class()))

	cfg := config.Default()
	cfg.MediaDir = dir
	cfg.VideoGenScript = writeLeaseProbeStub(t, dir)
	p := &Pipeline{cfg: cfg}

	start := time.Now()
	p.Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "a calm ocean at dawn"})
	elapsed := time.Since(start)

	raw, rerr := os.ReadFile(probePath)
	if rerr != nil {
		t.Fatalf("the runner never ran under an inherited lease (%v) after %s — "+
			"this is the self-deadlock: the child queued behind its own parent", rerr, elapsed.Round(time.Millisecond))
	}
	var got leaseProbe
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("probe unreadable: %v (%s)", err, raw)
	}
	if got.Epoch != strconv.FormatUint(held.Epoch(), 10) {
		t.Errorf("child ran under epoch %s, want the inherited %d — it took a SECOND lease", got.Epoch, held.Epoch())
	}
	if wait := p.gpuWait(); elapsed >= wait {
		t.Errorf("took %s, i.e. the whole %s wait window: it queued instead of inheriting", elapsed, wait)
	}
}

// A GPU_LEASE_* that names a lease which is no longer current means the holder above us
// was fenced out. Rendering anyway is unarbitrated, so the job must refuse rather than
// quietly take a fresh lease and hide that the reservation was lost.
func TestStaleAmbientLeaseRefusesInsteadOfRendering(t *testing.T) {
	requireNodePipeline(t)
	m, err := gpulease.OpenAt("", "")
	if err != nil {
		t.Fatalf("open lease manager: %v", err)
	}
	held, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "reserve", TTL: time.Hour})
	if err != nil {
		t.Fatalf("setup acquire: %v", err)
	}
	defer func() { _ = held.Release() }()

	dir := t.TempDir()
	probePath := filepath.Join(dir, "probe.json")
	t.Setenv("LEASE_PROBE", probePath)
	t.Setenv("GPU_LEASE_DIR", held.Dir())
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(held.Epoch()+1, 10)) // fenced out
	t.Setenv("GPU_LEASE_CLASS", string(gpulease.ClassMedia))

	cfg := config.Default()
	cfg.MediaDir = dir
	cfg.VideoGenScript = writeLeaseProbeStub(t, dir)
	p := &Pipeline{cfg: cfg}

	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "a calm ocean at dawn"})
	if res.OK || !res.Deferred {
		t.Fatalf("a fenced-out inherited lease must defer, got ok=%v", res.OK)
	}
	if !strings.Contains(res.Reason, "no longer current") {
		t.Errorf("reason = %q, want it to name the fenced-out lease", res.Reason)
	}
	if _, err := os.Stat(probePath); err == nil {
		t.Error("the runner was spawned despite a fenced-out lease — that is an unarbitrated render")
	}
}

// callsWithGpuSlot reports whether a runner actually INVOKES withGpuSlot, ignoring line
// comments. Matching the bare identifier flagged comfy-lifecycle.mjs, which only names
// it in prose — and a guard that cries wolf gets suppressed, which is how it stops
// guarding anything.
func callsWithGpuSlot(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if strings.Contains(line, "withGpuSlot(") {
			return true
		}
	}
	return false
}

// repoRootForLeaseTest walks up from the test's working directory to the module root.
func repoRootForLeaseTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("module root not found from the test working directory")
	return ""
}
