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
	// without the others bricks the service.
	Include26B bool

	// Seats are the tier's alias-backed media seats (vision / STT). Empty is the
	// common case and MUST render byte-identically to a build that had no seat
	// support at all — TestNoSeatsChangesNothing holds that line.
	Seats []mediaseat.Seat
	// Home is the install root substituted for __OFFLOAD_HOME__ in seat paths, and
	// GOOS is the TARGET platform (never the rendering one) selecting the executable
	// suffix. Both matter only when Seats is non-empty.
	Home string
	GOOS string
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
			if k == "env" {
				a.env = v
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
	// Seats go in AFTER the 26B removal and BEFORE substitution: after, so a seat
	// whose text happens to mention the 26B can never trip drop26B's post-check;
	// before, so seat blocks are written in the same token vocabulary as the rest
	// of the template and are resolved by the one substitution pass below.
	out, seatFrag, err := insertSeats(out, p)
	if err != nil {
		return "", err
	}
	// The 26B's set membership is a token rather than surgery on an expression: the
	// operator differs per template (an alternative in a swapping set, a conjunction
	// in an all-resident one) and only the template knows which it means.
	m26alt, m26and := "", ""
	if p.Include26B {
		m26alt, m26and = " | m26", " & m26"
	}
	for from, to := range map[string]string{
		"__M26_ALT__":         m26alt,
		"__M26_AND__":         m26and,
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
		block, err := seatBlock(s, p, anchors.env)
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
func seatBlock(s mediaseat.Seat, p Params, env string) (string, error) {
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
		if env != "" {
			fmt.Fprintf(&b, "    env: [%q]\n", env)
		}
		exe := ""
		if p.GOOS == "windows" {
			exe = ".exe"
		}
		fmt.Fprintf(&b, "    cmd: >-\n"+
			"      __LLAMA_BIN__/llama-server%s --model __MODELS__/%s --mmproj __MODELS__/%s\n"+
			"      --n-gpu-layers 99 --parallel 1 --ctx-size %d --flash-attn __FLASH_ATTN__\n"+
			"      --cache-type-k __KV_K__ --cache-type-v __KV_V__ --threads __NTHREADS__ --jinja\n"+
			"      --reasoning off --port ${PORT} --host 127.0.0.1\n", exe, s.Model, s.MMProj, s.CtxSize)
	case mediaseat.KindSTT:
		// POSIX only: a self-built whisper-server links its own shared objects and
		// dies at exec without them. The Windows builds are self-contained.
		if lib := p.seatExpand(s.LibDir); lib != "" && p.GOOS != "windows" {
			fmt.Fprintf(&b, "    env: [\"LD_LIBRARY_PATH=%s:${LD_LIBRARY_PATH:-}\"]\n", lib)
		}
		vad := ""
		if s.VADModel != "" {
			vad = fmt.Sprintf(" --vad --vad-model __MODELS__/%s", s.VADModel)
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
	b.WriteString("    checkEndpoint: /health\n")
	fmt.Fprintf(&b, "    ttl: %d", ttl)
	return b.String(), nil
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

// drop26B removes the 26B model block AND its matrix var. Both, or llama-swap refuses
// the config at startup: a var pointing at a model that does not exist is exactly the
// dangling reference the old group-members removal existed to prevent. Its SET
// membership is handled by the __M26_*__ tokens, not here — an expression edit would
// have to know whether the operator was `|` or `&`, which only the template knows.
func drop26B(tmpl string) (string, error) {
	const model = "gemma4-26b-a4b"
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
	if strings.Contains(res, model) {
		return "", fmt.Errorf("%s still referenced after removal — a config llama-swap would reject", model)
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
