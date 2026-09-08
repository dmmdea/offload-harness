// Package vllmseat is the tier schema's declaration of a PERSISTENT vLLM agent seat
// served behind llama-swap — the pattern decided in ADR 0035 and measured on the
// reference NVIDIA A2 16 GB (`ampere-16`, harness 0.113.19, 2026-09-06).
//
// Why it exists as a schema field rather than a runbook. ADR 0035 originally recorded
// "the tier SEED is unchanged: the seat is a hand-installed venv and unit, not
// something the installer renders", and the consequence was exactly the drift this
// repo keeps re-learning: the reference box moved its agent lane to the vLLM seat,
// the operator accepted it as the tier's answer — and setup/templates/profiles.json
// went on seeding `agent_model: qwen3.5-4b-agent`, so every fresh ampere-16 install
// got the arm that LOST.
//
// LOST ON QUALITY, not on speed. The 2026-09-04 bake-off scored only shape
// (`len(findings)>=3 and len(summary)>40`), so its 8/8 meant "emitted well-formed
// output", not "equally good". A blind re-evaluation of the retained answers against
// the ground-truth ADRs — 3 independent lenses, 24 judgements, no model name, engine
// or timing visible — put the vLLM seat first on mean overall (7.58 vs 6.79 for the
// same weights on llama.cpp and 6.38 for the 12B), on specificity, on coverage, and
// with zero filler findings. Wall time is not why this seat is here. Operator ruling 2026-09-07: "install
// defaults follow the measurement — the fix is to render the seat, not to ship a
// smaller number than the hardware was measured at."
//
// One declaration, three artifacts, on the same principle internal/mediaseat states:
// the llama-swap entry, the systemd/polkit/wrapper files, and the harness config
// binding (`agent_model`) all derive from THIS spec, so the seat and the model the
// agent lane routes to cannot disagree.
//
// PREREQUISITES ARE NOT RENDERED. The engine is a hand-built venv (vLLM 0.28,
// torch 2.13+cu130) holding a specific HF snapshot; the installer does not create it.
// A tier declaring this seat therefore renders it only when Detect finds the venv and
// the model path, and otherwise falls back to Fallback — the llama.cpp seat that
// still works. Rendering a unit that points at a venv nobody built would produce a
// seat that fails at boot, which is strictly worse than the fallback.
package vllmseat

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// safeID mirrors internal/mediaseat: the id becomes a YAML key and an inline
// flow-sequence member, where a comma silently splits it and a colon breaks the
// document outright.
var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Spec is a tier's persistent vLLM agent seat. Every field that carries a number is
// a MEASURED operating point, not a preference — the comments name what measured it.
type Spec struct {
	// ID is the llama-swap model id, the vLLM --served-model-name, and the value
	// `agent_model` binds to. One string, so the three cannot drift.
	ID string `json:"id"`
	// Aliases are the other names that must reach the seat. vLLM 404s any name it
	// does not serve, so the entry sets useModelName to ID and every alias is
	// rewritten to it.
	Aliases []string `json:"aliases,omitempty"`
	// Unit is the systemd unit's base name (no .service suffix).
	Unit string `json:"unit"`
	// Port is what the engine binds on the Tailscale address.
	Port int `json:"port"`
	// Device is CUDA_VISIBLE_DEVICES for the engine.
	Device string `json:"device,omitempty"`

	// ModelRepo is the HF hub repo directory RELATIVE to the deployment's HF home
	// (e.g. "hub/models--RedHatAI--Qwen3.5-4B-quantized.w4a16"). Relative because the
	// tier is a hardware class: where a box keeps its HF cache is a deployment fact.
	//
	// Original doc follows. The HF hub repo DIRECTORY (…/hub/models--<org>--<name>). The
	// concrete snapshot underneath it is a content hash that differs per download, so
	// a tier cannot name it — Resolve picks it at render time and refuses if there is
	// not exactly one.
	//
	// It must stay SHORT. LMCache's fs_native page names embed the model path, and the
	// reference box's long path produced 268-byte names against NAME_MAX 255, which
	// failed every L2 store silently (no errno, just an empty L2).
	ModelRepo string `json:"model_repo"`
	// ModelPath overrides the resolution above with an exact snapshot path. Normally
	// empty; set it only when a box holds several revisions and must pin one.
	ModelPath string `json:"model_path,omitempty"`
	// MaxModelLen is the served window. 131,072 on the reference A2 at util 0.65:
	// weights 4.48 GiB + KV 2.91 GiB = a 180,098-token pool, 1.37x concurrency at
	// that window.
	MaxModelLen int `json:"max_model_len"`
	// GPUMemoryUtilization is sized so the box's OTHER llama-swap seats still fit
	// beside the engine. 0.65 of a 15,356 MiB card = ~9.6 GB used, ~5.8 GB free
	// (sdxl-turbo 5.1 GB + embed 0.3 fit; the 12B and 26B agents do NOT co-reside).
	GPUMemoryUtilization float64 `json:"gpu_memory_utilization"`
	// MaxNumSeqs is the engine's concurrency AND the entry's concurrencyLimit —
	// llama-swap's per-model default of 10 would 429 the streams the seat was
	// provisioned for (review finding, 2026-09-06).
	MaxNumSeqs int `json:"max_num_seqs"`
	// MaxBatchedTokens is --max-num-batched-tokens.
	MaxBatchedTokens int `json:"max_num_batched_tokens"`
	// KVCacheDtype: fp8_e5m2 is free on Ampere and is what brings 262k into reach;
	// it needs the LMCache PR #4253 overlay before an L2 tier is configured.
	KVCacheDtype string `json:"kv_cache_dtype,omitempty"`
	// ToolCallParser / ReasoningParser are per model family and are NOT optional for
	// an agent seat: without --enable-auto-tool-choice and a parser every contract
	// fails at once with `"auto" tool choice requires --enable-auto-tool-choice`.
	ToolCallParser  string `json:"tool_call_parser"`
	ReasoningParser string `json:"reasoning_parser"`

	// HealthCheckTimeout is llama-swap's GLOBAL setting, raised because the vLLM
	// cold load is 125-250 s; the default 120 kills the attach mid-load and cmdStop
	// then stops the engine. llama.cpp seats still fail fast when broken.
	HealthCheckTimeout int `json:"health_check_timeout,omitempty"`
	// UnloadTimeout covers the unit's TimeoutStopSec.
	UnloadTimeout int `json:"unload_timeout,omitempty"`
	// TTLSeconds is the idle window after which llama-swap unloads the seat, which
	// for this seat means stopping the engine unit and freeing the whole card.
	// Defaults to the house 300 (5 minutes). It may NOT be 0: that is "never unload",
	// which is what this field exists to prevent.
	TTLSeconds int `json:"ttl_seconds,omitempty"`

	// Fallback is the llama.cpp seat id this tier serves when the vLLM prerequisites
	// are absent. Required: a tier may not declare an agent seat with no answer for
	// a box that has not built the venv.
	Fallback string `json:"fallback_agent_model"`
	// FallbackCtx is the window the fallback seat serves.
	FallbackCtx int `json:"fallback_agent_ctx_tokens,omitempty"`
	// AgentCtxTokens is the window the harness advertises when the vLLM seat runs.
	AgentCtxTokens int `json:"agent_ctx_tokens,omitempty"`

	// Measured records what measured this operating point, so a reader never has to
	// trust the numbers on faith.
	Measured string `json:"measured,omitempty"`
}

// Runtime is the per-DEPLOYMENT half of a render: the things a tier cannot know
// because they belong to the box, not the hardware class.
type Runtime struct {
	// User is the account llama-swap runs as; the polkit rule is scoped to it.
	User string
	// ProxyHost is the LITERAL address the engine binds. On the reference box the
	// MagicDNS name resolved to IPv6 only while vLLM bound the Tailscale IPv4, so a
	// hostname here would have failed llama-swap's health check forever.
	ProxyHost string
	// StackDir is the install root: the unit's WorkingDirectory and where its log goes.
	StackDir string
	// SeatDir holds the rendered unit + wrapper scripts.
	SeatDir string
	// VenvDir is the hand-built vLLM virtualenv (vLLM 0.28, torch 2.13+cu130). The
	// installer does not create it; Detect only reports whether it is there.
	VenvDir string
	// HFHome is the HF cache root. KEEP IT SHORT: LMCache's fs_native page names embed
	// the resolved model path, and the reference box's long path produced 268-byte
	// names against NAME_MAX 255, failing every L2 store silently. The reference
	// deployment mounts it at /hf for exactly this reason.
	HFHome string
}

// ModelDir is the absolute HF repo directory for this deployment.
func (r Runtime) ModelDir(s Spec) string {
	return path.Join(r.HFHome, s.ModelRepo)
}

// Validate refuses a half-specified deployment. Each of these has one honest source
// (a flag, the install root, or the box) and no safe default.
func (r Runtime) Validate() error {
	var missing []string
	for _, f := range []struct{ name, val string }{
		{"user", r.User}, {"proxy host", r.ProxyHost},
		{"stack dir", r.StackDir}, {"seat dir", r.SeatDir},
		{"venv dir", r.VenvDir}, {"hf home", r.HFHome},
	} {
		if f.val == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("vllm seat runtime: no %s", strings.Join(missing, ", no "))
	}
	return nil
}

// Validate refuses a spec at AUTHORING time — in a test over the committed tier
// table — rather than on someone's machine, where the symptom is a unit that will not
// start or an agent lane that quietly routes nowhere.
func (s Spec) Validate(tier string) error {
	var problems []string
	req := func(cond bool, msg string) {
		if !cond {
			problems = append(problems, msg)
		}
	}
	req(s.ID != "", "no id — the id IS the llama-swap model id, the served-model-name and the agent_model binding")
	req(s.ID == "" || safeID.MatchString(s.ID), fmt.Sprintf("id %q must match %s", s.ID, safeID))
	for _, a := range s.Aliases {
		req(safeID.MatchString(a), fmt.Sprintf("alias %q must match %s", a, safeID))
		req(a != s.ID, fmt.Sprintf("alias %q repeats the id", a))
	}
	req(s.Unit != "", "no unit name")
	req(!strings.HasSuffix(s.Unit, ".service"), "unit must be the BASE name, without the .service suffix")
	req(s.Port > 0 && s.Port < 65536, fmt.Sprintf("port %d out of range", s.Port))
	req(s.ModelRepo != "" || s.ModelPath != "", "no model_repo — vLLM needs the HF hub repo dir (the snapshot hash is per-download and is resolved at render time)")
	req(s.MaxModelLen > 0, "no max_model_len")
	req(s.GPUMemoryUtilization > 0 && s.GPUMemoryUtilization < 1,
		fmt.Sprintf("gpu_memory_utilization %v must be between 0 and 1", s.GPUMemoryUtilization))
	req(s.MaxNumSeqs > 0, "no max_num_seqs — it is also the entry's concurrencyLimit")
	req(s.ToolCallParser != "", "no tool_call_parser — an agent seat without one fails every contract at once")
	req(s.ReasoningParser != "", "no reasoning_parser")
	req(s.Fallback != "", "no fallback_agent_model — a box that has not built the vLLM venv must still get a working agent seat")
	req(s.Fallback != s.ID, "fallback_agent_model repeats the seat id, so there is no fallback")
	// 0 is the value that means "never unload". It is the one value this field must
	// never take; omit it to get the house default instead.
	req(s.TTLSeconds >= 0, "ttl_seconds cannot be negative")
	req(s.TTLSeconds != -1, "ttl_seconds -1 means never unload")
	req(!strings.HasPrefix(s.ModelRepo, "/"),
		"model_repo must be RELATIVE to the deployment's HF home — an absolute path hardens one box's layout into a hardware tier")
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("tier %s vllm_seat: %s", tier, strings.Join(problems, "; "))
}

// Resolve fills ModelPath from ModelRepo by finding the one snapshot on the box. An
// explicit ModelPath wins. Zero snapshots means the weights were never fetched; more
// than one means the box holds several revisions and the tier must pin which — both
// are refusals, because picking one silently is how a seat comes up serving weights
// nobody chose.
func (s Spec) Resolve(r Runtime) (Spec, error) {
	if s.ModelPath != "" {
		return s, nil
	}
	if s.ModelRepo == "" {
		return s, fmt.Errorf("vllm seat %s: neither model_repo nor model_path is set", s.ID)
	}
	snaps := filepath.Join(r.ModelDir(s), "snapshots")
	entries, err := os.ReadDir(snaps)
	if err != nil {
		return s, fmt.Errorf("vllm seat %s: cannot read %s: %w", s.ID, filepath.ToSlash(snaps), err)
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() {
			found = append(found, e.Name())
		}
	}
	sort.Strings(found)
	switch len(found) {
	case 1:
		s.ModelPath = filepath.ToSlash(filepath.Join(snaps, found[0]))
		return s, nil
	case 0:
		return s, fmt.Errorf("vllm seat %s: no snapshot under %s — the weights were never fetched",
			s.ID, filepath.ToSlash(snaps))
	default:
		return s, fmt.Errorf("vllm seat %s: %d snapshots under %s (%s) — set model_path to pin one rather than "+
			"letting the seat come up on whichever sorts first", s.ID, len(found), filepath.ToSlash(snaps), strings.Join(found, ", "))
	}
}

// Detect reports whether the box can actually run this seat: the venv's `vllm` entry
// point and exactly one resolvable model snapshot. A missing prerequisite is NOT an
// error — it is the documented fallback path, and the reason is returned so the
// installer can say WHY it fell back instead of silently shipping a lesser seat.
func (s Spec) Detect(r Runtime) (bool, string) {
	vllm := filepath.Join(r.VenvDir, "bin", "vllm")
	if fi, err := os.Stat(vllm); err != nil || fi.IsDir() {
		return false, "no vllm entry point at " + filepath.ToSlash(vllm)
	}
	got, err := s.Resolve(r)
	if err != nil {
		return false, err.Error()
	}
	if fi, err := os.Stat(got.ModelPath); err != nil || !fi.IsDir() {
		return false, "no model snapshot at " + filepath.ToSlash(got.ModelPath)
	}
	return true, ""
}

// Bindings are the harness config keys this seat derives. Kept beside the seat so a
// tier cannot bind an agent model that no seat serves.
func (s Spec) Bindings() map[string]any {
	out := map[string]any{"agent_model": s.ID}
	if s.AgentCtxTokens > 0 {
		out["agent_ctx_tokens"] = s.AgentCtxTokens
	}
	return out
}

// FallbackBindings are what a box WITHOUT the venv binds instead.
func (s Spec) FallbackBindings() map[string]any {
	out := map[string]any{"agent_model": s.Fallback}
	if s.FallbackCtx > 0 {
		out["agent_ctx_tokens"] = s.FallbackCtx
	}
	return out
}

// tokens is the substitution map shared by the llama-swap entry and every systemd
// artifact, so the entry's proxy target and the unit's bind address cannot disagree.
func (s Spec) tokens(r Runtime) map[string]string {
	served := append([]string{s.ID}, s.Aliases...)
	quoted := make([]string, 0, len(s.Aliases))
	for _, a := range s.Aliases {
		quoted = append(quoted, strconv.Quote(a))
	}
	pool := ""
	if len(s.Aliases) > 0 {
		pool = s.Aliases[0]
	}
	device := s.Device
	if device == "" {
		device = "0"
	}
	return map[string]string{
		"__SEAT_ID__":          s.ID,
		"__POOL_ALIAS__":       pool,
		"__SERVED_NAMES__":     strings.Join(served, " "),
		"__ALIASES__":          strings.Join(quoted, ", "),
		"__UNIT__":             s.Unit,
		"__USER__":             r.User,
		"__PROXY_HOST__":       r.ProxyHost,
		"__PORT__":             strconv.Itoa(s.Port),
		"__DEVICE__":           device,
		"__STACK_DIR__":        r.StackDir,
		"__SEAT_DIR__":         r.SeatDir,
		"__VENV_DIR__":         r.VenvDir,
		"__HF_HOME__":          r.HFHome,
		"__MODEL_PATH__":       s.ModelPath,
		"__MAX_MODEL_LEN__":    strconv.Itoa(s.MaxModelLen),
		"__GPU_UTIL__":         strconv.FormatFloat(s.GPUMemoryUtilization, 'g', -1, 64),
		"__MAX_NUM_SEQS__":     strconv.Itoa(s.MaxNumSeqs),
		"__MAX_BATCHED__":      strconv.Itoa(s.MaxBatchedTokens),
		"__KV_DTYPE__":         s.KVCacheDtype,
		"__TOOL_PARSER__":      s.ToolCallParser,
		"__REASONING_PARSER__": s.ReasoningParser,
		"__HEALTH_TIMEOUT__":   strconv.Itoa(s.healthTimeout()),
	}
}

func (s Spec) healthTimeout() int {
	if s.HealthCheckTimeout > 0 {
		return s.HealthCheckTimeout
	}
	return 480
}

// ttl is the idle window before the seat is unloaded and the card freed.
func (s Spec) ttl() int {
	if s.TTLSeconds > 0 {
		return s.TTLSeconds
	}
	return 300
}

func (s Spec) unloadTimeout() int {
	if s.UnloadTimeout > 0 {
		return s.UnloadTimeout
	}
	return 120
}

// Entry renders the llama-swap model block for a MATRIX-based template.
//
// The reference file setup/templates/vllm-seat/linux-systemd/llama-swap-entry.yaml
// expresses residency with a legacy `groups: {persistent: true}` block, which is the
// shape the reference box's llama-swap uses. Every template in this repo has moved to
// `matrix:` — and `groups: persistent:true` was measured FAILING on the Qube, where
// it silently degraded the memory stack to dense-only — so residency here is a matrix
// membership instead: the renderer joins this seat to the residents set and gives it
// a high evict cost. TestEntryMatchesTheReferenceTemplate keeps the two in step on
// every field that is not residency.
func (s Spec) Entry(r Runtime) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s:\n", s.ID)
	if len(s.Aliases) > 0 {
		fmt.Fprintf(&b, "    aliases: [%s]\n", strings.Join(s.Aliases, ", "))
	}
	fmt.Fprintf(&b, "    cmd: %s/vllm-seat-cmd.sh\n", r.SeatDir)
	fmt.Fprintf(&b, "    cmdStop: %s/vllm-seat-cmdstop.sh\n", r.SeatDir)
	fmt.Fprintf(&b, "    proxy: http://%s:%d\n", r.ProxyHost, s.Port)
	fmt.Fprintf(&b, "    checkEndpoint: /health\n")
	// vLLM 404s any name it does not serve, so every alias is rewritten to the id.
	fmt.Fprintf(&b, "    useModelName: %q\n", s.ID)
	// THE SEAT IDLES OUT LIKE EVERY OTHER SEAT. It was shipped `ttl: 0` — never
	// unloads — on the reasoning that the agent lane should not pay a 125-250 s cold
	// load. That reasoning is rejected: operator rule, "no model gets to be loaded for
	// more than 5 minutes if it goes unused, as the rest of the harness... 30 minutes
	// of loading time is way better than no use, and no use is what you cause by
	// leaving a model loaded and unused." A seat that never unloads is not availability,
	// it is a card permanently spent on nothing — measured on the reference A2, which
	// sat at 10,338 of 15,356 MiB with the engine idle.
	fmt.Fprintf(&b, "    ttl: %d\n", s.ttl())
	fmt.Fprintf(&b, "    unloadTimeout: %d\n", s.unloadTimeout())
	fmt.Fprintf(&b, "    concurrencyLimit: %d\n", s.MaxNumSeqs)
	return b.String()
}

// artifactFiles maps a template file to the name it is installed under. The unit and
// the polkit rule are named after the unit so two seats never collide on one box.
func (s Spec) artifactFiles() map[string]string {
	return map[string]string{
		"vllm-seat.service":             s.Unit + ".service",
		"vllm-seat-run.sh":              "vllm-seat-run.sh",
		"vllm-seat-cmd.sh":              "vllm-seat-cmd.sh",
		"vllm-seat-cmdstop.sh":          "vllm-seat-cmdstop.sh",
		"50-llama-swap-vllm-seat.rules": "50-llama-swap-" + s.Unit + ".rules",
	}
}

// Artifacts renders the systemd unit, the two llama-swap wrappers, the run script and
// the polkit rule from the reference templates in templatesDir. It returns
// installed-name -> content, and refuses to return a file that still holds a token:
// a half-substituted unit starts and misbehaves rather than failing loudly.
func (s Spec) Artifacts(templatesDir string, r Runtime) (map[string]string, error) {
	if err := s.Validate("(render)"); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	resolved, err := s.Resolve(r)
	if err != nil {
		return nil, err
	}
	tok := resolved.tokens(r)
	out := map[string]string{}
	for src, dst := range s.artifactFiles() {
		raw, err := os.ReadFile(filepath.Join(templatesDir, src))
		if err != nil {
			return nil, fmt.Errorf("vllm seat template %s: %w", src, err)
		}
		body := string(raw)
		for k, v := range tok {
			body = strings.ReplaceAll(body, k, v)
		}
		if i := strings.Index(body, "__"); i >= 0 {
			line := 1 + strings.Count(body[:i], "\n")
			return nil, fmt.Errorf("vllm seat %s: %s still holds an unsubstituted token at line %d (%.40s) — "+
				"a half-rendered unit starts and misbehaves instead of failing", s.ID, src, line, body[i:])
		}
		out[dst] = body
	}
	return out, nil
}

// Executable reports whether an installed artifact needs the execute bit.
func Executable(name string) bool { return strings.HasSuffix(name, ".sh") }
