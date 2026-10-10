package mediaremote

// A graph on the fleet (P0 plan, S4; ADR 0082 amendment).
//
// IDENTITY. An image is identified by its recipe; a graph is the caller's, so its identity is only that the node holds
// every file the graph names, and the NODE decides that at its door (internal/fleetnode graph_files.go answers 503 naming
// what it lacks, which the placer treats like any refusal at the door: the call goes on to the next node). The delegator
// therefore sends no family and no digest, and asks of a candidate only what it can read from health: that it serves
// run-graph with the route configured, runs this release (the file check ships with it), holds no lease, and the two rules
// below.
//
// ONE CARD. A graph the operator did not pin to cards holds the whole node and runs in the default ComfyUI instance, which
// sees every card. On a node with several cards that instance can land on the display card, and the delegator cannot tell
// which card it will pick. So a whole-node graph is placed only on a node that publishes exactly ONE card; a node that
// publishes no per-card list is not shown to have one and is a named miss, never a pass. This is a placement rule for the
// delegator, not a fix of the node: the same graph sent to such a node by hand, or run on its own machine, is the parked
// local item (docs/systems/media-generation.md).
//
// INPUT FILES. The files a graph's input loaders read (LoadImage, LoadVideo, LoadAudio) are the caller's own and carry
// generic names (image.png, ComfyUI_00001_.png [output]) that two machines share with different bytes. A node can match one
// only by name, so a node holding a same-named file would run the graph on the wrong bytes and answer ok with a verified
// file. A graph that reads one is therefore never auto-overflowed: the call waits for this machine's own lane, and the answer
// says the fleet was not asked and why (overflowOutcome.note). Carrying a size and a hash of each file to the node, which
// would restore the overflow for such graphs, is a named seam, not built. A call that must leave this machine anyway (route
// remote, or auto on a machine with no graph lane of its own, which has no queue to wait in) keeps the node's check by name:
// refusing it would turn a placement into a refusal, and the files it names live wherever the caller put them. The
// same-named-file risk stays there, a named limit.
//
// MIN_VRAM_GB. A call can ask for a node whose largest card is at least N GB. Without the argument a graph that names the
// family wan22 (model_family) and carries a height of 720 or more gets the default floor of 16 GB, CHOSEN from the film
// owner's stated constraint that Wan 2.2 14B at 720p needs a 16 GB card; it is not a measurement of the harness. The
// caller's number wins over the default, 0 included. It filters REMOTE candidates only: the local lane is the node's own grant.

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// graphNeed is what a run_graph call asks of a node beyond serving the task.
type graphNeed struct {
	// wholeNode: the graph names no devices, so it holds the whole node and is placed only on a node with one card.
	wholeNode bool
	// minVRAM is the least size, in GB, of the largest card of a node that may take the graph; 0 asks for none. minVRAMWhy
	// says whose number it is (the caller's, or the default table's, which it labels as chosen).
	minVRAM    float64
	minVRAMWhy string
}

// defaultMinVRAM is the floors a graph gets without asking: rows of (family, least height in pixels, GB). CHOSEN
// 2026-10-10 from the film owner's stated constraint that Wan 2.2 14B fp8 at 720x1280 needs a 16 GB card; the harness has
// not measured it. The family is the call's model_family, spelled wan22, wan2.2 or wan; the height is the tallest `height`
// input of any node of the graph (the harness reads no other meaning into a graph), and a graph with none gets no floor.
var defaultMinVRAM = []struct {
	family    string
	minHeight int
	gb        float64
}{
	{"wan22", 720, 16},
}

// familyKey folds the spellings of a model family the table keys on.
func familyKey(f string) string {
	switch k := strings.ToLower(strings.TrimSpace(f)); k {
	case "wan22", "wan2.2", "wan":
		return "wan22"
	default:
		return k
	}
}

// minVRAMArg reads the caller's min_vram_gb: a number of GB, 0 or more, as a number or a numeral in a string (the CLI's).
// set is false when the call gave none (absent or blank).
func minVRAMArg(p map[string]any) (gb float64, set bool, err error) {
	raw, present := p["min_vram_gb"]
	if !present || raw == nil {
		return 0, false, nil
	}
	if s, ok := raw.(string); ok && strings.TrimSpace(s) == "" {
		return 0, false, nil
	}
	v, ok := num(p, "min_vram_gb")
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, true, &contractError{fmt.Sprintf("min_vram_gb must be a number of GB, 0 or more (the least size of the largest card of the node that takes the call); got %v", raw)}
	}
	return v, true, nil
}

// graphHeight is the tallest `height` input of any node of the graph, 0 when it has none or cannot be read.
func graphHeight(graph []byte) int {
	var nodes map[string]json.RawMessage
	if json.Unmarshal(graph, &nodes) != nil {
		return 0
	}
	tallest := 0.0
	for _, raw := range nodes {
		var n struct {
			Inputs map[string]json.RawMessage `json:"inputs"`
		}
		if json.Unmarshal(raw, &n) != nil {
			continue
		}
		var h float64
		if json.Unmarshal(n.Inputs["height"], &h) == nil && h > tallest {
			tallest = h
		}
	}
	return int(tallest)
}

// inputFilesOf names the input files the graph of a planned run_graph reads, the way the loader names them ("ref.png",
// "prev/step.png [output]"); nil when it reads none or the graph cannot be read.
func inputFilesOf(pl planned) []string {
	raw, ok := pl.payload["graph"].(json.RawMessage)
	if !ok {
		return nil
	}
	files, err := mediacap.ScanGraph(raw)
	if err != nil {
		return nil
	}
	var names []string
	for _, in := range files.Inputs {
		if in.Dir != "input" {
			names = append(names, in.Name+" ["+in.Dir+"]")
		} else {
			names = append(names, in.Name)
		}
	}
	return names
}

// inputFilesNote is the sentence that says why a graph that reads input files was not offered to the fleet.
func inputFilesNote(names []string) string {
	shown := names
	if len(shown) > 3 {
		shown = append(append([]string(nil), names[:3]...), fmt.Sprintf("and %d more", len(names)-3))
	}
	return fmt.Sprintf("this machine did not offer the graph to the fleet: it reads input files (%s), and a node can match an input file only by name, so a node's same-named file of other bytes would run in its place; the call waits for this machine's own lane (route remote sends it to a node you name)", strings.Join(shown, ", "))
}

// graphNeedOf is the graph-specific part of a call's need; nil for any other task.
func graphNeedOf(req core.Request) *graphNeed {
	if req.Task != core.TaskRunGraph {
		return nil
	}
	g := &graphNeed{wholeNode: !hasDevices(req.Params)}
	if gb, set, err := minVRAMArg(req.Params); err != nil {
		return g // plan() refuses the call by name; nothing is placed
	} else if set {
		if gb > 0 {
			g.minVRAM, g.minVRAMWhy = gb, fmt.Sprintf("min_vram_gb %g, given by the caller", gb)
		}
		return g
	}
	fam := familyKey(str(req.Params, "model_family"))
	if fam == "" {
		return g
	}
	graph, err := readJSONObject(str(req.Params, "graph_path"), "graph")
	if err != nil || graph == nil {
		return g
	}
	height := graphHeight(graph)
	for _, row := range defaultMinVRAM {
		if fam == row.family && height >= row.minHeight {
			g.minVRAM = row.gb
			g.minVRAMWhy = fmt.Sprintf("the default for %s at height %d or more, a floor chosen from the film owner's stated constraint and not measured; pass min_vram_gb to change it, 0 to lift it", row.family, row.minHeight)
		}
	}
	return g
}

// refuses says why node v cannot take this call by the two rules above, "" when it can. It reads only what health
// publishes: gpu_devices[] (nil is unknown, and unknown is never enough).
func (n need) refuses(v delegate.NodeView) string {
	g := n.graph
	if g == nil {
		return ""
	}
	if g.wholeNode {
		switch len(v.Devices) {
		case 1:
		case 0:
			return "publishes no per-card list, so it is not shown to have a single card, and a whole-node graph runs in the default ComfyUI instance, which sees every card (the display card of a multi-GPU box included); it is placed only on a node with one card"
		default:
			return fmt.Sprintf("has %d cards, and a whole-node graph runs in the default ComfyUI instance, which sees every card (the display card of a multi-GPU box included); it is placed only on a node with one card", len(v.Devices))
		}
	}
	if g.minVRAM > 0 {
		if len(v.Devices) == 0 {
			return fmt.Sprintf("publishes no per-card list, so its VRAM cannot be checked against %s", g.minVRAMWhy)
		}
		largest := 0.0
		for _, d := range v.Devices {
			if d.TotalGiB > largest {
				largest = d.TotalGiB
			}
		}
		// A card is the size it rounds to: nvidia-smi reports MiB/1024, and a 16 GB card reads 15.92 or 15.93 GiB in this
		// fleet's own docs, so a literal comparison with 16 would shut out every 16 GB card.
		if math.Round(largest) < g.minVRAM {
			return fmt.Sprintf("its largest card is %.2f GiB (a %g GB card) and this call needs a card of at least %g GB (%s)", largest, math.Round(largest), g.minVRAM, g.minVRAMWhy)
		}
	}
	return ""
}

// matchFor is the verdict of node v for this call: the recipe match for an image, the graph rules for a graph.
func matchFor(local localID, v delegate.NodeView, n need) (nodeMatch, bool) {
	if n.graph != nil {
		return matchGraph(v, n)
	}
	return matchNode(local, v, n)
}

// matchGraph compares what node v publishes with what a graph asks. There is no recipe: the node holds the graph's files or
// refuses at its door.
func matchGraph(v delegate.NodeView, n need) (nodeMatch, bool) {
	miss := func(why string) (nodeMatch, bool) { return nodeMatch{why: why, state: "not-capable"}, false }
	if !contains(v.Tasks, taskRunGraph) {
		return miss("does not serve " + taskRunGraph)
	}
	if v.HarnessVersion != localVersion {
		return miss(fmt.Sprintf("runs harness %s and this machine %s: a graph's file check ships with the release, and a node on another one is not shown to make it", shownVersion(v.HarnessVersion), localVersion))
	}
	if st, known := v.RouteState("run_graph"); known && st != delegate.MediaRouteConfigured {
		return miss(fmt.Sprintf("its route run_graph is %s on that node", st))
	}
	if why := n.refuses(v); why != "" {
		return miss(why)
	}
	return nodeMatch{}, true
}
