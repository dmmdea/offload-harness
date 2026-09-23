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
	"github.com/dmmdea/offload-harness/internal/gpulease"
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
	{"local", func(s *Server, ctx context.Context, cfg config.Config) any { return s.statusLocal(ctx, cfg) }},
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
const statusSectionDoc = "all (default): everything. brief: fleet + one-line gpu_lease and local verdicts. Any other value: that block only."

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
// box's GPU lease and its local serving state. The two lines get their own keys
// rather than reusing gpu_lease/local, so a consumer that decodes those blocks
// as objects never meets a string under the same name.
func (s *Server) statusBrief(ctx context.Context, cfg config.Config) map[string]any {
	fleet := s.fleetView(ctx, cfg)
	seat, _ := fleet["local_agent_seat"].(map[string]any)
	return map[string]any{
		"fleet":             fleet,
		"gpu_lease_verdict": gpuLeaseVerdictLine(localLeaseView(ctx, cfg)),
		"local_verdict":     localVerdictLine(ctx, cfg, seat),
	}
}

// gpuLeaseVerdictLine renders the gpu_lease block as one line: the verdict word
// first (the thing to branch on), what the cards are doing, who holds them and
// why, how long the line is, and the queue command — a held card is a place in
// line, so the line always ends in how to join it.
func gpuLeaseVerdictLine(view map[string]any) string {
	var b strings.Builder
	verdict, _ := view["verdict"].(string)
	b.WriteString(verdict)
	if act, ok := view["activity"].(map[string]any); ok {
		if note, _ := act["note"].(string); note != "" {
			// The holder tail Assess appends is rebuilt below without the
			// holder's full command line, which is most of its length.
			note, _, _ = strings.Cut(note, " [holder pid")
			b.WriteString(" — " + oneLine(note, 240))
		}
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
	cmd, _, _ := strings.Cut(gpulease.QueueHint, "  (")
	b.WriteString("; queue with: " + cmd)
	return b.String()
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
