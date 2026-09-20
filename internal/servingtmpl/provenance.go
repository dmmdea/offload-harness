// provenance.go -- the rendered serving config carries its own provenance
// (plan v2 register K-02).
//
// The problem it closes. The llama-swap config is rendered ONCE, at install,
// from the tier table baked into the binary (`install render`, setup/install.ps1,
// setup/install.sh). Nothing re-renders it afterwards, and nothing recorded what
// it was rendered FROM -- so a tier revision landed in the repo and the box kept
// serving yesterday's config forever. That is not hypothetical: register A-39
// raised ampere-16's ctx_size from 32768 to 131072 and the live file stayed at
// 32768, while `audit-yaml` reported OK the whole time. It reported OK honestly:
// audit.go checks RULES (ttl 300, no -ngl 0, no empty CUDA_VISIBLE_DEVICES, no
// persistent group, no preload), and a config that is a tier revision stale
// breaks no rule. A rule gate cannot see staleness; only a provenance stamp can.
//
// The design is the one internal/agent/props.go already proved for seat pinning:
// a CLOSED, named, documented field set, canonicalised and hashed -- never a
// language-default hasher over a struct, never a hash of the raw bytes. A raw-byte
// hash churns on key order and on fields that cannot change the output; an open
// set silently stops covering whatever is added next. Here the closed set is
// SpecBasis, and TestParamsBasisMirrorsParams fails the build the moment a field
// is added to Params without being added here.
//
// TWO hashes, because they answer different questions:
//
//	spec_sha256 -- over the closed INPUT set (tier id, the render Params, the
//	  template bytes, the tier's own profiles.json entry, the harness version).
//	  It is the file's identity: "this config was rendered from exactly these
//	  inputs". /fleet/health publishes it so a fleet-wide drift check is a
//	  string comparison.
//	body_sha256 -- over the yaml bytes BELOW the stamp. It is what makes a HAND
//	  EDIT distinguishable from a seed change: a hand-edited file has an intact
//	  spec hash over inputs that no longer describe it, and only the body hash
//	  catches that. Reporting a hand edit as "stale" would send an operator to
//	  re-render and silently destroy their edit.
//
// What the stamp deliberately does NOT do: decide staleness by comparing hashes.
// A basis hash changes on inputs that cannot change the rendered output (a tier's
// `measured` prose, a version bump with no renderer change), and a gate that
// cries stale on documentation edits is a gate operators learn to ignore. So
// AgainstRender's verdict is settled by RE-RENDERING from the current binary's
// seeds and comparing the RESULT to the stamped body; the basis diff is what
// NAMES the keys that moved. MATCH therefore means the verifiable thing:
// re-installing this box today would produce this exact file.
package servingtmpl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediaseat"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// State is the verdict of `audit-yaml --against-render` over one file. Exactly
// one of these is reported per file -- they are ordered by what an operator must
// do about them, and the checks run in that order, because a hand-edited file
// that is ALSO stale must be reported as hand-edited (re-rendering it destroys
// the edit).
type State string

const (
	// StateMatch: the file carries a stamp, its body is untouched, and
	// re-rendering from this binary's seeds reproduces it byte for byte.
	StateMatch State = "MATCH"
	// StateStale: stamp intact, body untouched, but this binary's seeds now
	// render something different. The report names the basis keys that moved.
	StateStale State = "STALE"
	// StateUnstamped: no provenance block. Every config rendered before this
	// version is unstamped; it is a finding, not a failure.
	StateUnstamped State = "UNSTAMPED"
	// StateHandEdited: the body no longer hashes to what the stamp recorded (or
	// the stamp's own hash does not match the basis it carries). Someone edited
	// the file, or the stamp, after rendering.
	StateHandEdited State = "HAND-EDITED"
)

// SpecBasis is the CLOSED input set the spec hash is computed over.
//
// Closed and named for the same reason props.go's seatPinBasis is: hashing
// whatever a struct happens to hold makes the hash churn on accidents and makes
// "what changed?" unanswerable. Every field here is an input `install render`
// was given; nothing derived, nothing observed from the running box.
//
// The template and the tier entry are folded in as their sha256 rather than
// verbatim: a serving template is several KB and the tier table is ~90 KB, and
// the stamp has to fit in a yaml comment block a human will read.
//
// ProfilesEntrySHA256 covers the tier's OWN entry, not the whole profiles.json.
// Hashing the whole file was considered and rejected: every tier edit anywhere
// in the table would then mark every node's config changed, including nodes the
// edit cannot touch. That is the false-positive shape that trains an operator to
// ignore a gate. The per-entry hash still catches a seed field that Render does
// not consume today but might tomorrow -- which is its whole marginal value over
// the Params fields below.
type SpecBasis struct {
	// HarnessVersion is buildinfo.Version of the binary that rendered.
	//
	// It is in the IDENTITY hash (a config rendered by 0.117.2 is not the same
	// artifact as one rendered by 0.123.0) but it is NOT what decides staleness:
	// AgainstRender re-renders and compares output, so a version bump that
	// changed nothing about this tier still reports MATCH. A verdict wired to the
	// version instead would read STALE on every node after every release.
	HarnessVersion string `json:"harness_version"`
	// TierID is the resolved tier -- a profiles.json key, or the literal
	// "(off-matrix: <backend> defaults)" string an unknown box renders under.
	TierID string `json:"tier_id"`
	// TemplateSHA256 is over the serving template's bytes as embedded in the
	// rendering binary (setup/templates/llama-swap.<os>-<backend>.yaml).
	TemplateSHA256 string `json:"template_sha256"`
	// ProfilesEntrySHA256 is over the tier's own entry in profiles.json,
	// canonicalised (sorted keys) so a reformat of the table is not a change.
	// Empty for an off-matrix render, which has no entry.
	ProfilesEntrySHA256 string `json:"profiles_entry_sha256"`
	// Render holds the render inputs that are neither a seed nor a Params field
	// but change what the seeds RESOLVE to. Without them a re-derive cannot
	// reproduce the render, so they are recorded rather than guessed.
	Render RenderBasis `json:"render"`
	// Params is the full, mirrored Params the render was performed with.
	Params ParamsBasis `json:"params"`
}

// RenderBasis is the pair of `install render` flags that gate the seeds.
type RenderBasis struct {
	// RAMTier is --ram-tier (min|low|mid|high, or empty for "the caller does not
	// know"). It decides whether a tier's RAM-hungry 26B placement is served at
	// all, so the same tier renders differently under two values of it.
	RAMTier string `json:"ram_tier"`
	// FallbackBackend is --fallback-backend: non-empty means the box is
	// off-matrix and the render came from fallbackProfile, not from the table.
	FallbackBackend string `json:"fallback_backend"`
}

// ParamsBasis mirrors servingtmpl.Params ONE FIELD FOR ONE FIELD, by Go field
// name. The mirror is not cosmetic: TestParamsBasisMirrorsParams reflects over
// both structs and fails when the name lists differ, so a field added to Params
// cannot silently fall outside the hashed set. json tags are the stable wire
// names an operator reads in a STALE report ("params.ctx_size").
type ParamsBasis struct {
	LlamaBin      string           `json:"llama_bin"`
	ModelsDir     string           `json:"models_dir"`
	Listen        string           `json:"listen"`
	Ctx           int              `json:"ctx_size"`
	KVType        string           `json:"kv_type"`
	FlashAttn     string           `json:"flash_attn"`
	MoE26B        string           `json:"moe_26b"`
	Threads       int              `json:"threads"`
	CacheRAMMiB   int              `json:"cache_ram_mib"`
	Include26B    bool             `json:"include_26b"`
	IncludeQ38    bool             `json:"include_qwen38"`
	IncludeQ354B  bool             `json:"include_qwen35_4b"`
	IncludeQ359B  bool             `json:"include_qwen35_9b"`
	IncludeQ3827B bool             `json:"include_qwen38_27b"`
	Seats         []mediaseat.Seat `json:"seats"`
	Home          string           `json:"home"`
	GOOS          string           `json:"goos"`
	// DisplayLayer is hashed as the whole layer spec, not as its name: the
	// template substitutes the layer’s rungs, its device pin and its guards into
	// the rendered text (ADR 0039), so a change to any of them changes the
	// config while the name stays "display".
	DisplayLayer      *config.LayerSpec `json:"display_layer"`
	GPUEnv            []string          `json:"gpu_env"`
	Backend           string            `json:"backend"`
	AltCPULlamaBin    string            `json:"alt_cpu_llama_bin,omitempty"`
	DisableCUDAGraphs bool              `json:"disable_cuda_graphs"`
	VLLMSeat          *vllmseat.Spec    `json:"vllm_seat"`
	VLLMRuntime       vllmseat.Runtime  `json:"vllm_runtime"`
}

// BasisOf projects a Params into its hashed mirror.
func BasisOf(p Params) ParamsBasis {
	return ParamsBasis{
		LlamaBin: p.LlamaBin, ModelsDir: p.ModelsDir, Listen: p.Listen,
		Ctx: p.Ctx, KVType: p.KVType, FlashAttn: p.FlashAttn, MoE26B: p.MoE26B,
		Threads: p.Threads, CacheRAMMiB: p.CacheRAMMiB, Include26B: p.Include26B, IncludeQ38: p.IncludeQ38,
		IncludeQ354B: p.IncludeQ354B, IncludeQ359B: p.IncludeQ359B,
		IncludeQ3827B: p.IncludeQ3827B,
		Seats:         p.Seats, Home: p.Home, GOOS: p.GOOS, DisplayLayer: p.DisplayLayer, GPUEnv: p.GPUEnv,
		Backend: p.Backend, AltCPULlamaBin: p.AltCPULlamaBin, DisableCUDAGraphs: p.DisableCUDAGraphs,
		VLLMSeat: p.VLLMSeat, VLLMRuntime: p.VLLMRuntime,
	}
}

// Params is the inverse of BasisOf: the render Params a stamp recorded. The
// re-derive uses it to replay a render with the SAME per-box values (install
// paths, listen address, thread count) that the original had -- a binary cannot
// know another box's layout, and guessing it would report every node stale.
func (b ParamsBasis) Params() Params {
	return Params{
		LlamaBin: b.LlamaBin, ModelsDir: b.ModelsDir, Listen: b.Listen,
		Ctx: b.Ctx, KVType: b.KVType, FlashAttn: b.FlashAttn, MoE26B: b.MoE26B,
		Threads: b.Threads, CacheRAMMiB: b.CacheRAMMiB, Include26B: b.Include26B, IncludeQ38: b.IncludeQ38,
		IncludeQ354B: b.IncludeQ354B, IncludeQ359B: b.IncludeQ359B,
		IncludeQ3827B: b.IncludeQ3827B,
		Seats:         b.Seats, Home: b.Home, GOOS: b.GOOS, DisplayLayer: b.DisplayLayer, GPUEnv: b.GPUEnv,
		Backend: b.Backend, AltCPULlamaBin: b.AltCPULlamaBin, DisableCUDAGraphs: b.DisableCUDAGraphs,
		VLLMSeat: b.VLLMSeat, VLLMRuntime: b.VLLMRuntime,
	}
}

// SHA256Hex is the one hashing helper this file uses, so template bytes, tier
// entry bytes, the canonical basis and the yaml body are all hashed identically.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CanonicalEntrySHA canonicalises one tier's profiles.json entry and hashes it:
// sorted keys at every level, number literals preserved. Whitespace and key
// order in the table are then not a change, which is what lets an operator
// reformat profiles.json without marking every box stale. An empty entry (an
// off-matrix render has none) hashes to the empty string, not to the hash of
// "": absent and present-but-empty must not collide.
func CanonicalEntrySHA(entry []byte) (string, error) {
	if len(bytes.TrimSpace(entry)) == 0 {
		return "", nil
	}
	canonical, _, err := canonicalise(json.RawMessage(entry))
	if err != nil {
		return "", err
	}
	return SHA256Hex(canonical), nil
}

// SpecHash returns the canonical JSON of the basis and its sha256. Canonical
// means: sorted keys at every level (Go marshals a map key-sorted, so the basis
// is round-tripped through a map tree rather than marshalled straight from the
// struct), and number literals preserved verbatim through json.Number -- a
// float64 round-trip renders 131072 as 131072 today but could render some future
// large seed in exponent form, changing a hash with no input change.
func SpecHash(b SpecBasis) (hash string, canonical []byte, err error) {
	canonical, _, err = canonicalise(b)
	if err != nil {
		return "", nil, err
	}
	return SHA256Hex(canonical), canonical, nil
}

func canonicalise(v any) ([]byte, map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(tree) // map keys are marshalled sorted at every level
	if err != nil {
		return nil, nil, err
	}
	m, _ := tree.(map[string]any)
	return out, m, nil
}

// ---- the stamp ------------------------------------------------------------

// stampLead is the first line of every stamp block and the sentinel ParseStamp
// keys on. ASCII only, on purpose: these files are read back through PowerShell
// and over ssh, where a non-ASCII byte in a native command's output has already
// come back mojibaked on this fleet.
const stampLead = "# local-offload serving-config provenance -- generated; re-derive with `local-offload audit-yaml --against-render`"

// stampKeys are the recognised `# <key>: <value>` lines of the block, in the
// order Stamp writes them. ParseStamp accepts them in any order but consumes
// ONLY these, so the block's end is unambiguous even though the template's own
// first lines are comments too.
var stampKeys = []string{"spec_sha256", "body_sha256", "rendered_by", "tier", "rendered_at", "basis"}

// Stamp prepends the provenance block to a rendered serving config and returns
// the stamped text. `at` is taken from the caller so a test can render
// deterministically; production passes time.Now().UTC().
//
// The body is left byte-identical -- the block is prepended, never interleaved --
// so body_sha256 is a hash of exactly what Render produced. A stamp that rewrote
// any part of the body would make "hand-edited" undetectable.
func Stamp(rendered string, b SpecBasis, at time.Time) (string, error) {
	spec, canonical, err := SpecHash(b)
	if err != nil {
		return "", err
	}
	if bytes.ContainsAny(canonical, "\r\n") {
		// Defensive: the basis rides one comment line, and a newline inside it
		// would silently truncate the block. json.Marshal escapes newlines, so
		// this cannot happen -- it is here because a stamp that half-parses is
		// worse than one that refuses.
		return "", fmt.Errorf("serving-config stamp: the canonical basis contains a newline; refusing to write a stamp that cannot be parsed back")
	}
	var sb strings.Builder
	sb.WriteString(stampLead + "\n")
	fmt.Fprintf(&sb, "# spec_sha256: %s\n", spec)
	fmt.Fprintf(&sb, "# body_sha256: %s\n", SHA256Hex([]byte(rendered)))
	fmt.Fprintf(&sb, "# rendered_by: %s\n", b.HarnessVersion)
	fmt.Fprintf(&sb, "# tier: %s\n", b.TierID)
	fmt.Fprintf(&sb, "# rendered_at: %s\n", at.UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "# basis: %s\n", escapeDoubleUnderscore(canonical))
	sb.WriteString("\n") // one blank separator; ParseStamp consumes it back
	sb.WriteString(rendered)
	return sb.String(), nil
}

// escapeDoubleUnderscore is the stamp's one transport escape, and it exists for
// a rule this file would otherwise break.
//
// A rendered serving config must contain NO `__TOKEN__`: Render refuses to emit
// one (a llama-swap started with a literal `--ctx-size __CTX__` fails in a way
// that reads like a model problem), and setup/render.tests.ps1 greps every
// rendered config for that pattern on every tier it exercises. The basis,
// however, legitimately CARRIES such tokens: a tier's media seats declare their
// binaries as `__OFFLOAD_HOME__/...` and the hash covers the render inputs AS
// GIVEN, unsubstituted -- substituting them before hashing would hash something
// `install render` was never handed. Writing that basis into the config verbatim
// put the forbidden pattern back into four tiers' rendered output (caught by the
// installer self-test in CI, not by review).
//
// So the transport escapes the SECOND underscore of every doubled pair as the
// JSON escape \u005f. It is the same string after json.Unmarshal, so the
// basis round-trips and the spec hash -- computed over the UNescaped canonical
// bytes -- is untouched; the stamp simply stops re-introducing the one pattern a
// rendered config must never contain. A run of three or more underscores escapes
// in alternating pairs, which still leaves no doubled pair anywhere in the line.
func escapeDoubleUnderscore(b []byte) string {
	return strings.ReplaceAll(string(b), "__", `_\u005f`)
}

// Stamped is a parsed provenance block plus the exact body bytes beneath it.
type Stamped struct {
	SpecSHA256 string
	BodySHA256 string
	RenderedBy string
	TierID     string
	RenderedAt string
	// BasisJSON is the canonical basis exactly as the stamp carries it.
	BasisJSON string
	// Body is the yaml below the block, byte for byte.
	Body string
}

// Basis decodes the stamped basis back into the closed struct.
func (s Stamped) Basis() (SpecBasis, error) {
	var b SpecBasis
	if err := json.Unmarshal([]byte(s.BasisJSON), &b); err != nil {
		return SpecBasis{}, fmt.Errorf("serving-config stamp: basis is not decodable: %w", err)
	}
	return b, nil
}

// ParseStamp reads a provenance block off the front of a config. ok=false means
// the file carries no stamp at all (UNSTAMPED) -- a missing stamp is a normal
// state for every config rendered before this version, never an error.
//
// The block ends at the first line that is not one of the recognised
// `# <key>: <value>` lines, plus one optional blank separator. Everything after
// that byte offset is the body, returned unmodified: the split is by OFFSET and
// not by re-joining lines, so CRLF files, trailing whitespace and a missing
// final newline all survive the round trip and hash to what they hashed at
// render time.
func ParseStamp(text string) (Stamped, bool) {
	if !strings.HasPrefix(text, stampLead) {
		return Stamped{}, false
	}
	known := make(map[string]bool, len(stampKeys))
	for _, k := range stampKeys {
		known[k] = true
	}
	var st Stamped
	off := len(stampLead)
	if strings.HasPrefix(text[off:], "\r") {
		off++
	}
	if !strings.HasPrefix(text[off:], "\n") {
		return Stamped{}, false
	}
	off++
	for off < len(text) {
		line := text[off:]
		next := len(text)
		if end := strings.IndexByte(text[off:], '\n'); end >= 0 {
			line = text[off : off+end]
			next = off + end + 1
		}
		rest, isComment := strings.CutPrefix(strings.TrimRight(line, "\r"), "# ")
		if !isComment {
			break
		}
		key, val, hasColon := strings.Cut(rest, ": ")
		if !hasColon || !known[key] {
			break
		}
		switch key {
		case "spec_sha256":
			st.SpecSHA256 = val
		case "body_sha256":
			st.BodySHA256 = val
		case "rendered_by":
			st.RenderedBy = val
		case "tier":
			st.TierID = val
		case "rendered_at":
			st.RenderedAt = val
		case "basis":
			st.BasisJSON = val
		}
		off = next
	}
	// One blank separator belongs to the block, so Body is exactly the bytes
	// Render produced. Optional on parse: a hand-removed blank line must not
	// turn a stamped file into an unstamped one.
	if strings.HasPrefix(text[off:], "\r\n") {
		off += 2
	} else if strings.HasPrefix(text[off:], "\n") {
		off++
	}
	st.Body = text[off:]
	if st.SpecSHA256 == "" || st.BodySHA256 == "" || st.BasisJSON == "" {
		// A block missing a required key is not a stamp this binary wrote. It is
		// reported as unstamped rather than guessed at: half a provenance record
		// is a claim nothing backs.
		return Stamped{}, false
	}
	return st, true
}

// ---- the verdict ----------------------------------------------------------

// Report is what `audit-yaml --against-render` prints for one file.
type Report struct {
	State State
	// SpecSHA256 / TierID / RenderedBy / RenderedAt come from the stamp; empty
	// on an UNSTAMPED file.
	SpecSHA256 string
	TierID     string
	RenderedBy string
	RenderedAt string
	// Keys names the basis keys that differ, dotted ("params.ctx_size"), sorted.
	// Non-empty only for STALE.
	Keys []string
	// Detail is the operator-facing sentence.
	Detail string
}

// Line renders a Report the way the CLI prints it: one line, greppable, with the
// state as the first word after the path.
func (r Report) Line(path string) string {
	s := fmt.Sprintf("%s: %s", path, r.State)
	if len(r.Keys) > 0 {
		s += "(" + strings.Join(r.Keys, ", ") + ")"
	}
	if r.Detail != "" {
		s += " -- " + r.Detail
	}
	return s
}

// AgainstRender decides one file's state.
//
// The caller supplies the freshly derived basis and the freshly rendered body,
// because deriving them needs the tier table and the embedded templates, which
// live with the installer half (install_render.go) -- this package renders a
// template, it does not own the seeds. freshBody may be empty when the tier the
// stamp names can no longer be rendered by this binary; then the verdict rests
// on the basis alone and says so.
//
// Order matters and is the point: integrity of the stamp, then integrity of the
// body, then staleness. A hand-edited file reported as STALE would send an
// operator to re-render and destroy their edit, so the body check comes first.
func AgainstRender(text string, freshBasis SpecBasis, freshBody string) Report {
	st, ok := ParseStamp(text)
	if !ok {
		return Report{State: StateUnstamped, Detail: "no provenance block: rendered before serving-config stamping (0.123.0), or written by hand. `local-offload install render` stamps it"}
	}
	base := Report{SpecSHA256: st.SpecSHA256, TierID: st.TierID, RenderedBy: st.RenderedBy, RenderedAt: st.RenderedAt}

	stampedBasis, err := st.Basis()
	if err != nil {
		base.State = StateHandEdited
		base.Detail = err.Error()
		return base
	}
	// The stamp must be internally consistent: spec_sha256 has to be the hash of
	// the basis the same block carries. Without this check a hand-edited basis
	// (say, ctx_size raised to match a hand-edited body) would be re-derived
	// against and reported MATCH.
	if got, _, err := SpecHash(stampedBasis); err != nil || got != st.SpecSHA256 {
		base.State = StateHandEdited
		base.Detail = "the stamp's spec_sha256 is not the hash of the basis it carries -- the provenance block itself was edited"
		return base
	}
	if got := SHA256Hex([]byte(st.Body)); got != st.BodySHA256 {
		base.State = StateHandEdited
		base.Detail = fmt.Sprintf("the config below the stamp no longer hashes to what was rendered (stamp %s, file %s) -- someone edited it; re-rendering would discard that edit", short(st.BodySHA256), short(got))
		return base
	}

	// harness_version is excluded from the VERDICT (it stays in the identity
	// hash). Including it would put every node in the fleet permanently STALE
	// after the next release, for a version bump that changed nothing about the
	// tier -- a gate that is always red is a gate nobody reads.
	keys := withoutKey(DiffBasis(stampedBasis, freshBasis), "harness_version")
	versionNote := ""
	if freshBasis.HarnessVersion != "" && st.RenderedBy != freshBasis.HarnessVersion {
		versionNote = fmt.Sprintf(" (rendered by %s, this binary is %s)", st.RenderedBy, freshBasis.HarnessVersion)
	}
	switch {
	case freshBody == "":
		base.State = StateStale
		base.Keys = keys
		base.Detail = fmt.Sprintf("tier %q cannot be rendered by this binary (it is no longer in the tier table, or its template is gone)", st.TierID)
	case freshBody == st.Body && len(keys) == 0:
		base.State = StateMatch
		base.Detail = fmt.Sprintf("re-rendering tier %s from this binary's seeds reproduces this file byte for byte", st.TierID) + versionNote
	case freshBody == st.Body:
		// The seeds moved but the rendered TEXT did not. This is not MATCH: the
		// re-derive pins the per-box inputs a binary cannot know (install paths,
		// the vLLM deployment half), so a seed field those pin can change
		// without the replay exercising it, and claiming "re-installing
		// reproduces this file" would be exactly the certified-stale-as-current
		// failure this row exists to end. It is called out separately because
		// the ACTION differs: nothing served is wrong, the stamp is behind.
		base.State = StateStale
		base.Keys = keys
		base.Detail = fmt.Sprintf("tier %s's inputs moved since this file was rendered, though re-rendering produces the same config text -- nothing served is wrong; re-render to refresh the stamp", st.TierID)
	case len(keys) == 0:
		// Every hashed input matches and the output still moved: the renderer's
		// own code changed (Render's substitution logic), which no input field
		// can name. Saying so is more useful than naming nothing.
		base.State = StateStale
		base.Detail = fmt.Sprintf("re-rendering tier %s produces a different config although every hashed input matches -- the renderer's code changed between %s and %s", st.TierID, st.RenderedBy, freshBasis.HarnessVersion)
	default:
		base.State = StateStale
		base.Keys = keys
		base.Detail = fmt.Sprintf("re-rendering tier %s from this binary's seeds produces a different config", st.TierID)
	}
	return base
}

func withoutKey(keys []string, drop string) []string {
	out := keys[:0:0]
	for _, k := range keys {
		if k != drop {
			out = append(out, k)
		}
	}
	return out
}

// DiffBasis names every basis key whose value differs, as a dotted path
// ("params.ctx_size", "params.vllm_seat.max_model_len"), sorted. It walks into
// nested objects so a report points at the field that moved rather than at the
// whole struct that contains it.
func DiffBasis(a, b SpecBasis) []string {
	_, at, errA := canonicalise(a)
	_, bt, errB := canonicalise(b)
	if errA != nil || errB != nil {
		return nil
	}
	var out []string
	diffTree("", at, bt, &out)
	sort.Strings(out)
	return out
}

func diffTree(prefix string, a, b any, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		seen := make(map[string]bool, len(am)+len(bm))
		for k := range am {
			seen[k] = true
		}
		for k := range bm {
			seen[k] = true
		}
		keys := make([]string, 0, len(seen))
		for k := range seen {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			diffTree(p, am[k], bm[k], out)
		}
		return
	}
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	if errA != nil || errB != nil || !bytes.Equal(ja, jb) {
		if prefix == "" {
			prefix = "(root)"
		}
		*out = append(*out, prefix)
	}
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
