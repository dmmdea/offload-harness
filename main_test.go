package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/eval"
)

// TestVersionSourcesAgree enforces the versioning invariant: the VERSION file, the
// compiled-in `version` const (advertised in the MCP handshake), and the top
// CHANGELOG entry must all name the same version. They silently drifted once —
// VERSION 0.7.0 / const 0.6.2 / CHANGELOG 0.7.0 while the public mirror published
// 0.8.0 — which made this canonical repo look *behind* its own mirror and cost real
// debugging time. A bump that misses any of the three now fails `go test ./...`.
func TestVersionSourcesAgree(t *testing.T) {
	raw, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	fileVer := strings.TrimSpace(string(raw))

	if version != fileVer {
		t.Errorf("version drift: main.go const version = %q but VERSION file = %q — bump both together", version, fileVer)
	}

	cl, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	if want := "## [" + fileVer + "]"; !strings.Contains(string(cl), want) {
		t.Errorf("CHANGELOG.md has no %q entry for the current VERSION — add a changelog entry when you bump", want)
	}

	// .printing-press.json is the fourth version source and the only one the
	// checks above never covered: it sat at 0.1.0 while VERSION reached 0.17.0.
	var manifest struct {
		Version string `json:"version"`
	}
	readManifest(t, &manifest)
	if manifest.Version != fileVer {
		t.Errorf("version drift: .printing-press.json version = %q but VERSION file = %q — bump it too", manifest.Version, fileVer)
	}
}

// TestPrintingPressManifestListsEveryTool keeps the manifest's advertised MCP
// surface honest. It listed the original four text tools long after the server
// grew to nineteen, so anything reading the manifest instead of the running
// server (a catalog, an installer, a doc generator) saw a harness with no
// vision, media, or agent capability at all.
func TestPrintingPressManifestListsEveryTool(t *testing.T) {
	var manifest struct {
		MCP struct {
			Tools []string `json:"tools"`
		} `json:"mcp"`
	}
	readManifest(t, &manifest)

	// mcpserver.go was split into mcpserver_*.go by topic; scrape every one of them.
	files, err := filepath.Glob(filepath.Join("internal", "mcpserver", "mcpserver*.go"))
	if err != nil {
		t.Fatalf("glob mcpserver source: %v", err)
	}
	var src strings.Builder
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		src.Write(b)
		src.WriteString("\n")
	}
	registered := regexp.MustCompile(`(?m)^\s+Name:\s+"([a-z_]+)",`).FindAllStringSubmatch(src.String(), -1)
	if len(registered) == 0 {
		t.Fatal("found no registered tool names in mcpserver*.go — the scrape pattern needs updating")
	}

	var want []string
	for _, m := range registered {
		want = append(want, m[1])
	}
	got := manifest.MCP.Tools
	sort.Strings(want)
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("manifest MCP tools drifted from the server's registrations:\n manifest: %v\n   server: %v", got, want)
	}
}

func readManifest(t *testing.T, into any) {
	t.Helper()
	raw, err := os.ReadFile(".printing-press.json")
	if err != nil {
		t.Fatalf("read .printing-press.json: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("parse .printing-press.json: %v", err)
	}
}

func TestHoistGlobalConfig(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		wantSub  string
		wantArgs []string
		wantOK   bool
	}{
		{"leading --config space", []string{"--config", "c.json", "triage", "f.txt"}, "triage", []string{"--config", "c.json", "f.txt"}, true},
		{"leading --config equals", []string{"--config=c.json", "classify", "x"}, "classify", []string{"--config", "c.json", "x"}, true},
		{"leading -config single dash", []string{"-config", "c.json", "models"}, "models", []string{"--config", "c.json"}, true},
		{"trailing --config untouched", []string{"triage", "f.txt", "--config", "c.json"}, "triage", []string{"f.txt", "--config", "c.json"}, true},
		{"no global config", []string{"summarize", "f.txt", "--json"}, "summarize", []string{"f.txt", "--json"}, true},
		{"config but no subcommand", []string{"--config", "c.json"}, "", nil, false},
		{"empty", []string{}, "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, args, ok := hoistGlobalConfig(tc.in)
			if ok != tc.wantOK || sub != tc.wantSub || !reflect.DeepEqual(args, tc.wantArgs) {
				t.Fatalf("hoistGlobalConfig(%v) = (%q, %v, %v); want (%q, %v, %v)",
					tc.in, sub, args, ok, tc.wantSub, tc.wantArgs, tc.wantOK)
			}
		})
	}
}

// TestDiagnosticsDoNotGateVerdict is the load-bearing proof that AUGRC and ECE
// never influence the ADOPT/REJECT verdict. The verdict depends solely on
// confheadVerdict(lo), which reads only the CI lower bound.
//
// Fixture A: poor AUGRC/ECE (miscalibrated head, reversed ranking) BUT lo > 0
// → verdict must be ADOPT.
// Fixture B: perfect AUGRC/ECE (ideal calibration) BUT lo <= 0
// → verdict must NOT be ADOPT.
func TestDiagnosticsDoNotGateVerdict(t *testing.T) {
	// Fixture A: ci_lo > 0 → ADOPT regardless of bad AUGRC/ECE.
	// We construct OOF points where the head is miscalibrated (always predicts
	// 0.9 but only 50% correct — ECE ≈ 0.4, AUGRC is high) yet we directly
	// exercise confheadVerdict with lo > 0 to prove the path.
	miscalibrated := make([]eval.RCPoint, 20)
	for i := range miscalibrated {
		miscalibrated[i] = eval.RCPoint{Confidence: 0.9, Correct: i < 10}
	}
	augrcPoor := eval.AUGRC(miscalibrated)
	ecePoor := eval.ECE(miscalibrated, 10)
	if augrcPoor <= 0 || ecePoor < 0.3 {
		t.Fatalf("fixture A should have non-trivial AUGRC=%v ECE=%v (miscalibrated)", augrcPoor, ecePoor)
	}
	// Despite poor diagnostics, ci_lo > 0 → ADOPT.
	verdictA := confheadVerdict(0.01) // lo > 0
	if verdictA != "ADOPT" {
		t.Fatalf("fixture A: ci_lo>0 must give ADOPT regardless of AUGRC/ECE, got %q", verdictA)
	}

	// Fixture B: lo <= 0 → REJECT regardless of good AUGRC/ECE.
	// Perfect-ranking head: all correct rows ranked highest → low AUGRC/ECE.
	// Use 20 points so AUGRC is genuinely low.
	perfect := make([]eval.RCPoint, 20)
	for i := range perfect {
		// First 10: correct with high confidence; last 10: wrong with low confidence.
		if i < 10 {
			perfect[i] = eval.RCPoint{Confidence: 0.9, Correct: true}
		} else {
			perfect[i] = eval.RCPoint{Confidence: 0.1, Correct: false}
		}
	}
	augrcGood := eval.AUGRC(perfect)
	eceGood := eval.ECE(perfect, 10)
	// ECE: bin[0] (conf=0.1): 10 pts, mean_pred=0.1, mean_correct=0.0 → |diff|=0.1, weight=0.5 → 0.05
	// bin[8] (conf=0.9): 10 pts, mean_pred=0.9, mean_correct=1.0 → |diff|=0.1, weight=0.5 → 0.05
	// ECE = 0.10. AUGRC for perfect ranking with 50% base error is non-trivially low.
	// We just verify it's strictly below the miscalibrated fixture A (ecePoor ≈ 0.4).
	if eceGood >= ecePoor {
		t.Fatalf("fixture B ECE=%v should be lower than fixture A ECE=%v (better calibrated)", eceGood, ecePoor)
	}
	if augrcGood >= augrcPoor {
		t.Fatalf("fixture B AUGRC=%v should be lower than fixture A AUGRC=%v (better ranking)", augrcGood, augrcPoor)
	}
	// Despite perfect diagnostics, ci_lo <= 0 → not ADOPT.
	verdictB := confheadVerdict(0.0) // lo == 0, boundary: not > 0
	if verdictB == "ADOPT" {
		t.Fatalf("fixture B: ci_lo=0 must not give ADOPT; got %q", verdictB)
	}
	verdictBneg := confheadVerdict(-0.01) // lo < 0
	if verdictBneg == "ADOPT" {
		t.Fatalf("fixture B: ci_lo<0 must not give ADOPT; got %q", verdictBneg)
	}
}

// TestBuildAudioParams pins the generate-audio CLI param-building: kind defaults
// to voice, optional flags (clone/lang/seconds/seed/reserve_vram) are only set
// when non-zero, and the output path is carried through. Mirrors the inline
// param-building of runGenerateImage but factored out so the arg handling is
// unit-testable without a live render.
func TestBuildAudioParams(t *testing.T) {
	cases := []struct {
		name string
		in   audioFlags
		want map[string]any
	}{
		{
			"defaults to voice, no optional flags",
			audioFlags{kind: "voice"},
			map[string]any{"kind": "voice"},
		},
		{
			"empty kind still emits voice (CLI default)",
			audioFlags{kind: ""},
			map[string]any{"kind": "voice"},
		},
		{
			"voice with clone + lang + out",
			audioFlags{kind: "voice", clone: "ref.wav", lang: "es", out: "v.wav"},
			map[string]any{"kind": "voice", "clone": "ref.wav", "lang": "es", "out": "v.wav"},
		},
		{
			"music with seconds + seed + reserve_vram",
			audioFlags{kind: "music", seconds: 12, seed: 42, reserveVRAM: 1.5},
			map[string]any{"kind": "music", "seconds": 12, "seed": 42, "reserve_vram": "1.5"},
		},
		{
			"zero/empty optionals are omitted",
			audioFlags{kind: "voice", clone: "", lang: "", seconds: 0, seed: 0, reserveVRAM: 0, out: ""},
			map[string]any{"kind": "voice"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildAudioParams(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("buildAudioParams(%+v) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildVideoParams pins the generate-video CLI param-building: an EMPTY model
// is OMITTED so the pipeline applies the machine's videogen_family seat (the old
// forced-wan default silently defeated the family binding on every CLI call —
// live finding 2026-08-15); the still path is carried as "still"; optional flags
// (negative/frames/width/height/steps/seed/reserve_vram) are only set when
// non-zero. reserve_vram is stringified to match the MCP tool's wire shape.
// Mirrors buildAudioParams so the arg handling is unit-testable without a render.
func TestBuildVideoParams(t *testing.T) {
	cases := []struct {
		name string
		in   videoFlags
		want map[string]any
	}{
		{
			"defaults to hunyuan, still carried, no optional flags",
			videoFlags{model: "hunyuan", still: "still.png"},
			map[string]any{"model": "hunyuan", "still": "still.png"},
		},
		{
			"empty model is OMITTED — the pipeline applies the machine's videogen_family",
			videoFlags{model: "", still: "s.png"},
			map[string]any{"still": "s.png"},
		},
		{
			"frames + seed + reserve_vram + out + negative",
			videoFlags{model: "hunyuan", still: "s.png", out: "v.mp4", negative: "blurry", frames: 49, seed: 42, reserveVRAM: 2.0},
			map[string]any{"model": "hunyuan", "still": "s.png", "out": "v.mp4", "negative": "blurry", "frames": 49, "seed": 42, "reserve_vram": "2"},
		},
		{
			"zero/empty optionals are omitted",
			videoFlags{model: "hunyuan", still: "s.png", negative: "", frames: 0, width: 0, height: 0, steps: 0, seed: 0, reserveVRAM: 0, out: ""},
			map[string]any{"model": "hunyuan", "still": "s.png"},
		},
		{
			"wan model with width/height/steps",
			videoFlags{model: "wan", still: "s.png", width: 832, height: 480, steps: 30},
			map[string]any{"model": "wan", "still": "s.png", "width": 832, "height": 480, "steps": 30},
		},
		{
			"hero + upscale emit their bool params (only when set)",
			videoFlags{model: "wan", still: "s.png", hero: true, upscale: true},
			map[string]any{"model": "wan", "still": "s.png", "hero": true, "upscale": true},
		},
		{
			"fast emits the MCP tool's fast param (the distilled Wan recipe)",
			videoFlags{still: "s.png", fast: true},
			map[string]any{"still": "s.png", "fast": true},
		},
		{
			"transformer (gap 5: per-request LTX-2.5 transformer override) is carried when set, omitted when empty",
			videoFlags{model: "ltx25", still: "s.png", transformer: "ltx-2.5-22b-distilled-transformer-bf16.safetensors"},
			map[string]any{"model": "ltx25", "still": "s.png", "transformer": "ltx-2.5-22b-distilled-transformer-bf16.safetensors"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildVideoParams(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("buildVideoParams(%+v) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseGenerateVideoTransformer: --transformer is a value flag (never eats a
// positional) and lands in the request params exactly as the MCP tool's transformer
// (gap 5, bigger-models-2026-09-24.md: an opt-in bf16 LTX transformer for one hero
// render, without a config edit).
func TestParseGenerateVideoTransformer(t *testing.T) {
	c, err := parseGenerateVideo([]string{"o.mp4", "s.png", "push in", "--transformer", "ltx-2.5-22b-distilled-transformer-bf16.safetensors", "--model", "ltx25"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if c.video.transformer != "ltx-2.5-22b-distilled-transformer-bf16.safetensors" || c.video.out != "o.mp4" || c.video.still != "s.png" || c.prompt != "push in" {
		t.Fatalf("parseGenerateVideo = %+v prompt %q; want transformer with out/still/prompt intact", c.video, c.prompt)
	}
	if got := buildVideoParams(c.video)["transformer"]; got != "ltx-2.5-22b-distilled-transformer-bf16.safetensors" {
		t.Fatalf("params must carry transformer, got %v", got)
	}
	if !generateVideoValueFlags["transformer"] {
		t.Fatal("transformer must be a value flag, or splitThreeArgs would swallow its argument as a positional")
	}
	c2, err := parseGenerateVideo([]string{"o.mp4", "s.png", "p"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := buildVideoParams(c2.video)["transformer"]; ok {
		t.Fatal("no --transformer must mean no transformer param: this box's bound file (or the render script's default) applies")
	}
}

// TestParseGenerateVideoFast: --fast is a real CLI flag (the fast Wan recipe was
// reachable only through the MCP tool), it is a BOOL — wherever it sits it never eats
// a positional — and it lands in the request params exactly as the MCP tool's fast.
func TestParseGenerateVideoFast(t *testing.T) {
	for _, argv := range [][]string{
		{"o.mp4", "s.png", "push in", "--fast"},
		{"o.mp4", "--fast", "s.png", "push in", "--seed", "7"},
		{"--fast", "o.mp4", "s.png", "push in"},
	} {
		c, err := parseGenerateVideo(argv, flag.ContinueOnError)
		if err != nil {
			t.Fatalf("parseGenerateVideo(%v): %v", argv, err)
		}
		if !c.video.fast || c.video.out != "o.mp4" || c.video.still != "s.png" || c.prompt != "push in" {
			t.Fatalf("parseGenerateVideo(%v) = %+v prompt %q; want fast with out/still/prompt intact", argv, c.video, c.prompt)
		}
		if got := buildVideoParams(c.video); got["fast"] != true {
			t.Fatalf("params %v must carry fast=true", got)
		}
	}
	c, err := parseGenerateVideo([]string{"o.mp4", "s.png", "p"}, flag.ContinueOnError)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := buildVideoParams(c.video)["fast"]; ok {
		t.Fatal("no --fast must mean no fast param: the native recipe is the default")
	}
	if generateVideoValueFlags["fast"] {
		t.Fatal("fast must not be a value flag: splitThreeArgs would swallow the next positional")
	}
}

// TestSplitThreeArgs pins the three-positional split used by generate-video:
// <out> <still> "<prompt>" with the rest as flags. Value-flags consume the
// following token; a fourth+ positional is dropped onto flags (harmless).
func TestSplitThreeArgs(t *testing.T) {
	valueFlags := map[string]bool{
		"config": true, "model": true, "negative": true, "frames": true,
		"width": true, "height": true, "steps": true, "seed": true, "reserve-vram": true,
	}
	cases := []struct {
		name                string
		in                  []string
		wantA, wantB, wantC string
		wantFlags           []string
	}{
		{
			"three positionals then flags",
			[]string{"out.mp4", "still.png", "push in", "--model", "hunyuan", "--frames", "49"},
			"out.mp4", "still.png", "push in",
			[]string{"--model", "hunyuan", "--frames", "49"},
		},
		{
			"flags interleaved",
			[]string{"out.mp4", "--frames", "49", "still.png", "push in", "--seed", "7"},
			"out.mp4", "still.png", "push in",
			[]string{"--frames", "49", "--seed", "7"},
		},
		{
			"reserve-vram value consumed",
			[]string{"o.mp4", "s.png", "p", "--reserve-vram", "2.0"},
			"o.mp4", "s.png", "p",
			[]string{"--reserve-vram", "2.0"},
		},
		{
			"equals-form flag not consuming next",
			[]string{"o.mp4", "s.png", "p", "--model=wan"},
			"o.mp4", "s.png", "p",
			[]string{"--model=wan"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b, c, flags := splitThreeArgs(tc.in, valueFlags)
			if a != tc.wantA || b != tc.wantB || c != tc.wantC || !reflect.DeepEqual(flags, tc.wantFlags) {
				t.Fatalf("splitThreeArgs(%v) = (%q,%q,%q,%v); want (%q,%q,%q,%v)",
					tc.in, a, b, c, flags, tc.wantA, tc.wantB, tc.wantC, tc.wantFlags)
			}
		})
	}
}

// TestWarnOnDefaults pins the loud-fallback contract (live incident 2026-07-20: a box
// whose config lived at a non-conventional path served every bare CLI call from BUILT-IN
// DEFAULTS — empty VisionModel and all — and the only symptom was a misleading "no vision
// model configured" defer hours later). All three defaults shapes must warn: nothing
// resolved, an explicit path that does not exist, and a file that fails to load. A
// genuinely loaded file must stay silent.
func TestWarnOnDefaults(t *testing.T) {
	var buf bytes.Buffer
	if !config.WarnOnDefaults(config.Source{}, &buf) {
		t.Fatal("nothing resolved = built-in defaults; must warn")
	}
	for _, want := range []string{"BUILT-IN DEFAULTS", "--config", ".local-offload"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("warning must mention %q; got %q", want, buf.String())
		}
	}
	buf.Reset()
	if !config.WarnOnDefaults(config.Source{Path: "C:/typo.json", NotFound: true}, &buf) {
		t.Fatal("an explicit path that does not exist runs on defaults; must warn")
	}
	if !strings.Contains(buf.String(), "NOT FOUND") || !strings.Contains(buf.String(), "C:/typo.json") {
		t.Fatalf("not-found warning must name the path; got %q", buf.String())
	}
	buf.Reset()
	if !config.WarnOnDefaults(config.Source{Path: "C:/bad.json", LoadErr: os.ErrInvalid}, &buf) {
		t.Fatal("a file that failed to load runs on defaults; must warn")
	}
	buf.Reset()
	if config.WarnOnDefaults(config.Source{Path: "C:/real.json"}, &buf) {
		t.Fatal("a genuinely loaded file must NOT warn")
	}
	if buf.Len() != 0 {
		t.Fatalf("no output expected for a loaded file; got %q", buf.String())
	}
}

// TestConfigSourceLine pins doctor's config-source disclosure: it must name the file ONLY
// when that file was actually read — a resolved-but-unread path must disclose defaults
// (crediting an unread file is worse than no disclosure).
func TestConfigSourceLine(t *testing.T) {
	if got := config.SourceLine(config.Source{Path: "C:/x/config.json"}); !strings.Contains(got, "C:/x/config.json") {
		t.Fatalf("loaded file: source line must name it; got %q", got)
	}
	if got := config.SourceLine(config.Source{}); !strings.Contains(got, "BUILT-IN DEFAULTS") {
		t.Fatalf("nothing resolved: must state built-in defaults; got %q", got)
	}
	got := config.SourceLine(config.Source{Path: "C:/typo.json", NotFound: true})
	if !strings.Contains(got, "BUILT-IN DEFAULTS") || !strings.Contains(got, "C:/typo.json") {
		t.Fatalf("not-found: must state defaults AND name the bad path; got %q", got)
	}
	// AMENDED (review of #361): this assertion pinned a FALSE disclosure. A file
	// that fails validation is still the file this process runs on — Load returns
	// its settings with only the five composite keys stripped — so "BUILT-IN
	// DEFAULTS" sent an operator hunting a path problem while the real one was a
	// named key in the file they already had open. The line must name the file and
	// carry the validation error verbatim, because that error text is the only
	// thing that names the offending key.
	got = config.SourceLine(config.Source{Path: "C:/bad.json", LoadErr: os.ErrInvalid})
	if strings.Contains(got, "BUILT-IN DEFAULTS") {
		t.Fatalf("load-failed: the settings came from the FILE, not from defaults; got %q", got)
	}
	if !strings.Contains(got, "C:/bad.json") || !strings.Contains(got, os.ErrInvalid.Error()) {
		t.Fatalf("load-failed: must name the file and the validation error; got %q", got)
	}
}

// TestResolveCfgPath pins the config-path precedence: explicit --config flag >
// $LOCAL_OFFLOAD_CONFIG env > ./config.json when it exists (LO-4: the README
// quickstart's `cp config.example.json config.json`) > the conventional
// ~/.local-offload/config.json when it exists > "" (built-in defaults). The
// home-dir rule is the fix for a bare `local-offload mcp` (no flag, no env)
// silently running on defaults — which left shadow capture off and the
// flywheel starved.
func TestResolveCfgPath(t *testing.T) {
	def := filepath.Join("/home/u", ".local-offload", "config.json")
	always := func(string) bool { return true }
	never := func(string) bool { return false }
	onlyDefault := func(p string) bool { return p == def }
	onlyCwd := func(p string) bool { return p == "config.json" }
	cases := []struct {
		name, flagPath, envPath, home string
		exists                        func(string) bool
		want                          string
	}{
		{"flag wins over env and default", "c.json", "e.json", "/home/u", always, "c.json"},
		{"env when no flag", "", "e.json", "/home/u", always, "e.json"},
		{"cwd config.json beats the home default", "", "", "/home/u", always, "config.json"},
		{"cwd config.json when only it exists", "", "", "/home/u", onlyCwd, "config.json"},
		{"home default when no cwd config.json", "", "", "/home/u", onlyDefault, def},
		{"empty when nothing exists", "", "", "/home/u", never, ""},
		{"cwd config.json even when home unknown", "", "", "", always, "config.json"},
		{"empty when home unknown and no cwd file", "", "", "", never, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCfgPath(tc.flagPath, tc.envPath, tc.home, tc.exists)
			if got != tc.want {
				t.Fatalf("resolveCfgPath(%q,%q,%q) = %q; want %q",
					tc.flagPath, tc.envPath, tc.home, got, tc.want)
			}
		})
	}
}

func TestRunGraphArgsRequireGraph(t *testing.T) {
	_, _, err := runGraphParams([]string{}) // no --graph/--graph-json
	if err == nil {
		t.Fatal("expected error when neither --graph nor --graph-json is given")
	}
}

// ---- CT-52: a bare "--" ends the flags --------------------------------------------------
//
// The four positional splitters (splitArgs, splitTwoArgs, splitThreeArgs, splitFourArgs) sent every token
// that starts with "-" to the flags, so a positional that begins with a dash (a lyrics section marker such
// as "--- Intro ---", a prompt that opens with "-") was lost: `generate-audio out.wav "--- Intro ---"`
// left the text empty. A bare "--" now ends flag handling in all four: every later token is a positional,
// in order, even when it starts with "-". The user's "--" is consumed by the helper; when a positional
// arrives that the helper has no slot for, ONE helper-written "--" goes in front of it in the returned
// flags, so the caller's fs.Parse stops there and cannot read a leading dash as a flag.

// splitAdapters runs each splitter through one shape: the positionals it found (always arity long) and its flags.
var splitAdapters = []struct {
	name  string
	arity int
	run   func(args []string, valueFlags map[string]bool) ([]string, []string)
}{
	{"splitArgs", 1, func(a []string, v map[string]bool) ([]string, []string) {
		p, f := splitArgs(a, v)
		return []string{p}, f
	}},
	{"splitTwoArgs", 2, func(a []string, v map[string]bool) ([]string, []string) {
		x, y, f := splitTwoArgs(a, v)
		return []string{x, y}, f
	}},
	{"splitThreeArgs", 3, func(a []string, v map[string]bool) ([]string, []string) {
		x, y, z, f := splitThreeArgs(a, v)
		return []string{x, y, z}, f
	}},
	{"splitFourArgs", 4, func(a []string, v map[string]bool) ([]string, []string) {
		w, x, y, z, f := splitFourArgs(a, v)
		return []string{w, x, y, z}, f
	}},
}

var splitTestValueFlags = map[string]bool{"voice": true, "config": true, "seed": true}

// positionalsFor is n distinct positionals, the LAST one starting with dashes.
func positionalsFor(n int) []string {
	out := make([]string, 0, n)
	for i := 1; i < n; i++ {
		out = append(out, "p"+string(rune('0'+i)))
	}
	return append(out, "--- Intro ---")
}

// TestSplitTwoArgsBareDashDashEndsFlags is the register's case: `generate-audio out.wav -- "--- Intro ---"`
// returns both positionals and no flags. Before the fix the second positional came back empty.
func TestSplitTwoArgsBareDashDashEndsFlags(t *testing.T) {
	first, second, flags := splitTwoArgs([]string{"out.wav", "--", "--- Intro ---"}, splitTestValueFlags)
	if first != "out.wav" || second != "--- Intro ---" || len(flags) != 0 {
		t.Fatalf("splitTwoArgs(out.wav -- \"--- Intro ---\") = (%q, %q, %v); want (out.wav, --- Intro ---, no flags)", first, second, flags)
	}
	// the terminator may come first, and a flag before it still works
	first, second, flags = splitTwoArgs([]string{"--voice", "finetuned", "--", "out.wav", "-starts-with-a-dash"}, splitTestValueFlags)
	if first != "out.wav" || second != "-starts-with-a-dash" || !reflect.DeepEqual(flags, []string{"--voice", "finetuned"}) {
		t.Fatalf("got (%q, %q, %v); want (out.wav, -starts-with-a-dash, [--voice finetuned])", first, second, flags)
	}
}

func TestSplitThreeArgsBareDashDashEndsFlags(t *testing.T) {
	first, second, third, flags := splitThreeArgs([]string{"out.mp4", "still.png", "--", "- a prompt that opens with a dash"}, splitTestValueFlags)
	if first != "out.mp4" || second != "still.png" || third != "- a prompt that opens with a dash" || len(flags) != 0 {
		t.Fatalf("splitThreeArgs = (%q, %q, %q, %v); want the dash-led prompt as the third positional and no flags", first, second, third, flags)
	}
}

func TestSplitFourArgsBareDashDashEndsFlags(t *testing.T) {
	first, second, third, fourth, flags := splitFourArgs([]string{"--seed", "7", "out.mp4", "ref.png", "driver.mp4", "--", "--- the prompt ---"}, splitTestValueFlags)
	if first != "out.mp4" || second != "ref.png" || third != "driver.mp4" || fourth != "--- the prompt ---" || !reflect.DeepEqual(flags, []string{"--seed", "7"}) {
		t.Fatalf("splitFourArgs = (%q, %q, %q, %q, %v); want the dash-led prompt as the fourth positional and [--seed 7] as the flags", first, second, third, fourth, flags)
	}
}

func TestSplitArgsBareDashDashEndsFlags(t *testing.T) {
	pos, flags := splitArgs([]string{"--config", "c.json", "--", "--not-a-flag"}, splitTestValueFlags)
	if pos != "--not-a-flag" || !reflect.DeepEqual(flags, []string{"--config", "c.json"}) {
		t.Fatalf("splitArgs = (%q, %v); want the dash-led token as the positional and [--config c.json] as the flags", pos, flags)
	}
}

// TestSplitEveryHelperEndsFlagsAtTheTerminator runs the same property through all four splitters at their own arity.
func TestSplitEveryHelperEndsFlagsAtTheTerminator(t *testing.T) {
	for _, ad := range splitAdapters {
		t.Run(ad.name, func(t *testing.T) {
			want := positionalsFor(ad.arity)
			got, flags := ad.run(append([]string{"--"}, want...), splitTestValueFlags)
			if !reflect.DeepEqual(got, want) || len(flags) != 0 {
				t.Fatalf("%s(-- %v) = (%v, %v); want (%v, no flags)", ad.name, want, got, flags, want)
			}
		})
	}
}

// TestSplitArgsKeepsALoneDashAsThePositional: a lone "-" is the stdin positional, before and after the terminator.
func TestSplitArgsKeepsALoneDashAsThePositional(t *testing.T) {
	for _, in := range [][]string{{"-"}, {"--", "-"}, {"--json", "-"}} {
		pos, _ := splitArgs(in, splitTestValueFlags)
		if pos != "-" {
			t.Errorf("splitArgs(%v) positional = %q; want the lone dash", in, pos)
		}
	}
}

// TestSplitArgsFlagsBeforeTheTerminatorStillWork: everything before "--" is parsed as it always was.
func TestSplitArgsFlagsBeforeTheTerminatorStillWork(t *testing.T) {
	first, second, flags := splitTwoArgs([]string{"--voice", "x", "out.wav", "--", "-t"}, splitTestValueFlags)
	if first != "out.wav" || second != "-t" || !reflect.DeepEqual(flags, []string{"--voice", "x"}) {
		t.Fatalf("splitTwoArgs = (%q, %q, %v); want (out.wav, -t, [--voice x])", first, second, flags)
	}
}

// TestSplitTwoArgsOverArityAfterTheTerminatorIsHandedToParseBehindOneDashDash: a positional past the arity still
// goes to the flags (as it always did), but once the terminator was seen the helper puts ONE "--" of its own in
// front of the first such token, so fs.Parse stops there and "--voice" after the terminator is never a flag.
func TestSplitTwoArgsOverArityAfterTheTerminatorIsHandedToParseBehindOneDashDash(t *testing.T) {
	first, second, flags := splitTwoArgs([]string{"out.wav", "--", "A", "--voice", "x"}, splitTestValueFlags)
	if first != "out.wav" || second != "A" {
		t.Fatalf("positionals = (%q, %q); want (out.wav, A)", first, second)
	}
	if want := []string{"--", "--voice", "x"}; !reflect.DeepEqual(flags, want) {
		t.Fatalf("flags = %v; want exactly %v (one helper-written --, then the overflow in order)", flags, want)
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	voice := fs.String("voice", "", "")
	if err := fs.Parse(flags); err != nil || *voice != "" {
		t.Fatalf("fs.Parse(%v) set --voice=%q (err %v); a token after the terminator must never be read as a flag", flags, *voice, err)
	}
	// with no overflow the flags stay empty: the helper writes no -- of its own
	if _, _, flags := splitTwoArgs([]string{"out.wav", "--", "A"}, splitTestValueFlags); len(flags) != 0 {
		t.Fatalf("flags = %v; want none when nothing overflowed", flags)
	}
	// an overflow BEFORE the terminator is unchanged, and the one-"--" rule holds across several overflow tokens
	_, _, flags = splitTwoArgs([]string{"a", "b", "c", "--", "-d", "-e"}, splitTestValueFlags)
	if want := []string{"c", "--", "-d", "-e"}; !reflect.DeepEqual(flags, want) {
		t.Fatalf("flags = %v; want %v", flags, want)
	}
}

// TestSplitHelpersOverArityFenceTheirOverflow runs the over-arity rule through all four splitters.
func TestSplitHelpersOverArityFenceTheirOverflow(t *testing.T) {
	for _, ad := range splitAdapters {
		t.Run(ad.name, func(t *testing.T) {
			in := append([]string{"--"}, positionalsFor(ad.arity)...)
			in = append(in, "--voice", "x")
			got, flags := ad.run(in, splitTestValueFlags)
			if want := positionalsFor(ad.arity); !reflect.DeepEqual(got, want) {
				t.Fatalf("positionals = %v; want %v", got, want)
			}
			if want := []string{"--", "--voice", "x"}; !reflect.DeepEqual(flags, want) {
				t.Fatalf("flags = %v; want %v", flags, want)
			}
		})
	}
}

// TestSplitHelpersLeaveAnOverflowAloneWithoutATerminator: with no "--" in the arguments the helpers return
// exactly what they returned before (a positional past the arity goes to the flags with no fence), so no
// existing call changes meaning.
func TestSplitHelpersLeaveAnOverflowAloneWithoutATerminator(t *testing.T) {
	_, _, flags := splitTwoArgs([]string{"a", "b", "c", "--voice", "x"}, splitTestValueFlags)
	if want := []string{"c", "--voice", "x"}; !reflect.DeepEqual(flags, want) {
		t.Fatalf("flags = %v; want %v", flags, want)
	}
}

// TestSplitValueFlagStillTakesAFollowingDashDashAsItsValue: "--" directly after a value flag is that flag's value
// (the flag package's own rule), not the terminator.
func TestSplitValueFlagStillTakesAFollowingDashDashAsItsValue(t *testing.T) {
	first, second, flags := splitTwoArgs([]string{"--voice", "--", "out.wav", "text"}, splitTestValueFlags)
	if first != "out.wav" || second != "text" || !reflect.DeepEqual(flags, []string{"--voice", "--"}) {
		t.Fatalf("splitTwoArgs = (%q, %q, %v); want (out.wav, text, [--voice --])", first, second, flags)
	}
}

// TestParseGenerateAudioCarriesADashLedTextToTheRequest drives the generate-audio verb's own parse (the caller of
// splitTwoArgs): the lyrics marker after `--` reaches the request text instead of leaving it empty.
func TestParseGenerateAudioCarriesADashLedTextToTheRequest(t *testing.T) {
	c, err := parseGenerateAudio([]string{"out.wav", "--kind", "music", "--", "--- Intro ---\nla la la"}, flag.ContinueOnError)
	if err != nil {
		t.Fatalf("parseGenerateAudio: %v (the text was lost to the flag handling)", err)
	}
	if c.text != "--- Intro ---\nla la la" || c.audio.out != "out.wav" || c.audio.kind != "music" {
		t.Fatalf("parseGenerateAudio = text %q out %q kind %q; want the dash-led text, out.wav and music", c.text, c.audio.out, c.audio.kind)
	}
	// the ordinary shape is unchanged
	c, err = parseGenerateAudio([]string{"out.wav", "hola mundo", "--lang", "es"}, flag.ContinueOnError)
	if err != nil || c.text != "hola mundo" || c.audio.lang != "es" {
		t.Fatalf("parseGenerateAudio(ordinary) = text %q lang %q err %v; want hola mundo / es", c.text, c.audio.lang, err)
	}
	// a token past the two positionals is never read as a flag once the terminator was typed
	c, err = parseGenerateAudio([]string{"out.wav", "--", "text", "--voice", "finetuned"}, flag.ContinueOnError)
	if err != nil || c.text != "text" || c.audio.voice != "" {
		t.Fatalf("parseGenerateAudio(over arity) = text %q voice %q err %v; want text with no voice flag set", c.text, c.audio.voice, err)
	}
}

// TestParseGenerateVideoCarriesADashLedPromptToTheRequest is the same through the generate-video verb (splitThreeArgs).
func TestParseGenerateVideoCarriesADashLedPromptToTheRequest(t *testing.T) {
	c, err := parseGenerateVideo([]string{"o.mp4", "s.png", "--fast", "--", "- push in slowly"}, flag.ContinueOnError)
	if err != nil {
		t.Fatalf("parseGenerateVideo: %v", err)
	}
	if c.prompt != "- push in slowly" || c.video.out != "o.mp4" || c.video.still != "s.png" || !c.video.fast {
		t.Fatalf("parseGenerateVideo = prompt %q out %q still %q fast %v; want the dash-led prompt intact and --fast still parsed", c.prompt, c.video.out, c.video.still, c.video.fast)
	}
}
