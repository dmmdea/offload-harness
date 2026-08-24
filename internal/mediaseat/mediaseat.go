// Package mediaseat is the tier schema's declaration of the ALIAS-backed media
// capabilities a hardware tier serves: a vision (VLM) seat, a speech-to-text
// seat, and an OCR-specialist seat — each rendered into the node's llama-swap
// config AND into the harness config binding that routes to it.
//
// One declaration, two artifacts, on purpose. Before this existed the two were
// authored separately and drifted immediately: config.Default() bound
// stt_model="whisper-stt" while not one of the six serving templates defined a
// whisper seat, so every freshly installed node advertised `stt` to the fleet
// dispatcher and failed its own acceptance gate at the same time. A binding is
// now DERIVED from a seat — a tier that serves STT declares the seat and gets the
// binding; a tier that does not declares neither and the route honestly defers.
// The state "bound to a seat that does not exist" is no longer representable
// from the tier table.
//
// FILE-backed media (image/video/music generation) is deliberately NOT here.
// Those routes are spawn-per-job subprocesses arbitrated by internal/gpulease,
// not llama-swap seats — there is no sd-server client anywhere in this repo, and
// hosting one inside llama-swap would put two residency authorities over one
// card. internal/mediacap derives those; this package derives their counterpart.
package mediaseat

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// safeID is what may become a YAML key and an inline flow-sequence member. It is
// deliberately the same charset the closure gate's roster parser assumes.
var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// The kinds a seat may declare. A closed set: each one has a distinct binary,
// a distinct flag grammar and a distinct config binding, so an unrecognized kind
// is refused rather than rendered into something that looks plausible.
const (
	KindVision = "vision"
	KindSTT    = "stt"
	// KindOCR is a llama-server VLM seat specialized for document OCR, bound to
	// `ocr_model` so it can coexist with the tier's general vision seat. Added for
	// PaddleOCR-VL-class models (measured blackwell-8 2026-08-23: crops-driven,
	// ~2 s answers at 1.8 GB), which need a shipped chat template file and temp 0 —
	// the two knobs this kind carries that plain vision does not.
	KindOCR = "ocr"
)

// Residency is a ROLE, not a group name. Group names are a per-TEMPLATE
// namespace — heavy/support in linux-cuda, offload-family in the win-cuda,
// win-vulkan and win-cpu templates, architect/editor in win-dual-cuda, and none
// at all in win-cuda-resident — while a tier is a HARDWARE class that renders
// into several of them. A tier naming "heavy" would therefore be simultaneously
// valid and invalid for itself. The tier declares what a seat NEEDS; each
// template maps that need onto one of its own groups.
const (
	// Swappable: a seat big enough that only one of its residency class should be
	// on the card at a time.
	Swappable = "swappable"
	// Resident: a small seat that stays co-loaded alongside whatever else is up.
	Resident = "resident"
)

// Seat is one alias-backed media capability a tier serves.
type Seat struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"` // the llama-swap model id AND the config binding value
	Aliases []string `json:"aliases,omitempty"`
	// Model is relative to the node's models dir, matching how every existing
	// seat in the serving templates names its weights.
	Model string `json:"model"`
	// MMProj is the multimodal projector (vision only). A "vision" seat without
	// one is just a chat model that silently answers image questions blind.
	MMProj string `json:"mmproj,omitempty"`
	// VADModel enables voice-activity detection (stt only); empty = no VAD flags.
	VADModel string `json:"vad_model,omitempty"`
	// Bin is the seat's executable when it is NOT llama-server (stt only —
	// whisper.cpp ships its own server). May carry the __OFFLOAD_HOME__ and
	// __EXE__ tokens so one row renders on every OS.
	Bin string `json:"bin,omitempty"`
	// LibDir is the directory holding the seat binary's shared objects. A
	// self-built whisper-server links its own, and without it the process dies at
	// exec with a loader error that reads nothing like a config problem. POSIX
	// only; ignored on Windows.
	LibDir string `json:"lib_dir,omitempty"`
	// CtxSize is the seat's own served window. Vision runs a much smaller window
	// than the chat tier on the same card, so it is per-seat, not inherited.
	CtxSize int `json:"ctx_size,omitempty"`
	// ImageMaxTokens caps the tokens one image may expand into (vision only). This is
	// a VRAM bound, not a quality knob: on the measured 8 GB tier it is what keeps a
	// large screenshot from pushing the seat past the card. 0 = leave it to the server.
	ImageMaxTokens int `json:"image_max_tokens,omitempty"`
	// NoContextShift disables llama-server's context shifting for this seat (vision
	// only). Shifting a window that holds image embeddings is not meaningful.
	NoContextShift bool `json:"no_context_shift,omitempty"`
	// NoFlashAttn passes whisper.cpp's -nfa (stt only). Flash attention is default-ON
	// in whisper.cpp since v1.8.0 and DEGRADES non-English and noisy audio
	// (whisper.cpp #3020), so a tier serving Spanish or field recordings wants it off.
	NoFlashAttn bool `json:"no_flash_attn,omitempty"`
	// NoMmprojOffload keeps the multimodal projector (CLIP) on CPU while the LLM runs
	// on the GPU (vision only). llama.cpp #20081: mmproj on the Vulkan backend is
	// "extremely degraded" for some images, so a Vulkan vision tier decodes the LLM on
	// the GPU but keeps the vision encoder on CPU where it is correct. A no-op on CUDA.
	NoMmprojOffload bool `json:"no_mmproj_offload,omitempty"`
	// GPUEnv is env vars added to THIS seat's env list — a per-seat device pin. The
	// tier-level gpu_env applies to EVERY model uniformly; this is for a seat that must
	// sit on a SPECIFIC device the tier's other models do not (the dual-gpu media seats
	// pin to the editor card; a Vulkan seat pins its ICD).
	GPUEnv    []string `json:"gpu_env,omitempty"`
	Residency string   `json:"residency"`
	TTL       int      `json:"ttl,omitempty"`
	// ChatTemplate names a template file in the models dir, rendered as
	// --chat-template-file (vision/ocr only). PaddleOCR-VL ships its own
	// chat_template.jinja and produces degraded transcription without it —
	// exactly the silent-quality-loss shape that must be declared, not hand-wired.
	ChatTemplate string `json:"chat_template,omitempty"`
	// Temp pins the seat's server-side sampling temperature (vision/ocr only).
	// A POINTER so an explicit 0 — PaddleOCR-VL's vendor-required value — survives
	// JSON omitempty; nil = no --temp flag, the server's default stands.
	Temp *float64 `json:"temp,omitempty"`
}

// configKey is the harness config field a seat of this kind binds.
func configKey(kind string) string {
	switch kind {
	case KindVision:
		return "vision_model"
	case KindSTT:
		return "stt_model"
	case KindOCR:
		return "ocr_model"
	}
	return ""
}

// Bindings is the config fragment a tier's seats produce. This is the ONLY
// writer of these keys — a seed that also sets them by hand is refused, because
// two writers is exactly how the seat and its binding drifted apart before.
func Bindings(seats []Seat) map[string]any {
	out := map[string]any{}
	for _, s := range seats {
		if k := configKey(s.Kind); k != "" {
			out[k] = s.Name
		}
	}
	return out
}

// BoundKeys is every config key seats may write, for the seed validator.
func BoundKeys() []string { return []string{"ocr_model", "stt_model", "vision_model"} }

// Validate rejects a seat set at AUTHORING time — in a test over the committed
// tier table — rather than on someone's machine, where the symptom is a service
// that will not start or a route that quietly defers.
func Validate(seats []Seat, tier string) error {
	var problems []string
	seen := map[string]bool{}
	perKind := map[string]int{}

	for i, s := range seats {
		where := fmt.Sprintf("seat %d", i)
		if s.Name != "" {
			where = fmt.Sprintf("seat %q", s.Name)
		}
		switch s.Kind {
		case KindVision, KindSTT, KindOCR:
			perKind[s.Kind]++
		case "":
			problems = append(problems, where+": no kind (want "+KindVision+", "+KindSTT+" or "+KindOCR+")")
		default:
			problems = append(problems, fmt.Sprintf("%s: unknown kind %q (want %s, %s or %s)", where, s.Kind, KindVision, KindSTT, KindOCR))
		}
		switch {
		case s.Name == "":
			problems = append(problems, where+": no name — the name IS the llama-swap model id and the config binding")
		case !safeID.MatchString(s.Name):
			// A name is written into a YAML key AND into an inline flow sequence, so a
			// comma splits it into a member naming no model (a config llama-swap rejects
			// at startup) and a colon or bracket makes the document malformed outright.
			problems = append(problems, fmt.Sprintf("%s: a seat name must match %s — it becomes a YAML key and "+
				"a group member, where a comma silently splits it and a colon breaks the document", where, safeID))
		case seen[s.Name]:
			problems = append(problems, fmt.Sprintf("%s: declared twice", where))
		}
		seen[s.Name] = true
		for _, a := range s.Aliases {
			if !safeID.MatchString(a) {
				problems = append(problems, fmt.Sprintf("%s: alias %q must match %s", where, a, safeID))
			}
			if seen[a] {
				problems = append(problems, fmt.Sprintf("%s: alias %q collides with another seat name or alias", where, a))
			}
			seen[a] = true
		}
		if s.Model == "" {
			problems = append(problems, where+": no model file")
		}
		if s.Residency != Swappable && s.Residency != Resident {
			problems = append(problems, fmt.Sprintf("%s: residency %q is not %s or %s", where, s.Residency, Swappable, Resident))
		}
		// vision and ocr are both llama-server VLM seats and share the same
		// requirements; ocr additionally may carry chat_template/temp.
		if s.Kind == KindVision || s.Kind == KindOCR {
			if s.MMProj == "" {
				problems = append(problems, where+": a "+s.Kind+" seat needs an mmproj — without one it loads as a text model "+
					"and answers image questions blind instead of failing")
			}
			if s.CtxSize <= 0 {
				problems = append(problems, where+": a "+s.Kind+" seat needs its own ctx_size (it is not the chat tier's)")
			}
			if s.Bin != "" || s.LibDir != "" {
				problems = append(problems, where+": bin/lib_dir are for a seat that is NOT llama-server; a "+s.Kind+" seat "+
					"always runs the template's llama-server and would silently ignore them")
			}
			if s.NoFlashAttn {
				problems = append(problems, where+": no_flash_attn is a whisper.cpp flag (stt only); a "+s.Kind+" seat's "+
					"flash-attn comes from the tier's flash_attn")
			}
		}
		if s.Kind == KindSTT {
			if s.Bin == "" {
				problems = append(problems, where+": an stt seat needs a bin — whisper-server is a separate binary, not llama-server")
			}
			// Fields the renderer would silently ignore. Accepting them would let a
			// tier author believe a knob applies when it does nothing.
			if s.MMProj != "" || s.CtxSize > 0 || s.ImageMaxTokens > 0 || s.NoContextShift || s.NoMmprojOffload {
				problems = append(problems, where+": mmproj/ctx_size/image_max_tokens/no_context_shift/no_mmproj_offload "+
					"are vision/ocr-only and are ignored on an stt seat")
			}
			if s.ChatTemplate != "" || s.Temp != nil {
				problems = append(problems, where+": chat_template/temp are llama-server flags (vision/ocr only) and are "+
					"ignored on an stt seat")
			}
		}
		for field, v := range map[string]string{"model": s.Model, "mmproj": s.MMProj, "vad_model": s.VADModel, "bin": s.Bin, "lib_dir": s.LibDir} {
			if strings.Contains(strings.ToLower(v), ".exe") {
				problems = append(problems, fmt.Sprintf("%s: %s carries a literal \".exe\" — use the __EXE__ token so the tier renders on every OS", where, field))
			}
		}
		// Only bin/lib_dir are resolved against the install root. The model fields are
		// relative to the models dir, so a home token there renders nowhere and would
		// die at the token guard with no hint as to which field caused it.
		for field, v := range map[string]string{"model": s.Model, "mmproj": s.MMProj, "vad_model": s.VADModel, "chat_template": s.ChatTemplate} {
			if strings.Contains(v, "__OFFLOAD_HOME__") {
				problems = append(problems, fmt.Sprintf("%s: %s is relative to the models dir and may not carry __OFFLOAD_HOME__", where, field))
			}
		}
		if s.Temp != nil && (*s.Temp < 0 || *s.Temp > 2) {
			problems = append(problems, fmt.Sprintf("%s: temp %v is outside [0, 2] — an out-of-range pin renders a server "+
				"that refuses to start or samples garbage", where, *s.Temp))
		}
	}
	for _, k := range []string{KindVision, KindSTT, KindOCR} {
		if perKind[k] > 1 {
			problems = append(problems, fmt.Sprintf("%d %s seats: %q is a single config field, so a tier may declare at most one",
				perKind[k], k, configKey(k)))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("tier %q media_seats:\n  - %s", tier, strings.Join(problems, "\n  - "))
	}
	return nil
}
