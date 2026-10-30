// Shared media helpers: image model and ComfyUI binding, footprint key, request-param readers.

package pipeline

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/imagegen"
)

// imageModelFromConfig is the ONE mapping from config to the render-model
// binding, shared by the single and batch paths. It exists because the two
// paths each carried their own Model literal and the second silently dropped
// five newly added fields (a lightning4-bound seat rendered the 50-step full
// recipe in batch, review-caught pre-merge 2026-08-10) — a drift class this
// helper deletes. TestImageModelFromConfig reflect-checks that every field
// added to imagegen.Model gets mapped here.

func imageModelFromConfig(cfg config.Config) imagegen.Model {
	m := imagegen.Model{
		Ckpt:         cfg.ImageGenCkpt,
		VAE:          cfg.ImageGenVAE,
		Steps:        cfg.ImageGenSteps,
		CFG:          cfg.ImageGenCFG,
		Sampler:      cfg.ImageGenSampler,
		Scheduler:    cfg.ImageGenScheduler,
		ReserveVRAM:  cfg.ImageGenReserveVRAM,
		Family:       cfg.ImageGenFamily,
		Preset:       cfg.ImageGenPreset,
		CLIP:         cfg.ImageGenCLIP,
		LoRA:         cfg.ImageGenLoRA,
		LoRAStrength: cfg.ImageGenLoRAStrength,
		Shift:        cfg.ImageGenShift,
		PoolVvramGB:  cfg.ImageGenPoolVvramGB,
		PoolCompute:  cfg.ImageGenPoolCompute,
		PoolDonor:    cfg.ImageGenPoolDonor,
		Schedule:     cfg.ImageGenSchedule,
		Upsampler:    cfg.UpsamplerFile(),
		Placement:    cfg.ImageGenPlacement,
		// A pooled seat is placed by its pool keys; the device pin is for the
		// single-card shape only (see comfyLaunch).
		Launch: comfyLaunch(cfg, !cfg.ImagePooled()),
	}
	if cfg.ImageMulticard() {
		// The card-only placement is another placement than a pool: the recipe places the DiT itself on the lease's two cards, so the pool keys are not
		// passed (the runner refuses them beside it) and neither is the single-card launch (mediamulticard.go).
		m.PoolVvramGB, m.PoolCompute, m.PoolDonor = 0, "", ""
		m.Launch = multicardLaunch(cfg)
	}
	return m
}

// comfyLaunch maps a binding's comfy_* keys onto the runner's launch profile.
//
// singleCard decides whether comfy_cuda_device applies. It is true for the routes that
// render on ONE card — image generation when the binding does not pool, generative
// edit, upscale, inpaint, animate, music, and (since the 2026-09-24 A/B measurement
// below) video generation when the binding does not pool — and false for:
//   - a pooled image/video seat: its pool keys name the cards, and the blackwell-3x16
//     video pool must COMPUTE on cuda:0 (ComfyUI-MultiGPU #220: an int8 DiT cannot
//     compute on a non-default device), which a --cuda-device pin would hide;
//   - run-graph: the caller's graph owns its placement.
//
// generate_video used to NEVER take the pin, pooled or not, on the theory that "every
// seeded video seat is pooled." That was false on the blackwell-3x16 tier's own
// ComfyUI device ordering: an un-pooled LTX-2.5 render (videogen_pool_vvram_gb=0,
// comfy_dynamic_vram=on) measured landing on ComfyUI's DEFAULT device, which on this
// box's 3-card enumeration is nvidia-smi index 1 — the DISPLAY card, not card 2 as
// comfy_cuda_device="2" intends (15.7 GiB loaded onto it, confirmed via nvidia-smi;
// pinned to card 2 correctly once this fix landed). runGenerateVideo now passes
// !cfg.VideoPooled(), matching the image route's !cfg.ImagePooled() (pinned by
// TestLaunchProfileReachesEveryComfyRouteAndThePinOnlyTheSingleCardOnes): a pooled
// video seat still owns its placement through its pool keys (unaffected, still
// false); an un-pooled one now takes the pin like every other single-card route.
//
// Dynamic VRAM and the extra args are launch-wide and apply to every ComfyUI route.
func comfyLaunch(cfg config.Config, singleCard bool) imagegen.ComfyLaunch {
	l := imagegen.ComfyLaunch{DynamicVRAM: cfg.ComfyDynamicVRAM, ExtraArgs: cfg.ComfyExtraArgs}
	if singleCard {
		l.CudaDevice = strings.ReplaceAll(cfg.ComfyCudaDevice, " ", "")
	}
	return l
}

// comfyGenEnv is genEnv plus the launch profile, for the ComfyUI-backed runners that
// build their env from genEnv (video, animate, music).
func (p *Pipeline) comfyGenEnv(singleCard bool) []string {
	return append(p.genEnv(), comfyLaunch(p.cfg, singleCard).Env()...)
}

// comfyGenEnvFor is comfyGenEnv for a call that holds a grant: on a per-card instance the launch
// profile names the instance and the card.
func (p *Pipeline) comfyGenEnvFor(g mediaGrant, singleCard bool) []string {
	return append(p.genEnv(), g.launch(comfyLaunch(p.cfg, singleCard)).Env()...)
}

// declaredDevices reads the cards an operator declared for a call (run-graph "devices"): a list,
// or a comma-separated string, of nvidia-smi indices or UUID prefixes. Absent = none declared.
func declaredDevices(params map[string]any) []string {
	var raw []string
	switch v := params["devices"].(type) {
	case string:
		raw = strings.Split(v, ",")
	case []string:
		raw = v
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				raw = append(raw, s)
			}
		}
	}
	var out []string
	for _, d := range raw {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// imageFootprintKey is this box's image-render footprint identity: the
// configured imagegen_family (else the script's SDXL default), with quant
// "bf16" only for the HiDream-O1 checkpoint binding (the bf16 recipe) — every
// other binding is "node default" per the contract.
func imageFootprintKey(cfg config.Config) (family, quant string) {
	family = cfg.ImageGenFamily
	// J2 sdcpp engine: family still comes from imagegen_family (per-machine truth);
	// with no family set, "sdcpp" keeps its ledger tier distinct from the ComfyUI
	// SDXL default. Quant is read from the bound model's filename (GGUF quants
	// encode it: ...-Q8_0.gguf / Q4_K...), else node default.
	if cfg.ImageGenEngine == "sdcpp" {
		if family == "" {
			family = "sdcpp"
		}
		// Basename only (a "models-bf16" DIR must not hit) and longest-token-first
		// (BF16 before F16, Q4_K_M before Q4_K) so subsets never shadow supersets.
		up := strings.ToUpper(filepath.Base(cfg.SdcppModel))
		for _, q := range []string{"Q8_0", "Q6_K", "Q5_K_M", "Q5_K_S", "Q5_K", "Q5_1", "Q4_K_M", "Q4_K_S", "Q4_K", "Q4_1", "Q4_0", "Q3_K", "Q2_K", "BF16", "F16"} {
			if strings.Contains(up, q) {
				quant = strings.ToLower(q)
				break
			}
		}
		return family, quant
	}
	if family == "" {
		family = "sdxl"
	}
	if strings.HasPrefix(family, "hidream-o1") {
		quant = "bf16"
	}
	// qwen-image-2.1 ships in several precisions (bf16, int8_convrot, community
	// fp8/nvfp4/int4) whose VRAM peaks differ ~2x; one footprint bucket would average
	// them into a number true of neither. The precision is read from the bound DiT's
	// basename, most specific token first.
	if family == config.FamilyQwenImage21 {
		base := strings.ToLower(filepath.Base(cfg.ImageGenCkpt))
		for _, q := range []string{"nvfp4", "int8", "int4", "bf16", "fp16", "fp8"} {
			if strings.Contains(base, q) {
				quant = q
				break
			}
		}
	}
	// The card-only placement peaks per card and its host growth is the read-only loader's: a bucket of its own, not the single-card one it would pull toward
	// numbers true of neither (the VRAM peak, and the host p90 a single-card declaration is raised to).
	if cfg.ImageMulticard() {
		if quant == "" {
			quant = "multicard"
		} else {
			quant += "+multicard"
		}
	}
	return family, quant
}

// mintSeed returns a random positive seed (1..1e9) so an unspecified-seed render is still
// reproducible — the value is threaded into the render and reported back to the caller.
func mintSeed() int {
	n, err := crand.Int(crand.Reader, big.NewInt(1_000_000_000))
	if err != nil {
		return 1
	}
	return int(n.Int64()) + 1
}

// paramIntOr reads an int param (int / int64 / float64), or def if absent.
func paramIntOr(p map[string]any, k string, def int) int {
	switch n := p[k].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return def
}

// sanitizeStem replaces path separators and Windows-illegal filename characters
// with '_' so a media file always writes cleanly regardless of the source name.
func sanitizeStem(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, s)
}

// preview returns roughly the first n bytes of s trimmed at a word boundary,
// with an ellipsis when truncated — a cheap, deterministic gist (no model call).
// It is rune-safe: n may land mid-rune (e.g. a Spanish á/ñ), so any trailing
// partial UTF-8 rune is trimmed before returning.
func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	for len(cut) > 0 && !utf8.ValidString(cut) { // drop a split multibyte rune
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut) + "…"
}

// paramBool reads a bool param (JSON decodes to bool; tolerate "true").
func paramBool(p map[string]any, k string) bool {
	switch v := p[k].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}

// paramStr reads a string param.
func paramStr(p map[string]any, k string) string {
	if v, ok := p[k].(string); ok {
		return v
	}
	return ""
}

// sha256hex returns the hex-encoded SHA-256 of s (used to fold an image into the
// vision cache key without storing the whole data URI).
func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
