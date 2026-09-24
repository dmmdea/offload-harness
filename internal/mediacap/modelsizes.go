package mediacap

// knownModelSizes pins the exact byte size of every model file this harness's own
// installer (setup/install.ps1's $PINNED models section — Hugging Face resolve
// URLs, byte size recorded at pin time alongside the sha256) downloads under a
// fixed name. resolveBinding below uses it to catch a file that is only PARTWAY
// copied into place: a bare os.Stat reports FOUND the instant a same-named file of
// ANY size exists in the right class directory, which is exactly what happened on
// the Qube during the 2026-09-23 media-route remediation — doctor printed
// `animate_character: OK CONFIGURED` while a 16.65 GB unet was still ~70% written
// (`internal/mediacap/modelbindings.go`'s resolveBinding, register F-38).
//
// A file this map does not name — the overwhelming majority of ComfyUI-class
// weights on the fleet, sourced ad hoc from a peer box rather than this installer
// — keeps the pre-existing exists-only verdict: there is no known-good size to
// compare it against, and doctor must never fabricate one. This is deliberately
// NOT a hash: comparing a size is one already-open os.Stat's Size() field, free;
// hashing a multi-GB file on every doctor run is the opposite of what doctor is
// for (register: the operator's "never hash multi-GB files in doctor by default,
// keep it fast" instruction, F-38).
//
// Keep this in sync with setup/install.ps1's $PINNED whenever a pinned model's
// size changes (a version bump) — TestKnownModelSizesMatchInstaller parses that
// file directly and fails if the two disagree on any name both claim to track.
var knownModelSizes = map[string]int64{
	"z_image_turbo-Q8_0.gguf":            7224707136,
	"Qwen3-4B-Instruct-2507-Q4_K_M.gguf": 2497281120,
	"zimage_ae.safetensors":              335304388,
	"v1-5-pruned-emaonly.safetensors":    4265146304,
	"sd_xl_base_1.0.safetensors":         6938078334,
	"sdxl_vae_fp16_fix.safetensors":      334641162,
}
