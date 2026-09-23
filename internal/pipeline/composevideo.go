package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpugen"
)

// compose_video (ADR 0059): HyperFrames renders an HTML/CSS composition to video with
// headless Chrome in SOFTWARE GL + ffmpeg. The class decision is the whole design:
//
//   - CPU-class. No machine-wide media lease and no withGpuSlot: a media lease would make
//     load-triggering text admissions wait (ADR 0026) for work that never touches a card.
//     The runner forces software GL (--no-browser-gpu, PRODUCER_BROWSER_GPU_MODE=software)
//     and CPU encode, and refuses --gpu/--browser-gpu outright.
//   - Serialized in-process by composeSlot (capacity one) — NOT mediaSlot, which would park
//     a composition behind a 30-minute video render it does not contend with.
//   - One door: render/compose-hyperframes.mjs builds the allowlisted env, puts --json on
//     every call, refuses every subcommand outside lint/check/render/snapshot/browser/
//     --version and types every failure. This side adds its own allowlist to the RUNNER's
//     env (gpugen EnvExact), so no cloud key, lease token or NODE_OPTIONS from this process
//     reaches even the harness's own script.
//   - Every failure is a defer with the runner's typed class; never a cloud fallback.

// composeSlot is compose_video's in-process slot: one composition at a time per process
// (each render already fans out over up to 24 Chrome workers). A buffered channel for the
// same reasons mediaSlot is one: a bounded wait, and a waiter handed the slot on release.
var composeSlot = make(chan struct{}, 1)

func takeComposeSlot(wait time.Duration) bool {
	select {
	case composeSlot <- struct{}{}:
		return true
	default:
	}
	if wait <= 0 {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case composeSlot <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

func releaseComposeSlot() { <-composeSlot }

// composeRunnerEnvKeys is the ONLY parent environment the compose runner inherits — the
// same passthrough set the runner itself allows its HyperFrames child (plus nothing).
var composeRunnerEnvKeys = []string{
	"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "HOME",
	"LOCALAPPDATA", "APPDATA", "XDG_CACHE_HOME",
}

// composeRunnerEnv filters environ down to composeRunnerEnvKeys. Windows names are
// case-insensitive ("Path", "SystemRoot"), so matching folds case there and keeps the
// first spelling; elsewhere names match exactly.
func composeRunnerEnv(environ []string, goos string) []string {
	var out []string
	seen := map[string]bool{}
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		name := k
		if goos == "windows" {
			name = strings.ToUpper(k)
		}
		if !slices.Contains(composeRunnerEnvKeys, name) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, kv)
	}
	return out
}

var composeTemplateName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

var composeResolutions = []string{"landscape", "portrait", "landscape-4k", "portrait-4k", "square", "square-4k"}

// composeMaxHTML bounds an inline composition: a single-file page, not a media carrier.
const composeMaxHTML = 4 << 20

// composeRequest is a validated compose_video request.
type composeRequest struct {
	ProjectDir  string
	HTML        string
	Template    string
	Variables   map[string]any
	Composition string
	Out         string
	Format      string
	Quality     string
	FPS         int
	Resolution  string
	Workers     string
	Strict      bool
	Snapshots   []float64
}

// parseComposeRequest validates params before anything is spawned — a bad request is a
// BAD_INPUT defer that never costs a Chrome start.
func parseComposeRequest(params map[string]any, cfg config.Config) (composeRequest, error) {
	r := composeRequest{
		ProjectDir:  strings.TrimSpace(paramStr(params, "project_dir")),
		HTML:        paramStr(params, "html"),
		Template:    strings.TrimSpace(paramStr(params, "template")),
		Composition: strings.TrimSpace(paramStr(params, "composition")),
		Out:         strings.TrimSpace(paramStr(params, "out")),
		Format:      strings.TrimSpace(paramStr(params, "format")),
		Quality:     strings.TrimSpace(paramStr(params, "quality")),
		Resolution:  strings.TrimSpace(paramStr(params, "resolution")),
		Strict:      paramBoolOr(params, "strict", true),
	}
	n := 0
	for _, set := range []bool{r.ProjectDir != "", strings.TrimSpace(r.HTML) != "", r.Template != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return r, fmt.Errorf("exactly one of project_dir, html or template is required (got %d)", n)
	}
	if r.Template != "" && !composeTemplateName.MatchString(r.Template) {
		return r, fmt.Errorf("template name %q is invalid (lower-case letters, digits and dashes)", r.Template)
	}
	if r.ProjectDir != "" {
		if fi, err := os.Stat(r.ProjectDir); err != nil || !fi.IsDir() {
			return r, fmt.Errorf("project_dir %q is not a directory on this machine", r.ProjectDir)
		}
	}
	if len(r.HTML) > composeMaxHTML {
		return r, fmt.Errorf("html is %d bytes; inline compositions are capped at %d (use project_dir)", len(r.HTML), composeMaxHTML)
	}
	if r.Format == "" {
		r.Format = "mp4"
	}
	if !slices.Contains(config.ComposeFormats, r.Format) {
		return r, fmt.Errorf("format %q is not one of %v", r.Format, config.ComposeFormats)
	}
	if r.Quality == "" {
		r.Quality = cfg.ComposeQuality
	}
	if r.Quality == "" {
		r.Quality = "high"
	}
	if !slices.Contains(config.ComposeQualities, r.Quality) {
		return r, fmt.Errorf("quality %q is not one of %v", r.Quality, config.ComposeQualities)
	}
	if r.Resolution != "" && !slices.Contains(composeResolutions, r.Resolution) {
		return r, fmt.Errorf("resolution %q is not one of %v", r.Resolution, composeResolutions)
	}
	if raw, ok := params["fps"]; ok && raw != nil {
		f := paramFloat(params, "fps")
		if f != 0 && (f != math.Trunc(f) || f < 1 || f > 240) {
			return r, fmt.Errorf("fps %v must be an integer 1-240", raw)
		}
		r.FPS = int(f)
	}
	switch w := params["workers"].(type) {
	case nil:
		r.Workers = strings.TrimSpace(cfg.ComposeWorkers)
	case string:
		r.Workers = strings.TrimSpace(w)
	default:
		f := paramFloat(params, "workers")
		if f == 0 {
			r.Workers = strings.TrimSpace(cfg.ComposeWorkers)
		} else if f != math.Trunc(f) {
			return r, fmt.Errorf("workers %v must be an integer", w)
		} else {
			r.Workers = strconv.Itoa(int(f))
		}
	}
	if r.Workers == "" {
		r.Workers = "auto"
	}
	if !config.ValidComposeWorkers(r.Workers) {
		return r, fmt.Errorf("workers %q must be \"auto\" or an integer 1-24", r.Workers)
	}
	if r.Composition != "" {
		c := filepath.ToSlash(r.Composition)
		if filepath.IsAbs(r.Composition) || strings.HasPrefix(c, "/") || slices.Contains(strings.Split(c, "/"), "..") {
			return r, fmt.Errorf("composition %q must be a relative path inside the project", r.Composition)
		}
	}
	if raw, ok := params["variables"]; ok && raw != nil {
		b, err := json.Marshal(raw)
		if err != nil {
			return r, fmt.Errorf("variables: %v", err)
		}
		if err := json.Unmarshal(b, &r.Variables); err != nil || r.Variables == nil {
			return r, errors.New("variables must be a JSON object")
		}
	}
	if raw, ok := params["snapshots"]; ok && raw != nil {
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &r.Snapshots); err != nil {
			return r, errors.New("snapshots must be an array of seconds")
		}
		if len(r.Snapshots) > 16 {
			return r, fmt.Errorf("at most 16 snapshots (got %d)", len(r.Snapshots))
		}
		for _, s := range r.Snapshots {
			if s < 0 || math.IsNaN(s) || math.IsInf(s, 0) {
				return r, fmt.Errorf("snapshot time %v must be a non-negative number of seconds", s)
			}
		}
	}
	return r, nil
}

// composeExt is the default output extension per format ("" = a directory).
func composeExt(format string) string {
	switch format {
	case "png-sequence":
		return ""
	default:
		return "." + format
	}
}

// composeVideoResult is the compose_video payload: exactly what the runner measured with
// ffprobe after the render, never the request's values.
type composeVideoResult struct {
	VideoPath   string       `json:"video_path,omitempty"`
	FramesDir   string       `json:"frames_dir,omitempty"`
	Format      string       `json:"format"`
	Quality     string       `json:"quality"`
	Workers     string       `json:"workers"`
	Template    string       `json:"template,omitempty"`
	DurationSec float64      `json:"duration_sec"`
	FPS         float64      `json:"fps"`
	Frames      int          `json:"frames"`
	Width       int          `json:"width"`
	Height      int          `json:"height"`
	HasAlpha    bool         `json:"has_alpha"`
	HasAudio    bool         `json:"has_audio"`
	Codec       string       `json:"codec"`
	PixFmt      string       `json:"pix_fmt,omitempty"`
	RenderMs    int64        `json:"render_ms"`
	Lint        composeLint  `json:"lint"`
	Check       composeCheck `json:"check"`
	Snapshots   []string     `json:"snapshots"`
	Engine      string       `json:"engine"`
	Version     string       `json:"version"`
}

type composeLint struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
}

type composeCheck struct {
	OK       bool             `json:"ok"`
	Findings []composeFinding `json:"findings"`
}

type composeFinding struct {
	Section  string `json:"section"`
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// composeRunnerResult is the runner's --result file: the payload on success, the typed
// failure on a failure.
type composeRunnerResult struct {
	composeVideoResult
	OK     bool   `json:"ok"`
	Class  string `json:"class"`
	Detail string `json:"detail"`
}

const composeFailPrefix = "COMPOSE-FAIL: "

// composeFailFromTail extracts the runner's typed line out of a gpugen error tail — the
// fallback when the result file is missing (a runner killed before it could write one).
func composeFailFromTail(msg string) (class, detail string) {
	i := strings.LastIndex(msg, composeFailPrefix)
	if i < 0 {
		return "", ""
	}
	rest := msg[i+len(composeFailPrefix):]
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		rest = rest[:nl]
	}
	class, detail, _ = strings.Cut(rest, ": ")
	return strings.TrimSpace(class), strings.TrimSpace(detail)
}

// runComposeVideo is the compose_video branch of Run.
func (p *Pipeline) runComposeVideo(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	meta.Model = "hyperframes"
	inputChars := len(paramStr(req.Params, "html"))
	deferClass := func(class, detail string) core.Result {
		meta.ErrClass = strings.ToLower(class)
		return p.deferGen(req, meta, start, inputChars, "compose_video: "+class+": "+detail)
	}
	if !p.cfg.ComposeRouteConfigured() {
		meta.ErrClass = "not_configured"
		return p.deferGen(req, meta, start, inputChars,
			"compose_video: no composition route configured on this machine (compose_script, hyperframes_dir and hyperframes_browser_path must all be set — the installer's hyperframes step binds them; it needs node >= 22)")
	}
	creq, err := parseComposeRequest(req.Params, p.cfg)
	if err != nil {
		return deferClass("BAD_INPUT", err.Error())
	}
	script, serr := gpugen.ResolveScript(p.cfg.ComposeScript)
	if serr != nil {
		return deferClass("CLI_MISSING", serr.Error())
	}

	wait := p.gpuWait()
	if !takeComposeSlot(wait) {
		meta.ErrClass = "compose_busy"
		return p.deferGen(req, meta, start, inputChars,
			fmt.Sprintf("compose_video: busy — another composition in this process still holds the compose slot after %s", wait))
	}
	defer releaseComposeSlot()

	cacheDir := p.cfg.EffectiveComposeCacheDir()
	key := sha256hex(fmt.Sprint(creq.ProjectDir, creq.HTML, creq.Template, creq.Variables, creq.Composition,
		creq.Format, creq.Quality, creq.FPS, creq.Resolution))[:8]
	reqDir := filepath.Join(cacheDir, "req", key+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		return deferClass("DISK_HEADROOM", "cannot create the compose request dir: "+err.Error())
	}
	defer func() { _ = os.RemoveAll(reqDir) }()

	out := creq.Out
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		name := "compose-" + key
		if creq.Format == "png-sequence" {
			// A directory output is never overwritten, so a repeat request needs a fresh name.
			name += "-" + strconv.FormatInt(time.Now().Unix(), 10)
		}
		out = filepath.Join(p.cfg.MediaDir, name+composeExt(creq.Format))
	}
	resultPath := filepath.Join(reqDir, "result.json")
	args := []string{"render",
		"--hyperframes-dir", p.cfg.HyperframesDir,
		"--browser", p.cfg.HyperframesBrowserPath,
		"--ffmpeg", p.cfg.FFmpegPath,
		"--cache-dir", cacheDir,
		"--out", out,
		"--result", resultPath,
		"--format", creq.Format,
		"--quality", creq.Quality,
		"--workers", creq.Workers,
		"--timeout-sec", strconv.Itoa(p.composeTimeoutSec()),
	}
	switch {
	case creq.Template != "":
		args = append(args, "--template", creq.Template)
	case creq.ProjectDir != "":
		args = append(args, "--project-dir", creq.ProjectDir)
	default:
		htmlFile := filepath.Join(reqDir, "index.html")
		if err := os.WriteFile(htmlFile, []byte(creq.HTML), 0o644); err != nil {
			return deferClass("DISK_HEADROOM", "cannot stage the inline composition: "+err.Error())
		}
		args = append(args, "--html-file", htmlFile)
	}
	if len(creq.Variables) > 0 {
		b, _ := json.Marshal(creq.Variables)
		vf := filepath.Join(reqDir, "variables.json")
		if err := os.WriteFile(vf, b, 0o644); err != nil {
			return deferClass("DISK_HEADROOM", "cannot stage the variables: "+err.Error())
		}
		args = append(args, "--variables-file", vf)
		inputChars += len(b)
	}
	if creq.Composition != "" {
		args = append(args, "--composition", creq.Composition)
	}
	if creq.FPS > 0 {
		args = append(args, "--fps", strconv.Itoa(creq.FPS))
	}
	if creq.Resolution != "" {
		args = append(args, "--resolution", creq.Resolution)
	}
	if !creq.Strict {
		args = append(args, "--no-strict")
	}
	if len(creq.Snapshots) > 0 {
		ts := make([]string, len(creq.Snapshots))
		for i, s := range creq.Snapshots {
			ts[i] = strconv.FormatFloat(s, 'f', -1, 64)
		}
		args = append(args, "--snapshots", strings.Join(ts, ","))
	}

	spec := gpugen.Spec{
		Exe:    p.cfg.NodePath,
		Script: script,
		Args:   args,
		// The runner starts from an allowlist, not this process's environment.
		Env:           composeRunnerEnv(os.Environ(), runtime.GOOS),
		EnvExact:      true,
		Out:           resultPath,
		Timeout:       time.Duration(p.composeTimeoutSec())*time.Second + time.Minute,
		SkipFreeComfy: true, // no ComfyUI anywhere on this lane
	}
	_, gerr := gpugen.Generate(ctx, spec)
	var rr composeRunnerResult
	b, rerr := os.ReadFile(resultPath)
	if rerr == nil {
		rerr = json.Unmarshal(b, &rr)
	}
	if gerr != nil || rerr != nil || !rr.OK {
		switch {
		case rerr == nil && !rr.OK && rr.Class != "":
			return deferClass(rr.Class, rr.Detail)
		case gerr != nil:
			if class, detail := composeFailFromTail(gerr.Error()); class != "" {
				return deferClass(class, detail)
			}
			if gpugen.ClassifyErr(gerr) == "timeout" || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return deferClass("TIMEOUT", gerr.Error())
			}
			return deferClass("RENDER_FAILED", gerr.Error())
		default:
			return deferClass("RENDER_FAILED", "the runner left no readable result: "+fmt.Sprint(rerr))
		}
	}
	res := rr.composeVideoResult
	produced := res.VideoPath
	if produced == "" {
		produced = res.FramesDir
	}
	if fi, err := os.Stat(produced); produced == "" || err != nil || (!fi.IsDir() && fi.Size() == 0) {
		return deferClass("RENDER_FAILED", fmt.Sprintf("the runner reported success but its output %q is missing or empty", produced))
	}
	if res.Snapshots == nil {
		res.Snapshots = []string{}
	}
	if res.Check.Findings == nil {
		res.Check.Findings = []composeFinding{}
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(res)
	p.record(req.Task, meta, inputChars)
	return core.Result{OK: true, Data: data, Meta: meta}
}

func (p *Pipeline) composeTimeoutSec() int {
	if p.cfg.ComposeTimeoutSec > 0 {
		return p.cfg.ComposeTimeoutSec
	}
	return 1800
}
