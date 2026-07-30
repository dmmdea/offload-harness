package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/hwdetect"
	"github.com/dmmdea/offload-harness/internal/mediaseat"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
)

// The serving templates are EMBEDDED so a fetched binary can render a config on a
// machine that has no checkout. That is the whole shape of a real install: fetch one
// binary, ask it what this machine is, let it write the serving config.
//
//go:embed setup/templates/llama-swap.*.yaml
var servingTemplates embed.FS

// The tier table is embedded for the same reason as the templates: an install
// begins by fetching ONE binary onto a machine with no checkout. Reading it from
// a --root path is the development case, not the install case — and when the
// lookup silently failed, install.sh produced a config with no media bindings at
// all and said nothing, which is the failure this whole workstream exists to end.
//
//go:embed setup/templates/profiles.json
var embeddedProfiles []byte

// profilesJSON returns the tier table: the checkout's copy when --root names one
// that has it, else the embedded copy. A --root that was given explicitly and does
// NOT have it is an error, never a silent fallback.
func profilesJSON(root string) ([]byte, error) {
	if root == "" {
		return embeddedProfiles, nil
	}
	b, err := os.ReadFile(filepath.Join(root, "setup", "templates", "profiles.json"))
	if err != nil {
		if os.IsNotExist(err) && root == "." {
			return embeddedProfiles, nil // default root, no checkout: use the built-in table
		}
		return nil, fmt.Errorf("reading the tier table from --root %q: %w", root, err)
	}
	return b, nil
}

// servingProfile is the slice of a profiles.json entry that decides how a tier
// SERVES. (The media half lives in internal/tierseed.)
type servingProfile struct {
	CtxSize    int    `json:"ctx_size"`
	KVType     string `json:"kv_type"`
	FlashAttn  string `json:"flash_attn"`
	Backend    string `json:"backend"`
	Include26B bool   `json:"include_26b"`
	MoE26B     string `json:"moe_26b"`
	// NCPUMoE is the N for the partial `n_cpu_moe` placement (top N expert layers in
	// RAM, the rest on the GPU).
	NCPUMoE int `json:"n_cpu_moe"`
	// MediaSeats are rendered into the models map and the group their residency
	// role maps to. The same declaration produces the harness config binding via
	// internal/tierseed, so the seat and the alias routing to it cannot disagree.
	MediaSeats []mediaseat.Seat `json:"media_seats"`
	// GPUEnv is added to every model in the rendered config.
	GPUEnv []string `json:"gpu_env"`
	// moeLiteral is set ONLY by fallbackProfile and bypasses moeFlag: the off-matrix
	// defaults are literal flag strings (`--cpu-moe -ngl 999`, with 999 — not the 99
	// a declared "gpu" placement renders), and they must stay byte-identical to what
	// install.ps1 emitted or the delegation silently changes an off-matrix install.
	moeLiteral string
}

// fallbackProfile is what an UNKNOWN or absent tier renders. install.ps1 carried this
// table ("an unknown/absent profile renders the backend's ORIGINAL baked defaults, so
// the config is always valid even off-matrix") and it has to live here now that the
// installer delegates — otherwise delegating would turn a working off-matrix install
// into a hard failure.
func fallbackProfile(backend string) (servingProfile, error) {
	switch backend {
	case "cuda", "cuda-resident", "dual-cuda":
		return servingProfile{CtxSize: 16384, KVType: "q8_0", FlashAttn: "on", Backend: backend,
			Include26B: true, moeLiteral: "--cpu-moe -ngl 999"}, nil
	case "vulkan":
		return servingProfile{CtxSize: 8192, KVType: "f16", FlashAttn: "on", Backend: backend,
			Include26B: true, moeLiteral: "-ngl 999"}, nil
	case "cpu":
		// The cpu template carries no MoE token, so this is inert in the output; it is
		// non-empty only because a tier that serves the 26B must name a placement.
		return servingProfile{CtxSize: 8192, KVType: "f16", FlashAttn: "off", Backend: backend,
			Include26B: true, moeLiteral: "--cpu-moe"}, nil
	}
	return servingProfile{}, fmt.Errorf("no fallback defaults for backend %q (have: cuda, cuda-resident, dual-cuda, vulkan, cpu)", backend)
}

// moePlacement resolves BOTH the 26B flag form and whether the tier serves it at all.
// Ported rule-for-rule from install.ps1's Resolve-ProfileParams, and asserted against
// it: rendering the two side by side is what caught this function returning a bare
// "--cpu-moe" where the installer emits "--cpu-moe -ngl 999" (the -ngl is what keeps
// the NON-expert layers on the GPU while every expert sits in RAM).
//
// ramTier gates the RAM-hungry placements. "cpu_moe" puts EVERY expert in RAM, so it
// needs a real RAM path and is dropped on low/min; "n_cpu_moe" pushes only the top N
// expert layers out and survives `low`, but not `min`. An EMPTY ramTier means the
// caller does not know, and the gate is skipped — which is what the Linux installer
// does today, and is recorded as a gap rather than silently changed here.
func moePlacement(p servingProfile, ramTier string) (flag string, include bool) {
	include = p.Include26B
	mode := p.MoE26B
	gated := ramTier != ""
	switch {
	case mode == "drop":
		include = false
	case gated && mode == "cpu_moe" && ramTier != "mid" && ramTier != "high":
		include = false
	case gated && mode == "n_cpu_moe" && ramTier == "min":
		include = false
	}
	if !include {
		return "", false
	}
	switch mode {
	case "gpu":
		return "-ngl 99", true
	case "cpu_moe":
		return "--cpu-moe -ngl 999", true
	case "n_cpu_moe":
		// A tier naming the partial form without an N is a defect; fall back to the
		// safe all-experts-in-RAM form rather than emitting a broken flag.
		if p.NCPUMoE > 0 {
			return fmt.Sprintf("--n-cpu-moe %d -ngl 999", p.NCPUMoE), true
		}
		return "--cpu-moe -ngl 999", true
	}
	return "", false
}

// templateFor picks the serving template for an OS + backend pair, and says exactly
// what exists when there is no match — a wrong template is worse than none.
func templateFor(goos, backend string) (string, error) {
	name := fmt.Sprintf("setup/templates/llama-swap.%s-%s.yaml", osTag(goos), backend)
	b, err := servingTemplates.ReadFile(name)
	if err == nil {
		return string(b), nil
	}
	entries, _ := servingTemplates.ReadDir("setup/templates")
	var have []string
	for _, e := range entries {
		have = append(have, strings.TrimSuffix(strings.TrimPrefix(e.Name(), "llama-swap."), ".yaml"))
	}
	sort.Strings(have)
	return "", fmt.Errorf("no serving template for %s/%s (have: %s)", osTag(goos), backend, strings.Join(have, ", "))
}

// warnMissingSeatModels names every declared seat weight that is not on disk.
//
// This exists because llama-swap's /v1/models roster is built from the CONFIG, not
// from the filesystem: a seat whose .gguf was never downloaded still appears in the
// roster, so `doctor`'s alias diff and `acceptance`'s alias check both pass and the
// route fails only when someone actually calls it. Install time is the one moment
// where the fix — download the file — is still cheap, so that is where it is said.
//
// A warning rather than an error: rendering the serving config before fetching
// weights is a legitimate order of operations, and refusing would break it. It is
// skipped when rendering for another machine, where a local miss means nothing.
func warnMissingSeatModels(seats []mediaseat.Seat, modelsDir, target string) {
	if len(seats) == 0 || modelsDir == "" || target != runtime.GOOS {
		return
	}
	var missing []string
	for _, s := range seats {
		for label, rel := range map[string]string{"model": s.Model, "mmproj": s.MMProj, "vad_model": s.VADModel} {
			if rel == "" {
				continue
			}
			full := filepath.Join(modelsDir, rel)
			if _, err := os.Stat(full); err != nil {
				missing = append(missing, fmt.Sprintf("  %s (%s %s): %s", s.Name, s.Kind, label, full))
			}
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	fmt.Fprintf(os.Stderr, "WARNING: %d declared seat weight(s) are not on this machine. llama-swap lists a seat "+
		"from the CONFIG, so the alias checks in `doctor` and `acceptance` will PASS and the route will fail only "+
		"when called. Fetch these before relying on them:\n%s\n", len(missing), strings.Join(missing, "\n"))
}

// seatsPlaceable reports whether the serving template for this target can host
// tier-declared media seats, so `install seed` can refuse exactly the pairs
// `install render` refuses instead of writing a binding the node cannot honour.
func seatsPlaceable(goos, backend string) error {
	target := goos
	if target == "" {
		target = runtime.GOOS
	}
	tmpl, err := templateFor(target, backend)
	if err != nil {
		return err
	}
	if !servingtmpl.SupportsSeats(tmpl) {
		return fmt.Errorf("this tier declares media seats, but the %s/%s serving template carries no "+
			"`# offload-seats:` directive, so they cannot be placed. Seeding their bindings here would write a "+
			"config naming aliases this node will never serve — refusing both halves instead", osTag(target), backend)
	}
	return nil
}

func osTag(goos string) string {
	if goos == "windows" {
		return "win"
	}
	return goos
}

func runInstallRender(args []string) error {
	fs := flag.NewFlagSet("install render", flag.ExitOnError)
	profileID := fs.String("profile", "", "tier id (default: classify this machine)")
	llamaBin := fs.String("llama-bin", "", "directory holding llama-server and its shared objects")
	modelsDir := fs.String("models", "", "directory holding the GGUF model files")
	listen := fs.String("listen", "127.0.0.1:11436", "llama-swap listen address")
	threads := fs.Int("threads", 0, "--threads per server (default: half the logical CPUs)")
	goos := fs.String("os", "", "target OS: windows|linux (default: this machine)")
	out := fs.String("out", "", "write the rendered config here instead of stdout")
	root := fs.String("root", ".", "repo root holding setup/templates/profiles.json")
	home := fs.String("home", "", "install root, for media seat paths (__OFFLOAD_HOME__)")
	fallback := fs.String("fallback-backend", "", "render off-matrix defaults for this backend when --profile is unknown or empty (cuda|cuda-resident|dual-cuda|vulkan|cpu)")
	ramTier := fs.String("ram-tier", "", "min|low|mid|high — gates the RAM-hungry 26B placements. Empty = do not gate (the caller does not know)")
	_ = fs.Parse(args)

	target := *goos
	if target == "" {
		target = runtime.GOOS
	}
	id := *profileID
	// Classifying THIS machine is the convenience default, but it must not happen when
	// the caller asked for off-matrix defaults: an empty --profile with
	// --fallback-backend means "this box is not on the matrix", and classifying anyway
	// silently rendered the RENDERING machine's tier instead (caught by comparing the
	// delegated output against the PowerShell renderer's).
	if id == "" && *fallback == "" {
		id = hwdetect.Classify(hwdetect.Detect()).Profile
	}

	raw, err := profilesJSON(*root)
	if err != nil {
		return err
	}
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("profiles.json: %w", err)
	}
	p, ok := doc.Profiles[id]
	if !ok {
		// Off-matrix is a supported outcome, not an error, WHEN the caller says which
		// backend to fall back to. install.ps1 has always rendered a valid config for
		// an unrecognized box; delegating must not take that away.
		if *fallback == "" {
			return fmt.Errorf("unknown tier %q (pass --fallback-backend to render off-matrix defaults instead)", id)
		}
		if p, err = fallbackProfile(*fallback); err != nil {
			return err
		}
		id = "(off-matrix: " + *fallback + " defaults)"
	}

	tmpl, err := templateFor(target, p.Backend)
	if err != nil {
		return err
	}

	n := *threads
	if n <= 0 {
		n = runtime.NumCPU() / 2
		if n < 1 {
			n = 1
		}
	}
	moe, include26B := moePlacement(p, strings.ToLower(strings.TrimSpace(*ramTier)))
	if p.moeLiteral != "" {
		moe, include26B = p.moeLiteral, true // off-matrix defaults are literal flags
	}
	rendered, err := servingtmpl.Render(tmpl, servingtmpl.Params{
		LlamaBin: *llamaBin, ModelsDir: *modelsDir, Listen: *listen,
		Ctx: p.CtxSize, KVType: p.KVType, FlashAttn: p.FlashAttn,
		MoE26B: moe, Threads: n, Include26B: include26B,
		Seats: p.MediaSeats, Home: *home, GOOS: target, GPUEnv: p.GPUEnv, Backend: p.Backend,
	})
	if err != nil {
		return fmt.Errorf("tier %s: %w", id, err)
	}

	warnMissingSeatModels(p.MediaSeats, *modelsDir, target)

	if *out == "" {
		fmt.Print(rendered)
		return nil
	}
	if err := os.WriteFile(*out, []byte(rendered), 0o644); err != nil {
		return err
	}
	// stdout, not stderr: this is a SUCCESS line, and a PowerShell caller with
	// $ErrorActionPreference='Stop' turns any stderr output from a native command into
	// a terminating error — which is exactly how the delegated installer first broke.
	// stdout is free here because the config only goes there when --out is empty.
	fmt.Printf("wrote %s (tier %s, %s/%s)\n", *out, id, osTag(target), p.Backend)
	return nil
}
