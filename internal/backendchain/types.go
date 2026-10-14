// Package backendchain answers, for every lane and every seat of a node, one question: which serving
// engine is answering for it right now, and how do we know.
//
// The idea is the ordered backend chain with a real probe of Agent-Reach's `doctor` (a channel has an
// ordered list of backends, the first one whose probe passes is the active one, and the report says so),
// rebuilt here without any of its dependencies and kept engine-neutral: the report names an engine KIND
// ("llama.cpp", "vllm", "rkllm", and "strata" for the seat that is reserved for it), never a product
// path, so a new engine registers by adding one row to the classification table (classify.go) and
// changes no schema.
//
// Four fields describe a verdict, on every lane and every seat:
//
//	active_backend  the engine kind that is answering, or "none". On a lane it is the engine of the seat
//	                the lane is bound to: the lane routes there, whatever else of its chain is up
//	backend_chain   the ordered engine kinds the lane or seat may use
//	backend_probe   how deep the evidence goes: "process", "endpoint" or "completion"
//	backend_reason  why that backend was chosen, or why none was (it names the failures that came first)
//
// The default probe is cheap and LOADS NOTHING: it reads llama-swap's roster and its running list, and
// asks a seat that is already loaded for its own health endpoint. It never sends a completion, and never
// goes through llama-swap's /upstream path (that path loads a model on demand and resets the idle timer).
// A 1-token completion exists only behind an explicit option, because it loads an idle seat and can
// evict a vLLM seat (internal/seatguard exists for the same reason); it is refused for any seat that is
// not running while a vLLM seat is loaded, and llama-swap's running list is re-read after each one.
//
// The existing `backends` field of /fleet/health (hardware tags such as "vulkan" and "cpu", ADR 0054) is
// a different thing and is untouched.
package backendchain

// Engine kinds. The set is open: a consumer treats an unfamiliar kind as an opaque string.
const (
	EngineLlamaCpp = "llama.cpp"
	EngineVLLM     = "vllm"
	EngineRKLLM    = "rkllm"
	// EngineStrata is reserved for the Strata seat. Nothing classifies into it yet: the seat registers by
	// adding a row to engineRules, with no change to any field or schema.
	EngineStrata = "strata"
	// EngineNone is active_backend when no backend of the chain can serve.
	EngineNone = "none"
	// EngineUnknown is the kind of a seat that can serve but whose engine the config does not say (an
	// entry whose command names no engine this table knows, or no serving config to read it from).
	EngineUnknown = "unknown"
)

// Probe levels, shallowest first.
const (
	// ProbeProcess: the verdict rests on llama-swap's own state (the roster and the running list).
	ProbeProcess = "process"
	// ProbeEndpoint: the seat was loaded and its own health endpoint was asked (the seat's own address,
	// never through llama-swap).
	ProbeEndpoint = "endpoint"
	// ProbeCompletion: a 1-token completion ran against the seat. Only with the explicit option.
	ProbeCompletion = "completion"
)

// Entry is one verdict. It is the wire shape of /fleet/health's lane_backends and seat_backends and the
// source of doctor's rows. Every field is additive: a reader that predates it ignores the keys.
type Entry struct {
	// Lane is the lane the verdict is about (the config's status key: "agent", "workhorse", ...);
	// empty on a seat row.
	Lane string `json:"lane,omitempty"`
	// Seat is the seat the verdict names: the one that is answering (a lane's bound seat), or, when none
	// is, the lane's bound seat (the first seat of the chain on a lane with no binding).
	Seat          string   `json:"seat,omitempty"`
	ActiveBackend string   `json:"active_backend"`
	BackendChain  []string `json:"backend_chain"`
	BackendProbe  string   `json:"backend_probe"`
	BackendReason string   `json:"backend_reason"`
}

// Report is every lane and every seat of one node.
type Report struct {
	Lanes []Entry `json:"lane_backends,omitempty"`
	Seats []Entry `json:"seat_backends,omitempty"`
}

// Candidate is one backend of a chain: a seat and the engine kind that serves it.
type Candidate struct {
	// Seat is the model name the lane would call (a canonical id or an alias).
	Seat string
	// Engine is the kind; empty asks the collector to classify it from the serving config.
	Engine string
	// Bound is true for the seat the lane's config actually points at, and that seat's engine is what
	// answers for the lane. A chain may list preferred seats the lane is not bound to (a tier's preferred
	// vLLM seat while the lane serves its llama.cpp fallback): they are probed and named in the reason,
	// and they do not answer for the lane.
	Bound bool
}

// Lane is a named chain: candidates in preference order.
type Lane struct {
	Name  string
	Chain []Candidate
}

// Verdict is one candidate's probe result.
type Verdict struct {
	OK bool
	// Probe is the deepest level the evidence reached; empty reads as ProbeProcess.
	Probe string
	// Detail is one short clause: what was seen. It never carries an address, a path or a command line,
	// because it is published to every node that reads /fleet/health.
	Detail string
}
