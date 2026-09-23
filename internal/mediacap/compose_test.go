package mediacap

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// composeBox lays out a fully bound composition route under exeDir: the runner and two
// templates beside it, the pinned CLI entry, the pinned browser and ffmpeg + ffprobe.
func composeBox(t *testing.T) (exeDir string, cfg func() configForCompose) {
	t.Helper()
	exeDir = t.TempDir()
	touch(t, exeDir, "render/compose-hyperframes.mjs")
	touch(t, exeDir, "render/compose-templates/title-card/index.html")
	touch(t, exeDir, "render/compose-templates/lower-third/index.html")
	touch(t, exeDir, "render/compose-templates/_shared/fonts/x.woff2")
	hf := filepath.Join(exeDir, "hf")
	touch(t, exeDir, "hf/node_modules/hyperframes/bin/hyperframes.mjs")
	browser := touch(t, exeDir, "hf/chrome/chrome-headless-shell.exe")
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	ffmpeg := touch(t, exeDir, "ff/ffmpeg"+ext)
	touch(t, exeDir, "ff/ffprobe"+ext)
	return exeDir, func() configForCompose {
		return configForCompose{script: "render/compose-hyperframes.mjs", dir: hf, browser: browser, ffmpeg: ffmpeg}
	}
}

type configForCompose struct{ script, dir, browser, ffmpeg string }

func composeRouteFor(t *testing.T, exeDir string, c configForCompose) Route {
	t.Helper()
	cfg := bare()
	cfg.ComposeScript, cfg.HyperframesDir, cfg.HyperframesBrowserPath, cfg.FFmpegPath = c.script, c.dir, c.browser, c.ffmpeg
	cfg.NodePath = "node"
	got := byName(routesIn(cfg, exeDir))
	r, ok := got["compose_video"]
	if !ok {
		t.Fatal("no compose_video route reported")
	}
	if c.script != "" {
		if _, ok := got["node"]; !ok {
			t.Error("a bound compose route runs a node script: the node prereq must be reported")
		}
	}
	return r
}

func TestComposeRouteConfiguredListsTemplates(t *testing.T) {
	exeDir, base := composeBox(t)
	r := composeRouteFor(t, exeDir, base())
	if r.State != Configured || r.Engine != "hyperframes" {
		t.Fatalf("compose_video = %s/%s (%s), want CONFIGURED/hyperframes", r.State, r.Engine, r.Detail)
	}
	if !strings.Contains(r.Detail, "templates=lower-third,title-card") {
		t.Errorf("detail must list the vetted templates (and never _shared): %s", r.Detail)
	}
	if !strings.Contains(r.Detail, "ffprobe=") {
		t.Errorf("detail must name the ffprobe the gate will use: %s", r.Detail)
	}
}

// TestComposeRouteBoundButMissing: every half-bound shape is the middle verdict, naming
// the key or file at fault — never "not configured", which reads as a legitimate state.
func TestComposeRouteBoundButMissing(t *testing.T) {
	exeDir, base := composeBox(t)
	for name, tc := range map[string]struct {
		mut  func(*configForCompose)
		want string
	}{
		"install unbound": {func(c *configForCompose) { c.dir = "" }, "hyperframes_dir is unset"},
		"browser unbound": {func(c *configForCompose) { c.browser = "" }, "hyperframes_browser_path is unset"},
		"install absent":  {func(c *configForCompose) { c.dir = filepath.Join(exeDir, "nope") }, "hyperframes.mjs"},
		"browser absent":  {func(c *configForCompose) { c.browser = filepath.Join(exeDir, "nope.exe") }, "hyperframes_browser_path"},
		"runner absent":   {func(c *configForCompose) { c.script = "render/missing.mjs" }, "compose_script"},
		"ffmpeg absent":   {func(c *configForCompose) { c.ffmpeg = filepath.Join(exeDir, "nope", "ffmpeg") }, "ffmpeg_path"},
	} {
		t.Run(name, func(t *testing.T) {
			c := base()
			tc.mut(&c)
			r := composeRouteFor(t, exeDir, c)
			if r.State != BoundButMissing || !strings.Contains(r.Detail, tc.want) {
				t.Fatalf("compose_video = %s (%s), want BOUND-BUT-MISSING naming %q", r.State, r.Detail, tc.want)
			}
		})
	}
}

// TestComposeRouteNeedsFfprobe: the gate measures every output with ffprobe, so an ffmpeg
// with no ffprobe beside it (and none on PATH) is a route that would defer every render.
func TestComposeRouteNeedsFfprobe(t *testing.T) {
	exeDir, base := composeBox(t)
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	lone := touch(t, exeDir, "lone/ffmpeg"+ext)
	t.Setenv("PATH", t.TempDir())
	c := base()
	c.ffmpeg = lone
	r := composeRouteFor(t, exeDir, c)
	if r.State != BoundButMissing || !strings.Contains(r.Detail, "ffprobe not found") {
		t.Fatalf("compose_video = %s (%s), want BOUND-BUT-MISSING naming ffprobe", r.State, r.Detail)
	}
	if r := composeRouteFor(t, exeDir, base()); r.State != Configured {
		t.Fatalf("control: ffprobe beside ffmpeg must be CONFIGURED even with an empty PATH, got %s (%s)", r.State, r.Detail)
	}
}

func TestComposeRouteNotConfiguredWithoutRunner(t *testing.T) {
	exeDir, base := composeBox(t)
	c := base()
	c.script = ""
	if r := composeRouteFor(t, exeDir, c); r.State != NotConfigured {
		t.Fatalf("compose_video = %s (%s), want NOT CONFIGURED", r.State, r.Detail)
	}
}
