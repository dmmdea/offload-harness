package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/hwdetect"
	"github.com/dmmdea/offload-harness/internal/mediaseat"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
	"github.com/dmmdea/offload-harness/internal/tierseed"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
	"gopkg.in/yaml.v3"
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
	// IncludeQwen38 gates the Qwen3.8-27B coder/agent entry (and, in install.ps1,
	// its GGUF+mmproj downloads) the same way Include26B gates the 26B. Absent =
	// false: only the tiers that measured (or project) the seat set it.
	IncludeQwen38 bool `json:"include_qwen38"`
	// IncludeQwen354B gates the Qwen3.5-4B agent entry (and, in install.ps1, its
	// GGUF download) the same way IncludeQwen38 gates the 27B. Absent = false.
	IncludeQwen354B bool `json:"include_qwen35_4b"`
	// IncludeQwen359B gates the Qwen3.5-9B agent entry (and, in install.ps1, its
	// GGUF download) the same way IncludeQwen354B gates the 4B. Absent = false.
	// Mutually exclusive with IncludeQwen354B (shared `agent-seat` alias) —
	// servingtmpl.Render refuses a tier that sets both.
	IncludeQwen359B bool `json:"include_qwen35_9b"`
	// IncludeQwen3827B gates the Qwen3.8-27B agent entry (UD-IQ3_S + the MTP head
	// embedded in the same GGUF) — the 16GB-class agent seat measured in ADR 0047.
	// Unlike the 4B/9B pair it does NOT claim the `agent-seat` alias, so it is not
	// mutually exclusive with them: the smaller entry stays rendered as the fallback
	// and the lane binds here through config_seed.agent_model.
	IncludeQwen3827B bool   `json:"include_qwen38_27b"`
	MoE26B           string `json:"moe_26b"`
	// NCPUMoE is the N for the partial `n_cpu_moe` placement (top N expert layers in
	// RAM, the rest on the GPU).
	NCPUMoE int `json:"n_cpu_moe"`
	// MediaSeats are rendered into the models map and the group their residency
	// role maps to. The same declaration produces the harness config binding via
	// internal/tierseed, so the seat and the alias routing to it cannot disagree.
	MediaSeats []mediaseat.Seat `json:"media_seats"`
	// GPUEnv is added to every model in the rendered config.
	GPUEnv []string `json:"gpu_env"`
	// Composes / Layers are the composite declaration (ADR 0039): the tiers
	// this one is a complete instance of, and the device layers it places work
	// on. Absent on every ordinary tier, where the render is unchanged.
	Composes []string           `json:"composes"`
	Layers   []config.LayerSpec `json:"layers"`
	// DisableCUDAGraphs keeps GGML_CUDA_DISABLE_GRAPHS=1 on the 26B seats for tiers
	// that have never been measured with graphs on. Absent = false = graphs ON, which
	// is the measured win (+40-51% generation on sm_120, output identical).
	DisableCUDAGraphs bool `json:"disable_cuda_graphs"`
	// VLLMSeat is the tier's persistent vLLM agent seat (ADR 0035). It renders only
	// when the box actually has the hand-built venv and the weights — see
	// vllmRuntimeFor — and otherwise the tier falls back to the llama.cpp seat the
	// spec names, with the reason printed.
	VLLMSeat *vllmseat.Spec `json:"vllm_seat,omitempty"`
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
		for label, rel := range map[string]string{"model": s.Model, "mmproj": s.MMProj, "vad_model": s.VADModel, "chat_template": s.ChatTemplate} {
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

// warnMissingGatedModels is warnMissingSeatModels' sibling for the flag-gated
// chat entries: a tier setting include_26b / include_qwen38 renders the entry
// into the roster whether or not its weights are on disk, and llama-swap lists
// models from the CONFIG — so the alias checks pass and the route fails only
// when called, exactly like a seat. The filenames are the templates' own cmd
// contract (the same names install.ps1's $PINNED table downloads to).
// Same shape as the seat warning: a warning, never an error, and skipped when
// rendering for another machine, where a local miss means nothing.
func warnMissingGatedModels(include26B, includeQ38, includeQ354B, includeQ359B, includeQ3827B bool, modelsDir, target string) {
	warnMissingGatedModelsTo(include26B, includeQ38, includeQ354B, includeQ359B, includeQ3827B, modelsDir, target, os.Stderr)
}

// warnMissingGatedModelsTo carries the body with an injectable sink so the warning
// is testable (it had no coverage at all — 0.72.0 review finding I-2). The wrapper
// above keeps every production call site unchanged.
func warnMissingGatedModelsTo(include26B, includeQ38, includeQ354B, includeQ359B, includeQ3827B bool, modelsDir, target string, w io.Writer) {
	if modelsDir == "" || target != runtime.GOOS {
		return
	}
	var missing []string
	check := func(entry, label, rel string) {
		full := filepath.Join(modelsDir, rel)
		if _, err := os.Stat(full); err != nil {
			missing = append(missing, fmt.Sprintf("  %s (%s): %s", entry, label, full))
		}
	}
	if include26B {
		check("gemma4-26b-a4b", "model", "gemma-4-26B-A4B-it-qat-UD-Q4_K_XL.gguf")
	}
	if includeQ38 {
		check("qwen3.8-27b", "model", "Qwen3.8-27B-UD-Q4_K_XL.gguf")
		check("qwen3.8-27b", "mmproj", "mmproj-Qwen3.8-27B-F16.gguf")
	}
	if includeQ354B {
		check("qwen3.5-4b-agent", "model", "Qwen3.5-4B-UD-Q4_K_XL.gguf")
	}
	if includeQ359B {
		check("qwen3.5-9b-agent", "model", "Qwen3.5-9B-UD-Q4_K_XL.gguf")
	}
	if includeQ3827B {
		check("qwen38-27b-agent", "model", "Qwen3.8-27B-UD-IQ3_S.gguf")
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	fmt.Fprintf(w, "WARNING: %d gated model weight(s) are not on this machine. llama-swap lists a model "+
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

// renderRequest is every input `install render` was given that is NOT itself a
// seed: the per-box install layout, the target, and the two flags that gate
// which seeds apply. The provenance stamp records it (K-02) so
// `audit-yaml --against-render` can REPLAY the render on another machine -- a
// binary cannot know another box's install paths or thread count, and
// re-deriving them from its OWN would report every node in the fleet stale.
type renderRequest struct {
	TierID    string // --profile; empty (with no Fallback) classifies this machine
	Fallback  string // --fallback-backend: non-empty means off-matrix
	RAMTier   string // --ram-tier, normalised
	GOOS      string // --os, resolved to a concrete target
	LlamaBin  string
	ModelsDir string
	Listen    string
	Home      string
	Threads   int
	VLLM      vllmRuntimeFlags
	// PinnedVLLM, when set, REPLACES the vLLM seat resolution instead of running
	// it. Only the replay sets it: vllmSeatFor inspects the LOCAL box (does this
	// machine have the hand-built venv and the snapshot?), so re-running the
	// detection while auditing another node's config would render the llama.cpp
	// fallback and report a false drift on every node that has the seat.
	PinnedVLLM *pinnedVLLM
}

type pinnedVLLM struct {
	Seat    *vllmseat.Spec
	Runtime vllmseat.Runtime
}

// renderResult is one resolved render: the config text plus the provenance basis
// that describes exactly what produced it.
type renderResult struct {
	TierID     string
	Profile    servingProfile
	Params     servingtmpl.Params
	Config     string // rendered, UNSTAMPED -- the bytes body_sha256 covers
	Basis      servingtmpl.SpecBasis
	Include26B bool
	// Layers / Composed are the composite declaration (ADR 0039) as the render
	// resolved it: the SEEDED layers the config was rendered from, and the
	// capabilities of the tiers the composite claims to be a complete instance
	// of. They are carried out of the derivation because the composition check
	// is a WRITE-time gate (`install render` refuses) and not part of deriving,
	// while the profile TABLE it needs is only in scope in here. Empty on every
	// ordinary tier.
	Layers   []config.LayerSpec
	Composed []servingtmpl.ComposedTier
}

// deriveRender resolves a tier into a rendered serving config and its
// provenance basis. It is the ONE derivation: `install render` calls it to
// write a config, and `audit-yaml --against-render` calls it to re-derive one,
// so the two can never disagree about what a tier renders to. (A second copy of
// this logic for the audit is exactly how a gate comes to certify the thing it
// was written to catch.)
func deriveRender(profilesRaw []byte, req renderRequest) (renderResult, error) {
	target := req.GOOS
	if target == "" {
		target = runtime.GOOS
	}
	id := req.TierID
	// Classifying THIS machine is the convenience default, but it must not happen
	// when the caller asked for off-matrix defaults: an empty --profile with
	// --fallback-backend means "this box is not on the matrix", and classifying
	// anyway silently rendered the RENDERING machine's tier instead (caught by
	// comparing the delegated output against the PowerShell renderer's).
	if id == "" && req.Fallback == "" {
		id = hwdetect.Classify(hwdetect.Detect()).Profile
	}

	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(profilesRaw, &doc); err != nil {
		return renderResult{}, fmt.Errorf("profiles.json: %w", err)
	}
	// The SAME bytes a second time, unparsed: the provenance stamp hashes the
	// tier's own entry, and hashing the re-marshalled struct would hash only the
	// fields this Go type happens to know -- a seed field added to the table and
	// not yet to servingProfile would then change the render's meaning without
	// changing its stamp.
	var rawDoc struct {
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(profilesRaw, &rawDoc); err != nil {
		return renderResult{}, fmt.Errorf("profiles.json: %w", err)
	}

	p, ok := doc.Profiles[id]
	entry := rawDoc.Profiles[id]
	if !ok {
		// Off-matrix is a supported outcome, not an error, WHEN the caller says
		// which backend to fall back to. install.ps1 has always rendered a valid
		// config for an unrecognized box; delegating must not take that away.
		if req.Fallback == "" {
			return renderResult{}, fmt.Errorf("unknown tier %q (pass --fallback-backend to render off-matrix defaults instead)", id)
		}
		var err error
		if p, err = fallbackProfile(req.Fallback); err != nil {
			return renderResult{}, err
		}
		id = "(off-matrix: " + req.Fallback + " defaults)"
		entry = nil // an off-matrix render has no tier entry; its hash stays empty
	}

	tmpl, err := templateFor(target, p.Backend)
	if err != nil {
		return renderResult{}, err
	}

	n := req.Threads
	if n <= 0 {
		n = runtime.NumCPU() / 2
		if n < 1 {
			n = 1
		}
	}
	ramTier := strings.ToLower(strings.TrimSpace(req.RAMTier))
	moe, include26B := moePlacement(p, ramTier)
	if p.moeLiteral != "" {
		moe, include26B = p.moeLiteral, true // off-matrix defaults are literal flags
	}
	var seat *vllmseat.Spec
	var seatRT vllmseat.Runtime
	if req.PinnedVLLM != nil {
		seat, seatRT = req.PinnedVLLM.Seat, req.PinnedVLLM.Runtime
	} else {
		seat, seatRT = vllmSeatFor(p, req.Home, req.VLLM)
	}
	// The layers as a box SEEDS them (tierseed fills the bare agent seat from
	// the vLLM seat), so the render and the check reason about one shape.
	layers := tierseed.FillPairAgent(p.Layers, p.VLLMSeat, seat != nil)
	params := servingtmpl.Params{
		LlamaBin: req.LlamaBin, ModelsDir: req.ModelsDir, Listen: req.Listen,
		Ctx: p.CtxSize, KVType: p.KVType, FlashAttn: p.FlashAttn,
		MoE26B: moe, Threads: n, Include26B: include26B, IncludeQ38: p.IncludeQwen38,
		IncludeQ354B: p.IncludeQwen354B, IncludeQ359B: p.IncludeQwen359B,
		IncludeQ3827B: p.IncludeQwen3827B,
		Seats:         p.MediaSeats, Home: req.Home, GOOS: target, GPUEnv: p.GPUEnv, Backend: p.Backend,
		DisableCUDAGraphs: p.DisableCUDAGraphs,
		VLLMSeat:          seat, VLLMRuntime: seatRT,
		DisplayLayer: displayLayerOf(layers),
	}
	rendered, err := servingtmpl.Render(tmpl, params)
	if err != nil {
		return renderResult{}, fmt.Errorf("tier %s: %w", id, err)
	}
	entrySHA, err := servingtmpl.CanonicalEntrySHA(entry)
	if err != nil {
		return renderResult{}, fmt.Errorf("tier %s: hashing its profiles.json entry: %w", id, err)
	}
	return renderResult{
		TierID: id, Profile: p, Params: params, Config: rendered, Include26B: include26B,
		Layers: layers, Composed: composedCapabilities(doc.Profiles, p.Composes),
		Basis: servingtmpl.SpecBasis{
			HarnessVersion:      buildinfo.Version,
			TierID:              id,
			TemplateSHA256:      servingtmpl.SHA256Hex([]byte(tmpl)),
			ProfilesEntrySHA256: entrySHA,
			Render:              servingtmpl.RenderBasis{RAMTier: ramTier, FallbackBackend: req.Fallback},
			Params:              servingtmpl.BasisOf(params),
		},
	}, nil
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
	// The vLLM seat's DEPLOYMENT half. A tier is a hardware class, so it cannot know
	// the account llama-swap runs as, the address the engine binds, or where this box
	// keeps its venv and HF cache. Absent user/proxy = render the fallback seat.
	vllmUser := fs.String("vllm-user", "", "account llama-swap runs as; the vLLM seat's polkit rule is scoped to it")
	vllmProxy := fs.String("vllm-proxy-host", "", "LITERAL address the vLLM engine binds (an IP: the reference box's MagicDNS name resolved to IPv6 only)")
	vllmVenv := fs.String("vllm-venv", "", "hand-built vLLM virtualenv (default: <home>/vllm-env)")
	vllmSeatDir := fs.String("vllm-seat-dir", "", "where the rendered unit and wrappers live (default: <home>/seat)")
	hfHome := fs.String("hf-home", "", "HF cache root; KEEP IT SHORT (LMCache page names embed the model path against NAME_MAX 255). Default: $HF_HOME, else <home>/hf")
	_ = fs.Parse(args)

	raw, err := profilesJSON(*root)
	if err != nil {
		return err
	}
	res, err := deriveRender(raw, renderRequest{
		TierID: *profileID, Fallback: *fallback, RAMTier: *ramTier, GOOS: *goos,
		LlamaBin: *llamaBin, ModelsDir: *modelsDir, Listen: *listen, Home: *home, Threads: *threads,
		VLLM: vllmRuntimeFlags{user: *vllmUser, proxyHost: *vllmProxy, venv: *vllmVenv, seatDir: *vllmSeatDir, hfHome: *hfHome},
	})
	if err != nil {
		return err
	}
	// The serving-config gate (H-01): a rendered config that runs a model on
	// the CPU, keeps one loaded past five idle minutes, or preloads is REFUSED
	// here, before it can be written — the templates were fixed by hand twice
	// (0.115.4, 0.115.7) and nothing stopped the next regression.
	if vs := servingtmpl.Audit(res.Config); len(vs) != 0 {
		return fmt.Errorf("tier %s: the rendered config breaks %d operator rule(s) (INV-1/INV-2) — not written:\n%s", res.TierID, len(vs), servingtmpl.Violations(vs))
	}

	target := res.Params.GOOS
	// D5 (ADR 0039): a composite tier's render must be the CHECKED UNION of the
	// tiers it composes — every layer seat defined, every seat on the cards its
	// layer declares, every composed capability present. The tier shipped a media
	// block copied from the 2-card tier once (0.113.33) and nothing read the
	// result; this reads it, and refuses before the file is written.
	if len(res.Profile.Composes) > 0 {
		if err := servingtmpl.CheckComposite(res.Config, servingtmpl.CompositeDecl{
			Tier: res.TierID, Composes: res.Profile.Composes, Layers: res.Layers, MediaKinds: seatKinds(res.Profile.MediaSeats),
		}, res.Composed); err != nil {
			return fmt.Errorf("tier %s: %w — not written", res.TierID, err)
		}
	}
	warnMissingSeatModels(res.Profile.MediaSeats, *modelsDir, target)
	warnMissingGatedModels(res.Include26B, res.Profile.IncludeQwen38, res.Profile.IncludeQwen354B, res.Profile.IncludeQwen359B, res.Profile.IncludeQwen3827B, *modelsDir, target)

	// The provenance stamp (K-02) rides on every rendered config from here on.
	// It is prepended AFTER the rule audit so the audit sees exactly what a
	// pre-stamp build saw, and the body it hashes is byte-identical to what
	// Render produced.
	stamped, err := servingtmpl.Stamp(res.Config, res.Basis, time.Now().UTC())
	if err != nil {
		return err
	}

	if *out == "" {
		fmt.Print(stamped)
		return nil
	}
	if err := os.WriteFile(*out, []byte(stamped), 0o644); err != nil {
		return err
	}
	// stdout, not stderr: this is a SUCCESS line, and a PowerShell caller with
	// $ErrorActionPreference='Stop' turns any stderr output from a native command into
	// a terminating error — which is exactly how the delegated installer first broke.
	// stdout is free here because the config only goes there when --out is empty.
	spec, _, _ := servingtmpl.SpecHash(res.Basis)
	fmt.Printf("wrote %s (tier %s, %s/%s, spec_sha256 %s)\n", *out, res.TierID, osTag(target), res.Profile.Backend, spec)
	return nil
}

// runAuditYAML is the session-start half of the serving-config gate (H-01 /
// H-02): the same checker `install render` refuses on, over LIVE files. One
// line per violation, exit 1 when any file breaks a rule, so a start audit
// that pipes it cannot read a broken box as compliant.
//
// --against-render adds the K-02 half: every file is ALSO checked against what
// THIS binary's seeds would render today, and reported as exactly one of
// MATCH / STALE(<keys>) / UNSTAMPED / HAND-EDITED. The rule audit alone cannot
// see staleness — a config a tier revision behind breaks no rule, which is how
// ampere-16 served a 32768 window for weeks after register A-39 raised it to
// 131072 while `audit-yaml` reported OK (register K-02).
//
// Exit codes, deliberately asymmetric: a rule violation, a STALE config and a
// HAND-EDITED one exit 1; UNSTAMPED does not. Every config on the fleet today
// predates stamping, so failing on UNSTAMPED would make the session-start audit
// red on every box from the moment this ships — a gate that is always red is a
// gate that gets ignored, and the point of this one is that STALE is rare and
// means something. UNSTAMPED still prints, as a finding.
//
// FLAGS COME BEFORE FILES (`audit-yaml --against-render FILE...`): Go's flag
// package stops parsing at the first non-flag argument.
func runAuditYAML(args []string) error {
	fs := flag.NewFlagSet("audit-yaml", flag.ExitOnError)
	against := fs.Bool("against-render", false, "also re-derive each file from THIS binary's tier seeds and report MATCH / STALE(keys) / UNSTAMPED / HAND-EDITED")
	_ = fs.Parse(args)
	files := fs.Args()
	if len(files) == 0 {
		return fmt.Errorf("audit-yaml: at least one file is required")
	}
	bad := 0
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("audit-yaml: %w", err)
		}
		vs := servingtmpl.Audit(string(b))
		var doc struct {
			Models map[string]any `yaml:"models"`
		}
		_ = yaml.Unmarshal(b, &doc)
		if len(vs) == 0 {
			fmt.Printf("%s: OK (%d models, every entry ttl %d, cards only)\n", path, len(doc.Models), servingtmpl.TTLRequired)
		} else {
			bad++
			fmt.Printf("%s: %d violation(s)\n%s\n", path, len(vs), servingtmpl.Violations(vs))
		}
		if !*against {
			continue
		}
		rep := provenanceOf(string(b))
		fmt.Println(rep.Line(path))
		if rep.State == servingtmpl.StateStale || rep.State == servingtmpl.StateHandEdited {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("audit-yaml: %d finding(s) — see the lines above", bad)
	}
	return nil
}

// displayLayerOf finds a tier's display layer: the one that names a display
// device and substitutes rungs onto it. nil — every tier but the composite one
// — renders the template's display fences away entirely.
func displayLayerOf(layers []config.LayerSpec) *config.LayerSpec {
	for i := range layers {
		if layers[i].DisplayDevice == "" {
			continue
		}
		for _, s := range layers[i].Seats {
			if len(s.ModelMap) > 0 {
				return &layers[i]
			}
		}
	}
	return nil
}

// seatKinds lists the media-seat kinds a tier binds, for the composition check.
func seatKinds(seats []mediaseat.Seat) []string {
	out := make([]string, 0, len(seats))
	for _, s := range seats {
		out = append(out, s.Kind)
	}
	return out
}

// composedCapabilities reduces the tiers a composite claims to be a complete instance
// of to the capabilities that claim has to survive. A composes entry naming a
// tier the table does not define is itself a finding — reported as a composed
// tier with no capabilities would hide it, so it is surfaced as its own row
// with an id the check reports as missing.
func composedCapabilities(profiles map[string]servingProfile, composes []string) []servingtmpl.ComposedTier {
	out := make([]servingtmpl.ComposedTier, 0, len(composes))
	for _, id := range composes {
		p, ok := profiles[id]
		if !ok {
			out = append(out, servingtmpl.ComposedTier{ID: id, VLLMSeatID: "(tier " + id + " is not in the tier table)"})
			continue
		}
		c := servingtmpl.ComposedTier{ID: id, SeatKinds: seatKinds(p.MediaSeats)}
		if p.VLLMSeat != nil {
			c.VLLMSeatID, c.FallbackID = p.VLLMSeat.ID, p.VLLMSeat.Fallback
		}
		out = append(out, c)
	}
	return out
}

// provenanceOf re-derives a stamped config from THIS binary's embedded seeds and
// reports its state. The embedded table is the right source and not --root: the
// question this answers is "would the binary running right now render this file",
// and a checkout on the auditing box is not what installs a node.
func provenanceOf(text string) servingtmpl.Report {
	st, ok := servingtmpl.ParseStamp(text)
	if !ok {
		return servingtmpl.AgainstRender(text, servingtmpl.SpecBasis{}, "")
	}
	stamped, err := st.Basis()
	if err != nil {
		return servingtmpl.AgainstRender(text, servingtmpl.SpecBasis{}, "")
	}
	req, ok := replayRequest(stamped)
	if !ok {
		// Nothing to replay against: report the stamp's own integrity and let
		// AgainstRender call it stale rather than inventing a tier.
		return servingtmpl.AgainstRender(text, servingtmpl.SpecBasis{HarnessVersion: buildinfo.Version}, "")
	}
	res, err := deriveRender(embeddedProfiles, req)
	if err != nil {
		// The tier is gone from this binary's table, or its template is. Both are
		// real drift; AgainstRender says so from the empty body.
		return servingtmpl.AgainstRender(text, servingtmpl.SpecBasis{HarnessVersion: buildinfo.Version, TierID: stamped.TierID}, "")
	}
	return servingtmpl.AgainstRender(text, res.Basis, res.Config)
}

// replayRequest rebuilds the render inputs from a stamp. ok=false when the stamp
// names no tier and no fallback backend: deriveRender would then classify the
// AUDITING machine and compare a config against some other box's tier, which is
// a wrong answer rather than a missing one.
func replayRequest(b servingtmpl.SpecBasis) (renderRequest, bool) {
	req := renderRequest{
		TierID: b.TierID, Fallback: b.Render.FallbackBackend, RAMTier: b.Render.RAMTier,
		GOOS: b.Params.GOOS, LlamaBin: b.Params.LlamaBin, ModelsDir: b.Params.ModelsDir,
		Listen: b.Params.Listen, Home: b.Params.Home, Threads: b.Params.Threads,
		// The vLLM deployment half is a per-BOX fact (the account, the bound
		// address, where the venv lives), never a seed. Pinned from the stamp so
		// the replay measures seed drift and not "the auditing box is not the
		// node". A change to the tier's own vllm_seat block is still caught —
		// it moves profiles_entry_sha256, which the basis diff names.
		PinnedVLLM: &pinnedVLLM{Seat: b.Params.VLLMSeat, Runtime: b.Params.VLLMRuntime},
	}
	if b.Render.FallbackBackend != "" {
		// Off-matrix: the stamped tier id is the human label
		// "(off-matrix: <backend> defaults)", not a key in the table.
		req.TierID = ""
	} else if req.TierID == "" {
		return renderRequest{}, false
	}
	if req.GOOS == "" {
		return renderRequest{}, false
	}
	return req, true
}

// servingConfigReporter builds the /fleet/health provenance reporter for a
// node's rendered serving config (K-02). An empty path returns nil: the health
// fields are then omitted entirely, which is the honest answer for a node that
// was never told which file it serves.
//
// The result is CACHED on the file's (mtime, size). Health is polled every few
// seconds by every delegator on the fleet, and the verdict costs a re-render;
// re-deriving per poll would burn the node's CPU to answer a question whose
// answer only changes when the file does. A file that is replaced with the same
// size in the same mtime tick keeps a stale verdict for that tick, which is a
// price worth paying for a config a human re-renders by hand.
//
// A read error yields an empty state, which omits both fields: a node that
// cannot read its own config must not publish a verdict about it.
func servingConfigReporter(path string) func() (string, string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	var (
		mu       sync.Mutex
		haveKey  bool
		key      [2]int64 // unix nanos of mtime, size
		cachedID string
		cachedSt string
	)
	return func() (string, string) {
		fi, err := os.Stat(path)
		if err != nil {
			return "", ""
		}
		k := [2]int64{fi.ModTime().UnixNano(), fi.Size()}
		mu.Lock()
		defer mu.Unlock()
		if haveKey && key == k {
			return cachedID, cachedSt
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", ""
		}
		rep := provenanceOf(string(b))
		haveKey, key = true, k
		cachedID, cachedSt = rep.SpecSHA256, string(rep.State)
		return cachedID, cachedSt
	}
}
