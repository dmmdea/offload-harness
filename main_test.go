package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

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

	src, err := os.ReadFile(filepath.Join("internal", "mcpserver", "mcpserver.go"))
	if err != nil {
		t.Fatalf("read mcpserver.go: %v", err)
	}
	registered := regexp.MustCompile(`(?m)^\s+Name:\s+"([a-z_]+)",`).FindAllStringSubmatch(string(src), -1)
	if len(registered) == 0 {
		t.Fatal("found no registered tool names in mcpserver.go — the scrape pattern needs updating")
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

// TestBuildVideoParams pins the generate-video CLI param-building: model defaults
// to wan, the still path is carried as "still", and optional flags
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
			"empty model emits wan (CLI default)",
			videoFlags{model: "", still: "s.png"},
			map[string]any{"model": "wan", "still": "s.png"},
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

// TestSplitThreeArgs pins the three-positional split used by generate-video:
// <out> <still> "<prompt>" with the rest as flags. Value-flags consume the
// following token; a fourth+ positional is dropped onto flags (harmless).
func TestSplitThreeArgs(t *testing.T) {
	valueFlags := map[string]bool{
		"config": true, "model": true, "negative": true, "frames": true,
		"width": true, "height": true, "steps": true, "seed": true, "reserve-vram": true,
	}
	cases := []struct {
		name                      string
		in                        []string
		wantA, wantB, wantC       string
		wantFlags                 []string
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
	_, err := runGraphParams([]string{}) // no --graph/--graph-json
	if err == nil {
		t.Fatal("expected error when neither --graph nor --graph-json is given")
	}
}
