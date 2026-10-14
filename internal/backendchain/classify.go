package backendchain

import "strings"

// SeatFacts is what is known about one seat when its engine is classified.
type SeatFacts struct {
	// DeclaredVLLM: the box (vllm_seats) or the tier table says a vLLM engine serves the seat. A vLLM
	// entry runs through a wrapper script, so its command cannot name the engine.
	DeclaredVLLM bool
	// Cmd is the seat's command line from the serving config, macros expanded; empty when unreadable.
	Cmd string
}

// cmdNames matches a command line that names an engine's executable or launcher.
func cmdNames(needle string) func(SeatFacts) bool {
	return func(f SeatFacts) bool { return strings.Contains(strings.ToLower(f.Cmd), needle) }
}

// engineRules is the classification table, tried in order. It is the ONE place an engine is registered:
// a new engine adds a row and changes no field, no schema and no consumer. The Strata seat registers
// here (EngineStrata) when its launcher exists; nothing classifies into it before that, because a
// guessed kind is worse than an unknown one.
var engineRules = []struct {
	kind  string
	match func(SeatFacts) bool
}{
	{EngineVLLM, func(f SeatFacts) bool { return f.DeclaredVLLM }},
	{EngineLlamaCpp, cmdNames("llama-server")},
	{EngineRKLLM, cmdNames("rkllm")},
}

// ClassifyEngine names the engine kind that serves a seat, or EngineUnknown when neither a declaration
// nor the seat's command says.
func ClassifyEngine(f SeatFacts) string {
	for _, r := range engineRules {
		if r.match(f) {
			return r.kind
		}
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
