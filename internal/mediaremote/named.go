package mediaremote

// A call that names a node (ADR 0082 Amendment 3).
//
// `node` on a video, animation or audio call is a fleet node id (the node_id its /fleet/health publishes, in any case) or one of
// the delegate_remotes URLs. Naming a node is the caller's explicit act, as `route remote` is, and stronger: the named node is
// the ONLY candidate. No other node is tried, this machine's own lane is not asked, and a call that finds the node unable to take
// it is refused, by a word, before anything is sent: no POST, so no job on the node.
//
// The refusals come in two classes. A name that is wrong (node_unknown, node_is_self) or contradicts the call (route local) is the
// caller's mistake: a contract refusal. A reachable node that cannot take the call (node_lacks_route, node_lacks_media_job,
// node_lacks_model, node_below_min_vram) is a configuration fact that waiting does not change: it is filed like the misses
// pickNodeFor reports for a roster left out for configuration, as a capacity defer with err_class no_node_configured, narrowed to
// the one node the caller chose. A node that does not answer, or holds a text lease, is an ordinary capacity defer with no err
// class: it may take the call later. A roster entry the tailnet guard refuses is never dialled and has no id: an id nobody has is
// node_unknown however many such entries the roster holds (the reason names them), and a URL named that the guard refuses is filed
// like the configuration refusals (capacity, no_node_configured) without a refusal word. A machine with no delegate_remotes never
// reaches a roster: callWith answers the plain config defer of every remote call there.
//
// What a node publishes in media_recipes (the family rows of its video, animation and audio bindings) is read for one thing: the
// weight files a row says it lacks. A lane the node publishes no row for is UNKNOWN, never refused (an older node, an sd.cpp
// animation lane, voice), and the call goes to the node's own door as a forced call does. Rows are not read for a call that names
// no node.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

// The words of a refusal: data.refusal of a deferred answer that names a node.
const (
	refusalNodeUnknown     = "node_unknown"
	refusalNodeIsSelf      = "node_is_self"
	refusalNodeLacksRoute  = "node_lacks_route"
	refusalNodeLacksMedia  = "node_lacks_media_job"
	refusalNodeLacksModel  = "node_lacks_model"
	refusalNodeBelowMinVRM = "node_below_min_vram"
)

// shownNode is a name as it is printed and filed: an id as typed, a URL redacted the way every roster entry is (no credential an
// entry was pasted with reaches a message, a ledger row or an answer).
func shownNode(name string) string {
	if strings.Contains(name, "://") {
		return rosterprobe.Members([]string{name})[0].Shown()
	}
	return name
}

// namedTask reports whether a call of this task may name a node: video, animation and audio.
func namedTask(t core.TaskType) bool {
	return t == core.TaskGenerateVideo || t == core.TaskAnimateCharacter || t == core.TaskGenerateAudio
}

// namedNodeOf is the node a call names: req.Params["node"] trimmed, "" when it names none (absent or blank). A node that is not a
// string, or that rides a call that cannot name one (an image, a graph), is refused by name instead of being dropped.
func namedNodeOf(req core.Request) (string, error) {
	raw, present := req.Params["node"]
	if !present || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", &contractError{"node must be a string: a fleet node id (the node_id its /fleet/health publishes) or one of the delegate_remotes URLs"}
	}
	name := strings.TrimSpace(s)
	if name != "" && !namedTask(req.Task) {
		return "", &contractError{fmt.Sprintf("node names a fleet node for a video, animation or audio call; a %s call is placed with route and remotes", laneName(req))}
	}
	return name, nil
}

// namedRefusal is a named call that was not sent, with the word and the facts behind it. It is the typed data of the deferred
// answer: {refusal, node, lane, family, missing[], roster[]}.
type namedRefusal struct {
	// class is the answer's defer class; noNode files err_class no_node_configured beside it.
	class  string
	noNode bool
	msg    string

	Refusal string   `json:"refusal"`
	Node    string   `json:"node"`
	Lane    string   `json:"lane"`
	Family  string   `json:"family,omitempty"`
	Missing []string `json:"missing,omitempty"`
	Roster  []string `json:"roster,omitempty"`
}

func (e *namedRefusal) Error() string { return e.msg }

func (e *namedRefusal) data() json.RawMessage {
	b, err := json.Marshal(e)
	if err != nil {
		return nil
	}
	return b
}

// namedPick is what a named call learned about the family the node will render, from the row it published: the answer says so.
type namedPick struct {
	family string
	files  []string
}

// annotate adds rendered_family and rendered_files to the data of a finished call, beside the node's own answer.
func (p *namedPick) annotate(res core.Result) core.Result {
	if p == nil || !res.OK || len(res.Data) == 0 {
		return res
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(res.Data, &m) != nil || m == nil {
		return res
	}
	fam, err1 := json.Marshal(p.family)
	files, err2 := json.Marshal(p.files)
	if err1 != nil || err2 != nil {
		return res
	}
	m["rendered_family"], m["rendered_files"] = fam, files
	if b, err := json.Marshal(m); err == nil {
		res.Data = b
	}
	return res
}

// pickNamed finds the node a call names among the bases it may use, judges it, and returns where to send the job. It never
// returns another node: a node that cannot take the call is a refusal or a capacity defer, and the caller decides what to do.
func pickNamed(ctx context.Context, cfg config.Config, bases []string, name string, req core.Request, pl planned, doorTask string) (base, node string, pick *namedPick, err error) {
	lane := pl.fleetTask
	typed := name // what the caller typed; what is printed and filed is shownNode(typed)
	name = shownNode(typed)
	refuse := func(class string, noNode bool, word, msg, family string, missing []string) *namedRefusal {
		return &namedRefusal{class: class, noNode: noNode, msg: msg, Refusal: word, Node: name, Lane: lane, Family: family, Missing: missing}
	}
	byURL := strings.Contains(typed, "://")
	var (
		readings = rosterprobe.Probe(ctx, bases, cfg.FleetAuthToken, healthTimeout)
		match    = -1
		roster   []string
		silent   []string // the entries that were dialled and gave no health: one of them may be the node the caller means
		refused  []string // the entries the tailnet guard will not admit: never dialled, so none has an id to be meant by
	)
	for i, r := range readings {
		id := r.Shown()
		if r.Err == nil && strings.TrimSpace(r.View.NodeID) != "" {
			id = r.View.NodeID
		}
		hit := (byURL && strings.EqualFold(normBase(typed), r.Base)) || (!byURL && r.Err == nil && strings.EqualFold(strings.TrimSpace(r.View.NodeID), typed))
		if hit && match < 0 {
			match = i
		}
		if r.Err != nil {
			if r.Refused != nil {
				refused = append(refused, r.Miss())
			} else {
				silent = append(silent, r.Miss())
			}
			continue
		}
		roster = append(roster, id)
	}
	if match < 0 {
		if !byURL && len(silent) > 0 {
			// No node that answered has this id, but some entries did not answer: it may be one of them.
			return "", "", nil, &placementError{core.DeferClassCapacity, fmt.Sprintf(
				"no node that answered is called %q; these entries did not answer and one of them may be it: %s", name, strings.Join(silent, "; "))}
		}
		why := fmt.Sprintf("node %q is not on this call's roster (%s): name a node_id its /fleet/health publishes or one of the delegate_remotes URLs", name, strings.Join(roster, ", "))
		if len(refused) > 0 {
			why += "; these entries were not dialled and so have no node id to name: " + strings.Join(refused, "; ")
		}
		e := refuse(core.DeferClassContract, false, refusalNodeUnknown, why, "", nil)
		e.Roster = roster
		return "", "", nil, e
	}
	r := readings[match]
	if r.Err != nil {
		if r.Refused != nil {
			// The tailnet guard will not admit the entry, whatever the node is doing: a configuration fact that waiting does not
			// change, filed the way pickNodeFor files such an entry.
			return "", "", nil, &noNodeConfigured{&placementError{core.DeferClassCapacity, "the node you named is a roster entry this machine does not dial: " + r.Miss()}}
		}
		return "", "", nil, &placementError{core.DeferClassCapacity, "the node you named did not answer: " + r.Miss()}
	}
	v := r.View
	id := strings.TrimSpace(v.NodeID)
	if id == "" {
		id = r.Shown() // the node id reaches results and messages, so the redacted form
	}
	who := fmt.Sprintf("%s (%s)", r.Shown(), id)
	if self := selfNodeID(cfg); self != "" && strings.EqualFold(strings.TrimSpace(v.NodeID), self) {
		return "", "", nil, refuse(core.DeferClassContract, false, refusalNodeIsSelf, fmt.Sprintf(
			"node %q is this machine's own node (%s): a call it would run itself is a local call (route local)", name, who), "", nil)
	}

	wanted, family, judged := wantedRow(req, lane, rowsOf(v, lane))
	var missing []string
	if wanted != nil {
		missing = missingFiles(*wanted)
	}
	routes := nodeRoutes(req)
	if !contains(v.Tasks, pl.fleetTask) || (len(routes) > 0 && v.MediaRoutesKnown && !anyConfigured(v, routes)) {
		why := "does not serve " + pl.fleetTask
		if len(routes) > 0 && v.MediaRoutesKnown && !anyConfigured(v, routes) {
			why = fmt.Sprintf("route %s is %s on that node", strings.Join(routes, " or "), stateOf(v, routes))
		}
		if len(missing) > 0 {
			why += fmt.Sprintf("; its %s row lacks %s", wanted.Name, strings.Join(missing, ", "))
		}
		return "", "", nil, refuse(core.DeferClassCapacity, true, refusalNodeLacksRoute, fmt.Sprintf("%s: %s", who, why), nameOf(wanted, family), missing)
	}
	if doorTask == taskMediaJob && !contains(v.Tasks, taskMediaJob) {
		return "", "", nil, refuse(core.DeferClassCapacity, true, refusalNodeLacksMedia, fmt.Sprintf(
			"%s: does not advertise media-job, and this call carries input files (a node opens that door with fleet_media_inputs and a fleet_auth_token)", who), "", nil)
	}
	if judged && (wanted == nil || len(missing) > 0) {
		why := fmt.Sprintf("publishes no %s row", family)
		if wanted != nil {
			why = fmt.Sprintf("its %s row lacks %s", wanted.Name, strings.Join(missing, ", "))
		}
		return "", "", nil, refuse(core.DeferClassCapacity, true, refusalNodeLacksModel, fmt.Sprintf("%s: %s", who, why), family, missing)
	}
	if why := needOf(req).refuses(v); why != "" {
		return "", "", nil, refuse(core.DeferClassCapacity, true, refusalNodeBelowMinVRM, fmt.Sprintf("%s: %s", who, why), "", nil)
	}
	if v.LeasedText {
		return "", "", nil, &placementError{core.DeferClassCapacity, fmt.Sprintf("%s: a text lease holds its card; it may take the call later", who)}
	}
	if judged && wanted != nil {
		pick = &namedPick{family: wanted.Name, files: fileNames(*wanted)}
	}
	return r.Base, id, pick, nil
}

func nameOf(row *delegate.MediaRecipeView, fallback string) string {
	if row != nil {
		return row.Name
	}
	return fallback
}

// rowsOf are the rows node v published for lane.
func rowsOf(v delegate.NodeView, lane string) []delegate.MediaRecipeView {
	var out []delegate.MediaRecipeView
	for _, r := range v.MediaRecipes {
		if r.Lane == lane {
			out = append(out, r)
		}
	}
	return out
}

// wantedRow says which row of lane the call would render on a node that published rows, and whether this release can judge it.
// judged is false when the node published no row for the lane or the call names a family no row exists to judge (ace, voice, an
// animation lane the node publishes nothing for): the call goes on. row is nil when the call is judged and the node publishes no row
// for the family it names (family says which).
func wantedRow(req core.Request, lane string, rows []delegate.MediaRecipeView) (row *delegate.MediaRecipeView, family string, judged bool) {
	if len(rows) == 0 {
		return nil, "", false
	}
	find := func(name string) *delegate.MediaRecipeView {
		for i := range rows {
			if rows[i].Name == name {
				return &rows[i]
			}
		}
		return nil
	}
	switch lane {
	case mediacap.LaneVideo:
		model := str(req.Params, "model")
		if model == "" {
			for i := range rows {
				if rows[i].Default {
					return &rows[i], rows[i].Name, true
				}
			}
			return nil, "", false
		}
		// An sd.cpp family is matched by its own name, exactly; every other spelling folds the way the node folds it.
		if r := find(model); r != nil {
			return r, r.Name, true
		}
		fam, _ := mediacap.VideoBinding(config.Config{}, model)
		if fam == "ace" {
			return nil, "", false
		}
		return find(fam), fam, true
	case mediacap.LaneAnimate:
		if r := find(mediacap.AnimateRowName); r != nil {
			return r, r.Name, true
		}
	case mediacap.LaneAudio:
		if strings.EqualFold(str(req.Params, "kind"), "music") {
			if r := find(mediacap.MusicRowName); r != nil {
				return r, r.Name, true
			}
		}
	}
	return nil, "", false
}

func missingFiles(r delegate.MediaRecipeView) []string {
	var out []string
	for _, f := range r.Files {
		if f.Bytes < 0 {
			out = append(out, f.Name)
		}
	}
	return out
}

func fileNames(r delegate.MediaRecipeView) []string {
	out := make([]string, 0, len(r.Files))
	for _, f := range r.Files {
		out = append(out, f.Name)
	}
	return out
}
