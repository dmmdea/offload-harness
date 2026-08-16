// Package servingtmpl renders a llama-swap serving config from a tier's parameters.
//
// Rendering lived in install.ps1, so a Linux node had no way to produce a serving
// config at all — every Linux deployment hand-wrote one. On the measured 6 GB node
// the first two hand-written topologies each broke the box (an exclusive media group
// returned 502s for a full TTL after any render; the embedder inside the swapping
// tier evicted the chat model on every RAG query), which is what a rendered,
// reviewed template exists to prevent.
//
// Substitution is deliberately dumb — tokens in, values out — with one guard that
// matters: Render REFUSES to emit a config that still contains a token. install.ps1
// carries a comment about exactly that failure ("the fully-tokenized templates would
// otherwise leave __CTX__ etc. in a config"), and a llama-swap that starts with
// `--ctx-size __CTX__` fails in a way that reads like a model problem.
package servingtmpl

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/mediaseat"
)

// Params are the per-machine values a tier's template needs. Everything here comes
// from the classified tier (ctx/kv/flash/moe), the install layout (bin/models), or
// the box itself (threads) — never from the machine doing the rendering.
type Params struct {
	LlamaBin  string // dir holding llama-server AND its shared objects (LD_LIBRARY_PATH)
	ModelsDir string
	Listen    string // "127.0.0.1:11436"
	Ctx       int
	KVType    string // applied to both K and V; kept symmetric on purpose
	FlashAttn string // "on"/"off"
	MoE26B    string // the flag form the tier chose: -ngl 99 | --cpu-moe | --n-cpu-moe N
	Threads   int
	// Include26B false removes the 26B seat entirely — model block, matrix var, and
	// set membership. A set naming a var that does not exist, or a var naming a model
	// that does not exist, is a config llama-swap rejects at startup, so dropping one
	// without the others bricks the service. The 26B *-agent entry (same weights,
	// reasoning on) rides this same gate: it exists whenever the 26B exists.
	Include26B bool
	// IncludeQ38 gates the Qwen3.8-27B coder/agent entry the same way Include26B
	// gates the 26B: false removes the model block, its matrix var, and its set
	// membership (the __Q38_ALT__/__Q38_AND__ tokens render empty). It comes from
	// the tier's include_qwen38 field and defaults to false — mirrors include_26b.
	IncludeQ38 bool

	// Seats are the tier's alias-backed media seats (vision / STT). Empty is the
	// common case and MUST render byte-identically to a build that had no seat
	// support at all — TestNoSeatsChangesNothing holds that line.
	Seats []mediaseat.Seat
	// Home is the install root substituted for __OFFLOAD_HOME__ in seat paths, and
	// GOOS is the TARGET platform (never the rendering one) selecting the executable
	// suffix. Both matter only when Seats is non-empty.
	Home string
	GOOS string

	// GPUEnv is added to EVERY model's env list, appended after whatever the template
	// already declares. It comes from the tier's `gpu_env`, not from a name match: the
	// Blackwell tiers need CUDA_VISIBLE_DEVICES pinned (a hybrid-graphics trap where
	// the default can resolve to -1) and CUDA_MODULE_LOADING=LAZY (llama.cpp #22696).
	// This lived in install.ps1 as an `if ($profileId -match '^blackwell-')` branch,
	// so a Linux install of the same tier silently did not get it.
	GPUEnv []string
	// Backend is the tier's serving backend (cuda|vulkan|cpu|…). A vision seat renders
	// GPU flags (-ngl, --flash-attn) UNLESS this is "cpu", where the template's own
	// chat models carry neither and a GPU-less build would only ignore them.
	Backend string
}

// seatAnchors is the TEMPLATE's own declaration of which residency roles it can
// place, plus the env macro its llama-backed seats carry. It lives in the template
// because the matrix set layout and the loader-path macro are per-template facts —
// the Linux template's `ld` macro has no Windows equivalent, which is why that file
// could not be a translation of the others. The __SEATS_<ROLE>__ markers in the set
// expressions say WHERE; this directive says WHICH roles are accepted at all.
//
// A template with no directive renders no seats and says so by name. That is the
// point: a tier declaring seats against a template that cannot place them is the
// silent-capability-loss failure this whole workstream exists to end, so it is a
// refusal, never a quiet omission.
type seatAnchors struct {
	roles map[string]bool // residency roles this template can place
	env   string          // env macro for llama-backed seats, e.g. "${ld}"
	// noTTL: this template gives its models NO ttl, so a seat must not get one either.
	// A ttl on an all-resident tier means llama-swap unloads the seat after an idle
	// window — the exact opposite of what "resident" promises. It is per-TEMPLATE and
	// not per-role: win-cuda-resident carries no ttl anywhere, while win-dual-cuda is
	// also resident-only and DOES use ttl on every model.
	noTTL bool
}

var anchorRe = regexp.MustCompile(`(?m)^#\s*offload-seats:\s*(.+)$`)

func parseAnchors(tmpl string) (seatAnchors, error) {
	all := anchorRe.FindAllStringSubmatch(tmpl, -1)
	if len(all) == 0 {
		return seatAnchors{}, errNoAnchors
	}
	// Two directives means two answers to "where does a seat go", and first-match
	// would silently pick one. A template author editing this must see the conflict.
	if len(all) > 1 {
		return seatAnchors{}, fmt.Errorf("this template carries %d `# offload-seats:` directives; "+
			"exactly one may declare where seats go", len(all))
	}
	a := seatAnchors{roles: map[string]bool{}}
	for _, f := range strings.Fields(all[0][1]) {
		if k, v, ok := strings.Cut(f, "="); ok {
			switch k {
			case "env":
				a.env = v
			case "ttl":
				a.noTTL = v == "none"
			}
			continue
		}
		a.roles[f] = true // a bare word is a residency role this template can place
	}
	return a, nil
}

// errNoAnchors means the template declares no seat placement at all — a distinct
// case from a malformed directive, and the one that gets the actionable message.
var errNoAnchors = fmt.Errorf("no `# offload-seats:` directive")

// SupportsSeats reports whether a serving template can place tier-declared media
// seats. `install seed` asks BEFORE writing a config, so a target that cannot
// serve a seat never gets the binding for it written either — the two halves of
// an install refuse together or not at all.
func SupportsSeats(tmpl string) bool {
	_, err := parseAnchors(tmpl)
	return err == nil
}

var tokenRe = regexp.MustCompile(`__[A-Z0-9_]+__`)

// Render substitutes p into tmpl and returns the serving config.
func Render(tmpl string, p Params) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	out := tmpl
	if !p.Include26B {
		var err error
		if out, err = drop26B(out); err != nil {
			return "", err
		}
	}
	if !p.IncludeQ38 {
		var err error
		if out, err = dropQ38(out); err != nil {
			return "", err
		}
	}
	// Seats go in AFTER the 26B removal and BEFORE substitution: after, so a seat
	// whose text happens to mention the 26B can never trip drop26B's post-check;
	// before, so seat blocks are written in the same token vocabulary as the rest
	// of the template and are resolved by the one substitution pass below.
	out, seatFrag, err := insertSeats(out, p)
	if err != nil {
		return "", err
	}
	// GPU env lands AFTER seats so a tier's own media seats are pinned to the same
	// device as everything else — the PowerShell version applied it to every model
	// block in the map, and a seat is a model block.
	if len(p.GPUEnv) > 0 {
		out = injectGPUEnv(out, p.GPUEnv)
	}
	// The 26B's set membership is a token rather than surgery on an expression: the
	// operator differs per template (an alternative in a swapping set, a conjunction
	// in an all-resident one) and only the template knows which it means.
	m26alt, m26and := "", ""
	if p.Include26B {
		m26alt, m26and = " | m26", " & m26"
		// A template that declares the 26B *-agent entry (same weights, reasoning on)
		// joins it as an ALTERNATIVE to the cascade seat: one of the two at a time in
		// a swapping set, and an alternation inside the conjunction on a resident one
		// — double-loading the same weights is never a valid combination. Detected
		// from the template (post-drop) so the agent rides the 26B's own gate rather
		// than growing a second one.
		if definesModel(out, agentModel26B) {
			m26alt, m26and = " | m26 | a26", " & (m26 | a26)"
		}
	}
	// The Qwen3.8 membership mirrors the 26B's token pair, gated by IncludeQ38.
	q38alt, q38and := "", ""
	if p.IncludeQ38 {
		q38alt, q38and = " | q38", " & q38"
	}
	for from, to := range map[string]string{
		"__M26_ALT__":         m26alt,
		"__M26_AND__":         m26and,
		"__Q38_ALT__":         q38alt,
		"__Q38_AND__":         q38and,
		"__SEATS_SWAPPABLE__": seatFrag[roleSwappable],
		"__SEATS_RESIDENT__":  seatFrag[roleResident],
		"__LLAMA_BIN__":       strings.TrimRight(p.LlamaBin, "/"),
		"__MODELS__":          strings.TrimRight(p.ModelsDir, "/"),
		"__LISTEN__":          p.Listen,
		"__CTX__":             fmt.Sprint(p.Ctx),
		"__KV_K__":            p.KVType,
		"__KV_V__":            p.KVType,
		"__FLASH_ATTN__":      p.FlashAttn,
		"__MOE_26B__":         p.MoE26B,
		"__NTHREADS__":        fmt.Sprint(p.Threads),
	} {
		out = strings.ReplaceAll(out, from, to)
	}
	// A leftover token would start a server with a literal "__CTX__" argument, which
	// fails looking like a model problem. Refuse instead, naming what is unresolved.
	if left := uniqueTokens(out); len(left) > 0 {
		return "", fmt.Errorf("unresolved template token(s) after rendering: %s", strings.Join(left, ", "))
	}
	return out, nil
}

func (p Params) validate() error {
	var missing []string
	if p.LlamaBin == "" {
		missing = append(missing, "llama bin dir")
	}
	if p.ModelsDir == "" {
		missing = append(missing, "models dir")
	}
	if p.Listen == "" {
		missing = append(missing, "listen address")
	}
	if p.Ctx <= 0 {
		missing = append(missing, "ctx size")
	}
	if p.KVType == "" {
		missing = append(missing, "kv type")
	}
	if p.FlashAttn == "" {
		missing = append(missing, "flash-attn")
	}
	if p.Threads <= 0 {
		missing = append(missing, "threads")
	}
	if p.Include26B && strings.TrimSpace(p.MoE26B) == "" {
		missing = append(missing, "26B MoE placement (the tier includes the 26B but named no placement flag)")
	}
	if len(p.Seats) > 0 && p.Home == "" && seatsNeedHome(p.Seats) {
		missing = append(missing, "install home (a media seat names a path under "+tokenHome+")")
	}
	if len(missing) > 0 {
		return fmt.Errorf("serving template needs: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Tokens a seat path may carry, shared with internal/tierseed so ONE tier-table
// row renders on every machine and OS.
const (
	tokenHome = "__OFFLOAD_HOME__"
	tokenExe  = "__EXE__"
)

// seatsNeedHome scans exactly the fields seatExpand resolves. Scanning more would
// promise a substitution that never happens.
func seatsNeedHome(seats []mediaseat.Seat) bool {
	for _, s := range seats {
		if strings.Contains(s.Bin+s.LibDir, tokenHome) {
			return true
		}
	}
	return false
}

// seatExpand resolves the two seat-only tokens against the TARGET machine.
func (p Params) seatExpand(s string) string {
	exe := ""
	if p.GOOS == "windows" {
		exe = ".exe"
	}
	s = strings.ReplaceAll(s, tokenExe, exe)
	if p.Home != "" {
		s = strings.ReplaceAll(s, tokenHome, strings.TrimRight(strings.ReplaceAll(p.Home, `\`, "/"), "/"))
	}
	return s
}

// insertSeats places every declared seat into the models map AND into the matrix —
// a `vars` entry plus the var id joined into the set its residency role marks. All
// of it, always: llama-swap refuses a set naming an unknown var, and a model that
// reaches the models map but no set is one the solver has no combination for.
//
// It returns the per-role var-id fragments the caller substitutes into the set
// expressions, because the operator differs by role: a swappable seat is one more
// ALTERNATIVE in a mutually-exclusive set (`| vis`), while a resident seat is one
// more CO-RESIDENT member (`& vis`). Getting that backwards would either pin a big
// seat in memory forever or make the memory stack evictable.
func insertSeats(tmpl string, p Params) (string, map[string]string, error) {
	frag := map[string]string{roleSwappable: "", roleResident: ""}
	if len(p.Seats) == 0 {
		return tmpl, frag, nil
	}
	anchors, err := parseAnchors(tmpl)
	if err != nil && err != errNoAnchors {
		return "", nil, err
	}
	if err == errNoAnchors {
		var names []string
		for _, s := range p.Seats {
			names = append(names, s.Kind+":"+s.Name)
		}
		return "", nil, fmt.Errorf("this tier declares media seats (%s) but the target serving template carries no "+
			"`# offload-seats:` directive, so there is nowhere to place them. Rendering them into a residency "+
			"topology the template never declared is how a node loses a capability by accident of OS — add the "+
			"directive and the __SEATS_<ROLE>__ markers to the template, or render this tier for a target whose "+
			"template has them", strings.Join(names, ", "))
	}
	out := tmpl
	taken := existingVars(out)
	for _, s := range p.Seats {
		if !anchors.roles[s.Residency] {
			var roles []string
			for r := range anchors.roles {
				roles = append(roles, r)
			}
			sort.Strings(roles)
			return "", nil, fmt.Errorf("seat %q wants residency %q, which this template does not place (it places: %s)",
				s.Name, s.Residency, strings.Join(roles, ", "))
		}
		if definesModel(out, s.Name) {
			return "", nil, fmt.Errorf("seat %q is already defined by the template — a tier may not redeclare a seat "+
				"the template owns", s.Name)
		}
		id, err := seatVarID(s, taken)
		if err != nil {
			return "", nil, err
		}
		taken[id] = true
		block, err := seatBlock(s, p, anchors)
		if err != nil {
			return "", nil, err
		}
		if out, err = appendModel(out, block); err != nil {
			return "", nil, err
		}
		if out, err = addMatrixVar(out, id, s.Name); err != nil {
			return "", nil, err
		}
		frag[s.Residency] += matrixJoin(s.Residency) + id
	}
	return out, frag, nil
}

// Residency roles, mirrored from internal/mediaseat so the template vocabulary and
// the schema vocabulary cannot drift apart silently.
const (
	roleSwappable = mediaseat.Swappable
	roleResident  = mediaseat.Resident
)

// matrixJoin is the set operator a role's members are combined with. Swappable
// members are alternatives (one at a time); residents are conjunctions (all at once).
func matrixJoin(role string) string {
	if role == roleResident {
		return " & "
	}
	return " | "
}

// seatVarID derives the matrix var key for a seat. llama-swap REQUIRES a var key to
// be alphanumeric and 1-8 characters (verified against the binary: a key of
// "embeddinggemma" is rejected outright), so the seat's own name — which carries
// hyphens and is usually longer — can never be the key. The kind is used because a
// tier may declare at most one seat per kind, which makes the id both stable and
// unique by construction.
func seatVarID(s mediaseat.Seat, taken map[string]bool) (string, error) {
	base := map[string]string{mediaseat.KindVision: "vis", mediaseat.KindSTT: "stt"}[s.Kind]
	if base == "" {
		return "", fmt.Errorf("seat %q: no matrix var id for kind %q", s.Name, s.Kind)
	}
	if !taken[base] {
		return base, nil
	}
	for i := 2; i < 10; i++ {
		if id := fmt.Sprintf("%s%d", base, i); !taken[id] {
			return id, nil
		}
	}
	return "", fmt.Errorf("seat %q: cannot derive a free matrix var id from %q", s.Name, base)
}

var varLineRe = regexp.MustCompile(`(?m)^ {4}([A-Za-z0-9]{1,8}):\s*(\S+)\s*$`)

// varsBounds locates the matrix vars block: the index of the `  vars:` line, and the
// index of the LAST var line under it. Anchoring on the last var line rather than on
// "where the block ends" is deliberate — the templates carry prose comments between
// the vars and the next key, and a comment that happens to contain a colon once made
// the insertion land in the middle of a sentence.
func varsBounds(lines []string) (start, lastVar int) {
	start, lastVar = -1, -1
	for i, l := range lines {
		if start < 0 {
			if strings.TrimRight(l, " ") == "  vars:" {
				start = i
			}
			continue
		}
		if varLineRe.MatchString(l) {
			lastVar = i
			continue
		}
		// Anything at 2-space indent or shallower (and not blank) ends the block.
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "    ") {
			break
		}
	}
	return start, lastVar
}

// existingVars reads the template's matrix var keys so a generated id cannot collide
// with one the template already owns.
func existingVars(tmpl string) map[string]bool {
	lines := strings.Split(tmpl, "\n")
	start, last := varsBounds(lines)
	out := map[string]bool{}
	if start < 0 || last < 0 {
		return out
	}
	for _, l := range lines[start : last+1] {
		if m := varLineRe.FindStringSubmatch(l); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// addMatrixVar appends one `    <id>: <model>` line after the last existing var.
func addMatrixVar(tmpl, id, model string) (string, error) {
	lines := strings.Split(tmpl, "\n")
	start, last := varsBounds(lines)
	if start < 0 {
		return "", fmt.Errorf("this template declares no `matrix.vars:` block, so seat %q has no var to be named by "+
			"(llama-swap requires every set member to be a declared var)", model)
	}
	at := last
	if at < 0 {
		at = start // an empty vars block: the new var becomes its first entry
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:at+1]...)
	out = append(out, "    "+id+": "+model)
	out = append(out, lines[at+1:]...)
	return strings.Join(out, "\n"), nil
}

// seatBlock writes one seat. The command SHAPES here are the measured, running
// ones from the 6 GB reference node — a vision seat carries its own smaller
// window rather than inheriting the chat tier's, and whisper-server takes its own
// loader path because it is a separate self-built binary.
func seatBlock(s mediaseat.Seat, p Params, a seatAnchors) (string, error) {
	env := a.env
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 300
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %s:\n", s.Name)
	if len(s.Aliases) > 0 {
		fmt.Fprintf(&b, "    aliases: [%s]\n", strings.Join(s.Aliases, ", "))
	}
	switch s.Kind {
	case mediaseat.KindVision:
		var envEntries []string
		if env != "" {
			envEntries = append(envEntries, fmt.Sprintf("%q", env))
		}
		envEntries = append(envEntries, s.GPUEnv...) // per-seat device pin, barewords
		if len(envEntries) > 0 {
			fmt.Fprintf(&b, "    env: [%s]\n", strings.Join(envEntries, ", "))
		}
		exe := ""
		if p.GOOS == "windows" {
			exe = ".exe"
		}
		// A VRAM bound, and the reason it is per-seat: the same VLM on a 6 GB and an
		// 8 GB card wants different image budgets.
		bound := ""
		if s.ImageMaxTokens > 0 {
			bound += fmt.Sprintf(" --image-max-tokens %d", s.ImageMaxTokens)
		}
		if s.NoContextShift {
			bound += " --no-context-shift"
		}
		// Keep the CLIP encoder on CPU where the LLM backend degrades it (Vulkan,
		// llama.cpp #20081). The LLM still decodes on the GPU (-ngl 99).
		if s.NoMmprojOffload {
			bound += " --no-mmproj-offload"
		}
		// On the cpu backend the template's own chat models carry neither -ngl nor
		// --flash-attn; a GPU-less build would only ignore them, so the vision seat
		// omits them too and renders the same shape.
		gpuFlags := " --n-gpu-layers 99"
		fa := " --flash-attn __FLASH_ATTN__"
		if p.Backend == "cpu" {
			gpuFlags, fa = "", ""
		}
		fmt.Fprintf(&b, "    cmd: >-\n"+
			"      __LLAMA_BIN__/llama-server%s --model __MODELS__/%s --mmproj __MODELS__/%s\n"+
			"     %s --parallel 1 --ctx-size %d%s%s\n"+
			"      --cache-type-k __KV_K__ --cache-type-v __KV_V__ --threads __NTHREADS__ --jinja\n"+
			"      --reasoning off --port ${PORT} --host 127.0.0.1\n", exe, s.Model, s.MMProj, gpuFlags, s.CtxSize, bound, fa)
	case mediaseat.KindSTT:
		var envEntries []string
		// POSIX only: a self-built whisper-server links its own shared objects and
		// dies at exec without them. The Windows builds are self-contained.
		if lib := p.seatExpand(s.LibDir); lib != "" && p.GOOS != "windows" {
			envEntries = append(envEntries, fmt.Sprintf("%q", "LD_LIBRARY_PATH="+lib+":${LD_LIBRARY_PATH:-}"))
		}
		envEntries = append(envEntries, s.GPUEnv...) // per-seat device pin
		if len(envEntries) > 0 {
			fmt.Fprintf(&b, "    env: [%s]\n", strings.Join(envEntries, ", "))
		}
		vad := ""
		if s.VADModel != "" {
			vad = fmt.Sprintf(" --vad --vad-model __MODELS__/%s", s.VADModel)
		}
		// -nfa turns flash attention OFF. It is default-ON in whisper.cpp since v1.8.0
		// and measurably degrades non-English and noisy audio, so a tier serving Spanish
		// or field recordings asks for it.
		if s.NoFlashAttn {
			vad += " -nfa"
		}
		fmt.Fprintf(&b, "    cmd: >-\n"+
			"      %s --model __MODELS__/%s%s\n"+
			"      --threads __NTHREADS__ --port ${PORT} --host 127.0.0.1\n",
			p.seatExpand(s.Bin), s.Model, vad)
	default:
		return "", fmt.Errorf("seat %q: unknown kind %q", s.Name, s.Kind)
	}
	// whisper-server's /health is 200 when ready and 503 while loading, and
	// llama-server's is the same shape — correct for both, and correct ONLY
	// because neither seat repoints its request path.
	b.WriteString("    checkEndpoint: /health")
	// A template that gives its own models no ttl gets seats with no ttl either: on an
	// all-resident tier a ttl means llama-swap unloads the seat after an idle window,
	// which is exactly what "resident" is supposed to prevent. Per-TEMPLATE, not
	// per-role — win-dual-cuda is also resident-only and DOES use ttl on every model.
	if a.noTTL {
		return b.String(), nil
	}
	fmt.Fprintf(&b, "\n    ttl: %d", ttl)
	return b.String(), nil
}

var modelKeyRe = regexp.MustCompile(`^ {2}"?([A-Za-z0-9._-]+)"?:\s*$`)
var envLineRe = regexp.MustCompile(`^ {4}env:\s*\[(.*)\]\s*$`)

// injectGPUEnv appends vars to every model's env list, creating the line when a model
// has none. Ported from install.ps1's Add-GpuEnvToYaml and kept byte-identical to it
// on purpose: the delegation is only safe if the output does not move, and a golden
// comparison against the PowerShell renderer is what proved that.
//
// Order matters — existing entries stay FIRST. The 26B declares
// GGML_CUDA_DISABLE_GRAPHS=1 and must keep it in front, both because that is what
// shipped and because appending is the only edit that cannot reorder a template's
// own intent.
func injectGPUEnv(tmpl string, vars []string) string {
	add := strings.Join(vars, ", ")
	lines := strings.Split(tmpl, "\n")
	out := make([]string, 0, len(lines)+8)
	inModels := false
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "models:"):
			inModels = true
			out = append(out, l)
			continue
		case l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "#"):
			inModels = false
		}
		out = append(out, l)
		if !inModels || !modelKeyRe.MatchString(l) {
			continue
		}
		// Find this block's extent and any env line already in it.
		envAt := -1
		end := i + 1
		for ; end < len(lines); end++ {
			if t := lines[end]; t != "" && !strings.HasPrefix(t, "   ") {
				break // next key at 2-space indent or shallower ends the block
			}
			if envLineRe.MatchString(lines[end]) {
				envAt = end
			}
		}
		if envAt < 0 {
			out = append(out, "    env: ["+add+"]")
			continue
		}
		// Copy the block, rewriting its env line in place.
		for j := i + 1; j < end; j++ {
			if j == envAt {
				m := envLineRe.FindStringSubmatch(lines[j])
				existing := strings.TrimSpace(m[1])
				if existing == "" {
					out = append(out, "    env: ["+add+"]")
				} else {
					out = append(out, "    env: ["+existing+", "+add+"]")
				}
				continue
			}
			out = append(out, lines[j])
		}
		i = end - 1
	}
	return strings.Join(out, "\n")
}

func definesModel(tmpl, name string) bool {
	return regexp.MustCompile(`(?m)^ {2}"?` + regexp.QuoteMeta(name) + `"?:\s*$`).MatchString(tmpl)
}

// appendModel puts a block at the end of the models map, before any trailing
// blank lines so the file keeps its shape.
func appendModel(tmpl, block string) (string, error) {
	lines := strings.Split(tmpl, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "models:") {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("serving template has no models: block to add a seat to")
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if l := lines[i]; l != "" && !strings.HasPrefix(l, " ") {
			end = i
			break
		}
	}
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:end]...)
	out = append(out, block)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), nil
}

// addMember appends one entry to a group's inline members list — the mirror of
// removeMember, and the half whose absence would make llama-swap reject the file.
func addMember(tmpl, group, member string) (string, error) {
	lines := strings.Split(tmpl, "\n")
	in := false
	for i, l := range lines {
		if strings.HasPrefix(l, "  "+group+":") {
			in = true
			continue
		}
		if !in {
			continue
		}
		// A comment is not a key. The shipped templates carry indented comments that
		// contain colons ("# heavy: one big seat at a time", "# 1. exclusive:true"),
		// so treating them as the next group would abandon the search and report that
		// the group has no members list at all.
		if trimmed := strings.TrimSpace(l); strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.Contains(l, ":") {
			break // reached the next group without finding a members list
		}
		if strings.Contains(l, "members:") {
			open, closeIdx := strings.Index(l, "["), strings.LastIndex(l, "]")
			if open < 0 || closeIdx < open {
				return "", fmt.Errorf("group %q has a members: line that is not an inline list", group)
			}
			inner := strings.TrimSpace(l[open+1 : closeIdx])
			if inner == "" {
				inner = member
			} else {
				inner += ", " + member
			}
			lines[i] = l[:open+1] + inner + l[closeIdx:]
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", fmt.Errorf("this template declares no group %q with a members: list, so seat %q has nowhere to go "+
		"(a model in no group joins llama-swap's implicit default group, which swaps and evicts)", group, member)
}

// agentModel26B is the 26B thinking-agent entry: the same weights as the cascade
// seat with reasoning ON. It has no gate of its own — it exists whenever the 26B
// exists, so drop26B strips it and the __M26_*__ expansion joins it.
const agentModel26B = "gemma-4-26b-agent"

// drop26B removes the 26B model block AND its matrix var — and the 26B *-agent
// entry with them, because the agent seat shares the 26B's weights and gate (a
// no-op on templates that do not declare it). Both halves of each removal, or
// llama-swap refuses the config at startup: a var pointing at a model that does
// not exist is exactly the dangling reference the old group-members removal
// existed to prevent. SET membership is handled by the __M26_*__ tokens, not here
// — an expression edit would have to know whether the operator was `|` or `&`,
// which only the template knows.
func drop26B(tmpl string) (string, error) {
	out, err := dropModel(tmpl, "gemma4-26b-a4b")
	if err != nil {
		return "", err
	}
	return dropModel(out, agentModel26B)
}

// dropQ38 removes the Qwen3.8-27B coder/agent entry — the exact mirror of the 26B
// strip, keyed on the tier's include_qwen38. Its set membership is handled by the
// __Q38_*__ tokens.
func dropQ38(tmpl string) (string, error) {
	return dropModel(tmpl, "qwen3.8-27b")
}

// dropModel removes one model block AND the matrix var naming it, with a
// post-check that nothing functional still references it.
func dropModel(tmpl, model string) (string, error) {
	lines := strings.Split(tmpl, "\n")
	var out []string
	skipping := false
	for _, l := range lines {
		if strings.HasPrefix(l, "  "+model+":") {
			skipping = true
			continue
		}
		// The matrix var naming it goes too: `    m26: gemma4-26b-a4b`.
		if m := varLineRe.FindStringSubmatch(l); m != nil && m[2] == model {
			continue
		}
		if skipping {
			// The block ends at the next key at the same indent (two spaces, no more)
			// OR at any column-0 line, which starts a new top-level section. Without
			// the second case, a 26B declared LAST would swallow `groups:` and the
			// `# offload-seats:` directive, turning the first group into a model.
			switch {
			case l != "" && !strings.HasPrefix(l, " "):
				skipping = false
			case strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.Contains(l, ":"):
				skipping = false
			default:
				continue
			}
		}
		if strings.Contains(l, "members:") && strings.Contains(l, model) {
			l = removeMember(l, model)
		}
		out = append(out, l)
	}
	if skipping {
		return "", fmt.Errorf("template ended inside the %s block — its shape changed", model)
	}
	res := strings.Join(out, "\n")
	// Check FUNCTIONAL lines only. A comment may legitimately name the model — the cpu
	// template documents it as "OPTIONAL on CPU, needs >=48GB RAM" — and treating that
	// prose as a dangling reference refused a perfectly good config. This only surfaced
	// once the RAM gate started dropping the 26B on that template.
	for _, l := range strings.Split(res, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") && strings.Contains(l, model) {
			return "", fmt.Errorf("%s still referenced after removal (%q) — a config llama-swap would reject",
				model, strings.TrimSpace(l))
		}
	}
	return res, nil
}

// removeMember drops one entry from an inline YAML flow sequence.
func removeMember(line, member string) string {
	open, close := strings.Index(line, "["), strings.LastIndex(line, "]")
	if open < 0 || close < open {
		return line
	}
	var kept []string
	for _, m := range strings.Split(line[open+1:close], ",") {
		if strings.TrimSpace(m) != member {
			kept = append(kept, strings.TrimSpace(m))
		}
	}
	return line[:open+1] + strings.Join(kept, ", ") + line[close:]
}

func uniqueTokens(s string) []string {
	seen := map[string]bool{}
	for _, m := range tokenRe.FindAllString(s, -1) {
		seen[m] = true
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
