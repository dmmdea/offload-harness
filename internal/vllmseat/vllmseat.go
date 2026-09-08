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
	"encoding/json"
	"fmt"
	"net"
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
	// MPPort is the LMCache MP server's loopback port, used only when CacheServer is
	// set. 0 = Port-1, which is the reference pairing (18797 engine / 18796 server).
	//
	// It is a distinct port and not a detail: a scratch or benchmark stack on the same
	// box MUST take its own, or it serves the production seat's traffic — a day of
	// measurements was voided that way.
	MPPort int `json:"mp_port,omitempty"`
	// Device is CUDA_VISIBLE_DEVICES for the engine. It may name SEVERAL cards
	// ("0,2"), in which case TensorParallel must equal how many — see its comment.
	Device string `json:"device,omitempty"`
	// TensorParallel is --tensor-parallel-size. 0 and 1 both mean one card.
	//
	// It is validated against Device rather than left to agree by convention: a
	// two-card device list with tensor_parallel 1 starts an engine that loads the
	// whole model onto the first card and silently ignores the second, and a
	// one-card list with tensor_parallel 2 refuses to start at all. Both are
	// authoring mistakes a tier table can catch for free.
	//
	// PIPELINE parallelism is deliberately absent. The reference 3-card box can run
	// the 27B across three cards (pipeline 26/26/12), and that layout measured WORSE
	// on every axis this seat exists for: 100,352 served window against the 2-card
	// tensor-parallel seat's 163,072, 32 GB of host RAM against 8, the display card
	// drawn into the engine, and NO cache server — LMCache's documentation addresses
	// tensor parallelism (it "classifies per-TP-rank") and never mentions pipeline
	// parallelism at all, so the store has no per-stage story upstream. The 3-card
	// layout stays what the tier notes already call it: opt-in long-context work,
	// started by hand, not the delegation lane.
	TensorParallel int `json:"tensor_parallel,omitempty"`
	// Launch selects the artifact set, because a seat is started differently on a
	// Linux node and on a Windows box whose engine lives in WSL. Empty = the
	// historical LaunchLinuxSystemd.
	//
	// This is a field and not a build tag because the tier table is CROSS-COMPILED
	// evidence: a Windows workstation's tier is rendered and tested on any machine,
	// and a test that only passes on the host it describes is not a gate.
	Launch string `json:"launch,omitempty"`
	// CacheServer binds this seat to the tier's LMCache KV cache server. Optional:
	// a seat without one keeps its KV entirely in VRAM plus the L1 staging buffer.
	//
	// It lives on the SEAT rather than only in config.json's kv_cache_server block
	// because the two must agree and nothing checked that they did. The harness
	// block is declarative — internal/config/kvcacheserver.go validates it BY NAME
	// and never contacts the store — while the engine reads its own seat env, so a
	// chunk size changed in one place and not the other produced a store that
	// registered and then served nothing. ConfigBlock derives the harness half from
	// this one, so there is a single authority.
	CacheServer *CacheServer `json:"cache_server,omitempty"`

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
	// KVCacheDtype halves the KV footprint and is what brings a long window into
	// reach. It needs the LMCache PR #4253 overlay before an L2 tier is configured.
	//
	// "fp8 is free on Ampere" IS FALSE AS A BLANKET CLAIM — it is BACKEND-dependent.
	// Measured on the reference A2 (SM86) 2026-09-08: `FP8 KV cache is not supported
	// by the Triton attention backend on NVIDIA A2 (compute capability 8.6); native
	// FP8 (fp8e4nv) requires SM89+`. The tier's Qwen3.5 seat gets fp8 because it runs
	// the FlashAttention backend; a Gemma-4 seat on the SAME card falls back to Triton
	// and refuses to start. So this field belongs to the SEAT, not the tier: a tier
	// that hard-codes fp8 breaks every model whose backend lacks it. Leave it empty
	// (vLLM's `auto`) unless the exact model+backend pair has been measured with it.
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

// The launch shapes. A seat is the same engine either way; what differs is who
// supervises it and therefore what the llama-swap entry's cmd/cmdStop must run.
const (
	// LaunchLinuxSystemd is the default: a systemd unit on a Linux node, driven by
	// the wrapper scripts through a polkit rule.
	LaunchLinuxSystemd = "linux-systemd"
	// LaunchWindowsWSL is a Windows box whose engine runs inside a WSL distro.
	// llama-swap runs as SYSTEM there and cannot start a WSL session itself, so the
	// entry shells out through a hidden Windows Script Host launcher — the shape the
	// reference 3-card workstation already runs.
	LaunchWindowsWSL = "windows-wsl"
)

// CacheServer is the seat's half of the LMCache KV cache server binding.
//
// Every number here is an operating point that must match the engine's, not a
// preference. The failure this type exists to prevent is silent: a store whose chunk
// size disagrees with the engine's unified block registers cleanly and then serves
// nothing, and a store whose namespace outlives a layout change fails reads with
// "value size exceeds buffer capacity" while reporting success.
type CacheServer struct {
	// Store is the L2 adapter: "fs_native" (a filesystem export mounted on the
	// serving box) or "valkey" (Redis protocol). Empty = fs_native.
	Store string `json:"store,omitempty"`
	// Address is the mount point for fs_native, or host:port for valkey.
	Address string `json:"address"`
	// L1StagingGB is LMCache MP's pinned host buffer beside the engine. It is a
	// staging area, not the tier itself: 8 GB restored a 24k context entirely from
	// the store. 0 = 8.
	L1StagingGB int `json:"l1_staging_gb,omitempty"`
	// ChunkSize is LMCache's chunk in TOKENS and MUST equal the engine's unified
	// block size for this model and KV dtype (1568 for Qwen3.8-27B with fp8 KV, 784
	// with fp16). A mismatch fails registration at start, loudly — which is the good
	// case; the bad case is a stale value that merely halves reuse.
	ChunkSize int `json:"chunk_size"`
	// KeyPrefix namespaces the store. It MUST change whenever the engine layout, the
	// KV dtype or the LMCache build changes, or the seat reads pages written under a
	// layout it no longer has.
	KeyPrefix string `json:"key_prefix"`
	// MaxCapacityGB caps what the running MP server will hold, and PruneGB is what
	// the wrapper prunes the store down to before starting.
	//
	// BOTH are needed and they are not the same number. LMCache's own eviction counts
	// only pages the RUNNING server wrote (upstream F10), so a store carrying pages
	// from a previous run is invisible to it — measured 2026-09-06, real fan-out took
	// the reference store from 28 to 99 GB against a 100 GB quota in 75 minutes, and
	// a dataset AT its quota can refuse the very deletes that would free it.
	MaxCapacityGB int `json:"max_capacity_gb,omitempty"`
	PruneGB       int `json:"prune_gb,omitempty"`
	// NumWorkers is the fs_native adapter's writer count. 0 = 8.
	NumWorkers int `json:"num_workers,omitempty"`
	// MountDir is where an fs_native export is mounted on the serving box. The
	// EXPORT ITSELF is not here: which machine hosts the store, and the credentials
	// to reach it, are deployment facts and live on Runtime beside the install root
	// and the HF home. A tier is a hardware class, and naming one host's share in it
	// hardens that deployment's LAN into every box that classifies the same way.
	MountDir string `json:"mount_dir,omitempty"`
	// MinMBPS is a write-throughput FLOOR the wrapper measures before accepting the
	// mount, and it is the field that makes a LAN store safe to depend on. The
	// reference export measured 570 MB/s over the wired path, 124 through WireGuard
	// and 4.6 over Wi-Fi; without a floor the seat silently ran at the Wi-Fi rate,
	// which is far below the parity the tier is justified by. 0 = no floor.
	MinMBPS int `json:"min_mbps,omitempty"`
}

// StoreName is the adapter, defaulted.
func (c CacheServer) StoreName() string {
	if s := strings.ToLower(strings.TrimSpace(c.Store)); s != "" {
		return s
	}
	return "fs_native"
}

// EffectiveL1StagingGB is the pinned host buffer, defaulted.
func (c CacheServer) EffectiveL1StagingGB() int {
	if c.L1StagingGB > 0 {
		return c.L1StagingGB
	}
	return 8
}

// numWorkers is the fs_native writer count, defaulted.
func (c CacheServer) numWorkers() int {
	if c.NumWorkers > 0 {
		return c.NumWorkers
	}
	return 8
}

// L2JSON is the adapter configuration the seat wrapper hands LMCache, rendered with
// encoding/json so a path holding a quote or a backslash cannot break out of the
// string and turn a store into a parse error at seat start.
func (c CacheServer) L2JSON() string {
	type fsNative struct {
		Type          string `json:"type"`
		BasePath      string `json:"base_path"`
		NumWorkers    int    `json:"num_workers"`
		UseODirect    bool   `json:"use_odirect"`
		MaxCapacityGB int    `json:"max_capacity_gb,omitempty"`
	}
	type valkey struct {
		Type    string `json:"type"`
		Address string `json:"address"`
	}
	var v any
	switch c.StoreName() {
	case "valkey":
		v = valkey{Type: "valkey", Address: c.Address}
	default:
		v = fsNative{
			Type: "fs_native", BasePath: c.Address,
			NumWorkers: c.numWorkers(), UseODirect: false,
			MaxCapacityGB: c.MaxCapacityGB,
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		// Every field is a string or an int; Marshal cannot fail on those. Returning
		// empty rather than panicking keeps a render honest: an empty SEAT_L2 is "no
		// cache server", which the wrapper handles, and Artifacts' token sweep would
		// not catch a silently malformed one.
		return ""
	}
	return string(b)
}

// Validate refuses a cache-server binding that could not work, at AUTHORING time.
func (c CacheServer) Validate() error {
	var problems []string
	switch c.StoreName() {
	case "fs_native":
		if !strings.HasPrefix(c.Address, "/") {
			problems = append(problems, fmt.Sprintf("address %q must be an absolute mount path for fs_native", c.Address))
		}
	case "valkey":
		if _, _, err := net.SplitHostPort(c.Address); err != nil {
			problems = append(problems, fmt.Sprintf("address %q must be host:port for valkey", c.Address))
		}
	default:
		problems = append(problems, fmt.Sprintf("store %q is not supported (fs_native, valkey)", c.Store))
	}
	if c.ChunkSize <= 0 {
		problems = append(problems, "no chunk_size — it must equal the engine's unified block size for this model and KV dtype")
	}
	if c.KeyPrefix == "" {
		problems = append(problems, "no key_prefix — the store namespace must be deliberate, never a shared constant")
	}
	if c.L1StagingGB < 0 {
		problems = append(problems, "l1_staging_gb cannot be negative")
	}
	// The prune target must leave room for what the running server will then write,
	// or the seat prunes to a level its own cap immediately exceeds.
	if c.PruneGB > 0 && c.MaxCapacityGB > 0 && c.PruneGB < c.MaxCapacityGB {
		problems = append(problems, fmt.Sprintf(
			"prune_gb %d is below max_capacity_gb %d — the wrapper would prune to a level the running server immediately exceeds",
			c.PruneGB, c.MaxCapacityGB))
	}
	if c.MountDir != "" && c.Address != "" && c.StoreName() == "fs_native" &&
		!strings.HasPrefix(c.Address, c.MountDir) {
		problems = append(problems, fmt.Sprintf(
			"address %q is not under mount_dir %q — the store path would be on the local disk, not the export",
			c.Address, c.MountDir))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("cache_server: %s", strings.Join(problems, "; "))
	}
	return nil
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

	// CacheMountSrc is the fs_native export the seat wrapper mounts before the MP
	// server starts (e.g. a UNC share on the second device), and CacheMountOpts are
	// its mount options, which carry the credentials path. Both are per-DEPLOYMENT:
	// the tier declares that the seat HAS a cache server and how it is shaped, the box
	// says which machine holds it. Empty CacheMountSrc = already mounted.
	CacheMountSrc  string
	CacheMountOpts string

	// LMCacheOverlay is a directory prepended to PYTHONPATH so the MP server and the
	// engine import a patched LMCache. Empty = stock LMCache.
	//
	// It is REQUIRED for an fp8 KV seat with a cache server, and Artifacts refuses the
	// pair without it: stock LMCache restores fp8 pages CORRUPT (the rank-5 group-edit
	// bug, upstream PR #4253), and the failure is silent — the store reports hits, the
	// engine serves, and the text is wrong. A seat that looks healthy while returning
	// corrupted context is the worst outcome this package can produce, so the render
	// refuses rather than trusting an operator to remember.
	LMCacheOverlay string

	// Distro is the WSL distribution the engine runs in. LaunchWindowsWSL only.
	Distro string
	// HostPrefix maps the engine's absolute paths into this process's filesystem for
	// the prerequisite CHECK only — normally `\\wsl.localhost\<distro>`. Empty (the
	// Linux case) means the two are the same path. It never reaches a rendered file.
	HostPrefix string
	// WSLSeatDir is the directory INSIDE that distro holding the seat env file and
	// the two wrapper scripts. LaunchWindowsWSL only; empty = /root/g7.
	//
	// It is a distro path, not a Windows one: the wrappers are executed by bash
	// inside the distro, and a \\wsl$ path handed to bash is not a path at all.
	WSLSeatDir string
}

// wslSeatDir is the in-distro seat directory, defaulted.
func (r Runtime) wslSeatDir() string {
	if r.WSLSeatDir != "" {
		return r.WSLSeatDir
	}
	return "/root/g7"
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
	switch s.Launch {
	case "", LaunchLinuxSystemd, LaunchWindowsWSL:
	default:
		problems = append(problems, fmt.Sprintf("launch %q is not a launch shape (%s, %s)",
			s.Launch, LaunchLinuxSystemd, LaunchWindowsWSL))
	}
	req(s.TensorParallel >= 0, fmt.Sprintf("tensor_parallel %d cannot be negative", s.TensorParallel))
	// The device list and the parallel width are two statements of the same fact, and
	// a tier that lets them disagree ships an engine that ignores a card or refuses to
	// start. Check them against each other rather than trusting the author.
	if n := len(s.devices()); n > 0 && s.tensorParallel() != n {
		problems = append(problems, fmt.Sprintf(
			"device %q names %d card(s) but tensor_parallel is %d — the engine would %s",
			s.Device, n, s.tensorParallel(),
			map[bool]string{true: "load the whole model onto the first card and ignore the rest",
				false: "refuse to start"}[s.tensorParallel() < n]))
	}
	if s.CacheServer != nil {
		if err := s.CacheServer.Validate(); err != nil {
			problems = append(problems, err.Error())
		}
	}
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
	vllm := r.hostPath(filepath.Join(r.VenvDir, "bin", "vllm"))
	if fi, err := os.Stat(vllm); err != nil || fi.IsDir() {
		return false, "no vllm entry point at " + filepath.ToSlash(vllm)
	}
	got, err := s.Resolve(r)
	if err != nil {
		return false, err.Error()
	}
	if fi, err := os.Stat(r.hostPath(got.ModelPath)); err != nil || !fi.IsDir() {
		return false, "no model snapshot at " + filepath.ToSlash(r.hostPath(got.ModelPath))
	}
	return true, ""
}

// hostPath maps a path the ENGINE will see to one this process can stat.
//
// They are the same path on a Linux node. On a Windows box whose engine lives in WSL
// they are not: the venv and the HF cache are distro paths, and statting "/root/g7/..."
// from Windows reports missing every time — which Detect would report as "build the
// venv", sending an operator to rebuild something that is already there. The distro's
// filesystem is reachable from the host under \\wsl.localhost\<distro>, so that is
// what gets statted. The RENDERED artifacts always carry the distro path, because that
// is what bash inside the distro must open.
func (r Runtime) hostPath(p string) string {
	if r.HostPrefix == "" || !strings.HasPrefix(p, "/") {
		return p
	}
	return r.HostPrefix + filepath.FromSlash(p)
}

// Bindings are the harness config keys this seat derives. Kept beside the seat so a
// tier cannot bind an agent model that no seat serves.
func (s Spec) Bindings() map[string]any {
	out := map[string]any{"agent_model": s.ID}
	if s.AgentCtxTokens > 0 {
		out["agent_ctx_tokens"] = s.AgentCtxTokens
	}
	if b := s.ConfigBlock(); b != nil {
		out["kv_cache_server"] = b
	}
	return out
}

// ConfigBlock is the harness's `kv_cache_server` block DERIVED from this seat, or nil
// when the seat has no cache server.
//
// The harness block and the seat env are two descriptions of one store, and until this
// existed they were maintained by hand in two files — internal/config/kvcacheserver.go
// says so itself ("the seat wrapper reads its own seat.env, so the operator keeps the
// two in agreement"). A chunk size or a key prefix that agrees in one place and not the
// other produces a store that registers cleanly and then serves nothing, which is why
// this derives rather than duplicates.
func (s Spec) ConfigBlock() map[string]any {
	if s.CacheServer == nil {
		return nil
	}
	c := s.CacheServer
	return map[string]any{
		"enabled":       true,
		"store":         c.StoreName(),
		"address":       c.Address,
		"l1_staging_gb": c.EffectiveL1StagingGB(),
		"chunk_size":    c.ChunkSize,
		"key_prefix":    c.KeyPrefix,
		"seat":          s.ID,
	}
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
	cs := s.CacheServer
	if cs == nil {
		// A seat with no cache server still renders every token, as empty. The
		// wrapper reads an empty SEAT_L2 as "no L2 tier" and runs on VRAM plus L1,
		// which is a supported configuration — whereas leaving the tokens in place
		// would trip Artifacts' unsubstituted-token sweep and refuse the render.
		cs = &CacheServer{}
	}
	l2, mountSrc, mountDir, mountOpts := "", "", "", ""
	prune, minMBPS := "", ""
	if s.CacheServer != nil {
		l2 = cs.L2JSON()
		mountSrc, mountDir, mountOpts = r.CacheMountSrc, cs.MountDir, r.CacheMountOpts
		if cs.PruneGB > 0 {
			prune = strconv.Itoa(cs.PruneGB)
		}
		if cs.MinMBPS > 0 {
			minMBPS = strconv.Itoa(cs.MinMBPS)
		}
	}
	return map[string]string{
		"__SEAT_ID__":          s.ID,
		"__POOL_ALIAS__":       pool,
		"__TENSOR_PARALLEL__":  strconv.Itoa(s.tensorParallel()),
		"__MP_PORT__":          strconv.Itoa(s.mpPort()),
		"__LMCACHE_OVERLAY__":  r.LMCacheOverlay,
		"__DISTRO__":           r.Distro,
		"__WSL_SEAT_DIR__":     r.wslSeatDir(),
		"__SEAT_ENV__":         s.ID + ".env",
		"__L1_GB__":            strconv.Itoa(cs.EffectiveL1StagingGB()),
		"__CHUNK__":            strconv.Itoa(cs.ChunkSize),
		"__KEY_PREFIX__":       cs.KeyPrefix,
		"__L2_JSON__":          l2,
		"__MOUNT_SRC__":        mountSrc,
		"__MOUNT_DIR__":        mountDir,
		"__MOUNT_OPTS__":       mountOpts,
		"__PRUNE_GB__":         prune,
		"__MIN_MBPS__":         minMBPS,
		"__EXTRA_ARGS__":       s.extraArgs(),
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

// devices is the CUDA_VISIBLE_DEVICES list, split and trimmed. An empty Device is
// one implicit card, reported as no list so Validate does not fight the default.
func (s Spec) devices() []string {
	if strings.TrimSpace(s.Device) == "" {
		return nil
	}
	var out []string
	for _, d := range strings.Split(s.Device, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// tensorParallel is --tensor-parallel-size, defaulted to one card.
func (s Spec) tensorParallel() int {
	if s.TensorParallel > 0 {
		return s.TensorParallel
	}
	return 1
}

// extraArgs is the engine's per-model argument tail, assembled from the fields that
// already carry those facts so a tier states each of them exactly once.
//
// The two parser flags are NOT optional for an agent seat and they travel together:
// the harness's agent loop sends tool_choice=auto, and vLLM answers 400 unless BOTH
// --enable-auto-tool-choice and a parser are present.
func (s Spec) extraArgs() string {
	args := []string{"--enable-auto-tool-choice"}
	if s.ToolCallParser != "" {
		args = append(args, "--tool-call-parser", s.ToolCallParser)
	}
	if s.ReasoningParser != "" {
		args = append(args, "--reasoning-parser", s.ReasoningParser)
	}
	if s.KVCacheDtype != "" {
		args = append(args, "--kv-cache-dtype", s.KVCacheDtype)
	}
	return strings.Join(args, " ")
}

// mpPort is the LMCache MP server's loopback port, defaulted beside the engine's.
func (s Spec) mpPort() int {
	if s.MPPort > 0 {
		return s.MPPort
	}
	return s.Port - 1
}

// launch is the artifact set this seat is started by, defaulted.
func (s Spec) launch() string {
	if s.Launch != "" {
		return s.Launch
	}
	return LaunchLinuxSystemd
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
	// The two launch shapes differ ONLY here. On Linux the wrappers are executed
	// directly; on Windows llama-swap runs as SYSTEM and a WSL distro belongs to the
	// interactive user, so the wrapper is a PowerShell stub that triggers a scheduled
	// task in the operator's session (SYSTEM and S4U both measured unable to start the
	// distro). Everything below this point is identical either way.
	if s.launch() == LaunchWindowsWSL {
		fmt.Fprintf(&b, "    cmd: %s\n", strconv.Quote(winStub(r.SeatDir, "seat-cmd.ps1", s.ID)))
		fmt.Fprintf(&b, "    cmdStop: %s\n", strconv.Quote(winStub(r.SeatDir, "seat-cmdstop.ps1", s.ID)))
	} else {
		fmt.Fprintf(&b, "    cmd: %s/vllm-seat-cmd.sh\n", r.SeatDir)
		fmt.Fprintf(&b, "    cmdStop: %s/vllm-seat-cmdstop.sh\n", r.SeatDir)
	}
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

// winStub is the llama-swap cmd/cmdStop command line for a Windows/WSL seat: the
// 64-bit PowerShell by absolute path, because llama-swap runs as SYSTEM and its PATH
// is not the operator's — a bare "powershell" resolved to a different host once and
// the seat never started.
func winStub(seatDir, script, seat string) string {
	return `C:/Windows/System32/WindowsPowerShell/v1.0/powershell.exe -NoProfile -ExecutionPolicy Bypass -File ` +
		filepath.ToSlash(filepath.Join(seatDir, script)) + ` ` + seat
}

// TemplatesDir is the reference-template directory for this seat's launch shape.
func (s Spec) TemplatesDir(root string) string {
	return filepath.Join(root, "setup", "templates", "vllm-seat", s.launch())
}

// artifactFiles maps a template file to the name it is installed under. The unit and
// the polkit rule are named after the unit so two seats never collide on one box.
func (s Spec) artifactFiles() map[string]string {
	if s.launch() == LaunchWindowsWSL {
		// The env file carries the seat ID so the two seats a 3-card box may run
		// (the tensor-parallel pair and the opt-in 3-card layout) never overwrite
		// each other's operating point.
		return map[string]string{
			"seat.env":                s.ID + ".env",
			"seat-cmd.ps1":            "seat-cmd.ps1",
			"seat-cmdstop.ps1":        "seat-cmdstop.ps1",
			"hidden.vbs":              "hidden.vbs",
			"register-seat-tasks.ps1": "register-seat-tasks.ps1",
		}
	}
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
	// fp8 KV pages restore CORRUPT through stock LMCache (rank-5 group edit, upstream
	// PR #4253) and nothing reports it: the store logs hits, the engine answers, and
	// the recovered context is wrong. Refuse the combination rather than render a seat
	// whose failure mode is bad text that looks like good text.
	if s.CacheServer != nil && strings.HasPrefix(strings.ToLower(s.KVCacheDtype), "fp8") && r.LMCacheOverlay == "" {
		return nil, fmt.Errorf("vllm seat %s: kv_cache_dtype %q with a cache_server needs an LMCache overlay "+
			"(Runtime.LMCacheOverlay) — stock LMCache restores fp8 pages corrupt (upstream PR #4253) and reports "+
			"success while doing it, so this pair would serve wrong context silently", s.ID, s.KVCacheDtype)
	}
	if s.launch() == LaunchWindowsWSL && r.Distro == "" {
		return nil, fmt.Errorf("vllm seat %s: launch %s needs Runtime.Distro — the wrappers must name the WSL "+
			"distribution the engine runs in", s.ID, LaunchWindowsWSL)
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
