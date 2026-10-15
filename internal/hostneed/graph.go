package hostneed

// STUB: red first.

// maxGraphBytes bounds the graph document ForGraphFile will read.
const maxGraphBytes = 8 << 20

// ForGraph sizes a ComfyUI API-format graph by the weight files its own loaders name.
func ForGraph(graph []byte, f Facts) (Need, bool) { return Need{}, false }

// ForGraphFile reads the graph at path and sizes it with ForGraph.
func ForGraphFile(path string, f Facts) (Need, bool) { return Need{}, false }
