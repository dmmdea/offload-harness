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
	"math/bits"
	"regexp"
	"sort"
	"strconv"
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
	// KindRKLLM is an LLM/VLM seat served by the Rockchip RKLLM runtime on the NPU
	// (accelerators/rknpu/rkllm_server.py behind a launcher script) rather than by
	// llama-server. The rockchip-rk3588 tier declares one beside the llama.cpp Vulkan GPU
	// entry its own template serves: two accelerators, one shared RAM budget. It is a
	// CHAT seat first; with a vision_encoder it also answers image questions and binds
	// vision_model. See Seat.VisionEncoder / Seat.CPUMask.
	KindRKLLM = "rkllm"
)

// The task names a seat may declare in Seat.Tasks. The first three are the fleet vision
// lane's single-image tasks; classify and extract are the text tasks the fleet text lane
// serves. Bindings writes the vision subset as vision_tasks and the text subset as text_tasks.
// summarize and triage are not declarable: the RKLLM runtime returns no logprobs, so the
// decision-margin gate cannot run, and blind quality on the reference seat was 0/4 for each.
const (
	TaskVQA         = "vqa"
	TaskOCR         = "ocr"
	TaskAssessImage = "assess_image"
	TaskClassify    = "classify"
	TaskExtract     = "extract"
)

// visionTaskOrder is the canonical order of the vision lane's tasks: the order vision_tasks is
// written in, whatever order a tier declared them, so the same declaration always renders the
// same config.
var visionTaskOrder = []string{TaskVQA, TaskOCR, TaskAssessImage}

// textTaskOrder is the canonical order of the text lane's tasks: the order text_tasks is written
// in, whatever order a tier declared them.
var textTaskOrder = []string{TaskClassify, TaskExtract}

// knownTasks is every name Seat.Tasks accepts.
var knownTasks = map[string]bool{
	TaskVQA: true, TaskOCR: true, TaskAssessImage: true, TaskClassify: true, TaskExtract: true,
}

// What an rkllm seat runs when the tier names no launcher or CPU mask.
const (
	// DefaultRKLLMBin is the launcher the rknpu accelerator ships beside its sidecar. It rides
	// __RKNPU_HOME__, the token the sidecar's own command uses, so one RKNPU_HOME moves both.
	DefaultRKLLMBin = "__RKNPU_HOME__/rkllm-serve.sh"
	// DefaultRKLLMCPUMask is the RK3588's A55 cluster (cpu0-3): the runtime's host
	// threads stay off the A76 cores until the operator chooses otherwise, which is
	// what keeps the box's other workload responsive.
	DefaultRKLLMCPUMask = "0x0f"
	// rkllmMinCPUs is the fewest CPUs an rkllm seat may enable. The RKLLM runtime
	// REFUSES to start with fewer enabled CPUs than the SoC has NPU cores (3 on the
	// RK3588): "The number of enabled CPUs must be greater than or equal to the number
	// of NPU cores."
	rkllmMinCPUs = 3
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
	// Extra marks a REGISTERED EXTRA: a seat the tier renders into llama-swap and serves by
	// its name and aliases, but that is NOT the route's binding. BindingKey() answers ""
	// for it, so Bindings writes no config key for it and the one-writer check does not
	// count it: a tier can serve a second vision-class model (a small screenshot reader, a
	// general VLM beside the OCR specialist) while the first vision seat keeps
	// vision_model. Nothing routes to an extra by default; a caller reaches it by naming
	// the seat or one of its aliases. Allowed on vision, ocr and stt seats (an rkllm seat
	// writes unconstrained_seats and text_tasks that this flag would not suppress), and
	// it takes no Tasks (they are read only through the vision binding the extra lacks).
	Extra bool `json:"extra,omitempty"`
	// ChatTemplate names a template file in the models dir, rendered as
	// --chat-template-file (vision/ocr only). PaddleOCR-VL ships its own
	// chat_template.jinja and produces degraded transcription without it —
	// exactly the silent-quality-loss shape that must be declared, not hand-wired.
	ChatTemplate string `json:"chat_template,omitempty"`
	// Temp pins the seat's server-side sampling temperature (vision/ocr only).
	// A POINTER so an explicit 0 — PaddleOCR-VL's vendor-required value — survives
	// JSON omitempty; nil = no --temp flag, the server's default stands.
	Temp *float64 `json:"temp,omitempty"`
	// TopP and TopK complete a vendor sampler recipe (vision/ocr only). Pointers for
	// the same reason as Temp. Qwen3-VL ships an official 0.7 / 0.8 / 20 recipe, and
	// a VLM run off its own sampler is a quality change nobody measured.
	TopP *float64 `json:"top_p,omitempty"`
	TopK *int     `json:"top_k,omitempty"`

	// SplitMode is llama.cpp's -sm for a seat too large for one card: "layer" or
	// "tensor". Empty = no flag, which is right for every single-card seat.
	//
	// The two are NOT interchangeable. Measured on the reference 5060 Ti pair
	// (2026-09-05 CUDA-X): moving Qwen3-VL-32B from -sm layer to -sm tensor at the
	// same --tensor-split produced BYTE-IDENTICAL output on a fixed image+prompt 5/5
	// while raising generation 19.6 -> 34.1 t/s; prefill went 995 -> 926. Identical
	// output is what makes it a free win rather than a quality trade.
	SplitMode string `json:"split_mode,omitempty"`
	// TensorSplit is llama.cpp's --tensor-split proportion list ("25,25"), in the
	// order of the seat's own visible devices. It is meaningless without SplitMode,
	// and the proportions are a per-BOX measurement: they encode which card is
	// already carrying residents or a desktop, so copying a ratio between tiers with
	// different layouts is how a seat ends up lopsided.
	TensorSplit string `json:"tensor_split,omitempty"`
	// ImageMinTokens is llama-server's --image-min-tokens (vision/ocr only): the
	// FLOOR an image expands to, where ImageMaxTokens is the ceiling. A large VLM
	// given too few visual tokens answers confidently off a thumbnail; the reference
	// vision seat pins 1024.
	ImageMinTokens int `json:"image_min_tokens,omitempty"`

	// VisionEncoder is the separate vision model an rkllm seat pairs with its LLM (a
	// .rknn under the models dir), rendered as --vision-encoder. With one the seat is a
	// VLM and binds vision_model; without, it is text-only. rkllm only.
	VisionEncoder string `json:"vision_encoder,omitempty"`
	// CPUMask is the hex mask of CPUs the RKLLM runtime may use ("0x0f" = cpu0-3, the
	// RK3588's A55 cluster; the default). The mask is per seat because it is a
	// measured trade on a big.LITTLE part: on the reference board (Qwen3.5-0.8B W8A8, a
	// 1,256-token prompt, mainline kernel) 0x0f prefills at 44.5 tok/s and decodes at
	// 9.07, 0xf0 (the A76 cluster) at 174.5 / 16.67 — while the box's other workload
	// keeps whichever cores are left. At least 3 bits must be set (rkllmMinCPUs). rkllm
	// only.
	CPUMask string `json:"cpu_mask,omitempty"`
	// RepeatPenalty is the repeat penalty an rkllm seat applies to a request that names none
	// (rendered as --repeat-penalty; rkllm only). A POINTER so the tier can tell "not set" (the
	// server's 1.0 = off stands, and no flag renders) from a value. It is a seat default, not a
	// lock: a request that sends its own repeat_penalty wins. Measured on the reference RK3588
	// board (2026-09-30): greedy decoding at 1.0 loops on VQA until the 256-token
	// cap (the vision lane defers "vision output truncated"), while 1.1 answered 3 of 4 blind VQA
	// questions. Range 0.01..10, the range the server accepts per request.
	RepeatPenalty *float64 `json:"repeat_penalty,omitempty"`

	// Tasks is the task set this seat declares it serves: any of vqa, ocr, assess_image (the
	// fleet vision lane's tasks) and classify, extract (text tasks, accepted now for the door
	// that will read them). Empty = unrestricted, which is today's behaviour. The declaration is
	// honest about what the RUNTIME can do: the RKLLM runtime cannot constrain sampling, so a
	// grammar-carrying task (assess_image always sends one) would be refused with a 400 after
	// the node had already taken the job. Bindings writes the vision subset as the node's
	// vision_tasks, which the node enforces at ack time and publishes in health, and which the
	// delegator's placement reads to keep such a task off the seat. Valid on vision and rkllm
	// seats only; a vision task needs a seat that reads images.
	Tasks []string `json:"tasks,omitempty"`

	// Measured records what measured this seat -- the box, the build, the bake-off and
	// the numbers -- so a reader never has to take the roster on faith and a future
	// edit can see what it would be overturning. It is data, not configuration: it
	// renders nothing. Tiers carried the key before the field existed, so the records
	// were parsed and silently dropped.
	Measured string `json:"measured,omitempty"`
}

// BindingKey is the harness config field this seat writes, "" when it writes none.
// A vision, stt or ocr seat binds the field of its kind. An rkllm seat is a chat
// model first — model and triage_model stay the tier's config_seed to name, as for
// any tier — and binds vision_model only when it carries a vision encoder, because
// only then can it answer an image question; binding it without one would advertise
// a route the seat cannot serve. A registered extra (Seat.Extra) binds nothing whatever its
// kind: it is rendered and reachable by name, never the route's default.
func (s Seat) BindingKey() string {
	if s.Extra {
		return ""
	}
	switch s.Kind {
	case KindVision:
		return "vision_model"
	case KindSTT:
		return "stt_model"
	case KindOCR:
		return "ocr_model"
	case KindRKLLM:
		if s.VisionEncoder != "" {
			return "vision_model"
		}
	}
	return ""
}

// EffectiveBin is the executable the seat runs: its own bin, or for an rkllm seat
// that names none the launcher the rknpu accelerator ships. The renderer and the
// "does this seat need the install home" check both read it, so a default that
// carries __RKNPU_HOME__ can never render without a home to expand it against.
func (s Seat) EffectiveBin() string {
	if s.Bin == "" && s.Kind == KindRKLLM {
		return DefaultRKLLMBin
	}
	return s.Bin
}

// EffectiveCPUMask is the mask an rkllm seat is started with: its own, or the A55
// cluster default. "" for every other kind, which has no CPU mask.
func (s Seat) EffectiveCPUMask() string {
	if s.Kind != KindRKLLM {
		return ""
	}
	if s.CPUMask == "" {
		return DefaultRKLLMCPUMask
	}
	return s.CPUMask
}

// VisionTasks is the vision subset of the seat's declared Tasks, in canonical order (vqa, ocr,
// assess_image) and de-duplicated. nil when the seat declares no vision task, which the node
// reads as "serves all three" — today's behaviour.
func (s Seat) VisionTasks() []string {
	var out []string
	for _, t := range visionTaskOrder {
		for _, d := range s.Tasks {
			if d == t {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// TextTasks is the text subset of the seat's declared Tasks, in canonical order (classify,
// extract) and de-duplicated. nil when the seat declares no text task, which the node reads as
// "no text lane": unlike the vision lane the text lane is dark unless declared.
func (s Seat) TextTasks() []string {
	var out []string
	for _, t := range textTaskOrder {
		for _, d := range s.Tasks {
			if d == t {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// UnconstrainedNames is every model id an rkllm seat answers to (its name and its aliases): the
// ids the node's unconstrained_seats lists, because a cascade model configured by alias reaches
// the same runtime, which refuses a grammar whichever name it was asked by. nil for any other kind.
func (s Seat) UnconstrainedNames() []string {
	if s.Kind != KindRKLLM {
		return nil
	}
	return append([]string{s.Name}, s.Aliases...)
}

// Bindings is the config fragment a tier's seats produce. This is the ONLY
// writer of these keys — a seed that also sets them by hand is refused, because
// two writers is exactly how the seat and its binding drifted apart before.
//
// vision_tasks rides with vision_model: the seat that binds the vision route also says which
// of the three vision tasks it serves, and only when it declares a vision subset (a seat that
// declares none leaves the key absent, so the node serves all three).
//
// Every rkllm seat, bound to a route or not, also writes unconstrained_seats (its model ids):
// the runtime cannot constrain decoding, so the node's own pipeline must not send it a grammar.
// text_tasks is the seat's declared text subset (classify, extract); absent = no fleet text lane.
func Bindings(seats []Seat) map[string]any {
	out := map[string]any{}
	var unconstrained []string
	for _, s := range seats {
		unconstrained = append(unconstrained, s.UnconstrainedNames()...)
		if tasks := s.TextTasks(); len(tasks) > 0 {
			out["text_tasks"] = tasks
		}
		k := s.BindingKey()
		if k == "" {
			continue
		}
		out[k] = s.Name
		if k == "vision_model" {
			if tasks := s.VisionTasks(); len(tasks) > 0 {
				out["vision_tasks"] = tasks
			}
		}
	}
	if len(unconstrained) > 0 {
		out["unconstrained_seats"] = unconstrained
	}
	return out
}

// BoundKeys is every config key seats may write, for the seed validator.
func BoundKeys() []string {
	return []string{"ocr_model", "stt_model", "text_tasks", "unconstrained_seats", "vision_model", "vision_tasks"}
}

// Validate rejects a seat set at AUTHORING time — in a test over the committed
// tier table — rather than on someone's machine, where the symptom is a service
// that will not start or a route that quietly defers.
func Validate(seats []Seat, tier string) error {
	var problems []string
	seen := map[string]bool{}
	// The seats that write each config key. The cap is per KEY, not per kind: a vision
	// seat and an rkllm seat with a vision encoder both write vision_model.
	writers := map[string][]string{}

	for i, s := range seats {
		where := fmt.Sprintf("seat %d", i)
		if s.Name != "" {
			where = fmt.Sprintf("seat %q", s.Name)
		}
		if s.Extra && s.Kind == KindRKLLM {
			problems = append(problems, where+": extra is not available on an rkllm seat — it still writes "+
				"unconstrained_seats and text_tasks, so the seat would not be the bind-free extra it claims to be")
		}
		switch s.Kind {
		case KindVision, KindSTT, KindOCR, KindRKLLM:
			if k := s.BindingKey(); k != "" {
				writers[k] = append(writers[k], where)
			}
			// text_tasks is one node key too: two seats declaring text tasks would leave it decided
			// by slice order.
			if len(s.TextTasks()) > 0 {
				writers["text_tasks"] = append(writers["text_tasks"], where)
			}
		case "":
			problems = append(problems, where+": no kind (want "+KindVision+", "+KindSTT+", "+KindOCR+" or "+KindRKLLM+")")
		default:
			problems = append(problems, fmt.Sprintf("%s: unknown kind %q (want %s, %s, %s or %s)", where, s.Kind, KindVision, KindSTT, KindOCR, KindRKLLM))
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
			if s.ImageMinTokens > 0 && s.ImageMaxTokens > 0 && s.ImageMinTokens > s.ImageMaxTokens {
				problems = append(problems, fmt.Sprintf("%s: image_min_tokens %d exceeds image_max_tokens %d — the floor "+
					"cannot be above the ceiling", where, s.ImageMinTokens, s.ImageMaxTokens))
			}
		}
		// -sm / --tensor-split belong to any seat too large for one card. They are
		// checked for every kind because getting them wrong does not fail loudly: a
		// --tensor-split without -sm is ignored, and a proportion list that does not
		// match the seat's device count silently lands the model unevenly.
		if s.SplitMode != "" && s.SplitMode != "layer" && s.SplitMode != "tensor" {
			problems = append(problems, fmt.Sprintf("%s: split_mode %q is not layer or tensor", where, s.SplitMode))
		}
		if s.TensorSplit != "" {
			if s.SplitMode == "" {
				problems = append(problems, where+": tensor_split without split_mode — llama.cpp ignores the proportions "+
					"unless -sm asks for a split, so the seat would silently land on one card")
			}
			parts := strings.Split(s.TensorSplit, ",")
			for _, v := range parts {
				if strings.TrimSpace(v) == "" {
					problems = append(problems, fmt.Sprintf("%s: tensor_split %q has an empty proportion", where, s.TensorSplit))
					break
				}
			}
			// The proportion list is positional over the seat's OWN visible devices,
			// so a mismatch puts weights where the author did not mean.
			if n := seatDeviceCount(s); n > 0 && n != len(parts) {
				problems = append(problems, fmt.Sprintf("%s: tensor_split lists %d proportions but the seat makes %d device(s) "+
					"visible — the list is positional over the seat's own devices", where, len(parts), n))
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
		if s.Kind == KindRKLLM {
			// The RKLLM runtime takes a model, an optional vision encoder, a window and a
			// CPU mask. Every llama-server / whisper-server / device-pin knob is one the
			// renderer would silently drop, so a tier author who sets one is refused.
			if s.MMProj != "" || s.VADModel != "" || s.LibDir != "" || s.ImageMaxTokens > 0 || s.ImageMinTokens > 0 ||
				s.NoContextShift || s.NoMmprojOffload || s.NoFlashAttn || s.ChatTemplate != "" || s.Temp != nil ||
				s.TopP != nil || s.TopK != nil || s.SplitMode != "" || s.TensorSplit != "" || len(s.GPUEnv) > 0 {
				problems = append(problems, where+": mmproj/vad_model/lib_dir/image_*_tokens/no_*/chat_template/temp/top_p/top_k/"+
					"split_mode/tensor_split/gpu_env are llama-server or whisper-server settings and are ignored on an rkllm seat "+
					"(the RKLLM runtime takes model, vision_encoder, ctx_size, cpu_mask and, as a seat default, repeat_penalty)")
			}
			if s.CtxSize <= 0 {
				problems = append(problems, where+": an rkllm seat needs its own ctx_size — the runtime is started with it")
			}
			if err := checkCPUMask(s.CPUMask); err != nil {
				problems = append(problems, where+": "+err.Error())
			}
			if s.RepeatPenalty != nil && !(*s.RepeatPenalty >= minRepeatPenalty && *s.RepeatPenalty <= maxRepeatPenalty) {
				problems = append(problems, fmt.Sprintf("%s: repeat_penalty %v is outside [%v, %v] — the server refuses to start "+
					"with a --repeat-penalty outside the range it accepts per request", where, *s.RepeatPenalty, minRepeatPenalty, maxRepeatPenalty))
			}
		} else if s.VisionEncoder != "" || s.CPUMask != "" || s.RepeatPenalty != nil {
			problems = append(problems, where+": vision_encoder/cpu_mask/repeat_penalty are rkllm-only and are ignored on a "+s.Kind+" seat")
		}
		problems = append(problems, checkTasks(s, where)...)
		for field, v := range map[string]string{"model": s.Model, "mmproj": s.MMProj, "vad_model": s.VADModel, "vision_encoder": s.VisionEncoder, "bin": s.Bin, "lib_dir": s.LibDir} {
			if strings.Contains(strings.ToLower(v), ".exe") {
				problems = append(problems, fmt.Sprintf("%s: %s carries a literal \".exe\" — use the __EXE__ token so the tier renders on every OS", where, field))
			}
		}
		// Only bin/lib_dir are resolved against the install root. The model fields are
		// relative to the models dir, so a home token there renders nowhere and would
		// die at the token guard with no hint as to which field caused it.
		for field, v := range map[string]string{"model": s.Model, "mmproj": s.MMProj, "vad_model": s.VADModel, "vision_encoder": s.VisionEncoder, "chat_template": s.ChatTemplate} {
			if strings.Contains(v, "__OFFLOAD_HOME__") {
				problems = append(problems, fmt.Sprintf("%s: %s is relative to the models dir and may not carry __OFFLOAD_HOME__", where, field))
			}
			if strings.Contains(v, "__RKNPU_HOME__") {
				problems = append(problems, fmt.Sprintf("%s: %s is relative to the models dir and may not carry __RKNPU_HOME__", where, field))
			}
		}
		if s.Temp != nil && (*s.Temp < 0 || *s.Temp > 2) {
			problems = append(problems, fmt.Sprintf("%s: temp %v is outside [0, 2] — an out-of-range pin renders a server "+
				"that refuses to start or samples garbage", where, *s.Temp))
		}
	}
	for _, k := range BoundKeys() {
		if len(writers[k]) > 1 {
			problems = append(problems, fmt.Sprintf("%d seats (%s) write %q: it is a single config field, so a tier may declare at most one",
				len(writers[k]), strings.Join(writers[k], ", "), k))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("tier %q media_seats:\n  - %s", tier, strings.Join(problems, "\n  - "))
	}
	return nil
}

// The range an rkllm seat's repeat_penalty may take: the one rkllm_server.py accepts for a
// request's repeat_penalty, and so for its --repeat-penalty flag.
const (
	minRepeatPenalty = 0.01
	maxRepeatPenalty = 10.0
)

// checkTasks validates a seat's declared task set. Tasks is read only by the vision and text
// bindings, so it is refused where nothing reads it, and a vision task is refused on
// a seat that cannot read an image: each would otherwise be a declaration that silently does
// nothing, or one that advertises a task the seat cannot serve.
func checkTasks(s Seat, where string) []string {
	if len(s.Tasks) == 0 {
		return nil
	}
	var problems []string
	seen := map[string]bool{}
	hasVision, hasText := false, false
	for _, t := range s.Tasks {
		switch {
		case !knownTasks[t]:
			problems = append(problems, fmt.Sprintf("%s: unknown task %q (want %s, %s, %s, %s or %s)", where, t,
				TaskVQA, TaskOCR, TaskAssessImage, TaskClassify, TaskExtract))
		case seen[t]:
			problems = append(problems, fmt.Sprintf("%s: task %q is listed twice", where, t))
		}
		seen[t] = true
		for _, v := range visionTaskOrder {
			if t == v {
				hasVision = true
			}
		}
		for _, v := range textTaskOrder {
			if t == v {
				hasText = true
			}
		}
	}
	switch {
	case s.Kind == KindSTT || s.Kind == KindOCR:
		problems = append(problems, where+": tasks are read only for a vision or rkllm seat and are ignored on a "+s.Kind+" seat")
	case hasText && s.Kind != KindRKLLM:
		problems = append(problems, where+": a text task (classify/extract) is served by the node's own cascade on an unconstrained seat, "+
			"which only an rkllm seat is; this "+s.Kind+" seat constrains decoding and needs no declaration")
	case hasVision && s.Extra:
		problems = append(problems, where+": an extra seat binds no route, so a declared vision task (vqa/ocr/assess_image) "+
			"would be read by nothing — tasks ride the vision_model binding an extra does not write")
	case hasVision && s.BindingKey() != "vision_model":
		problems = append(problems, where+": a vision task (vqa/ocr/assess_image) needs a seat that reads images — "+
			"an rkllm seat needs a vision_encoder; this seat binds no vision_model, so the declaration would advertise a task it cannot serve")
	case s.BindingKey() == "vision_model" && !hasVision:
		problems = append(problems, where+": this seat binds vision_model but its tasks name no vision task (vqa/ocr/assess_image) — "+
			"the node would still serve all three, the opposite of the declaration")
	}
	return problems
}

// checkCPUMask validates an rkllm seat's cpu_mask: empty is the default, otherwise a
// 0x-prefixed hex mask that fits RKLLM's uint32 enabled_cpus_mask and enables at
// least rkllmMinCPUs CPUs. The value is rendered into the seat's command line, so a
// strict shape is also what keeps anything but hex digits out of it.
func checkCPUMask(mask string) error {
	if mask == "" {
		return nil
	}
	digits, ok := strings.CutPrefix(mask, "0x")
	if !ok {
		digits, ok = strings.CutPrefix(mask, "0X")
	}
	if !ok || digits == "" {
		return fmt.Errorf("cpu_mask %q is not a hex mask (want e.g. %s)", mask, DefaultRKLLMCPUMask)
	}
	n, err := strconv.ParseUint(digits, 16, 32)
	if err != nil {
		return fmt.Errorf("cpu_mask %q is not a hex mask that fits RKLLM's 32-bit enabled_cpus_mask", mask)
	}
	if bits.OnesCount32(uint32(n)) < rkllmMinCPUs {
		return fmt.Errorf("cpu_mask %q enables %d CPU(s), fewer than the %d the RKLLM runtime demands (it refuses to start with "+
			"fewer enabled CPUs than NPU cores)", mask, bits.OnesCount32(uint32(n)), rkllmMinCPUs)
	}
	return nil
}

// seatDeviceCount reports how many CUDA devices the seat's own gpu_env makes visible,
// or 0 when it names none (then the tier-level gpu_env decides and this package
// cannot know). Used to check a --tensor-split list against the devices it indexes.
func seatDeviceCount(s Seat) int {
	for _, e := range s.GPUEnv {
		v, ok := strings.CutPrefix(strings.TrimSpace(e), "CUDA_VISIBLE_DEVICES=")
		if !ok {
			continue
		}
		if strings.TrimSpace(v) == "" {
			return 0
		}
		return len(strings.Split(v, ","))
	}
	return 0
}
