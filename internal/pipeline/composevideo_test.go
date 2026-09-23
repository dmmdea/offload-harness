package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// composeProbe is what the stub compose runner reports about how it was spawned.
type composeProbe struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
	Cwd  string            `json:"cwd"`
	Vars map[string]any    `json:"vars"`
	HTML string            `json:"html"`
}

// writeComposeStub writes a node stand-in for render/compose-hyperframes.mjs. It records
// argv/env/cwd to probe.json BESIDE ITSELF (its environment is an allowlist, so a probe
// path cannot travel by env), then behaves per mode:
//
//	ok        write the --out file and a success --result
//	fail      write a typed failure --result and exit 1
//	tailonly  print a COMPOSE-FAIL line and exit 1 without any --result
//	noout     report success but never write the output file
func writeComposeStub(t *testing.T, dir, mode string) string {
	t.Helper()
	stub := filepath.Join(dir, "compose-stub.mjs")
	src := `import { writeFileSync, mkdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
const here = dirname(fileURLToPath(import.meta.url));
const argv = process.argv.slice(2);
const flag = (n) => { const i = argv.indexOf(n); return i >= 0 ? argv[i + 1] : undefined; };
const vf = flag("--variables-file"), hf = flag("--html-file");
writeFileSync(join(here, "probe.json"), JSON.stringify({ argv, env: process.env, cwd: process.cwd(),
  vars: vf ? JSON.parse(readFileSync(vf, "utf8")) : null, html: hf ? readFileSync(hf, "utf8") : "" }));
const mode = "` + mode + `";
const out = flag("--out"), result = flag("--result");
if (mode === "fail") { writeFileSync(result, JSON.stringify({ ok: false, class: "LINT_ERRORS", detail: "2 lint error(s): missing_duration" })); console.log("COMPOSE-FAIL: LINT_ERRORS: 2 lint error(s)"); process.exit(1); }
if (mode === "tailonly") { console.log("COMPOSE-FAIL: BROWSER_MISSING: pinned chrome-headless-shell not found"); process.exit(1); }
if (mode === "ok") { mkdirSync(dirname(out), { recursive: true }); writeFileSync(out, "video"); }
writeFileSync(result, JSON.stringify({ ok: true, engine: "hyperframes", version: "0.8.61", format: flag("--format"),
  quality: flag("--quality"), workers: flag("--workers"), template: flag("--template") || "", video_path: out,
  duration_sec: 5, fps: 30, frames: 150, width: 1920, height: 1080, has_alpha: false, has_audio: false, codec: "h264",
  pix_fmt: "yuv420p", render_ms: 21245, lint: { errors: 0, warnings: 1 }, check: { ok: true, findings: [] }, snapshots: [] }));
`
	if err := os.WriteFile(stub, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return stub
}

func composeCfg(t *testing.T, dir, stub string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.MediaDir = filepath.Join(dir, "media")
	cfg.ComposeScript = stub
	cfg.HyperframesDir = filepath.Join(dir, "hf")
	cfg.HyperframesBrowserPath = filepath.Join(dir, "chrome-headless-shell.exe")
	cfg.FFmpegPath = "ffmpeg"
	return cfg
}

func readComposeProbe(t *testing.T, dir string) composeProbe {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "probe.json"))
	if err != nil {
		t.Fatalf("the compose runner never ran: %v", err)
	}
	var p composeProbe
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("probe unreadable: %v", err)
	}
	return p
}

func argAfter(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// TestComposeVideoSpawnsTheRunnerWithAScrubbedEnvAndNoLease is the lane's contract end to
// end through Pipeline.Run: the runner gets the configured bindings as argv, an ALLOWLISTED
// environment (no cloud key, no NODE_OPTIONS and — the CPU-class rule — no GPU_LEASE_*, even
// when this process runs under an ambient lease), and the payload is the runner's measurement.
func TestComposeVideoSpawnsTheRunnerWithAScrubbedEnvAndNoLease(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	for k, v := range map[string]string{
		"OPENROUTER_API_KEY": "sk-or-test", "GEMINI_API_KEY": "g", "NVIDIA_API_KEY": "nv", "HEYGEN_API_KEY": "hg",
		"NODE_OPTIONS": "--no-deprecation", "GPU_LEASE_DIR": filepath.Join(dir, "lease"), "GPU_LEASE_EPOCH": "7",
		"GPU_LEASE_CLASS": "media", "COMPOSE_PROBE_UNRELATED": "x",
	} {
		t.Setenv(k, v)
	}
	cfg := composeCfg(t, dir, writeComposeStub(t, dir, "ok"))
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{Task: core.TaskComposeVideo, Door: "offload_compose_video", Params: map[string]any{
		"template": "title-card", "variables": map[string]any{"title": "Q3", "duration": 4.0},
		"snapshots": []any{1.0, 2.5},
	}})
	if !res.OK {
		t.Fatalf("compose_video deferred: %s", res.Reason)
	}
	pr := readComposeProbe(t, dir)
	for k := range pr.Env {
		up := strings.ToUpper(k)
		if strings.HasPrefix(up, "GPU_LEASE_") || strings.HasSuffix(up, "_API_KEY") || up == "NODE_OPTIONS" || up == "COMPOSE_PROBE_UNRELATED" {
			t.Errorf("%s reached the compose runner", k)
		}
	}
	if pr.Env["PATH"] == "" && pr.Env["Path"] == "" {
		t.Error("the runner lost PATH")
	}
	if pr.Argv[0] != "render" {
		t.Fatalf("op = %q, want render", pr.Argv[0])
	}
	for flag, want := range map[string]string{
		"--hyperframes-dir": cfg.HyperframesDir, "--browser": cfg.HyperframesBrowserPath, "--ffmpeg": "ffmpeg",
		"--cache-dir": filepath.Join(cfg.MediaDir, ".compose-cache"), "--format": "mp4", "--quality": "high",
		"--workers": "auto", "--template": "title-card", "--snapshots": "1,2.5", "--timeout-sec": "1800",
	} {
		if got, _ := argAfter(pr.Argv, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	out, _ := argAfter(pr.Argv, "--out")
	if filepath.Dir(out) != cfg.MediaDir || !strings.HasPrefix(filepath.Base(out), "compose-") || filepath.Ext(out) != ".mp4" {
		t.Errorf("default out = %q, want <media_dir>/compose-<hash8>.mp4", out)
	}
	if pr.Vars["title"] != "Q3" || pr.Vars["duration"] != 4.0 {
		t.Errorf("variables not staged for the runner: %v", pr.Vars)
	}
	var payload map[string]any
	if err := json.Unmarshal(res.Data, &payload); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"video_path": out, "codec": "h264", "frames": 150.0, "width": 1920.0, "render_ms": 21245.0, "has_alpha": false} {
		if payload[k] != want {
			t.Errorf("payload %s = %v, want %v", k, payload[k], want)
		}
	}
	if _, ok := payload["ok"]; ok {
		t.Error("the runner's ok flag must not leak into the payload")
	}
	if res.Meta.Model != "hyperframes" {
		t.Errorf("meta.Model = %q, want hyperframes", res.Meta.Model)
	}
	if entries, _ := os.ReadDir(filepath.Join(cfg.MediaDir, ".compose-cache", "req")); len(entries) != 0 {
		t.Errorf("the request staging dir was not cleaned up: %d entries", len(entries))
	}
}

// TestComposeRunnerEnvFoldsWindowsCase: on Windows "Path"/"SystemRoot" are the real
// spellings; the filter must keep them (once) and still drop everything else.
func TestComposeRunnerEnvFoldsWindowsCase(t *testing.T) {
	env := composeRunnerEnv([]string{"Path=C:\\w", "PATH=C:\\dup", "SystemRoot=C:\\Windows", "OpenRouter_API_Key=x", "GPU_LEASE_DIR=d", "TEMP=C:\\t"}, "windows")
	if strings.Join(env, "|") != "Path=C:\\w|SystemRoot=C:\\Windows|TEMP=C:\\t" {
		t.Fatalf("windows env = %v", env)
	}
	if got := composeRunnerEnv([]string{"Path=/x", "PATH=/usr/bin", "HOME=/h"}, "linux"); strings.Join(got, "|") != "PATH=/usr/bin|HOME=/h" {
		t.Fatalf("posix names are case-sensitive: %v", got)
	}
}

func TestComposeVideoDefersWhenUnbound(t *testing.T) {
	for name, mut := range map[string]func(*config.Config){
		"no script":  func(c *config.Config) { c.ComposeScript = "" },
		"no install": func(c *config.Config) { c.HyperframesDir = "" },
		"no browser": func(c *config.Config) { c.HyperframesBrowserPath = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := composeCfg(t, t.TempDir(), "render/compose-hyperframes.mjs")
			mut(&cfg)
			res := (&Pipeline{cfg: cfg}).Run(context.Background(), core.Request{Task: core.TaskComposeVideo, Params: map[string]any{"template": "title-card"}})
			if res.OK || !res.Deferred || !strings.Contains(res.Reason, "no composition route configured") {
				t.Fatalf("want a not-configured defer, got ok=%v deferred=%v %q", res.OK, res.Deferred, res.Reason)
			}
		})
	}
}

// TestComposeVideoBadInputNeverSpawns: every malformed request is a BAD_INPUT defer
// decided in Go — the stub records a probe if it is ever spawned.
func TestComposeVideoBadInputNeverSpawns(t *testing.T) {
	dir := t.TempDir()
	cfg := composeCfg(t, dir, writeComposeStub(t, dir, "ok"))
	cases := map[string]map[string]any{
		"no input":           {},
		"two inputs":         {"template": "title-card", "html": "<html></html>"},
		"bad template name":  {"template": "../etc"},
		"missing project":    {"project_dir": filepath.Join(dir, "nope")},
		"bad format":         {"template": "title-card", "format": "hls"},
		"bad quality":        {"template": "title-card", "quality": "looks"},
		"fractional fps":     {"template": "title-card", "fps": 29.97},
		"fps out of range":   {"template": "title-card", "fps": 500.0},
		"too many workers":   {"template": "title-card", "workers": 99.0},
		"bad resolution":     {"template": "title-card", "resolution": "8k"},
		"escaping comp":      {"template": "title-card", "composition": "../x.html"},
		"negative snapshot":  {"template": "title-card", "snapshots": []any{-1.0}},
		"variables not obj":  {"template": "title-card", "variables": []any{"x"}},
		"too many snapshots": {"template": "title-card", "snapshots": []any{1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 9.0, 10.0, 11.0, 12.0, 13.0, 14.0, 15.0, 16.0, 17.0}},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			res := (&Pipeline{cfg: cfg}).Run(context.Background(), core.Request{Task: core.TaskComposeVideo, Params: params})
			if res.OK || !res.Deferred || !strings.Contains(res.Reason, "BAD_INPUT") {
				t.Fatalf("want a BAD_INPUT defer, got ok=%v %q", res.OK, res.Reason)
			}
			if res.Meta.ErrClass != "bad_input" {
				t.Errorf("ErrClass = %q, want bad_input", res.Meta.ErrClass)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "probe.json")); err == nil {
		t.Fatal("a bad request spawned the runner")
	}
	cfg.ComposeWorkers = "lots"
	res := (&Pipeline{cfg: cfg}).Run(context.Background(), core.Request{Task: core.TaskComposeVideo, Params: map[string]any{"template": "title-card"}})
	if !strings.Contains(res.Reason, "BAD_INPUT") {
		t.Fatalf("an invalid compose_workers default must defer BAD_INPUT, got %q", res.Reason)
	}
}

// TestComposeVideoRunnerFailuresAreTypedDefers: the runner's class survives to the caller,
// whether it arrives in the --result file or only on the COMPOSE-FAIL line.
func TestComposeVideoRunnerFailuresAreTypedDefers(t *testing.T) {
	requireNodePipeline(t)
	for mode, want := range map[string]string{
		"fail":     "compose_video: LINT_ERRORS: 2 lint error(s): missing_duration",
		"tailonly": "compose_video: BROWSER_MISSING: pinned chrome-headless-shell not found",
		"noout":    "compose_video: RENDER_FAILED: the runner reported success but its output",
	} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			cfg := composeCfg(t, dir, writeComposeStub(t, dir, mode))
			res := (&Pipeline{cfg: cfg}).Run(context.Background(), core.Request{Task: core.TaskComposeVideo, Params: map[string]any{"html": "<html><body></body></html>"}})
			if res.OK || !res.Deferred || !strings.HasPrefix(res.Reason, want) {
				t.Fatalf("reason = %q (deferred=%v), want prefix %q", res.Reason, res.Deferred, want)
			}
		})
	}
}

// TestComposeVideoInlineHTMLIsStagedForTheRunner: html travels as a file (a page can exceed
// any argv limit), never on the command line.
func TestComposeVideoInlineHTMLIsStagedForTheRunner(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := composeCfg(t, dir, writeComposeStub(t, dir, "ok"))
	page := `<html><body><div data-composition-id="x" data-duration="2"></div></body></html>`
	res := (&Pipeline{cfg: cfg}).Run(context.Background(), core.Request{Task: core.TaskComposeVideo, Params: map[string]any{
		"html": page, "format": "webm", "fps": 24.0, "quality": "draft", "workers": 1.0, "strict": false, "resolution": "landscape",
	}})
	if !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	pr := readComposeProbe(t, dir)
	if pr.HTML != page {
		t.Fatalf("staged html = %q", pr.HTML)
	}
	for _, a := range pr.Argv {
		if strings.Contains(a, "data-composition-id") {
			t.Fatal("the page itself must never ride on argv")
		}
	}
	for flag, want := range map[string]string{"--format": "webm", "--fps": "24", "--quality": "draft", "--workers": "1", "--resolution": "landscape"} {
		if got, _ := argAfter(pr.Argv, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	if !strings.Contains(strings.Join(pr.Argv, " "), "--no-strict") {
		t.Error("strict=false must reach the runner as --no-strict")
	}
}

// TestComposeVideoBusySlotDefers: a second composition in the same process waits its
// bounded window on the compose slot, then defers compose_busy — it never queues behind
// (or takes) the GPU media slot.
func TestComposeVideoBusySlotDefers(t *testing.T) {
	dir := t.TempDir()
	cfg := composeCfg(t, dir, writeComposeStub(t, dir, "ok"))
	cfg.GPUWaitMs = 1
	if !takeComposeSlot(time.Second) {
		t.Fatal("setup: the compose slot is already held")
	}
	defer releaseComposeSlot()
	res := (&Pipeline{cfg: cfg}).Run(context.Background(), core.Request{Task: core.TaskComposeVideo, Params: map[string]any{"template": "title-card"}})
	if res.OK || res.Meta.ErrClass != "compose_busy" {
		t.Fatalf("want compose_busy, got ok=%v class=%q %q", res.OK, res.Meta.ErrClass, res.Reason)
	}
	if !takeMediaSlot(0) {
		t.Fatal("compose_video must not hold the GPU media slot")
	}
	releaseMediaSlot()
}

// TestComposeLaneIsNotAGPURunner: the lease-coverage completeness scan keys on
// withGpuSlot; the compose runner must never call it (CPU-class, ADR 0059).
func TestComposeLaneIsNotAGPURunner(t *testing.T) {
	root := repoRootForLeaseTest(t)
	b, err := os.ReadFile(filepath.Join(root, "render", "compose-hyperframes.mjs"))
	if err != nil {
		t.Skipf("render/ not reachable: %v", err)
	}
	if callsWithGpuSlot(string(b)) || strings.Contains(string(b), "gpu-lock.mjs") {
		t.Fatal("render/compose-hyperframes.mjs takes a GPU slot; the compose lane is CPU-class and must not")
	}
}

// TestFleetComposeWritesOnlyUnderTheMediaDir is the fleet trust boundary end to end: a
// remote compose-video dispatch naming an EXISTING file outside media_dir (and a `..`
// escape) as its out goes through fleetnode.BuildRequest and Pipeline.Run, and the runner
// is told to write under media_dir — the named file is never touched.
func TestFleetComposeWritesOnlyUnderTheMediaDir(t *testing.T) {
	requireNodePipeline(t)
	for _, name := range []string{"existing file", "traversal"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := composeCfg(t, dir, writeComposeStub(t, dir, "ok"))
			victim := filepath.Join(dir, "victim.mp4")
			if err := os.WriteFile(victim, []byte("do not overwrite"), 0o644); err != nil {
				t.Fatal(err)
			}
			target := victim
			if name == "traversal" {
				target = filepath.Join(cfg.MediaDir, "..", "victim.mp4")
			}
			raw, _ := json.Marshal(map[string]any{"template": "title-card", "out": target})
			req, cleanup, err := fleetnode.BuildRequest(context.Background(), cfg, true, fleetnode.ComposeTask, raw)
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			defer cleanup()
			res := (&Pipeline{cfg: cfg}).Run(context.Background(), req)
			if !res.OK {
				t.Fatalf("deferred: %s", res.Reason)
			}
			out, _ := argAfter(readComposeProbe(t, dir).Argv, "--out")
			rel, err := filepath.Rel(cfg.MediaDir, out)
			if err != nil || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") || filepath.Dir(rel) != "." {
				t.Fatalf("the runner was told to write %q, not directly under media_dir %q", out, cfg.MediaDir)
			}
			if b, _ := os.ReadFile(victim); string(b) != "do not overwrite" {
				t.Fatalf("the caller-named file was overwritten: %q", b)
			}
		})
	}
}
