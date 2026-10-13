package mcpserver

// The agent_run door and the fleet (register D-125, and the C-46 schema defect).
//
// Two defects shared one door.
//
//  1. agent_run(model: X) validated X against THIS box's llama-swap roster and refused when it was absent, however many
//     fleet nodes ran it: the door's roster message was true of this box and silent about the fleet, and the one thing it
//     could not do was run a seat from another node from a box that does not serve it. The fleet publishes who runs what
//     (health agent_seat, served_models), and the delegator already reads it for every placement, so the door asks it:
//     a name this box does not serve becomes ONE contract, handed to the delegator at route=remote with its remotes
//     narrowed to the nodes whose agent seat is that name.
//
//  2. A routed agent_run (route: remote, auto, spread, queue) built its contract with no output_schema. The delegator
//     refuses a schema-less contract on every node (`noschema`, delegate/gate.go: remote output merges only after a
//     mechanical check) and a node's own ack refuses it too, so a routed agent_run could never be placed on a node. Every contract
//     this door sends to the fleet now carries the flat answer schema below.
//
// What this does not do. It changes no wire field and no node: the contract names no seat, so the node runs ITS agent
// seat, and the door can honour a name only when it IS a node's agent seat (or the agent-role seat of one of its layers,
// which the contract names through its existing layer field). A name a node merely serves, as an alias or a non-agent
// seat, is refused naming the seat that node does run; nothing is guessed. A node that changes its agent seat between the
// probe and the dispatch is caught by the seat check on the result: an answer from another seat is withheld, and so is the
// finished prose a deferral carries (schema_miss) when another seat wrote it. A contract that names a seat is also a reasoned
// remote pin (delegate.PinOperator, contractOnFleetAt): a second chance for it is another node that runs the name or none,
// never this box's own agent seat, which is a different model. Running a
// non-agent seat on a node needs the contract to name the seat, which is a wire field and a node-side change that widens
// what a fleet-token holder can make a node load; it is not here.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

// agentRunAnswerSchema is the output schema every agent_run contract carries to the fleet. A remote run requires one
// (the delegator must hold a mechanical check before it merges a node's output, and a node's own ack refuses a contract
// without it), and a goal that came through agent_run has no shape of its own: the answer is prose. The node's loop still
// answers in prose and that prose is what the response publishes as `output`; the schema is the check that the node
// produced a finished answer at all. Flat and string-only, like offload_ask's: one field is the whole promise.
const agentRunAnswerSchema = `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`

// agentRunFleetCall is what the agent_run door hands its fleet path: the call's own arguments.
type agentRunFleetCall struct {
	Goal         string
	Model        string
	ReadRoot     string
	MaxSteps     int
	TimeoutSec   int
	Profile      string
	Thinking     string
	ContextClass string
	SetupActions []core.AgentSetupAction
	Judge        bool
	AllowBrowse  bool
}

// leftBehind says which of the call's arguments did not travel to the node, so a caller who passed them is told instead
// of left to think the goal saw them.
func (c agentRunFleetCall) leftBehind() []string {
	var notes []string
	if strings.TrimSpace(c.ReadRoot) != "" {
		notes = append(notes, "read_root did not travel: it names a directory on this box, and the node runs the goal against its own root (self-contained goals and setup_actions only)")
	}
	if c.Judge {
		notes = append(notes, "judge was not run: the end-of-run audit is a same-seat completion on this box's door")
	}
	return notes
}

// agentRunFleetContract is the single contract an agent_run call becomes at the fleet. It goes through the same intake as
// every delegation contract (delegate.PrepareContractWithCap: the surface is the origin, the step and wall ceilings
// clamp, the wall is sized by the executing node unless the caller named one, the whole contract validates), so a
// malformed field dies here naming it, never at a node's ack. layer is the layer a composite node must run it on ("" for
// a node that runs the name as its agent seat).
func agentRunFleetContract(cfg config.Config, c agentRunFleetCall, layer string) (core.AgentContract, error) {
	return delegate.PrepareContractWithCap(delegate.SubtaskSpec{AgentContract: core.AgentContract{
		Goal: c.Goal, MaxSteps: c.MaxSteps, TimeoutSec: c.TimeoutSec, Profile: c.Profile, Thinking: c.Thinking,
		SetupActions: c.SetupActions, ContextClass: c.ContextClass, Layer: layer,
		OutputSchema: json.RawMessage(agentRunAnswerSchema), Door: "agent_run",
	}}, "", cfg.AgentContextCapBytes())
}

// fleetSeat is what the fleet's own views say about who runs a named seat.
type fleetSeat struct {
	// Bases are the nodes whose agent seat is the name (or whose layer's agent seat is), in roster order, spelled as the
	// roster spells them: the remotes the engine is handed.
	Bases []string
	// Layer is the layer the contract must name, "" when the nodes run the name as their own agent seat. A composite node
	// that serves the name from a layer other than its default is reached by naming the layer.
	Layer string
	// Looked is what the fleet showed, one clause per roster member, for the refusal of a name nobody runs.
	Looked string
	// Class is the defer class of that refusal: config when every node answered (the name is nobody's), infrastructure
	// when one did not (it might have been).
	Class string
	// Asked is false when this box lists no node to ask, or lists only blank entries.
	Asked bool
}

// fleetSeatFor reads the fleet and finds the nodes that run model as their agent seat. One roster read, concurrent and
// bounded (rosterprobe.Probe, the reading every single-shot lane shares), so a name nobody runs costs the call one probe
// bound at worst and a node that is down is named, not waited on.
func fleetSeatFor(ctx context.Context, cfg config.Config, model string) fleetSeat {
	model = strings.TrimSpace(model)
	if model == "" || len(cfg.DelegateRemotes) == 0 {
		return fleetSeat{}
	}
	readings := rosterprobe.Probe(ctx, cfg.DelegateRemotes, cfg.FleetAuthToken, rosterprobe.DefaultTimeout)
	if len(readings) == 0 {
		return fleetSeat{}
	}
	fs := fleetSeat{Asked: true, Class: core.DeferClassConfig}
	type hit struct{ base, layer string }
	var plain, layered []hit
	var clauses []string
	for _, r := range readings {
		if r.Err != nil {
			// Not answering (or never dialled: the tailnet guard refused the entry): the node might have run the name.
			clauses = append(clauses, r.Miss())
			if !rosterprobe.IsRefusal(r.Err) {
				fs.Class = core.DeferClassInfrastructure
			}
			continue
		}
		v := r.View
		name := v.NodeID
		if strings.TrimSpace(name) == "" {
			name = r.Shown()
		}
		agentSeat := strings.TrimSpace(v.AgentSeat)
		switch {
		case !v.AgentEnabled:
			clauses = append(clauses, fmt.Sprintf("%s (the agent lane is not advertised)", name))
		case agentSeat != "" && strings.EqualFold(agentSeat, model):
			plain = append(plain, hit{base: r.Base})
		default:
			if layer, ok := layerRunningAsAgent(v.Layers, model); ok {
				layered = append(layered, hit{base: r.Base, layer: layer})
				continue
			}
			clause := fmt.Sprintf("%s (agent seat %q", name, agentSeat)
			if servesName(v.ServedModels, model) {
				clause += fmt.Sprintf("; it serves %q but not as its agent seat, and a contract runs the agent seat", model)
			}
			clauses = append(clauses, clause+")")
		}
	}
	switch {
	case len(plain) > 0:
		for _, h := range plain {
			fs.Bases = append(fs.Bases, h.base)
		}
	case len(layered) > 0:
		// One contract names one layer: take the first node's and keep the nodes that run the name from the same one.
		fs.Layer = layered[0].layer
		for _, h := range layered {
			if h.layer == fs.Layer {
				fs.Bases = append(fs.Bases, h.base)
			}
		}
	}
	fs.Looked = "looked at " + strings.Join(clauses, "; ")
	return fs
}

// layerRunningAsAgent finds the layer of a composite node whose agent-role seat is model, matched by the seat's model id
// only (a router seat's model_map twins are not guessed at).
func layerRunningAsAgent(rows []placement.LayerRow, model string) (string, bool) {
	for _, row := range rows {
		for _, seat := range row.Seats {
			if seat.Role == placement.RoleAgent && strings.EqualFold(strings.TrimSpace(seat.Model), model) {
				return row.Name, true
			}
		}
	}
	return "", false
}

// ranSeat reports whether the seat a result says it came from is the model the caller named (without regard to case).
func ranSeat(seat, model string) bool {
	return strings.EqualFold(strings.TrimSpace(seat), strings.TrimSpace(model))
}

// seatLabel words the seat a result reports for a reason: its name, or that it named none.
func seatLabel(seat string) string {
	if seat = strings.TrimSpace(seat); seat != "" {
		return fmt.Sprintf("seat %q", seat)
	}
	return "a seat that reported no name"
}

// carriesFinishedAnswer reports a deferral that withFinishedAnswer would publish the prose of: the loop finished and only
// its structuring failed.
func carriesFinishedAnswer(w core.AgentWireResult) bool {
	return w.SchemaMiss && strings.TrimSpace(w.Output) != ""
}

func servesName(served []string, model string) bool {
	for _, m := range served {
		if strings.EqualFold(strings.TrimSpace(m), model) {
			return true
		}
	}
	return false
}

// explicitModelRefusal is the door's refusal of a named model this box's roster does not serve: the established sentence,
// which every caller already reads, unchanged.
func explicitModelRefusal(model string) string {
	return fmt.Sprintf("explicitly requested model %q is not in the endpoint's served roster — pick a served model (offload_status lists them)", model)
}

// namedSeatOffBox is the fork the agent_run door takes when the caller named a model this box's roster does not serve
// (rosterReason is the door's own refusal sentence for it). Exactly one of the three results is meaningful: a handled
// result (the run went to the fleet, or the fleet path refused with its own reason), or a refusal for the door to return
// through its sizing stamp (the roster sentence, with where the fleet looked when it was asked), or an error.
//
// route is the call's own route as the caller spelled it. An explicit local route never looks at the fleet: it asked for
// this box, whatever the fleet serves.
func (s *Server) namedSeatOffBox(ctx context.Context, cfg config.Config, call agentRunFleetCall, route, rosterReason string) (*mcp.CallToolResult, map[string]any, error) {
	plain := map[string]any{"deferred": true, "reason": rosterReason}
	if route == "local" || strings.TrimSpace(call.Model) == "" {
		return nil, plain, nil
	}
	if call.AllowBrowse {
		// The browse tool drives THIS machine's own browser, and a seat this box does not serve can only run on a fleet
		// node, where the grant cannot travel.
		return nil, map[string]any{"deferred": true, "defer_class": core.DeferClassConfig,
			"reason": rosterReason + "; and allow_browse drives this machine's own browser, so the run could only go to a fleet node, where the grant cannot travel — drop allow_browse, or name a seat this box serves"}, nil
	}
	fs := fleetSeatFor(ctx, cfg, call.Model)
	if len(fs.Bases) == 0 {
		if fs.Asked {
			plain["reason"] = rosterReason + "; the fleet was asked and no node runs it as its agent seat: " + fs.Looked
			plain["defer_class"] = fs.Class
		}
		return nil, plain, nil
	}
	res, err := s.runOnFleet(ctx, cfg, call, "remote", fs)
	return res, nil, err
}

// agentRunOnRoute is the agent_run door at a route other than local (register C-46): the goal becomes ONE contract
// through the delegator. read_root and judge do not travel (the executing node reads its own root), and a model rides
// only route=remote, where it selects the nodes whose agent seat has that name; auto, spread and queue place by capacity,
// not by seat name, so a model there would be silently ignored, and is refused instead.
func (s *Server) agentRunOnRoute(ctx context.Context, cfg config.Config, call agentRunFleetCall, route string) (*mcp.CallToolResult, error) {
	model := strings.TrimSpace(call.Model)
	if model == "" {
		return s.runOnFleet(ctx, cfg, call, route, fleetSeat{})
	}
	if route != "remote" {
		return jsonResult(map[string]any{"deferred": true, "defer_class": core.DeferClassContract, "route": route, "steps": 0,
			"reason": fmt.Sprintf("model %q cannot ride route=%s: the delegator places by capacity, not by seat name, so the run could land on any node's own agent seat and the name would be ignored — use route=remote (the run goes to a node whose agent seat is that model), or drop route to run on a seat this box serves", model, route)})
	}
	fs := fleetSeatFor(ctx, cfg, model)
	if len(fs.Bases) == 0 {
		reason, class := fmt.Sprintf("no fleet node runs model %q as its agent seat", model), core.DeferClassConfig
		if fs.Asked {
			reason, class = reason+": "+fs.Looked, fs.Class
		} else {
			reason += ": this box lists no fleet node (delegate_remotes is empty)"
		}
		return jsonResult(map[string]any{"deferred": true, "defer_class": class, "reason": reason, "route": route, "steps": 0})
	}
	return s.runOnFleet(ctx, cfg, call, route, fs)
}

// runOnFleet sends the call's one contract to the delegator at route and words the outcome. fs narrows the nodes asked
// (the engine's remotes list REPLACES the configured roster, and is checked against it) and names the layer; the zero
// fleetSeat leaves the roster as configured and the seat unnamed.
func (s *Server) runOnFleet(ctx context.Context, cfg config.Config, call agentRunFleetCall, route string, fs fleetSeat) (*mcp.CallToolResult, error) {
	model := strings.TrimSpace(call.Model)
	contract, perr := agentRunFleetContract(cfg, call, fs.Layer)
	if perr != nil {
		return jsonResult(map[string]any{"deferred": true, "defer_class": core.DeferClassContract, "route": route, "steps": 0,
			"reason": "the call cannot ride a contract: " + perr.Error()})
	}
	wire, extra, note, ok := s.contractOnFleetAt(ctx, contract, route, "", fs.Bases)
	notes := call.leftBehind()
	if !ok {
		// Nothing ran (the fleet could not be asked, or no node took it), or a node ran it and deferred: either way its own
		// reason and class, and its finished answer when only the structuring failed (the loop's prose is never thrown away).
		out := map[string]any{"deferred": true, "reason": note, "route": route, "steps": wire.Steps}
		if wire.DeferClass != "" {
			out["defer_class"] = wire.DeferClass
		}
		if len(notes) > 0 {
			out["notes"] = notes
		}
		if model != "" && carriesFinishedAnswer(wire) && !ranSeat(wire.Seat, model) {
			// The prose a deferral carries is an answer like any other: from a seat other than the name it is not published
			// (the check below is for a result that came back done). The deferral stays what the node filed, and says what was held.
			out["reason"] = fmt.Sprintf("%s; the finished answer it carries came from %s, not the model named (%q), so it is withheld", note, seatLabel(wire.Seat), model)
			return jsonResult(withReviewExtra(out, extra))
		}
		return jsonResult(withReviewExtra(withFinishedAnswer(out, wire), extra))
	}
	if model != "" && !ranSeat(wire.Seat, model) {
		// The delegator placed the contract on a node whose agent seat WAS the name when it probed, and the node says it ran
		// another (an operator edit in between): the answer is not from the model the caller named, so it is not published.
		// The node is named as the placement named it (extra["node"], results[].node), not by the id on its own result.
		node := wire.NodeID
		if placed, _ := extra["node"].(string); placed != "" {
			node = placed
		}
		return jsonResult(withReviewExtra(map[string]any{"deferred": true, "defer_class": core.DeferClassConfig, "route": route, "steps": wire.Steps,
			"reason": fmt.Sprintf("node %s answered from seat %q, not the model named (%q): its agent seat changed after the fleet was read, so the answer is withheld", node, wire.Seat, model)}, extra))
	}
	out := map[string]any{"deferred": false, "output": wire.Output, "steps": wire.Steps, "seat": wire.Seat, "stop_reason": wire.StopReason, "route": route}
	if model != "" {
		out["model"] = wire.Seat // the seat that ran it: visibility is the cure for a silent seat, as on the local door
	}
	if len(wire.Structured) > 0 {
		out["structured"] = json.RawMessage(wire.Structured)
	}
	if wire.Placed != nil {
		out["placed"] = wire.Placed
	}
	if len(notes) > 0 {
		out["notes"] = notes
	}
	return jsonResult(withReviewExtra(out, extra))
}
