package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dmmdea/offload-harness/internal/acceptance"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/judge"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/mediaops"
	"github.com/dmmdea/offload-harness/internal/swapclient"
	"github.com/dmmdea/offload-harness/internal/videoio"
)

// `acceptance --render`: one REAL call per bound route, each checked against a known answer.
// Every call is this binary run again as a subprocess with the real CLI verb, --json and the
// same --config, so it goes through the doors a caller uses, GPU lease and admission included.
// A route the config does not bind is a Skip with its reason and never makes a node unready.

// renderDefaultTimeout is the per-call limit when the route has no timeout of its own in the config.
const renderDefaultTimeout = 30 * time.Minute

const (
	renderSentence = "The quick brown fox jumps over the lazy dog near the river."
	renderPrompt   = "a red bicycle leaning on a blue wall, photo"
)

// renderSentenceWords are the words of renderSentence a transcript may carry.
var renderSentenceWords = []string{"quick", "brown", "fox", "jumps", "lazy", "dog", "river"}

// renderStep is one call of a group. A step with local set runs in this process (the embedding
// call has no CLI verb); every other step runs the verb as a subprocess.
type renderStep struct {
	label   string
	pre     []string // ffmpeg arguments run before the call (the animate driver clip)
	verb    string
	args    []string // everything after the verb except --config and --json
	own     bool     // the verb prints its own JSON and has no --json flag (delegate)
	exe     string   // another binary to run with args as given, "" for this one (local-agent)
	timeout time.Duration
	out     string // the file the call must leave behind, "" when none
	check   func(res core.Result, raw string) (string, error)
	local   func(ctx context.Context) (string, error)
	note    string // what --list shows for a local step
}

// renderGroup is one route group: skip returns why it is not bound on this config ("" when bound).
type renderGroup struct {
	name  string
	skip  func(e *renderEnv) string
	steps func(e *renderEnv) []renderStep
}

// renderEnv is what the groups read: the config, the derived routes and the output folder.
type renderEnv struct {
	cfg             config.Config
	routes          []mediacap.Route
	aliases         map[string]string
	exe, cfgPath    string
	dir             string
	as              acceptance.Assets
	ffmpeg, ffprobe string
	live            bool // false for --list: no network, no files
	roster          *swapclient.Roster
	rosterErr       error
}

func (e *renderEnv) p(name string) string { return filepath.Join(e.dir, name) }

func (e *renderEnv) alias(key string) string { return e.aliases[key] }

// timeout is the route's own config timeout when it has one, else the default.
func renderTimeout(sec int) time.Duration {
	if sec > 0 {
		return time.Duration(sec) * time.Second
	}
	return renderDefaultTimeout
}

// routeSkip is "" when one of the named routes is derived and configured, else why not.
func (e *renderEnv) routeSkip(names ...string) string {
	var why []string
	for _, n := range names {
		for _, r := range e.routes {
			if r.Name != n {
				continue
			}
			if r.OK() {
				return ""
			}
			why = append(why, fmt.Sprintf("route %s is %s: %s", r.Name, strings.ToLower(string(r.State)), r.Detail))
		}
	}
	if len(why) == 0 {
		return "no route " + strings.Join(names, " or ") + " is derived on this config"
	}
	return strings.Join(why, "; ")
}

// laneSkip is "" when one of the model keys names an alias, else why not.
func (e *renderEnv) laneSkip(keys ...string) string {
	if !hasLocalModel(e.cfg) {
		return "no local model is configured (a delegation client); the fleet serves every model"
	}
	for _, k := range keys {
		if e.alias(k) != "" {
			return ""
		}
	}
	return strings.Join(keys, " / ") + " is unset"
}

// embedSkip: the embedding call runs only when the alias the config binds is in the live roster.
func (e *renderEnv) embedSkip() string {
	if !hasLocalModel(e.cfg) {
		return "no local model is configured (a delegation client); the fleet serves every model"
	}
	if !e.live {
		return ""
	}
	if e.roster == nil && e.rosterErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r, err := swapclient.FetchRoster(ctx, e.cfg.Endpoint, 10*time.Second)
		if err != nil {
			e.rosterErr = err
		} else {
			e.roster = &r
		}
	}
	if e.rosterErr != nil {
		return "the live roster cannot be read: " + e.rosterErr.Error()
	}
	if !e.roster.Serves(e.cfg.EmbedModel()) {
		return fmt.Sprintf("embedding alias %q is not in the live roster", e.cfg.EmbedModel())
	}
	return ""
}

// ---- checks ----------------------------------------------------------------------------------

func needAny(what string, words ...string) func(core.Result, string) (string, error) {
	return func(res core.Result, _ string) (string, error) {
		if len(res.Data) == 0 {
			return "", errors.New("the result is empty")
		}
		if w := acceptance.HasAny(string(res.Data), words...); w != "" {
			return fmt.Sprintf("%s (names %q)", what, w), nil
		}
		return "", fmt.Errorf("the result names none of %v: %s", words, acceptance.Tail(string(res.Data), 160))
	}
}

func needAll(what string, words ...string) func(core.Result, string) (string, error) {
	return func(res core.Result, _ string) (string, error) {
		if len(res.Data) == 0 {
			return "", errors.New("the result is empty")
		}
		if w := acceptance.HasAll(string(res.Data), words...); w != "" {
			return "", fmt.Errorf("the result lacks %q: %s", w, acceptance.Tail(string(res.Data), 160))
		}
		return fmt.Sprintf("%s (has %s)", what, strings.Join(words, " and ")), nil
	}
}

// fileCheck runs a check on the output file after the generic result checks passed.
func fileCheck(path string, f func(path string) (string, error)) func(core.Result, string) (string, error) {
	return func(core.Result, string) (string, error) { return f(path) }
}

func probeCheck(e *renderEnv, path, kind string) func(core.Result, string) (string, error) {
	return fileCheck(path, func(p string) (string, error) {
		d, err := acceptance.Probe(context.Background(), e.ffprobe, p, kind)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("ffprobe: a %s stream, %.1fs", kind, d), nil
	})
}

func classifyCheck(res core.Result, _ string) (string, error) {
	var m map[string]any
	if err := json.Unmarshal(res.Data, &m); err != nil {
		return "", fmt.Errorf("the result is not an object: %w", err)
	}
	l, _ := m["label"].(string)
	if !strings.EqualFold(strings.TrimSpace(l), "documentation") {
		return "", fmt.Errorf("label is %q, want documentation", l)
	}
	return "label is documentation", nil
}

// agentLoopCheck reads local-agent's plain answer.
func agentLoopCheck(_ core.Result, raw string) (string, error) {
	if acceptance.HasAny(raw, "391") == "" {
		return "", fmt.Errorf("the answer lacks 391: %s", acceptance.Tail(raw, 160))
	}
	return "answer contains 391", nil
}

func agentCheck(_ core.Result, raw string) (string, error) {
	var w struct {
		Results []struct {
			Output     string          `json:"output"`
			Structured json.RawMessage `json:"structured"`
			Deferred   bool            `json:"deferred"`
			Failed     bool            `json:"failed"`
			Reason     string          `json:"reason"`
		} `json:"results"`
	}
	if err := acceptance.JSONObject(raw, &w); err != nil {
		return "", fmt.Errorf("unreadable answer: %w", err)
	}
	if len(w.Results) == 0 {
		return "", errors.New("the answer holds no result")
	}
	r := w.Results[0]
	switch {
	case r.Deferred:
		return "", fmt.Errorf("deferred: %s", r.Reason)
	case r.Failed:
		return "", fmt.Errorf("failed: %s", r.Reason)
	}
	if acceptance.HasAny(r.Output+" "+string(r.Structured), "391") == "" {
		return "", fmt.Errorf("the answer lacks 391: %s", acceptance.Tail(r.Output+" "+string(r.Structured), 160))
	}
	return "answer contains 391", nil
}

// ---- the table -------------------------------------------------------------------------------

var renderGroups = []renderGroup{
	{"summarize",
		func(e *renderEnv) string { return e.laneSkip("model") },
		func(e *renderEnv) []renderStep {
			return []renderStep{{label: "summarize", verb: "summarize", args: []string{e.as.Paragraph, "--max-points", "3"}, timeout: renderDefaultTimeout,
				check: needAny("result is non-empty", acceptance.ParagraphWords...)}}
		}},
	{"classify",
		func(e *renderEnv) string { return e.laneSkip("triage_model", "model") },
		func(e *renderEnv) []renderStep {
			return []renderStep{{label: "classify", verb: "classify", args: []string{e.as.Paragraph, "--labels", "documentation,source-code,chat-transcript"},
				timeout: renderDefaultTimeout, check: classifyCheck}}
		}},
	{"embed",
		func(e *renderEnv) string { return e.embedSkip() },
		func(e *renderEnv) []renderStep {
			alias := e.cfg.EmbedModel()
			return []renderStep{{label: "embed", timeout: renderDefaultTimeout,
				note: fmt.Sprintf("POST %s/v1/embeddings model=%s (in this process, behind the GPU-lease fence; runs only when the alias is in the live roster)", e.cfg.Endpoint, alias),
				local: func(ctx context.Context) (string, error) {
					vec, err := judge.NewEmbedder(e.cfg.Endpoint, alias, renderDefaultTimeout).Embed(acceptance.Paragraph)
					if err != nil {
						return "", err
					}
					if len(vec) == 0 {
						return "", errors.New("the vector is empty")
					}
					return fmt.Sprintf("one vector of length %d from %s", len(vec), alias), nil
				}}}
		}},
	{"vision",
		func(e *renderEnv) string { return e.laneSkip("vision_model", "ocr_model") },
		func(e *renderEnv) []renderStep {
			var s []renderStep
			if e.alias("vision_model") != "" {
				s = append(s, renderStep{label: "vqa", verb: "vqa", timeout: renderDefaultTimeout,
					args:  []string{e.as.Sign, "--question", "What shape is drawn in red? Answer in one word.", "--route", "local"},
					check: needAny("answer names the shape", "circle")})
			}
			return append(s, renderStep{label: "ocr", verb: "ocr", timeout: renderDefaultTimeout,
				args:  []string{e.as.Sign, "--route", "local"},
				check: needAll("text read from the sign", "OFFLOAD", "HARNESS")})
		}},
	{"stt",
		func(e *renderEnv) string { return e.laneSkip("stt_model") },
		func(e *renderEnv) []renderStep {
			return []renderStep{{label: "transcribe", verb: "transcribe", timeout: renderTimeout(e.cfg.STTRequestTimeoutSec),
				args:  []string{e.as.Speech, "--language", "en", "--route", "local"},
				check: needAll("transcript of the speech sample", "country", "ask")}}
		}},
	{"agent",
			func(e *renderEnv) string { return e.laneSkip("agent_model") },
		func(e *renderEnv) []renderStep {
			if !e.cfg.AgentDelegationEnabled {
				// Delegating is the operator's opt-in and no tier seeds it, but a worker node still serves this seat to
				// the fleet, so the seat is proven through the agent loop that runs it.
				exe := filepath.Join(filepath.Dir(e.exe), "local-agent"+filepath.Ext(e.exe))
				return []renderStep{{label: "local-agent", own: true, exe: exe, timeout: renderTimeout(e.cfg.AgentTimeoutSec),
					args: []string{"What is 17 times 23? Answer with the number only.", "-base", swapclient.BaseURL(e.cfg.Endpoint),
						"-model", e.alias("agent_model"), "-ctx-tokens", fmt.Sprint(e.cfg.AgentCtxTokens)},
					check: agentLoopCheck}}
			}
			return []renderStep{{label: "delegate", verb: "delegate", own: true, timeout: renderTimeout(e.cfg.AgentTimeoutSec),
				args:  []string{"--contract", e.as.Contract, "--route", "local", "--pin-reason", "measurement"},
				check: agentCheck}}
		}},
	{"image",
		func(e *renderEnv) string { return e.routeSkip("generate_image") },
		func(e *renderEnv) []renderStep {
			out := e.p("image.png")
			s := []renderStep{{label: "generate-image", verb: "generate-image", out: out, timeout: renderTimeout(e.cfg.ImageGenTimeoutSec),
				args: []string{renderPrompt, "--width", "512", "--height", "512", "--seed", "7", "--refine=false", "--route", "local", "--out", out},
				check: fileCheck(out, func(p string) (string, error) {
					n, err := acceptance.NotBlank(p)
					if err != nil {
						return "", err
					}
					w, h, _ := acceptance.PNGSize(p)
					return fmt.Sprintf("PNG decodes, %dx%d, %d distinct colours on the grid", w, h, n), nil
				})}}
			if e.alias("vision_model") != "" {
				s = append(s, renderStep{label: "vqa on the picture", verb: "vqa", timeout: renderDefaultTimeout,
					args:  []string{out, "--question", "What object is in the picture? Answer in a few words.", "--route", "local"},
					check: needAny("answer names the object", "bicycle", "bike")})
			}
			return s
		}},
	{"video",
		func(e *renderEnv) string { return e.routeSkip("generate_video") },
		func(e *renderEnv) []renderStep {
			out := e.p("video.mp4")
			return []renderStep{{label: "generate-video", verb: "generate-video", out: out, timeout: renderTimeout(e.cfg.VideoGenTimeoutSec),
				args:  []string{out, e.as.Sign, "a red circle sign swaying gently in the wind", "--frames", "17", "--width", "256", "--height", "256", "--route", "local"},
				check: probeCheck(e, out, "video")}}
		}},
	{"animate",
		func(e *renderEnv) string { return e.routeSkip("animate_character") },
		func(e *renderEnv) []renderStep {
			out, driver := e.p("animate.mp4"), e.p("driver.mp4")
			return []renderStep{{label: "animate-character", verb: "animate-character", out: out, timeout: renderTimeout(e.cfg.AnimateGenTimeoutSec),
				// The driver clip is the sign swaying, made with ffmpeg: motion for the depth control, 2 s at 16 fps.
				pre: []string{"-y", "-loop", "1", "-framerate", "16", "-i", e.as.Sign, "-t", "2",
					"-vf", "scale=256:256,rotate=0.3*sin(2*PI*t):c=white,format=yuv420p", driver},
				args:  []string{out, e.as.Sign, driver, "a red circle sign swaying gently", "--frames", "17", "--width", "256", "--height", "256", "--route", "local"},
				check: probeCheck(e, out, "video")}}
		}},
	{"voice",
		func(e *renderEnv) string { return e.routeSkip("generate_audio:voice", "generate_audio:voice:endpoint") },
		func(e *renderEnv) []renderStep {
			out := e.p("voice.wav")
			s := []renderStep{{label: "generate-audio voice", verb: "generate-audio", out: out, timeout: renderTimeout(e.cfg.AudioGenTimeoutSec),
				args:  []string{out, renderSentence, "--kind", "voice", "--lang", "en", "--route", "local"},
				check: probeCheck(e, out, "audio")}}
			if e.alias("stt_model") != "" {
				s = append(s, renderStep{label: "transcribe the voice", verb: "transcribe", timeout: renderTimeout(e.cfg.STTRequestTimeoutSec),
					args:  []string{out, "--language", "en", "--route", "local"},
					check: needAny("transcript carries a word of the sentence", renderSentenceWords...)})
			}
			return s
		}},
	{"music",
		func(e *renderEnv) string { return e.routeSkip("generate_audio:music") },
		func(e *renderEnv) []renderStep {
			out := e.p("music.wav")
			return []renderStep{{label: "generate-audio music", verb: "generate-audio", out: out, timeout: renderTimeout(e.cfg.AudioGenTimeoutSec),
				args:  []string{out, "calm piano melody, slow tempo", "--kind", "music", "--seconds", "5", "--route", "local"},
				check: probeCheck(e, out, "audio")}}
		}},
	{"compose",
		func(e *renderEnv) string { return e.routeSkip("compose_video") },
		func(e *renderEnv) []renderStep {
			out := e.p("compose.mp4")
			return []renderStep{{label: "compose-video", verb: "compose-video", out: out, timeout: renderTimeout(e.cfg.ComposeTimeoutSec),
				args: []string{"--template", "title-card", "--variables", `{"title":"Acceptance check","subtitle":"Render gate","duration":2}`,
					"--format", "mp4", "--quality", "draft", "--out", out, "--route", "local"},
				check: probeCheck(e, out, "video")}}
		}},
	{"edit",
		func(e *renderEnv) string { return e.routeSkip("edit_image") },
		func(e *renderEnv) []renderStep {
			out := e.p("edit.png")
			return []renderStep{{label: "edit-image", verb: "edit-image", out: out, timeout: renderTimeout(e.cfg.EditTimeoutSec),
				args: []string{e.as.Sign, "--ops", `[{"op":"resize","width":128}]`, "--out", out},
				check: fileCheck(out, func(p string) (string, error) {
					w, h, err := acceptance.PNGSize(p)
					if err != nil {
						return "", err
					}
					if w != 128 {
						return "", fmt.Errorf("resized to width 128 but the output is %dx%d", w, h)
					}
					return fmt.Sprintf("PNG decodes, resized to %dx%d", w, h), nil
				})}}
		}},
	{"media",
		func(e *renderEnv) string { return e.routeSkip("media") },
		func(e *renderEnv) []renderStep {
			out := e.p("media.jpg")
			return []renderStep{{label: "media convert", verb: "media", out: out, timeout: renderDefaultTimeout,
				args: []string{"convert", "--in", e.as.Sign, "--out", out},
				check: fileCheck(out, func(p string) (string, error) {
					fi, err := os.Stat(p)
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("ffmpeg convert wrote %d bytes", fi.Size()), nil
				})}}
		}},
	{"inpaint",
		func(e *renderEnv) string { return e.routeSkip("inpaint_image") },
		func(e *renderEnv) []renderStep {
			out := e.p("inpaint.png")
			return []renderStep{{label: "inpaint-image", verb: "inpaint-image", out: out, timeout: renderTimeout(e.cfg.InpaintTimeoutSec),
				args: []string{e.as.Sign, "--mask", e.as.Mask, "--prompt", "a solid green square", "--seed", "7", "--out", out},
				check: fileCheck(out, func(p string) (string, error) {
					w, h, err := acceptance.PNGSize(p)
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("PNG decodes, %dx%d", w, h), nil
				})}}
		}},
	{"upscale",
		func(e *renderEnv) string { return e.routeSkip("upscale_image") },
		func(e *renderEnv) []renderStep {
			out := e.p("upscale.png")
			return []renderStep{{label: "upscale-image", verb: "upscale-image", out: out, timeout: renderTimeout(e.cfg.UpscaleTimeoutSec),
				args: []string{e.as.Sign, "--scale", "2", "--out", out},
				check: fileCheck(out, func(p string) (string, error) {
					w, h, err := acceptance.PNGSize(p)
					if err != nil {
						return "", err
					}
					sw, _, serr := acceptance.PNGSize(e.as.Sign)
					if serr != nil || w <= sw {
						return "", fmt.Errorf("the output is %dx%d, not larger than the source (width %d)", w, h, sw)
					}
					return fmt.Sprintf("PNG decodes, enlarged to %dx%d", w, h), nil
				})}}
		}},
	{"gen-edit",
		func(e *renderEnv) string {
			if why := e.routeSkip("edit_image_generative"); why != "" {
				return why
			}
			return "bound, but generative edit has no CLI verb (it is an MCP tool only), so there is no real door to call from here"
		}, nil},
	{"run-graph",
		func(e *renderEnv) string {
			if why := e.routeSkip("run_graph"); why != "" {
				return why
			}
			return "bound, but run-graph runs a caller's own ComfyUI API graph and there is no sensible embedded default"
		}, nil},
}

// ---- running ---------------------------------------------------------------------------------

// renderOpts are the flags of --render.
type renderOpts struct {
	only   map[string]bool
	outDir string
}

func newRenderOpts(only, outDir string) (renderOpts, error) {
	o := renderOpts{outDir: outDir}
	if strings.TrimSpace(only) == "" {
		return o, nil
	}
	known := map[string]bool{}
	var names []string
	for _, g := range renderGroups {
		known[g.name] = true
		names = append(names, g.name)
	}
	o.only = map[string]bool{}
	for _, n := range strings.Split(only, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !known[n] {
			return o, fmt.Errorf("acceptance: --only %q is not a route group; the groups are %s", n, strings.Join(names, ", "))
		}
		o.only[n] = true
	}
	return o, nil
}

func (o renderOpts) wants(name string) bool { return o.only == nil || o.only[name] }

func newRenderEnv(cfg config.Config, src config.Source, routes []mediacap.Route, dir string, live bool) *renderEnv {
	e := &renderEnv{cfg: cfg, routes: routes, aliases: map[string]string{}, dir: dir, live: live, as: acceptance.AssetPaths(dir)}
	for _, a := range modelAliases(cfg) {
		e.aliases[a.Key] = a.Alias
	}
	e.exe, _ = os.Executable()
	if src.Loaded() {
		e.cfgPath = src.Path
		if abs, err := filepath.Abs(src.Path); err == nil {
			e.cfgPath = abs
		}
	}
	e.ffmpeg, _ = mediaops.ResolveBinary(cfg.FFmpegPath)
	if p, ok := mediaops.ResolveBinary(videoio.ProbePath(cfg.FFmpegPath)); ok {
		e.ffprobe = p
	} else if p, ok := mediaops.ResolveBinary("ffprobe"); ok {
		e.ffprobe = p
	}
	return e
}

// argv is the verb's command line: the same --config, the verb, its flags and --json.
func (e *renderEnv) argv(s renderStep) []string {
	if s.exe != "" {
		return s.args
	}
	var a []string
	if e.cfgPath != "" {
		a = append(a, "--config", e.cfgPath)
	}
	a = append(a, s.verb)
	a = append(a, s.args...)
	if !s.own {
		a = append(a, "--json")
	}
	return a
}

// commands are the exact command lines a step runs, for --list.
func (e *renderEnv) commands(s renderStep) []string {
	if s.local != nil {
		return []string{s.note}
	}
	var out []string
	if len(s.pre) > 0 {
		out = append(out, quoteCmd(append([]string{"ffmpeg"}, s.pre...)))
	}
	return append(out, quoteCmd(append([]string{e.binary(s)}, e.argv(s)...)))
}

// binary is what a step runs: this binary unless the step names another.
func (e *renderEnv) binary(s renderStep) string {
	if s.exe != "" {
		return s.exe
	}
	return e.exe
}

func quoteCmd(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		switch {
		case a != "" && !strings.ContainsAny(a, " \t\"'<>"):
			q[i] = a
		case !strings.Contains(a, "'"):
			q[i] = "'" + a + "'"
		default:
			q[i] = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		}
	}
	return strings.Join(q, " ")
}

// runStep runs one step and returns its detail, or the reason it failed.
func (e *renderEnv) runStep(ctx context.Context, s renderStep) (string, error) {
	start := time.Now()
	wall := func() string { return fmt.Sprintf("%.1fs", time.Since(start).Seconds()) }
	if s.local != nil {
		what, err := s.local(ctx)
		if err != nil {
			return "", fmt.Errorf("%s: %s: %w", s.label, wall(), err)
		}
		return fmt.Sprintf("%s %s: %s", s.label, wall(), what), nil
	}
	if len(s.pre) > 0 {
		if e.ffmpeg == "" {
			return "", fmt.Errorf("%s: ffmpeg is needed to make the driver clip and was not found", s.label)
		}
		if o := acceptance.Run(ctx, 5*time.Minute, e.ffmpeg, s.pre...); o.StartErr != nil || o.Exit != 0 {
			return "", fmt.Errorf("%s: the driver clip could not be made: %s", s.label, acceptance.Tail(o.Stderr+fmt.Sprint(o.StartErr), 200))
		}
	}
	callStart := time.Now()
	o := acceptance.Run(ctx, s.timeout, e.binary(s), e.argv(s)...)
	where := fmt.Sprintf("%s %.1fs", s.label, o.Wall.Seconds())
	if s.out != "" {
		where += " out=" + s.out
	}
	switch {
	case o.StartErr != nil:
		return "", fmt.Errorf("%s: cannot start %s: %v", where, e.binary(s), o.StartErr)
	case o.TimedOut:
		return "", fmt.Errorf("%s: timed out after %s", where, s.timeout)
	case o.Exit != 0:
		return "", fmt.Errorf("%s: exit %d: %s", where, o.Exit, acceptance.Tail(o.Stderr+" "+o.Stdout, 300))
	}
	var res core.Result
	if !s.own {
		if err := acceptance.JSONObject(o.Stdout, &res); err != nil {
			return "", fmt.Errorf("%s: unreadable --json answer (%v): %s", where, err, acceptance.Tail(o.Stdout, 200))
		}
		switch {
		case res.Deferred:
			class := ""
			if res.DeferClass != "" {
				class = " (" + res.DeferClass + ")"
			}
			return "", fmt.Errorf("%s: deferred%s: %s", where, class, res.Reason)
		case !res.OK:
			return "", fmt.Errorf("%s: ok is false: %s", where, res.Reason)
		}
	}
	if s.out != "" {
		fi, err := os.Stat(s.out)
		switch {
		case err != nil:
			return "", fmt.Errorf("%s: the call left no output file", where)
		case fi.Size() == 0:
			return "", fmt.Errorf("%s: the output file is empty", where)
		case fi.ModTime().Before(callStart.Add(-2 * time.Second)):
			return "", fmt.Errorf("%s: the output file is older than this call (a stale file)", where)
		}
	}
	what, err := s.check(res, o.Stdout)
	if err != nil {
		return "", fmt.Errorf("%s: %w", where, err)
	}
	return fmt.Sprintf("%s: checked %s", where, what), nil
}

// runRender runs every wanted group and returns one Check per group.
func runRender(ctx context.Context, cfg config.Config, src config.Source, routes []mediacap.Route, o renderOpts) []acceptance.Check {
	dir := o.outDir
	if dir == "" {
		d, err := os.MkdirTemp("", "local-offload-acceptance-")
		if err != nil {
			return []acceptance.Check{{Name: "render outputs", Status: acceptance.Fail, Detail: "cannot create the output folder: " + err.Error()}}
		}
		dir = d
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if _, err := acceptance.WriteAssets(dir); err != nil {
		return []acceptance.Check{{Name: "render outputs", Status: acceptance.Fail, Detail: "cannot write the assets into " + dir + ": " + err.Error()}}
	}
	e := newRenderEnv(cfg, src, routes, dir, true)
	out := []acceptance.Check{{Name: "render outputs", Status: acceptance.Pass, Detail: dir + " (kept; nothing is deleted)"}}
	for _, g := range renderGroups {
		if !o.wants(g.name) {
			continue
		}
		name := "render " + g.name
		if why := g.skip(e); why != "" || g.steps == nil {
			out = append(out, acceptance.Check{Name: name, Status: acceptance.Skip, Detail: why})
			continue
		}
		fmt.Fprintf(os.Stderr, "acceptance: %s ...\n", name)
		var details []string
		failed := false
		for _, s := range g.steps(e) {
			d, err := e.runStep(ctx, s)
			if err != nil {
				out = append(out, acceptance.Check{Name: name, Status: acceptance.Fail, Detail: strings.Join(append(details, err.Error()), "; ")})
				failed = true
				break
			}
			details = append(details, d)
		}
		if !failed {
			out = append(out, acceptance.Check{Name: name, Status: acceptance.Pass, Detail: strings.Join(details, "; ")})
		}
	}
	return out
}

// listRow is one group of `acceptance --list`.
type listRow struct {
	Group    string   `json:"group"`
	Bound    bool     `json:"bound"`
	Reason   string   `json:"reason,omitempty"`
	Commands []string `json:"commands,omitempty"`
}

// listRender prints each group, whether this config binds it and the commands it would run.
// It runs nothing and writes nothing.
func listRender(cfg config.Config, src config.Source, routes []mediacap.Route, o renderOpts, asJSON bool) error {
	dir := o.outDir
	if dir == "" {
		dir = "<out-dir>"
	}
	e := newRenderEnv(cfg, src, routes, dir, false)
	var rows []listRow
	for _, g := range renderGroups {
		if !o.wants(g.name) {
			continue
		}
		if why := g.skip(e); why != "" || g.steps == nil {
			rows = append(rows, listRow{Group: g.name, Reason: why})
			continue
		}
		r := listRow{Group: g.name, Bound: true}
		for _, s := range g.steps(e) {
			r.Commands = append(r.Commands, e.commands(s)...)
		}
		rows = append(rows, r)
	}
	if asJSON {
		b, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Fprintln(os.Stdout, config.SourceLine(src))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		if !r.Bound {
			fmt.Fprintf(w, "%s\tSKIP\t%s\n", r.Group, r.Reason)
			continue
		}
		for i, c := range r.Commands {
			g, st := r.Group, "BOUND"
			if i > 0 {
				g, st = "", ""
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", g, st, c)
		}
	}
	return w.Flush()
}
