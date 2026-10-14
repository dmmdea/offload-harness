package backendchain

// SeatFacts is what is known about one seat when its engine is classified.
type SeatFacts struct {
	// DeclaredVLLM: the box (vllm_seats) or the tier table says a vLLM engine serves the seat. A vLLM
	// entry runs through a wrapper script, so its command cannot name the engine.
	DeclaredVLLM bool
	// Cmd is the seat's command line from the serving config, macros expanded; empty when unreadable.
	Cmd string
}

// ClassifyEngine names the engine kind that serves a seat, or EngineUnknown.
func ClassifyEngine(f SeatFacts) string { return "" }
