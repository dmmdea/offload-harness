package core

// Placed is the placement decision published on every result a composite box
// produces (ADR 0039, 0.116.0): which tier, layer and seat served the work, on
// which device pin, and why — or, on a defer, which guard refused. It exists
// because the composite-tier spec's fourth defect was "placement is
// invisible": a result named neither the cards nor the seat that served it, so
// a pair-seat run read the same as a 3-card one and a defer caused by the
// display-card floor read the same as one caused by a full seat.
//
// nil means "the box is not composite". Every carrier (AgentWireResult, Meta,
// the delegate wire) declares it `omitempty`, so a box whose config declares no
// layers stays byte-identical on the wire, in the ledger and on every status
// surface — the Global Constraint every composite change is pinned against.
//
// The block is published under the key `placed`. The older `placement` REASON
// STRING on delegate results is untouched: the opencode plugin and
// fleet_smoke read it as text, and this block is additive beside it.
type Placed struct {
	// Tier is the composed tier the layer belongs to (blackwell-16 for the
	// single layer of a blackwell-3x16 box). Omitted when the layer carries no
	// tier of its own.
	Tier string `json:"tier,omitempty"`
	// Layer is always present when the block is: a Placed with no layer would
	// be a decision that names nothing.
	Layer string `json:"layer"`
	// Role is the seat role inside the layer (router | agent | long | ocr | …).
	Role string `json:"role,omitempty"`
	// Seat is the llama-swap id or alias that served the work.
	Seat string `json:"seat,omitempty"`
	// Devices is the SEAT's CUDA_VISIBLE_DEVICES pin, never the layer's list
	// of alternative device sets (R3) — a reader must be able to say which
	// cards actually held the work.
	Devices []string `json:"devices,omitempty"`
	// CtxTokens is the seat's window as declared in config — never probed, so
	// no status path loads a model to fill this in.
	CtxTokens int `json:"ctx_tokens,omitempty"`
	// Reason is always present: prose for the operator that says why THIS
	// layer/seat (or why none), including a recorded-only saturation note
	// such as "pair at 32/32 in flight — queued in the seat" (R2).
	Reason string `json:"reason"`
	// Guard names the guard that refused (display_floor | presence | host_ram)
	// on a defer, so a guard defer is branchable, never just a sentence.
	Guard string `json:"guard,omitempty"`
	// Evicts names the seat this placement displaces on a shared device set,
	// when the decision knows it (the pair's long seat evicting the agent
	// seat), so a capacity wait can say what it is waiting to avoid.
	Evicts string `json:"evicts,omitempty"`
}
