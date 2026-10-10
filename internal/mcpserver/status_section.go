package mcpserver

// status_section.go: offload_status's one optional argument, section.
//
// The full answer is 5.5-6k tokens and it is the usual FIRST call of a
// delegating session, while sizing a contract needs only the fleet block (the
// seats, their live context ceilings, their queues: ~1.7k tokens). Half of the
// rest is gpu_lease, whose per-card process list alone runs to dozens of rows on
// a desktop. So a caller can ask for one block, or for "brief". The default stays
// the whole payload, byte-identical to the answer before the argument existed
// (pinned by TestStatusDefaultIsByteIdenticalToTheGolden).
//
// A section computes only what it returns: the fleet section does not run
// nvidia-smi, the gpu_lease section does not probe the fleet. That is the latency
// half of the saving, and the reason the blocks are built from one table instead
// of cut out of the full payload after the fact.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpucards"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

const (
	statusSectionAll   = "all"
	statusSectionBrief = "brief"
)

// statusBlock is one top-level block of the offload_status answer. build
// returning nil means "absent from the full answer" (only accelerators, on a box
// that lists none).
type statusBlock struct {
	key   string
	build func(s *Server, ctx context.Context, cfg config.Config) any
}

// statusBlockTable is every block, in the order the full answer has always
// computed them (the order matters only for side-effect timing; the JSON is a
// map and marshals sorted). The section enum and the dispatch both read it, so
// a new block cannot be reachable in one and missing from the other.
var statusBlockTable = []statusBlock{
	// withDisplayStatus (layers_view.go) adds the composite box's presence reading and the display
	// guard's last action to the block; it returns a plain box's block untouched.
	{"local", func(s *Server, ctx context.Context, cfg config.Config) any {
		return withDisplayStatus(ctx, cfg, s.statusLocal(ctx, cfg))
	}},
	{"media", func(_ *Server, _ context.Context, cfg config.Config) any { return statusMedia(cfg) }},
	{"remote", func(_ *Server, _ context.Context, cfg config.Config) any { return statusRemote(cfg) }},
	// Accelerators (ADR 0024): reported only when listed, one entry per device
	// in config order; a quick health probe that NEVER spawns the sidecar —
	// status must stay side-effect free (acceltools.go accelStatus). A nil map
	// must come back as an untyped nil, or it would read as a present block.
	{"accelerators", func(_ *Server, ctx context.Context, cfg config.Config) any {
		if a := accelStatus(ctx, cfg); a != nil {
			return a
		}
		return nil
	}},
	{"reuse", func(s *Server, _ context.Context, cfg config.Config) any { return s.statusReuse(cfg) }},
	{"fleet", func(s *Server, ctx context.Context, cfg config.Config) any { return s.fleetView(ctx, cfg) }},
	{"kv_cache_server", func(_ *Server, ctx context.Context, cfg config.Config) any { return kvCacheServerView(ctx, cfg) }},
	{"gpu_lease", func(_ *Server, ctx context.Context, cfg config.Config) any { return localLeaseView(ctx, cfg) }},
	// How this box's PAIR cards are reported (docs/systems/pair-workloads.md): reported only when
	// pair_workloads_enabled is on, so a box that never opted in answers byte-identically to before.
	{"pair", func(_ *Server, _ context.Context, cfg config.Config) any { return statusPair(cfg) }},
}

// statusSectionValues is the section enum: the two composites, then every block.
func statusSectionValues() []string {
	out := []string{statusSectionAll, statusSectionBrief}
	for _, b := range statusBlockTable {
		out = append(out, b.key)
	}
	return out
}

// statusSectionDoc is the section argument's schema description. Kept short on
// purpose: clients that inline tool schemas pay for these bytes on every call.
const statusSectionDoc = "all (default): everything. brief: fleet + one-line gpu_lease and local verdicts + the per-card table (gpu_cards). Any other value: that block only."

// statusInputSchema is offload_status's input schema, its enum built from the
// same table the handler dispatches on.
func statusInputSchema() json.RawMessage {
	enum, _ := json.Marshal(statusSectionValues())
	doc, _ := json.Marshal(statusSectionDoc)
	return json.RawMessage(`{"type":"object","properties":{"section":{"type":"string","enum":` + string(enum) + `,"description":` + string(doc) + `}}}`)
}

// parseStatusSection decodes offload_status's arguments and resolves the section.
// Absent, null, {} and "" are the default. Anything it cannot name — an unknown
// argument, a wrong type, an unknown section — is a defer that lists the valid
// values: silently answering with the full dump would spend the very tokens the
// argument exists to save, and the caller would never notice. Case and
// surrounding space are normalized, which resolves a spelling rather than
// falling back.
func parseStatusSection(raw json.RawMessage) (string, *mcp.CallToolResult) {
	usage := "offload_status takes one optional argument, section: one of " + strings.Join(statusSectionValues(), ", ")
	var in struct {
		Section string `json:"section"`
	}
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			res, _ := jsonResult(map[string]any{"deferred": true, "reason": "bad arguments: " + err.Error() + " — " + usage})
			return "", res
		}
	}
	section := strings.ToLower(strings.TrimSpace(in.Section))
	if section == "" {
		return statusSectionAll, nil
	}
	for _, v := range statusSectionValues() {
		if section == v {
			return section, nil
		}
	}
	res, _ := jsonResult(map[string]any{"deferred": true, "reason": fmt.Sprintf("unrecognized section %q — %s", in.Section, usage)})
	return "", res
}

// statusPayload builds the answer for one resolved section.
func (s *Server) statusPayload(ctx context.Context, cfg config.Config, section string) map[string]any {
	switch section {
	case statusSectionAll:
		payload := map[string]any{}
		for _, b := range statusBlockTable {
			if v := b.build(s, ctx, cfg); v != nil {
				payload[b.key] = v
			}
		}
		return payload
	case statusSectionBrief:
		return s.statusBrief(ctx, cfg)
	}
	for _, b := range statusBlockTable {
		if b.key == section {
			v := b.build(s, ctx, cfg)
			if v == nil {
				// Asked for by name, an absent block is an empty one — zero
				// accelerator devices — never null, which would read as unknown.
				v = map[string]any{}
			}
			return map[string]any{b.key: v}
		}
	}
	// Unreachable: parseStatusSection admits only statusSectionValues.
	return map[string]any{}
}

// statusBrief is the sizing answer: the whole fleet block (who the delegation
// seats are, their live ctx ceilings, their queues) plus ONE line each for this
// box's GPU lease and its local serving state, and the per-card table (gpu_cards:
// each card, its holder, the per-lease device sets, the queue). The two lines get
// their own keys rather than reusing gpu_lease/local, so a consumer that decodes
// those blocks as objects never meets a string under the same name.
func (s *Server) statusBrief(ctx context.Context, cfg config.Config) map[string]any {
	fleet := s.fleetView(ctx, cfg)
	seat, _ := fleet["local_agent_seat"].(map[string]any)
	leaseView, act := localLeaseViewWithActivity(ctx, cfg)
	return map[string]any{
		"fleet":             fleet,
		"gpu_lease_verdict": gpuLeaseVerdictLine(leaseView),
		// The per-card table (plan P3): which card each lease holds, free VRAM, the
		// display card, the per-lease device sets and the queue with its device sets.
		// A NEW key beside the verdict line; the line itself is unchanged.
		"gpu_cards":     gpuCardsSection(cfg, act),
		"local_verdict": localVerdictLine(ctx, cfg, seat),
	}
}

// gpuCardsSection is the brief's per-card block. The card table comes from the same
// nvidia-smi sample the lease verdict already took (act.GPUs), so the brief costs no
// second call; without one the block says there is no table and still lists the leases.
// The rows are gpucards.Section's, the same ones `gpu status` prints.
func gpuCardsSection(cfg config.Config, act gpuactivity.View) map[string]any {
	devs := make([]gpuprobe.Device, 0, len(act.GPUs))
	for _, g := range act.GPUs {
		devs = append(devs, gpuprobe.Device{
			Index: g.Index, UUID: g.UUID, Name: g.Name,
			TotalGiB: float64(g.MemTotalMiB) / 1024, FreeGiB: float64(g.MemTotalMiB-g.MemUsedMiB) / 1024,
			UtilPct: g.UtilPct, UtilKnown: g.UtilKnown, DisplayActive: g.DisplayActive, DisplayAttached: g.DisplayAttached,
		})
	}
	cards, note := gpuprobe.BuildCards(devs, cfg.GPUComfyOrder)
	if len(devs) == 0 {
		reason := act.GPUErr
		if reason == "" {
			reason = "the GPU sample was not taken"
		}
		note = "no card table (" + reason + ")"
	}
	var leases []gpulease.Info
	var waiters []gpulease.Waiter
	scoped := false
	if m, err := gpulease.OpenAt(cfg.GPULockPath, cfg.StateDir); err == nil {
		_ = m.ApplyCardScopedConfig(cfg.GPUCardScopedLeases)
		leases, waiters, scoped = modelaffinity.ScopeLeases(m.Dir(), m.Leases()), m.Waiters(), m.CardScoped()
	}
	sec := gpucards.Section(cards, note, leases, waiters)
	sec["card_scoped_leases"] = scoped
	return sec
}

// standingLead maps the standing verdicts to the capitalised word the brief line leads with.
var standingLead = map[string]string{
	gpuactivity.VerdictHeldOrphaned: "ORPHANED",
	gpuactivity.VerdictTreeOrphan:   "ORPHANED",
	gpuactivity.VerdictHeldOverdue:  "OVERDUE",
	gpuactivity.VerdictHeldStalled:  "STALLED",
}

// gpuLeaseVerdictLine renders the gpu_lease block as one line: the verdict word
// first (the thing to branch on), what the cards are doing, who holds them and
// why, how long the line is, and the queue command — a held card is a place in
// line, so the line always ends in how to join it.
func gpuLeaseVerdictLine(view map[string]any) string {
	var b strings.Builder
	// A host that is paging leads the line, whatever the cards are doing: "free" at the head of it
	// would read as "the box has room" while committed memory is above physical RAM.
	hostLead, hostLoud, hostTail := hostMemoryClauses(view)
	if hostLoud {
		b.WriteString(hostLead + "; ")
	}
	verdict, _ := view["verdict"].(string)
	// A lease that is orphaned, overdue or stalled leads with that word in capitals
	// (plan P8): "held" read as "busy, queue behind it" for the twenty hours an abandoned
	// render sat on the cards, and the word it needed was one a skimming reader skips.
	// The lead is driven by the lease the verdict is about, not only by the verdict word:
	// `working` (work in flight on the seat) outranks the standing verdicts but must not hide
	// them, so a stalled, orphaned or overdue holder leads with its word whatever the verdict.
	act, _ := view["activity"].(map[string]any)
	holder, _ := act["holder"].(*gpuactivity.Holder)
	word := verdict
	if _, own := standingLead[word]; !own {
		word = holder.StandingWord()
	}
	lead, escalated := standingLead[word]
	if escalated {
		b.WriteString(lead + " (" + verdict + ")")
	} else {
		b.WriteString(verdict)
	}
	if note, _ := act["note"].(string); note != "" {
		// The holder tail Assess appends is rebuilt below without the
		// holder's full command line, which is most of its length.
		note, _, _ = strings.Cut(note, " [holder pid")
		// The takeover command is re-stated once, below, where it cannot be clipped.
		note, _, _ = strings.Cut(note, "; to take it over")
		// An escalated line keeps more of the note: what is wrong with the lease must not be
		// the part a fixed width cuts off.
		clip := 240
		if escalated {
			clip = 480
		}
		b.WriteString(" — " + oneLine(note, clip))
	}
	if held, _ := view["held"].(bool); held {
		fmt.Fprintf(&b, "; held by pid %v (%v", view["pid"], view["class"])
		if ex, _ := view["exclusive"].(bool); ex {
			b.WriteString(", exclusive")
		}
		if dr, _ := view["draining"].(bool); dr {
			b.WriteString(", draining")
		}
		fmt.Fprintf(&b, ", %vs)", view["age_s"])
		if r, _ := view["reason"].(string); r != "" {
			b.WriteString(": " + oneLine(r, 100))
		}
	} else {
		b.WriteString("; no lease held")
	}
	if q, _ := view["queued"].(int); q > 0 {
		fmt.Fprintf(&b, "; %d queued", q)
	}
	if escalated {
		// The epoch of the lease the verdict is about (the Holder), never the lowest live one.
		epoch, _ := view["epoch"].(uint64)
		if holder != nil && holder.Epoch != 0 {
			epoch = holder.Epoch
		}
		if epoch != 0 {
			fmt.Fprintf(&b, "; a human-authorised session frees it with: %s (not in this build yet: until it ships, ask whoever owns it or the operator)", gpulease.TakeoverCommand(epoch))
		}
	}
	if hostTail != "" {
		b.WriteString("; " + hostTail)
	}
	cmd, _, _ := strings.Cut(gpulease.QueueHint, "  (")
	b.WriteString("; queue with: " + cmd)
	return b.String()
}

// hostMemoryView builds the host_memory block of the gpu_lease view (m is nil when the lease
// directory could not be opened: then nothing is declared and nothing is pending).
func hostMemoryView(cfg config.Config, m *gpulease.Manager) map[string]any {
	var declared, pending float64
	if m != nil {
		declared, pending = gpulease.DeclaredHostRAMGiB(m.Leases()), m.HostRAMPending()
	}
	mem, ok := gpuprobe.ReadHostMemory()
	return gpucards.NewHostView(mem, ok, cfg.GPUHostRAMHeadroom(), declared, pending).Map()
}

// hostMemoryClauses reads the verdict back out of the view's host_memory block and words it for the
// one-line brief: OVER is the loud lead, NEAR a tail, OK and unknown say nothing there (the block
// carries them). It reads the block and not the host again, so the line and the block cannot disagree.
func hostMemoryClauses(view map[string]any) (lead string, loud bool, tail string) {
	hm, _ := view["host_memory"].(map[string]any)
	if hm == nil {
		return "", false, ""
	}
	num := func(k string) float64 { f, _ := hm[k].(float64); return f }
	switch hm["verdict"] {
	case string(gpuprobe.HostOver):
		return fmt.Sprintf("HOST RAM OVER (committed %.1f of %.1f GiB physical: the box is paging)", num("commit_used_gib"), num("physical_gib")), true, ""
	case string(gpuprobe.HostNear):
		return "", false, fmt.Sprintf("host RAM NEAR the limit (committed %.1f of %.1f GiB physical, %.1f GiB headroom)", num("commit_used_gib"), num("physical_gib"), num("headroom_gib"))
	}
	return "", false, ""
}

// localVerdictLine renders this box's serving state as one line: is the local
// endpoint up and how much does it serve, what the local agent seat is doing
// (the same reading the fleet block's local_agent_seat carries), and which
// roster entries are empty — those capabilities defer on this box.
func localVerdictLine(ctx context.Context, cfg config.Config, seat map[string]any) string {
	var b strings.Builder
	if ids, err := probeServedModels(ctx, cfg.Endpoint); err != nil {
		fmt.Fprintf(&b, "local endpoint %s unreachable (%s)", cfg.Endpoint, oneLine(err.Error(), 120))
	} else {
		fmt.Fprintf(&b, "local endpoint %s up, %d models served", cfg.Endpoint, len(ids))
	}
	if model, _ := seat["model"].(string); model == "" {
		b.WriteString("; no local agent seat")
	} else {
		verdict, _ := seat["verdict"].(string)
		if verdict == "" {
			verdict = "unknown"
		}
		fmt.Fprintf(&b, "; agent seat %s: %s", model, verdict)
		if n, ok := seat["in_flight"]; ok {
			fmt.Fprintf(&b, ", %v in flight", n)
		}
		if n, ok := seat["ctx_tokens"]; ok {
			fmt.Fprintf(&b, ", ctx %v", n)
		}
		for _, k := range []string{"inflight_probe_error", "ctx_probe_error"} {
			if e, _ := seat[k].(string); e != "" {
				b.WriteString(" (" + k + ": " + oneLine(e, 100) + ")")
			}
		}
	}
	var gaps []string
	for _, r := range cfg.ModelRoutes() {
		if r.Effective == "" {
			gaps = append(gaps, r.StatusKey)
		}
	}
	if len(gaps) > 0 {
		b.WriteString("; no model, so these defer here: " + strings.Join(gaps, ", "))
	}
	return b.String()
}

// oneLine collapses every whitespace run (newlines included) to one space and
// caps the result at max runes, so a probe error or a lease reason can never
// break a verdict line.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// statusPair is the "pair" block: which way this box's PAIR emitter reports (local ingress, node-info
// fallback, relay <member route URL>, or off with the reason). nil (absent from the full answer) when
// pair_workloads_enabled is off. It reads the per-config emitter the wire headers use (identity, and in
// auto relay mode the delegate_remotes' health, cached), so a status call probes nothing a call just did.
func statusPair(cfg config.Config) any {
	if !cfg.PairWorkloadsEnabled {
		return nil
	}
	m := pairworkloads.ModeFor(cfg)
	out := map[string]any{"mode": m.Mode}
	if m.Relay != "" {
		out["relay"] = m.Relay
	}
	if m.Reason != "" {
		out["reason"] = m.Reason
	}
	return out
}
