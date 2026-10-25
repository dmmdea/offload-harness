// Package backendchain answers, for every lane and every seat of a node, one question: which serving
// engine is answering for it right now, and how do we know.
//
// The idea is the ordered backend chain with a real probe of Agent-Reach's `doctor` (a channel has an
// ordered list of backends, each with a real probe, and the report says which one is active and why),
// rebuilt here without any of its dependencies and kept engine-neutral: the report names an engine KIND
// ("llama.cpp", "vllm", "rkllm", "whisper.cpp"), never a product path, so a new engine registers by
// adding one row to the classification table (classify.go) and changes no schema.
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
// A 1-token completion exists only behind an explicit option, because it loads an idle seat and the load
// can evict a seat that is running (internal/seatguard exists for the same reason). It goes only to a seat
// that is already running, or, on a box where nothing is running, to one seat; any other seat is reported
// as not probed, over the running seat the load would have been, and a seat with no chat route is never
// sent one.
//
// The existing `backends` field of /fleet/health (hardware tags such as "vulkan" and "cpu", ADR 0054) is
// a different thing and is untouched.
package backendchain

// Engine kinds. The set is open: a consumer treats an unfamiliar kind as an opaque string.
const (
	EngineLlamaCpp = "llama.cpp"
	EngineVLLM     = "vllm"
	EngineRKLLM    = "rkllm"
	// EngineWhisperCpp is the speech-to-text server.
	EngineWhisperCpp = "whisper.cpp"
	// EngineForecast is the time-series forecast server (`forecast-serve`, a Python server).
	EngineForecast = "forecast"
	// EngineNone is active_backend when no backend of the chain can serve.
	EngineNone = "none"
	// EngineUnknown is the kind of a seat that can serve but whose engine the config does not say (an
	// entry whose command names no engine this table knows, or no serving config to read it from), and
	// the active_backend of a row whose probe saw nothing because it timed out, or whose seat is served by
	// another node.
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
	// Unknown: the probe saw nothing (it timed out), so the candidate is neither up nor down, and a lane
	// bound to it reads "unknown", not "none". OK is false.
	Unknown bool
	// Probe is the deepest level the evidence reached; empty reads as ProbeProcess.
	Probe string
	// Detail is one short clause: what was seen. It never carries an address, a path or a command line,
	// because it is published to every node that reads /fleet/health.
	Detail string
}
