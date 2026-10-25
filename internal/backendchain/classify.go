package backendchain

import "strings"

// SeatFacts is what is known about one seat when its engine is classified.
type SeatFacts struct {
	// DeclaredVLLM: the box (vllm_seats) or the tier table says a vLLM engine serves the seat. A vLLM
	// entry runs through a wrapper script, so its command cannot name the engine.
	DeclaredVLLM bool
	// Cmd is the seat's command line from the serving config, macros expanded, or the command llama-swap
	// reports it spawned when the seat is loaded; empty when neither is known.
	Cmd string
	// Declared is the kind a table the box ships (the tier table) declares for the seat. It answers only
	// when nothing the seat's own command or declaration says does, so one seat has one kind wherever it
	// is read.
	Declared string
}

// cmdNames matches a command line that names an engine's executable or launcher.
func cmdNames(needle string) func(SeatFacts) bool {
	return func(f SeatFacts) bool { return strings.Contains(strings.ToLower(f.Cmd), needle) }
}

// engineRules is the classification table, tried in order. It is the ONE place an engine is registered:
// a new engine adds a row and changes no field, no schema and no consumer. A guessed kind is worse than
// an unknown one, so a row matches a statement the box made (a declaration, or the executable its
// command names), never a name.
var engineRules = []struct {
	kind  string
	match func(SeatFacts) bool
}{
	{EngineVLLM, func(f SeatFacts) bool { return f.DeclaredVLLM }},
	{EngineLlamaCpp, cmdNames("llama-server")},
	{EngineRKLLM, cmdNames("rkllm")},
	{EngineWhisperCpp, cmdNames("whisper-server")},
	{EngineForecast, cmdNames("forecast-serve")},
}

// ClassifyEngine names the engine kind that serves a seat: the table's first row that matches what the
// seat's own declaration and command say, else the kind a shipped table declares for it, else
// EngineUnknown.
func ClassifyEngine(f SeatFacts) string {
	for _, r := range engineRules {
		if r.match(f) {
			return r.kind
		}
	}
	if k := strings.TrimSpace(f.Declared); k != "" {
		return k
	}
	return EngineUnknown
}

// nonChatFlags are the llama-server flags that make a seat serve embeddings or reranking, which have no
// chat route. They are read from the seat's own command, the way the serving config's author wrote them.
var nonChatFlags = []string{"--embedding", "--embeddings", "--rerank", "--reranking"}

// cmdHasNonChatFlag reports whether a command line carries one of nonChatFlags as a whole argument.
func cmdHasNonChatFlag(cmd string) bool {
	for _, arg := range strings.Fields(cmd) {
		for _, f := range nonChatFlags {
			if arg == f {
				return true
			}
		}
	}
	return false
}
