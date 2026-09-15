// Package placement is the ONE decision table of the composite tier (ADR
// 0039, 0.116.0): it turns (task class, token need, quality gate,
// context_class, budget, live occupancy, live guards) into a Decision — which
// layer, which seat, on which device pin, and why. It exists as a pure package
// because three callers must agree on the same answer: the local box placing
// its own work, the delegator placing a contract onto a remote node's
// advertised layers (built from health rows through FromRows), and the node
// itself re-running the decision for the layer a contract names, with its OWN
// live readers so the display-card guards are evaluated where the card is
// (council R5: no second "fit" heuristic across layers). Everything the table
// needs about the machine arrives through Live; nothing here execs, dials or
// loads a model — the readers do, and only through the memoised Snapshot.
package placement

import (
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// Class is the task class the table keys on. It is a closed vocabulary rather
// than the caller's free-form kind because each class maps to exactly one
// default (layer, role) pair and a misspelt class must not silently take the
// agent row.
type Class string

const (
	// ClassMechanical is single-shot mechanical text (summarize / classify /
	// extract / triage): the cascade's router rung on the single layer, or the
	// display layer's twin when the pair holds the cards and the layer is awake.
	ClassMechanical Class = "mechanical"
	// ClassAgent is a delegation contract: the pair's agent seat by default,
	// the pair's long seat on window overflow, the triple's long seat on an
	// explicit context_class long.
	ClassAgent Class = "agent"
	// ClassOCR is the ocr vision role (single layer, device 2 on the Qube).
	ClassOCR Class = "ocr"
	// ClassVision is the vision role (the pair's vl-32b); documentary — no
	// media row is placed here.
	ClassVision Class = "vision"
)

// Layer names the table resolves by. They are the closed set config.ValidateLayers
// admits on a composite box, named here so a row that says "the pair's long
// seat" reads the same literal the fixture and the tier table declare.
const (
	LayerSingle  = "single"
	LayerPair    = "pair"
	LayerTriple  = "triple"
	LayerDisplay = "display"
)

// Seat roles the rows key on — the same closed vocabulary config holds
// LayerSeat.Role to.
const (
	RoleRouter = "router"
	RoleAgent  = "agent"
	RoleLong   = "long"
	RoleOCR    = "ocr"
	RoleVision = "vision"
)

// defaultMaxTokens is the completion budget assumed when a request carries
// none: the harness's usual max_tokens for an agent step, so a contract that
// omits it is still measured against a window with room to answer.
const defaultMaxTokens = 1024

// Request is everything the table needs to know about ONE unit of work. It
// carries an estimate, never a tokenizer count, because placement happens
// before any seat is touched (the estimate leans conservative: chars/3).
type Request struct {
	Class Class
	// EstTokens is the prompt-side estimate (EstimateTokens over goal, docs,
	// schema and acceptance).
	EstTokens int
	// MaxTokens is the completion budget (0 → 1024). need = EstTokens + MaxTokens
	// is what a window must hold.
	MaxTokens int
	// QualityGated is true when the contract carries acceptance checks or an
	// output_schema. Recorded for the reason text; the table never trades a
	// gated contract down to a weaker seat (council R2 removed the only row
	// that would have).
	QualityGated bool
	// ContextClass is "" (the estimate decides) or core.ContextClassLong (the
	// caller asks for the triple's long seat explicitly).
	ContextClass string
	// BudgetSec is the contract's timeout_sec (0 → core.AgentTimeoutSecDefault);
	// it bounds the prefill feasibility check on a long seat.
	BudgetSec int
	// RouteKey (mechanical only) is the cascade route being placed
	// (workhorse | triage | escalation) — the key into a router seat's
	// model_map when the display layer substitutes its twin.
	RouteKey string
}

// SeatState is one live occupancy reading. Known=false means "could not
// read" — the table treats that as NEUTRAL for occupancy (no saturation note,
// no wait) because occupancy only shapes the reason and the wait, never a
// guard; a guard that cannot read refuses instead.
type SeatState struct {
	Known    bool
	Loaded   bool
	Inflight int
}

// Presence is the operator-presence reading the presence guard consumes.
// Mode echoes the config (present | away | auto); Known=false means the probe
// could not tell (no console session, an API failure) and the guard refuses.
type Presence struct {
	Mode    string
	Known   bool
	Away    bool
	Locked  bool
	IdleSec int
	Note    string
}

// Live is the reader contract: how the table sees the machine. Every reader
// may be nil (= unknown): a guard whose reader is nil refuses (fail closed)
// unless Verdict supplies a remote row's own answer; an occupancy reader that
// is nil is neutral. The local box builds it from a Snapshot (memoised, read
// once per decision); a delegator builds it from a remote node's health rows
// through FromRows, where only Seat and Verdict are set — the display-card
// numbers were read where the card is and are carried as the row's verdict.
type Live struct {
	// Seat reads a (layer, role) seat's live occupancy.
	Seat func(layer, role string) SeatState
	// DeviceFree reads a device's free VRAM in GiB by CUDA index or GPU-UUID
	// prefix; !ok = not visible = refuse.
	DeviceFree func(device string) (float64, bool)
	// DeviceIndex resolves a GPU-UUID prefix to the CUDA index nvidia-smi
	// reports for it, so a UUID-pinned display device (the Qube pins it by
	// UUID because the board reorders indices on power loss) can be matched
	// against a seat's index pin. !ok = cannot resolve = the guard refuses.
	DeviceIndex func(device string) (string, bool)
	// HostFree reads free host RAM in GiB; !ok = refuse on the host_ram guard.
	HostFree func() (float64, bool)
	// Presence reads the operator-presence state (ProbePresence).
	Presence func() Presence
	// Verdict returns a remote row's own admissibility for a layer and the
	// node's reason (nil = no verdict). It stands in for a guard ONLY where
	// that guard's reader is nil: a live reading always wins over a carried
	// verdict, and the node re-checks at admission anyway. The reason rides
	// along so a delegator's defer can say what the node saw ("free 3.0 GiB −
	// footprint 10.5 < floor 4"), not merely that it refused.
	Verdict func(layer string) (verdict *bool, reason string)
}

// Decision is what the table hands back: the Placed block every result
// publishes, plus the three flags a caller acts on. On a Defer whose cause is
// a layer that exists (a guard, an infeasible prefill), Placed.Layer names it;
// on a Defer with no candidate layer at all (no layer serves the class, no
// window holds the contract) Layer is the largest-window layer considered or
// "" when there was none — the reason always says which.
type Decision struct {
	core.Placed
	// Wait asks the caller to hold in the capacity wait until the seat named
	// in Evicts drains, then run (the pair-long eviction rule, council R1).
	Wait bool
	// Defer is a refusal; DeferClass is core.DeferClassContract (the contract
	// cannot be served as written) or core.DeferClassCapacity (a guard refused
	// right now; Guard names it).
	Defer      bool
	DeferClass string
}

// EstimateTokens is the prompt-side estimate every placement starts from:
// ceil(chars/3) over goal, each context doc's name and text, the raw output
// schema and every acceptance string — the exact arithmetic
// delegate.EstimateTokens has always used, moved here so the delegate can call
// this and both sides of the wire size a contract identically. chars/3 is a
// deliberate conservative bound, not a tokenizer: it overshoots English prose
// by ~33 % and is used only as a gate, never to claim a fit.
func EstimateTokens(c core.AgentContract) int {
	chars := len(c.Goal) + len(c.OutputSchema)
	for _, d := range c.Context {
		chars += len(d.Name) + len(d.Text)
	}
	for _, a := range c.Acceptance {
		chars += len(a)
	}
	return (chars + 2) / 3
}

// RequestForContract builds the agent-class Request for a contract from the
// estimate and completion budget the caller already computed, so the two
// agent doors, the delegate runner and the node all describe a contract to
// the table the same way (quality gate = acceptance or schema present;
// budget = timeout_sec; class = the contract's context_class).
func RequestForContract(c core.AgentContract, est, maxTokens int) Request {
	return Request{
		Class:        ClassAgent,
		EstTokens:    est,
		MaxTokens:    maxTokens,
		QualityGated: len(c.Acceptance) > 0 || len(c.OutputSchema) > 0,
		ContextClass: c.ContextClass,
		BudgetSec:    c.TimeoutSec,
	}
}

// need is the tokens a window must hold for req: the prompt estimate plus the
// completion budget (defaulted), never the estimate alone — a contract that
// fits its prompt with no room to answer does not fit.
func (r Request) need() int {
	mt := r.MaxTokens
	if mt <= 0 {
		mt = defaultMaxTokens
	}
	return r.EstTokens + mt
}

// budget is the contract's wall budget with the door's default applied.
func (r Request) budget() int {
	if r.BudgetSec <= 0 {
		return core.AgentTimeoutSecDefault
	}
	return r.BudgetSec
}

// Decide runs the table over every declared layer. A box with no layers gets
// the zero Decision — the caller publishes nothing and behaves exactly as the
// pre-layer build (the byte-identical constraint).
func Decide(req Request, layers []config.LayerSpec, live Live) Decision {
	if len(layers) == 0 {
		return Decision{}
	}
	return decide(req, layers, live, "")
}

// DecideOnLayer is Decide restricted to one named layer: the node uses it for
// the layer a contract carries (contract.layer), the delegator for a remote's
// single advertised layer. Every other layer's OCCUPANCY is still read (the
// single layer's time-share note needs the pair's state) — only the CHOICE is
// pinned. A dormant layer named explicitly is a capacity defer: the operator
// decides when it wakes, never a contract.
func DecideOnLayer(req Request, layers []config.LayerSpec, name string, live Live) Decision {
	if len(layers) == 0 {
		return Decision{}
	}
	l, ok := findLayer(layers, name)
	if !ok {
		return deferContract("", fmt.Sprintf("layer %s is not declared on this box", name))
	}
	if l.Dormant {
		return Decision{Placed: core.Placed{Tier: l.Tier, Layer: l.Name, Reason: fmt.Sprintf("layer %s is dormant (operator decision)", l.Name)}, Defer: true, DeferClass: core.DeferClassCapacity}
	}
	return decide(req, layers, live, name)
}

// decide is the table. `only` is "" for the free choice or a layer name that
// pins it; allowed() gates every CHOICE while occupancy reads stay global.
func decide(req Request, layers []config.LayerSpec, live Live, only string) Decision {
	t := table{req: req, layers: layers, live: live, only: only}
	switch req.Class {
	case ClassMechanical:
		return t.mechanical()
	case ClassOCR:
		return t.byRole(LayerSingle, RoleOCR, "ocr → the single layer's ocr seat")
	case ClassVision:
		return t.byRole(LayerPair, RoleVision, "vision → the pair's vision seat")
	case ClassAgent:
		return t.agent()
	}
	return deferContract("", fmt.Sprintf("task class %q is not one the placement table serves (mechanical, agent, ocr, vision)", req.Class))
}

// table is one evaluation's state; its methods are the rows.
type table struct {
	req    Request
	layers []config.LayerSpec
	live   Live
	only   string
}

func (t table) allowed(name string) bool { return t.only == "" || t.only == name }

// choosable is a layer the free choice may land on: declared, not dormant,
// and not excluded by a restriction.
func (t table) choosable(name string) (config.LayerSpec, bool) {
	l, ok := findLayer(t.layers, name)
	if !ok || l.Dormant || !t.allowed(name) {
		return config.LayerSpec{}, false
	}
	return l, true
}

func (t table) seat(layer, role string) (config.LayerSpec, config.LayerSeat, bool) {
	l, ok := t.choosable(layer)
	if !ok {
		return config.LayerSpec{}, config.LayerSeat{}, false
	}
	s, ok := findSeat(l, role)
	return l, s, ok
}

// occupancy reads a seat's live state regardless of the restriction —
// another layer's load is a fact about the machine, not a choice.
func (t table) occupancy(layer, role string) SeatState {
	if t.live.Seat == nil {
		return SeatState{}
	}
	return t.live.Seat(layer, role)
}

// admissible runs LayerAdmissible with the layer's carried verdict, and on a
// refusal that the node itself had already recorded, appends the node's own
// reason so the delegator's defer names the reading the card actually gave.
func (t table) admissible(l config.LayerSpec, s config.LayerSeat) (ok bool, reason, guard string) {
	var (
		v          *bool
		nodeReason string
	)
	if t.live.Verdict != nil {
		v, nodeReason = t.live.Verdict(l.Name)
	}
	ok, reason, guard = LayerAdmissible(l, s, t.live, v)
	if !ok && v != nil && !*v && nodeReason != "" {
		reason += " — node reports: " + nodeReason
	}
	return ok, reason, guard
}

// pairAgentModel names the pair's agent seat for eviction/time-share notes;
// "" when the box has none.
func (t table) pairAgentModel() string {
	if l, ok := findLayer(t.layers, LayerPair); ok {
		if s, ok := findSeat(l, RoleAgent); ok {
			return s.Model
		}
	}
	return ""
}

// mechanical is row 2: the single router, or the display twin when the pair
// holds the cards and the display layer is awake and its guards pass.
func (t table) mechanical() Decision {
	pair := t.occupancy(LayerPair, RoleAgent)
	pairLoaded := pair.Known && pair.Loaded
	single, router, haveSingle := t.seat(LayerSingle, RoleRouter)

	var refusal string
	if display, twin, ok := t.seat(LayerDisplay, RoleRouter); ok && (pairLoaded || t.only == LayerDisplay) {
		seat := twin.ModelMap[t.req.RouteKey]
		if seat == "" {
			refusal = fmt.Sprintf("display layer has no twin for route %q", t.req.RouteKey)
		} else if ok, reason, guard := t.admissible(display, twin); ok {
			return Decision{Placed: core.Placed{
				Tier: display.Tier, Layer: display.Name, Role: RoleRouter, Seat: seat, Devices: twin.DeviceList(),
				Reason: fmt.Sprintf("mechanical text → display layer (router twin %s on device %s; the pair holds its cards): %s", seat, twin.Device, reason),
			}}
		} else {
			refusal = fmt.Sprintf("display layer refused (%s: %s)", guard, reason)
			if !haveSingle {
				return Decision{Placed: core.Placed{Tier: display.Tier, Layer: display.Name, Role: RoleRouter, Reason: refusal, Guard: guard}, Defer: true, DeferClass: core.DeferClassCapacity}
			}
		}
	}
	if !haveSingle {
		return deferContract("", t.restricted("no layer serves mechanical text (role router)"))
	}
	reason := fmt.Sprintf("mechanical text → single layer (router rung on device %s)", router.Device)
	d := Decision{Placed: core.Placed{Tier: single.Tier, Layer: single.Name, Role: RoleRouter, Devices: router.DeviceList()}}
	if pairLoaded {
		reason += "; the single layer time-shares the pair's cards — the rung waits behind or displaces the pair seat"
		d.Evicts = t.pairAgentModel()
	}
	if refusal != "" {
		reason += "; " + refusal
	}
	d.Reason = reason
	return d
}

// byRole is row 3: a documentary placement onto a fixed (layer, role).
func (t table) byRole(layer, role, why string) Decision {
	l, s, ok := t.seat(layer, role)
	if !ok {
		return deferContract("", t.restricted(fmt.Sprintf("no layer serves role %s on this box", role)))
	}
	return Decision{Placed: core.Placed{Tier: l.Tier, Layer: l.Name, Role: role, Seat: s.Model, Devices: s.DeviceList(), CtxTokens: s.CtxTokens, Reason: fmt.Sprintf("%s (%s on device %s)", why, s.Model, s.Device)}}
}

// agent is rows 4–8.
func (t table) agent() Decision {
	need := t.req.need()
	// Row 4: an explicit long ask takes the biggest long-context layer the box
	// declares — the triple's guarded long seat where one exists, otherwise the
	// pair's. The fallback is the 2026-09-10 operator rule (RAM is overflow
	// only): the three-card Flash-Next arms held 28-32 expert layers in host
	// memory and were removed from the tier table (0.115.4), so the reference
	// box declares NO triple layer and `context_class: long` means the pair's
	// 262k twin. Refusing it instead would make the field dead on every box
	// that ships today, and the pair's long seat is measured (pp 1,197 t/s).
	if t.req.ContextClass == core.ContextClassLong {
		if l, s, ok := t.seat(LayerTriple, RoleLong); ok {
			return t.longSeat(l, s, need, fmt.Sprintf("context_class long → %s layer's long seat", l.Name))
		}
		if d, ok := t.pairLong(need, fmt.Sprintf("context_class long (~%d tokens) → the pair's long seat", need)); ok {
			return d
		}
		return deferContract("", t.restricted("no layer serves context_class long on this box"))
	}
	largest, largestLayer := 0, ""
	note := func(s config.LayerSeat, layer string) {
		if s.CtxTokens > largest {
			largest, largestLayer = s.CtxTokens, layer
		}
	}
	// Row 5: fits the pair's agent seat — always, saturation recorded only.
	if l, s, ok := t.seat(LayerPair, RoleAgent); ok {
		note(s, l.Name)
		if need <= s.CtxTokens {
			d := Decision{Placed: core.Placed{Tier: l.Tier, Layer: l.Name, Role: RoleAgent, Seat: s.Model, Devices: s.DeviceList(), CtxTokens: s.CtxTokens}}
			reason := fmt.Sprintf("agent contract (~%d tokens) fits the pair's agent seat %s (window %d)", need, s.Model, s.CtxTokens)
			st := t.occupancy(l.Name, RoleAgent)
			if st.Known && st.Loaded && s.MaxInflight > 0 && st.Inflight >= s.MaxInflight {
				reason += fmt.Sprintf("; pair at %d/%d in flight — queued in the seat (recorded, not re-placed: no other layer can hold this contract beside a loaded pair)", st.Inflight, s.MaxInflight)
			}
			d.Reason = reason
			return d
		}
	}
	// Row 6: window overflow onto the pair's long seat, waiting out a busy agent seat.
	if l, s, ok := t.seat(LayerPair, RoleLong); ok {
		note(s, l.Name)
		agentWindow := 0
		if as, ok := findSeat(l, RoleAgent); ok {
			agentWindow = as.CtxTokens
		}
		if d, ok := t.pairLong(need, fmt.Sprintf("window overflow (need ~%d > %d); the pair's long seat holds it", need, agentWindow)); ok {
			return d
		}
	}
	// Row 7: the triple's long seat, on boxes whose pair has no long seat (or when restricted to it).
	if l, s, ok := t.seat(LayerTriple, RoleLong); ok {
		note(s, l.Name)
		if need <= s.CtxTokens {
			return t.longSeat(l, s, need, fmt.Sprintf("window overflow (need ~%d) → %s layer's long seat", need, l.Name))
		}
	}
	// Row 8.
	if largest == 0 {
		return deferContract("", t.restricted("no layer serves an agent contract (roles agent / long)"))
	}
	return deferContract(largestLayer, t.restricted(fmt.Sprintf("contract needs ~%d tokens; no layer window holds it (largest %d)", need, largest)))
}

// pairLong is the pair's long-seat row, shared by window overflow (row 6) and
// an explicit long ask on a box with no three-card layer (row 4). ok=false
// when the pair declares no long seat or the contract does not fit its window,
// so the caller falls through to the next row rather than deferring here.
//
// The eviction rule is council R1 and it lives HERE so both entries obey it:
// the long seat and the agent seat share the pair's cards, so loading one
// evicts the other — a mid-flight agent seat is WAITED for (the capacity wait
// credits the idle time) and named in Evicts; a cold or idle one is displaced
// with a note; occupancy that could not be read is treated as cold and waited
// on by nobody, because a wait on an unknown is a wait that never ends.
func (t table) pairLong(need int, head string) (Decision, bool) {
	l, s, ok := t.seat(LayerPair, RoleLong)
	if !ok || need > s.CtxTokens {
		return Decision{}, false
	}
	d := Decision{Placed: core.Placed{Tier: l.Tier, Layer: l.Name, Role: RoleLong, Seat: s.Model, Devices: s.DeviceList(), CtxTokens: s.CtxTokens}}
	head = fmt.Sprintf("%s (%s, window %d)", head, s.Model, s.CtxTokens)
	agent := t.pairAgentModel()
	st := t.occupancy(l.Name, RoleAgent)
	switch {
	case agent == "":
		d.Reason = head + " — the pair has no agent seat to evict"
	case !st.Known:
		d.Reason = head + fmt.Sprintf(" — %s occupancy unknown (treated as cold; nothing waited on)", agent)
	case st.Loaded && st.Inflight > 0:
		d.Wait, d.Evicts = true, agent
		d.Reason = head + fmt.Sprintf(" — evicts %s (%d in flight): waits for it to drain", agent, st.Inflight)
	case st.Loaded:
		d.Evicts = agent
		d.Reason = head + fmt.Sprintf(" — evicts %s (idle)", agent)
	default:
		d.Reason = head + fmt.Sprintf(" — %s is not loaded (cold), nothing to evict", agent)
	}
	return d, true
}

// longSeat is the shared body of rows 4 and 7: the prefill feasibility check
// (tokens ÷ measured rate ≤ the contract's budget — flash-next-262k prefills at
// 69 t/s, so a 200k prompt is ~48 min against a 900 s cap and can never finish
// inside any contract) then the layer's guards, admitting with the readings in
// the reason or deferring on the named guard.
func (t table) longSeat(l config.LayerSpec, s config.LayerSeat, need int, head string) Decision {
	if need > s.CtxTokens {
		return deferContract(l.Name, fmt.Sprintf("contract needs ~%d tokens; the %s layer's long seat window is %d", need, l.Name, s.CtxTokens))
	}
	budget := t.req.budget()
	secs := 0.0
	if s.PrefillTPS > 0 {
		secs = float64(need) / s.PrefillTPS
	}
	if s.PrefillTPS <= 0 || secs > float64(budget) {
		rate := "an undeclared prefill rate"
		if s.PrefillTPS > 0 {
			rate = fmt.Sprintf("%.0f t/s", s.PrefillTPS)
		}
		return deferContract(l.Name, fmt.Sprintf("context_class long on %s: ~%d tokens at %s prefill ≈ %.0f s exceeds the contract budget %d s (raise timeout_sec or shrink the context)", s.Model, need, rate, secs, budget))
	}
	ok, reason, guard := t.admissible(l, s)
	if !ok {
		return Decision{Placed: core.Placed{Tier: l.Tier, Layer: l.Name, Role: RoleLong, Seat: s.Model, Devices: s.DeviceList(), CtxTokens: s.CtxTokens,
			Reason: fmt.Sprintf("%s layer refused by %s: %s", l.Name, guard, reason), Guard: guard}, Defer: true, DeferClass: core.DeferClassCapacity}
	}
	return Decision{Placed: core.Placed{Tier: l.Tier, Layer: l.Name, Role: RoleLong, Seat: s.Model, Devices: s.DeviceList(), CtxTokens: s.CtxTokens,
		Reason: fmt.Sprintf("%s (%s, window %d): prefill ~%d tokens at %.0f t/s ≈ %.0f s within budget %d s; guards: %s", head, s.Model, s.CtxTokens, need, s.PrefillTPS, secs, budget, reason)}}
}

// restricted appends the restriction to a "nothing serves this" reason so a
// node told "layer single" learns the layer, not just the class, is the problem.
func (t table) restricted(reason string) string {
	if t.only == "" {
		return reason
	}
	return reason + fmt.Sprintf(" (restricted to layer %s)", t.only)
}

func deferContract(layer, reason string) Decision {
	return Decision{Placed: core.Placed{Layer: layer, Reason: reason}, Defer: true, DeferClass: core.DeferClassContract}
}

func findLayer(layers []config.LayerSpec, name string) (config.LayerSpec, bool) {
	for _, l := range layers {
		if l.Name == name {
			return l, true
		}
	}
	return config.LayerSpec{}, false
}

func findSeat(l config.LayerSpec, role string) (config.LayerSeat, bool) {
	for _, s := range l.Seats {
		if s.Role == role {
			return s, true
		}
	}
	return config.LayerSeat{}, false
}

// joinReadings renders the per-guard readings an admission carries.
func joinReadings(parts []string) string { return strings.Join(parts, "; ") }
