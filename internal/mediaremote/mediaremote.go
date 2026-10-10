// Package mediaremote places one image, video, character-animation, audio or ComfyUI-graph job on a fleet
// node and brings the output back, so a machine with no render lane of its own (or one the caller names a
// node for) calls offload_generate_* exactly as a render node does (ADR 0077). It mirrors composeremote:
// probe delegate_remotes through /fleet/health, pick a node that advertises the task and the route, send
// the job with the fleet bearer, poll it, and map every failure to a typed defer.
//
// Two paths carry a job:
//   - a job with no input file goes through POST /fleet/dispatch, the tokenless door every node with the
//     task serves (image-gen and run-graph always; video, animate and audio when no still, reference,
//     driver or clone sample is given);
//   - a job with input files packs them into a bundle (internal/composebundle) and goes through the
//     token-gated POST /fleet/media-job, which only nodes that opened it advertise.
//
// The node writes its outputs into its own media dir. Each one is fetched back by bare name from
// /fleet/media and verified against the sha256 the node published for it (`artifacts`), and the result's
// paths are rewritten to the local copies.
package mediaremote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/composebundle"
	"github.com/dmmdea/offload-harness/internal/composeremote"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

const (
	RouteLocal  = composeremote.RouteLocal
	RouteAuto   = composeremote.RouteAuto
	RouteRemote = composeremote.RouteRemote
)

// healthTimeout is how long one call waits for one node's /fleet/health: the single-shot lanes' shared
// bound, probed concurrently and cached (internal/rosterprobe). A var so a test compresses it; production
// never mutates it.
var healthTimeout = rosterprobe.DefaultTimeout

const (
	dispatchTimeout = 20 * time.Minute // a bundle may be hundreds of MB
	pollTimeout     = 20 * time.Second
	fetchTimeout    = 30 * time.Minute
	maxBody         = 4 << 20
	maxPollFailures = 5
)

// pollEvery is the job poll interval (a var so tests need not wait out real seconds).
var pollEvery = 2 * time.Second

// Budgets bound one remote job end to end when the caller set no deadline: a render's own wall, its queue
// behind the node's media slots, and the transfers.
var Budgets = map[string]time.Duration{
	taskImage:    2 * time.Hour,
	taskVideo:    6 * time.Hour,
	taskAnimate:  6 * time.Hour,
	taskAudio:    1 * time.Hour,
	taskRunGraph: 2 * time.Hour,
}

// HTTPClient is the transport every request uses. It is composeremote's client, which rides
// netguard.SafeTransport: the lane reaches loopback and the operator's tailnet only, checked at dial time
// (never cloud, ADR 0001). Tests swap it.
var HTTPClient = composeremote.HTTPClient

// Runner runs one request in-process; *pipeline.Pipeline satisfies it.
type Runner interface {
	Run(ctx context.Context, req core.Request) core.Result
}

// NormalizeRoute maps the caller's route to one of the three values; "" is auto.
func NormalizeRoute(route string) (string, bool) { return composeremote.NormalizeRoute(route) }

// routesFn derives this machine's media routes: the filesystem walk mediacap does. A var so tests can stand
// in a derivation without building a ComfyUI tree.
var routesFn = mediacap.Routes

// localClock is the route cache's clock; tests move it.
var localClock = time.Now

// localRoutesTTL bounds how stale this machine's lane verdict may be, the freshness a fleet node's
// advertisement has (60 s).
const localRoutesTTL = 60 * time.Second

// localRoutes memoizes the derivation, so a media call (and every default `auto` call on a box that has
// the lane) does not walk the disk each time.
var localRoutes = mediacap.NewCache(localRoutesTTL,
	func() time.Time { return localClock() },
	func(cfg config.Config) []mediacap.Route { return routesFn(cfg) })

// LocalConfigured reports whether THIS machine has the lane req needs, by the same derivation the node's
// advertisement uses (internal/mediacap reads the files, not only the config): the named image family
// resolves, the video, animation or run-graph route is CONFIGURED, music has its script and weights, and
// voice has its script or a speech endpoint. A default config binds every script, so a thin client reads
// as having a lane only when the files behind the binding are really there.
func LocalConfigured(cfg config.Config, req core.Request) bool {
	routes := map[string]bool{}
	for _, r := range localRoutes.Routes(cfg) {
		routes[r.Name] = r.OK()
	}
	switch req.Task {
	case core.TaskGenerateImage:
		if fam := str(req.Params, "family"); fam != "" {
			// A named family has its own derived route (ADR 0058); the default family's name is the
			// default binding.
			if _, _, err := cfg.ResolveImageFamily(fam); err != nil {
				return false
			}
			return routes[mediacap.ImageFamilyRoute(fam)] || (fam == cfg.ImageGenFamily && routes["generate_image"])
		}
		return routes["generate_image"]
	case core.TaskGenerateVideo:
		return routes["generate_video"]
	case core.TaskAnimateCharacter:
		return routes["animate_character"]
	case core.TaskRunGraph:
		return routes["run_graph"]
	case core.TaskGenerateAudio:
		return anyRoute(routes, audioRoutes(req.Params))
	}
	return false
}

func anyRoute(have map[string]bool, names []string) bool {
	for _, n := range names {
		if have[n] {
			return true
		}
	}
	return false
}

// audioRoutes are the mediacap routes that can run an audio request, by its kind and voice.
func audioRoutes(p map[string]any) []string {
	if strings.EqualFold(str(p, "kind"), "music") {
		return []string{"generate_audio:music"}
	}
	switch strings.ToLower(str(p, "voice")) {
	case "endpoint":
		return []string{"generate_audio:voice:endpoint"}
	case "generalist", "finetuned":
		return []string{"generate_audio:voice"}
	}
	return []string{"generate_audio:voice", "generate_audio:voice:endpoint"}
}

// nodeRoutes are the media_routes names that satisfy req on a node: the task is servable there when ANY of
// them is CONFIGURED. nil = the task has no route name to check (image: the node judges family and engine).
func nodeRoutes(req core.Request) []string {
	switch req.Task {
	case core.TaskGenerateVideo:
		return []string{"generate_video"}
	case core.TaskAnimateCharacter:
		return []string{"animate_character"}
	case core.TaskRunGraph:
		return []string{"run_graph"}
	case core.TaskGenerateAudio:
		return audioRoutes(req.Params)
	}
	return nil
}

// Run is the entry point the MCP tools and the CLI verbs share. local runs here; remote runs on a fleet
// node; auto runs here when this box has the lane (byte-identical to before the route existed) and on a
// fleet node when it has none and a node is configured. remotes, when given, replaces delegate_remotes for
// this call and must be a subset of it.
func Run(ctx context.Context, cfg config.Config, runner Runner, req core.Request, route string, remotes []string) core.Result {
	r, ok := NormalizeRoute(route)
	if !ok {
		return contractDefer(fmt.Sprintf("unrecognized route %q; use local, auto or remote", route), core.Meta{})
	}
	if r == RouteLocal {
		return runner.Run(ctx, req)
	}
	if r == RouteAuto {
		if LocalConfigured(cfg, req) || (len(cfg.DelegateRemotes) == 0 && len(remotes) == 0) {
			// A lane here, or no fleet to hand it to: the call is exactly the one that existed before
			// the route did (a box with neither still gets the lane's own deferral and its reason).
			return runner.Run(ctx, req)
		}
	}
	placement := "remote: forced"
	if r == RouteAuto {
		placement = "remote: no " + laneName(req) + " lane on this machine"
	}
	// The call is the remote lane's own from here on: it writes its asker ledger row and, once a node is
	// chosen, its one PAIR card, like composeremote and the other remote lanes (0.165.0, D5-D11). The
	// handle is finished on both exits below; the local branches above never reach it.
	h := core.BeginRemote(runner, req, r)
	defer core.CloseOnPanic(h) // a panic in the dispatch, the poll or the fetch closes the card before the door dies of it
	res, err := callWith(ctx, cfg, req, remotes, h)
	if err != nil {
		res = placementDefer(err, placement)
	} else {
		res.Meta.Placement = placement
	}
	h.Finish(res)
	return res
}

func laneName(req core.Request) string {
	switch req.Task {
	case core.TaskGenerateImage:
		return "image"
	case core.TaskGenerateVideo:
		return "video"
	case core.TaskAnimateCharacter:
		return "animation"
	case core.TaskGenerateAudio:
		return "audio"
	case core.TaskRunGraph:
		return "graph"
	}
	return string(req.Task)
}

type placementError struct {
	class string
	msg   string
}

func (e *placementError) Error() string { return e.msg }

func contractDefer(msg string, meta core.Meta) core.Result {
	res := core.Deferf(msg, "", meta)
	res.DeferClass = core.DeferClassContract
	return res
}

func placementDefer(err error, placement string) core.Result {
	res := core.Deferf(err.Error(), "", core.Meta{Placement: placement})
	var pe *placementError
	var ce *contractError
	switch {
	case errors.As(err, &pe):
		res.DeferClass = pe.class
	case errors.As(err, &ce):
		res.DeferClass = core.DeferClassContract
	default:
		res.DeferClass = core.DeferClassInfrastructure
	}
	return res
}

// Call runs req on the best eligible fleet node and fetches its outputs. A placement, transfer or bundle
// problem is an error the caller maps to a defer class; the node's own verdict (a render that deferred)
// comes back as a deferred core.Result.
func Call(ctx context.Context, cfg config.Config, req core.Request, remotes []string) (core.Result, error) {
	return callWith(ctx, cfg, req, remotes, core.NopAttribution{})
}

// callWith is Call reporting the dispatch and the node's progress to h (core.RemoteAttribution).
func callWith(ctx context.Context, cfg config.Config, req core.Request, remotes []string, h core.RemoteAttribution) (core.Result, error) {
	start := time.Now()
	bases, err := candidateBases(cfg, remotes)
	if err != nil {
		return core.Result{}, err
	}
	pl, err := plan(req)
	if err != nil {
		return core.Result{}, err
	}
	// Everything that can be refused from this machine alone is refused before the network is touched:
	// the input files' total size against the cap, and an out_dir this machine cannot create (found
	// after a render that took hours, it would cost the render).
	if err := checkInputSizes(cfg, pl.inputs); err != nil {
		return core.Result{}, err
	}
	if pl.outDir != "" {
		if err := os.MkdirAll(pl.outDir, 0o755); err != nil {
			return core.Result{}, &contractError{fmt.Sprintf("out_dir %s: %v", pl.outDir, err)}
		}
	}
	rawPayload, err := json.Marshal(pl.payload)
	if err != nil {
		return core.Result{}, &contractError{fmt.Sprintf("the %s request cannot be encoded for the wire: %v", pl.fleetTask, err)}
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Budgets[pl.fleetTask])
		defer cancel()
	}
	jobID, err := newJobID()
	if err != nil {
		return core.Result{}, &placementError{core.DeferClassInfrastructure, "job id: " + err.Error()}
	}

	// The node is chosen BEFORE anything is packed: with no eligible node the call must cost one health
	// probe, not a bundle's worth of memory and minutes. With input files the job goes through the
	// media-job door (a node that opened it), without through the plain dispatch.
	route, taskForNode := "/fleet/dispatch", pl.fleetTask
	if len(pl.inputs) > 0 {
		route, taskForNode = "/fleet/media-job", taskMediaJob
	}
	base, node, err := pickNode(ctx, cfg, bases, pl.fleetTask, taskForNode, nodeRoutes(req))
	if err != nil {
		return core.Result{}, err
	}

	var body []byte
	if len(pl.inputs) > 0 {
		bundle, names, berr := buildBundle(cfg, pl.inputs)
		if berr != nil {
			return core.Result{}, berr
		}
		sum := sha256.Sum256(bundle)
		inputs := map[string]string{}
		for i, in := range pl.inputs {
			inputs[in.field] = names[i]
		}
		body, err = json.Marshal(map[string]any{
			"job_id": jobID, "task_type": pl.fleetTask, "payload": json.RawMessage(rawPayload),
			"bundle": base64.StdEncoding.EncodeToString(bundle), "bundle_sha256": hex.EncodeToString(sum[:]), "inputs": inputs,
		})
	} else {
		body, err = json.Marshal(map[string]any{"job_id": jobID, "task_type": pl.fleetTask, "payload": json.RawMessage(rawPayload)})
		if err == nil && len(body) > maxDispatchBody {
			// The node's /fleet/dispatch refuses a body over 1 MiB; a graph and a manifest that each fit
			// can together not. Said here, by size, instead of as the node's 400 after a round trip.
			return core.Result{}, &contractError{fmt.Sprintf("the %s request is %d bytes, over the %d bytes a fleet dispatch carries", pl.fleetTask, len(body), maxDispatchBody)}
		}
	}
	if err != nil {
		return core.Result{}, &contractError{fmt.Sprintf("the %s request cannot be encoded for the wire: %v", pl.fleetTask, err)}
	}

	// The call's one PAIR card opens here, on the node about to receive the job.
	h.Dispatched(base, node, jobID)
	if err := post(ctx, cfg, base, route, body); err != nil {
		if be := budgetEnded(ctx, phaseSending, node, jobID, err); be != nil {
			return core.Result{}, be
		}
		return core.Result{}, err
	}
	res, data, err := wait(ctx, cfg, base, jobID, h)
	if err != nil {
		if be := budgetEnded(ctx, phaseRendering, node, jobID, err); be != nil {
			return core.Result{}, be
		}
		var pe *placementError
		if errors.As(err, &pe) {
			return core.Result{}, &placementError{pe.class, fmt.Sprintf("node %s (remote job %s): %s", node, jobID, pe.msg)}
		}
		return core.Result{}, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("node %s: %v", node, err)}
	}
	if !res.OK {
		res.Meta.Node = node
		return res, nil
	}
	local, err := fetchOutputs(ctx, cfg, base, data, outputDest{out: str(req.Params, "out"), outDir: pl.outDir}, jobID, node)
	if err != nil {
		if be := budgetEnded(ctx, phaseFetching, node, jobID, err); be != nil {
			return core.Result{}, be
		}
		var pe *placementError
		if errors.As(err, &pe) {
			return core.Result{}, &placementError{pe.class, fmt.Sprintf("node %s rendered the job but %s", node, pe.msg)}
		}
		var se *httpStatusError
		if errors.As(err, &se) && se.rejectedToken() {
			return core.Result{}, &placementError{core.DeferClassConfig, fmt.Sprintf("node %s rendered the job but refused to hand it over: %v (check fleet_auth_token)", node, err)}
		}
		return core.Result{}, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("node %s rendered the job but fetching it failed: %v", node, err)}
	}
	return core.Result{OK: true, Data: local, Meta: core.Meta{Node: node, LatencyMs: time.Since(start).Milliseconds()}}, nil
}

// phase is the step of a call a deadline can end.
type phase int

const (
	phaseSending phase = iota
	phaseRendering
	phaseFetching
)

// budgetEnded answers a failure that surfaced after this call's own deadline passed (the budget this call
// set itself, or the caller's shorter one) as a budget defer naming the node and the remote job, whatever
// transport error the expiry looked like on the way. While the job is sent or rendering, the node may
// still be running it, and it cannot be recalled: a media job is claimed to running as soon as it is
// admitted, and the node's withdraw (DELETE /fleet/jobs/{id}) is for agent jobs only (ADR 0064), so the
// message says so rather than the call spending time on a request the node always refuses. A deadline
// that passes while the outputs are fetched is a different thing: the render is finished and the files
// are still on the node. nil when the deadline did not pass.
func budgetEnded(ctx context.Context, ph phase, node, jobID string, cause error) error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	if ph == phaseFetching {
		return &placementError{core.DeferClassBudget, fmt.Sprintf(
			"node %s (remote job %s) finished the render, but the deadline for this call passed while its outputs were being fetched; the files are still on the node and none was kept here: %v", node, jobID, cause)}
	}
	doing := "sending the job"
	if ph == phaseRendering {
		doing = "the job was rendering"
	}
	return &placementError{core.DeferClassBudget, fmt.Sprintf(
		"the deadline for this call passed while %s on node %s (remote job %s); the node may still be running the job and it cannot be recalled (a media job cannot be withdrawn, ADR 0064): %v", doing, node, jobID, cause)}
}

func newJobID() (string, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	return "media-" + hex.EncodeToString(rnd[:]), nil
}

func normBase(b string) string { return strings.TrimRight(strings.TrimSpace(b), "/") }

// candidateBases is the nodes this call may use: the caller's remotes (each of which must also be one of
// delegate_remotes, so a call can narrow the fleet but never reach past the operator's list) or, when it
// gave none, delegate_remotes.
func candidateBases(cfg config.Config, remotes []string) ([]string, error) {
	allowed := map[string]bool{}
	var configured []string
	for _, b := range cfg.DelegateRemotes {
		if b = normBase(b); b != "" {
			allowed[b] = true
			configured = append(configured, b)
		}
	}
	var asked []string
	for _, b := range remotes {
		if b = normBase(b); b != "" {
			asked = append(asked, b)
		}
	}
	if len(asked) == 0 {
		if len(configured) == 0 {
			return nil, &placementError{core.DeferClassConfig, "no delegate_remotes configured: there is no fleet node to run the job on"}
		}
		return configured, nil
	}
	for _, b := range asked {
		if !allowed[b] {
			return nil, &contractError{fmt.Sprintf("remote %s is not in delegate_remotes: a call may narrow the configured fleet, never extend it", b)}
		}
	}
	return asked, nil
}

// checkInputSizes refuses, from the files' sizes alone and before anything is read or sent, input files
// that cannot fit the bundle cap as this machine knows it (the node's own cap may differ).
func checkInputSizes(cfg config.Config, inputs []input) error {
	max := cfg.EffectiveMediaInputsMaxBytes()
	var total int64
	for _, in := range inputs {
		if fi, err := os.Stat(in.path); err == nil {
			total += fi.Size()
		}
	}
	if total > max {
		return &contractError{fmt.Sprintf("the input files are %d bytes, over fleet_media_inputs_max_mb (%d MiB); the node's own cap may differ", total, max>>20)}
	}
	return nil
}

// buildBundle copies each input file into a temp directory under its bundle name and packs the directory
// (regular files only, within the node's cap as this machine knows it). names[i] is the name inputs[i]
// travels under. A file the caller named that cannot be read is the caller's (a contract defer); this
// machine's own temp directory, disk and packer failing are infrastructure.
func buildBundle(cfg config.Config, inputs []input) ([]byte, []string, error) {
	tmp, err := os.MkdirTemp("", "media-bundle-")
	if err != nil {
		return nil, nil, &placementError{core.DeferClassInfrastructure, "bundle: " + err.Error()}
	}
	defer os.RemoveAll(tmp)
	names := make([]string, len(inputs))
	for i, in := range inputs {
		names[i] = bundleName(in.field, in.path)
		if err := copyFile(in.path, filepath.Join(tmp, names[i])); err != nil {
			var ie *inputFileError
			if errors.As(err, &ie) {
				return nil, nil, &contractError{fmt.Sprintf("%s: %v", in.field, ie.err)}
			}
			return nil, nil, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("bundling %s on this machine: %v", in.field, err)}
		}
	}
	max := cfg.EffectiveMediaInputsMaxBytes()
	// Every way Pack refuses a file (not regular, over a limit, too many) was ruled out above (plan checked
	// each input is a regular file, checkInputSizes the total), so what is left of a Pack error is this
	// machine failing to read what it just copied.
	bundle, err := packFn(tmp, composebundle.Limits{MaxFiles: 8, MaxFileBytes: max, MaxTotal: max})
	if err != nil {
		return nil, nil, &placementError{core.DeferClassInfrastructure, "packing the input files on this machine: " + err.Error()}
	}
	if int64(len(bundle)) > max {
		return nil, nil, &contractError{fmt.Sprintf("the bundle is %d bytes, over fleet_media_inputs_max_mb (%d MiB); the node's own cap may differ", len(bundle), max>>20)}
	}
	return bundle, names, nil
}

// packFn packs the staged input directory. A var so a test can make the packer fail or count its calls.
var packFn = composebundle.Pack

// inputFileError marks a copy failure that is about the caller's source file (it vanished or cannot be
// opened), as opposed to this machine's destination side.
type inputFileError struct{ err error }

func (e *inputFileError) Error() string { return e.err.Error() }
func (e *inputFileError) Unwrap() error { return e.err }

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return &inputFileError{err}
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// pickNode probes the candidate nodes and returns the best eligible one. A node is eligible when it is
// reachable, lists the task (and media-job when input files travel), reports every route the task needs as
// CONFIGURED when it reports routes at all (an older node reports none: unknown, not refused), and does not
// hold a TEXT lease (it would answer 503 anyway). Among them a node with no held lease ranks first, then
// the shortest queue (queued plus running), then config order. Every miss is named in the error.
func pickNode(ctx context.Context, cfg config.Config, bases []string, task, doorTask string, routes []string) (base, node string, err error) {
	type cand struct {
		base, node string
		held       bool
		queue, i   int
	}
	var (
		cands  []cand
		misses []string
	)
	// The candidates are read through internal/rosterprobe like every other single-shot lane (ADR 0074):
	// each entry is judged by the tailnet shape check before it is dialled (a refused entry is a named miss,
	// never a dial), the probes run concurrently in configured order, and the shared memo and negative
	// cache keep a dead node from costing a bound on every call. Reading.Index is the entry's slot in the
	// list probed here, which is what "config order" breaks ties by.
	for _, r := range rosterprobe.Probe(ctx, bases, cfg.FleetAuthToken, healthTimeout) {
		b, v := r.Base, r.View
		if r.Err != nil {
			misses = append(misses, r.Miss())
			continue
		}
		who := fmt.Sprintf("%s (%s)", r.Shown(), v.NodeID)
		if !contains(v.Tasks, task) {
			misses = append(misses, fmt.Sprintf("%s: does not serve %s", who, task))
			continue
		}
		if doorTask == taskMediaJob && !contains(v.Tasks, taskMediaJob) {
			misses = append(misses, fmt.Sprintf("%s: does not advertise media-job (a node opens that door with fleet_media_inputs and a fleet_auth_token)", who))
			continue
		}
		if len(routes) > 0 && v.MediaRoutesKnown && !anyConfigured(v, routes) {
			misses = append(misses, fmt.Sprintf("%s: route %s is %s on that node", who, strings.Join(routes, " or "), stateOf(v, routes)))
			continue
		}
		if v.LeasedText {
			misses = append(misses, fmt.Sprintf("%s: a text lease holds its card", who))
			continue
		}
		id := v.NodeID
		if id == "" {
			id = r.Shown() // the node id reaches results and messages, so the redacted form
		}
		cands = append(cands, cand{b, id, v.LeaseHeld || v.LeaseBusy || v.LeaseOverdue, v.QueueDepth + v.JobsRunning, r.Index})
	}
	if len(cands) == 0 {
		why := "no fleet node serves " + task
		if doorTask == taskMediaJob {
			why += " with input files"
		}
		return "", "", &placementError{core.DeferClassCapacity, why + ": probed " + strings.Join(misses, "; ")}
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].held != cands[b].held {
			return !cands[a].held
		}
		if cands[a].queue != cands[b].queue {
			return cands[a].queue < cands[b].queue
		}
		return cands[a].i < cands[b].i
	})
	return cands[0].base, cands[0].node, nil
}

func anyConfigured(v delegate.NodeView, routes []string) bool {
	for _, r := range routes {
		if st, ok := v.RouteState(r); ok && st == delegate.MediaRouteConfigured {
			return true
		}
	}
	return false
}

// stateOf names the node's verdict on the route(s) a miss is about.
func stateOf(v delegate.NodeView, routes []string) string {
	var parts []string
	for _, r := range routes {
		if st, ok := v.RouteState(r); ok {
			parts = append(parts, st)
		} else {
			parts = append(parts, "not reported")
		}
	}
	return strings.Join(parts, " / ")
}

func post(ctx context.Context, cfg config.Config, base, route string, body []byte) error {
	url := base + route
	dctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(dctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &placementError{core.DeferClassInfrastructure, err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	auth(cfg, hreq)
	// Who asked, and whether the serving node must card the job because this box will not. Both doors
	// (the tokenless dispatch and the token-gated media-job) are work-creating requests.
	pairworkloads.WireHeadersFor(cfg, hreq.Header)
	resp, err := HTTPClient.Do(hreq)
	if err != nil {
		return &placementError{core.DeferClassInfrastructure, rosterprobe.Scrub(base, fmt.Errorf("dispatch %s: %w", url, err))}
	}
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		class := core.DeferClassInfrastructure
		switch resp.StatusCode {
		case http.StatusServiceUnavailable, http.StatusTooManyRequests:
			class = core.DeferClassCapacity
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
			class = core.DeferClassContract
		case http.StatusUnauthorized, http.StatusForbidden:
			class = core.DeferClassConfig
		}
		return &placementError{class, rosterprobe.Scrub(base, fmt.Errorf("dispatch %s: status %d: %s", url, resp.StatusCode, truncate(rb)))}
	}
	// The node accepted the job: whatever the negative cache holds against it is out of date.
	rosterprobe.Default.Forget(base)
	return nil
}

type jobWire struct {
	State string          `json:"state"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
	// ErrClass is the err_class the node's lane filed a failed job under, beside Error: what tells a job
	// another job held back (gpu_busy, gpu_queued) from one that ran and broke, so the call's PAIR card
	// closes quiet for the first and failed for the second. A node too old to publish it leaves it empty,
	// and the card closes failed, as it always did.
	ErrClass string `json:"err_class"`
}

// httpStatusError is a node answering a GET with a status that is not 200.
type httpStatusError struct {
	code int
	what string
	body string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.what, e.code, e.body)
}

// rejectedToken: the node refused the fleet bearer.
func (e *httpStatusError) rejectedToken() bool {
	return e.code == http.StatusUnauthorized || e.code == http.StatusForbidden
}

// wait polls the job until it is done or errored. A done media job's data is the pipeline's result object;
// an errored one carries the node's typed reason and the err_class its lane filed it under, which come
// back as a deferred result (Meta.ErrClass, what the call's PAIR card keys on). A node that says
// it does not hold the job (restarted, or evicted it) and one that refuses the bearer end the wait at once;
// any other failure is tolerated until maxPollFailures of them happen in a row.
func wait(ctx context.Context, cfg config.Config, base, jobID string, h core.RemoteAttribution) (core.Result, json.RawMessage, error) {
	failures := 0
	for {
		pctx, cancel := context.WithTimeout(ctx, pollTimeout)
		j, err := getJSON[jobWire](pctx, cfg, base+"/fleet/jobs/"+jobID)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return core.Result{}, nil, fmt.Errorf("job %s: %w", jobID, ctx.Err())
			}
			var se *httpStatusError
			if errors.As(err, &se) {
				switch {
				case se.code == http.StatusNotFound:
					return core.Result{}, nil, fmt.Errorf("job %s: the node denies holding it (evicted or restarted): %w", jobID, err)
				case se.rejectedToken():
					return core.Result{}, nil, &placementError{core.DeferClassConfig, fmt.Sprintf("the node refused the fleet token while polling job %s: %v (check fleet_auth_token)", jobID, err)}
				}
			}
			failures++
			if failures >= maxPollFailures {
				return core.Result{}, nil, fmt.Errorf("job %s: %d consecutive poll failures, last: %w", jobID, failures, err)
			}
		} else {
			failures = 0
			switch j.State {
			case "running":
				h.Running()
			case "done":
				if len(j.Data) == 0 {
					return core.Deferf("the node finished the job with no result", "", core.Meta{}), nil, nil
				}
				return core.Result{OK: true}, j.Data, nil
			case "error":
				return core.Deferf(j.Error, "", core.Meta{ErrClass: j.ErrClass}), nil, nil
			}
		}
		select {
		case <-ctx.Done():
			return core.Result{}, nil, fmt.Errorf("job %s: %w", jobID, ctx.Err())
		case <-time.After(pollEvery):
		}
	}
}

// ---- outputs -------------------------------------------------------------------------------------

// outputKeys are the result keys that name one output file each; run-graph also names files under outputs.
var outputKeys = []string{"video_path", "image_path", "audio_path"}

// outputDest is where the caller wants the outputs: out is the file for the primary output, outDir the
// directory for the rest (run-graph's out_dir, which then replaces media_dir for every output).
type outputDest struct {
	out, outDir string
}

// renameFile moves a verified download into place. A var so a test can make a placement fail.
var renameFile = os.Rename

// staged is one output downloaded to a unique temp name beside its destination and verified, waiting to be
// moved into place. reserved marks a destination this call created (empty) to claim the name.
type staged struct {
	name     string // the node's bare file name
	tmp, dst string
	reserved bool
	explicit bool // the caller's own out: the one destination that may replace an existing file
}

// fetchOutputs downloads every file the node's result names, verifies each against the sha256 the node
// published, and returns the result object with those paths rewritten to the local copies.
//
// Nothing lands until every file is downloaded and verified, and no file that already exists is replaced
// except the caller's explicit out. Every output is staged under a unique temp name in its destination
// directory. Each destination other than out is CLAIMED first by creating it exclusively, so two jobs (or
// two nodes' counter names) can never write the same file: the primary takes the node's file name in
// media_dir when it is free, every other output takes the remote job id as a prefix (an out_dir takes the
// node's names, and the prefix only where a name is taken). The caller's out is decided first and no other
// output may claim its path. A mismatch or a failed download removes every temp and every claimed name and
// leaves the caller's destinations as they were; if a placement fails part-way the error says which files
// already landed. The caller's out is placed last. Fetched files are 0644 less the umask, except that an
// out which already exists keeps that file's permission bits (the stage is chmod-ed to them, so the umask
// does not mask them). Before claiming a name in a directory, leftovers of a fetch that never finished
// (stale temps and empty job-prefixed claims) are swept.
func fetchOutputs(ctx context.Context, cfg config.Config, base string, data json.RawMessage, to outputDest, jobID, node string) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("the node's result is not an object: %w", err)
	}
	expected := map[string]string{} // name -> sha256 the node published
	sizes := map[string]int64{}     // name -> bytes the node published (absent: no size was published)
	published := false
	if arts, ok := m["artifacts"].([]any); ok {
		for _, a := range arts {
			am, _ := a.(map[string]any)
			name, _ := am["name"].(string)
			sum, _ := am["sha256"].(string)
			if name != "" && sum != "" {
				expected[name] = strings.ToLower(sum)
				published = true
				if n, ok := am["bytes"].(json.Number); ok {
					if v, err := n.Int64(); err == nil && v >= 0 {
						sizes[name] = v
					}
				}
			}
		}
	}

	// Collect the named paths: the primary first, then every other output.
	var named []string
	add := func(p string) {
		if p != "" && !contains(named, p) {
			named = append(named, p)
		}
	}
	for _, k := range outputKeys {
		if p, _ := m[k].(string); p != "" {
			add(p)
		}
	}
	if outs, ok := m["outputs"].(map[string]any); ok {
		keys := make([]string, 0, len(outs))
		for k := range outs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			files, _ := outs[k].([]any)
			for _, f := range files {
				if fm, ok := f.(map[string]any); ok {
					if p, _ := fm["path"].(string); p != "" {
						add(p)
					}
				}
			}
		}
	}
	if len(named) == 0 {
		return nil, &placementError{core.DeferClassInfrastructure, errNoOutput.Error()}
	}

	// Every name is checked before anything is created or fetched. The node serves a file by its bare name
	// only, so two different paths with one base name would be one file under two claims: refused.
	names := make([]string, len(named))
	byName := map[string]string{}
	for i, p := range named {
		name, err := nodeName(p)
		if err != nil {
			return nil, err
		}
		if prev, dup := byName[name]; dup && prev != p {
			return nil, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("the node named two different outputs with the file name %s", name)}
		}
		byName[name] = p
		names[i] = name
	}

	dir, prefixSecondaries := cfg.MediaDir, true
	if to.outDir != "" {
		dir, prefixSecondaries = to.outDir, false
	}
	if dir == "" {
		dir = "." // config validation tolerates an empty media_dir; MkdirAll("") is an error on every OS
	}
	// The caller's out is the FIRST destination decided: its path is reserved, so no other output of this
	// job can claim that name (an out inside out_dir that shares a base name with a secondary would
	// otherwise be renamed over it last, destroying it). out itself is not created empty: it may replace
	// an existing file.
	reservedOut := to.out
	swept := map[string]bool{}
	sweep := func(d string) {
		if k := filepath.Clean(d); !swept[k] {
			swept[k] = true
			sweepStale(d, time.Now())
		}
	}
	var items []*staged
	var landed []string
	cleanup := func() {
		for _, it := range items {
			if it.tmp != "" {
				os.Remove(it.tmp)
			}
			if it.reserved {
				os.Remove(it.dst)
			}
		}
	}
	for i := range named {
		it := &staged{name: names[i]}
		items = append(items, it)
		if i == 0 && to.out != "" {
			it.dst, it.explicit = to.out, true
			if err := os.MkdirAll(filepath.Dir(it.dst), 0o755); err != nil {
				cleanup()
				return nil, err
			}
			sweep(filepath.Dir(it.dst))
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			cleanup()
			return nil, err
		}
		sweep(dir)
		dst, err := claimName(dir, names[i], jobID, i != 0 && prefixSecondaries, reservedOut)
		if err != nil {
			cleanup()
			return nil, err
		}
		it.dst, it.reserved = dst, true
	}

	unverified := false
	for _, it := range items {
		perm, exact := stagePerm, false
		if it.explicit {
			// Replacing a file the caller already has keeps its permission bits (a private 0600 out stays
			// 0600); a new out is 0644 less the umask like every other fetched file.
			if fi, err := os.Stat(it.dst); err == nil && fi.Mode().IsRegular() {
				perm, exact = fi.Mode().Perm(), true
			}
		}
		limit, sized := sizes[it.name]
		if !sized {
			limit = -1
		}
		tmp, got, err := download(ctx, cfg, base, it.name, filepath.Dir(it.dst), perm, exact, limit)
		if err != nil {
			cleanup()
			return nil, err
		}
		it.tmp = tmp
		// A long multi-output fetch must never look stale to another call's sweep: refresh the mtime of
		// everything this call holds (its claims and its finished temps) after each download.
		touch(items)
		if want, ok := expected[it.name]; ok {
			if got != want {
				cleanup()
				return nil, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("the file %s arrived with sha256 %s but the node published %s (job %s): it was discarded", it.name, got, want, jobID)}
			}
		} else {
			unverified = true
		}
	}

	// Place the claimed names first and the caller's own out last, so a failure part-way never has
	// replaced the one file this call was allowed to replace.
	order := make([]*staged, 0, len(items))
	for _, it := range items {
		if !it.explicit {
			order = append(order, it)
		}
	}
	for _, it := range items {
		if it.explicit {
			order = append(order, it)
		}
	}
	for k, it := range order {
		if err := renameFile(it.tmp, it.dst); err != nil {
			for _, rest := range order[k:] {
				os.Remove(rest.tmp)
				if rest.reserved {
					os.Remove(rest.dst)
				}
			}
			already := "none"
			if len(landed) > 0 {
				already = strings.Join(landed, ", ")
			}
			return nil, &placementError{core.DeferClassInfrastructure, fmt.Sprintf(
				"placing %s failed: %v (files already in place: %s; every other download was removed)", it.dst, err, already)}
		}
		it.tmp = ""
		landed = append(landed, it.dst)
	}

	dest := map[string]string{} // node path -> local path
	for i, p := range named {
		dest[p] = items[i].dst
	}
	rewrite := func(p string) string {
		if l, ok := dest[p]; ok {
			return l
		}
		return p
	}
	for _, k := range outputKeys {
		if p, _ := m[k].(string); p != "" {
			m[k] = rewrite(p)
		}
	}
	if outs, ok := m["outputs"].(map[string]any); ok {
		for _, files := range outs {
			list, _ := files.([]any)
			for _, f := range list {
				if fm, ok := f.(map[string]any); ok {
					if p, _ := fm["path"].(string); p != "" {
						fm["path"] = rewrite(p)
					}
				}
			}
		}
	}
	m["node"] = node
	m["remote_job_id"] = jobID
	if unverified || !published {
		m["unverified"] = true
	}
	return json.Marshal(m)
}

// claimName creates, exclusively and empty, the file an output will be moved onto, and returns its path:
// the node's own name when it is wanted and free, else the remote job id as a prefix, else that with a
// counter. An existing file (or directory) of any of those names is never touched, and neither is the path
// in reserved (the caller's explicit out, which this call is going to write): a candidate that is that path
// counts as taken.
func claimName(dir, name, jobID string, prefixed bool, reserved string) (string, error) {
	candidates := []string{name, jobID + "-" + name}
	if prefixed {
		candidates = candidates[1:]
	}
	for n := 2; n <= 9; n++ {
		candidates = append(candidates, fmt.Sprintf("%s-%d-%s", jobID, n, name))
	}
	for _, c := range candidates {
		p := filepath.Join(dir, c)
		if reserved != "" && samePath(p, reserved) {
			continue
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			f.Close()
			return p, nil
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("no free file name for %s in %s", name, dir)
}

// nodeName is the bare file name of a path the node reported, whichever OS wrote it. The path is the
// node's, but the name becomes a file in this machine's media dir, so it must be a plain name: never ".",
// "..", a root, or a name with a colon (a drive or an NTFS stream).
func nodeName(p string) (string, error) {
	n := path.Base(strings.ReplaceAll(p, `\`, "/"))
	if n == "." || n == ".." || n == "/" || strings.ContainsAny(n, ":\x00") {
		return "", &placementError{core.DeferClassInfrastructure, fmt.Sprintf("the node named an output %q that is not a plain file name", p)}
	}
	return n, nil
}

// download fetches one file into a unique temp file in dir and returns that temp path and the sha256 of
// the bytes written. exact means perm is the mode of a file being replaced and is applied with fchmod, past
// the umask. limit is the size the node published for the file (-1: none published): the fetch stops one
// byte past it and fails, so a node that streams more than it announced cannot fill the disk before the
// sha256 check would have refused the file. On any failure no temp file is left behind.
func download(ctx context.Context, cfg config.Config, base, name, dir string, perm os.FileMode, exact bool, limit int64) (tmp, sum string, err error) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, base+"/fleet/media/"+url.PathEscape(name), nil)
	if err != nil {
		return "", "", err
	}
	auth(cfg, req)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("GET %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", &httpStatusError{code: resp.StatusCode, what: "GET " + name, body: truncate(b)}
	}
	f, err := createStage(dir, perm)
	if err != nil {
		return "", "", err
	}
	tmp = f.Name()
	if exact {
		// fchmod ignores the umask: an existing out of 0664 must come back 0664, not 0644.
		if err := f.Chmod(perm); err != nil {
			f.Close()
			os.Remove(tmp)
			return "", "", err
		}
	}
	h := sha256.New()
	body := io.Reader(resp.Body)
	if limit >= 0 {
		body = io.LimitReader(resp.Body, limit+1)
	}
	n, err := io.Copy(io.MultiWriter(f, h), body)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return "", "", fmt.Errorf("GET %s: %w", name, err)
	}
	if limit >= 0 && n > limit {
		f.Close()
		os.Remove(tmp)
		return "", "", &placementError{core.DeferClassInfrastructure, fmt.Sprintf("the file %s is larger than the %d bytes the node published for it: the fetch was stopped and nothing was kept", name, limit)}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	return tmp, hex.EncodeToString(h.Sum(nil)), nil
}

// stagePrefix and stageSuffix name a download's temp file; sweepStale recognises its leftovers by them.
const (
	stagePrefix = ".media-fetch-"
	stageSuffix = ".part"
)

// stagePerm is the mode a fetched file gets when no file of that name existed: 0644 less the umask.
const stagePerm os.FileMode = 0o644

// createStage creates a download's temp file under a unique name in dir with mode perm (less the umask):
// 0644 for a new file, so it is readable by whoever reads this machine's media dir as the old direct write
// was, or the permission bits of the file it is going to replace. (A temp made by os.CreateTemp is always
// 0600, and the rename carries that mode onto the output.)
func createStage(dir string, perm os.FileMode) (*os.File, error) {
	for range 100 {
		var rnd [8]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filepath.Join(dir, stagePrefix+hex.EncodeToString(rnd[:])+stageSuffix), os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no free temp file name in %s", dir)
}

// samePath reports whether two paths name the same file, without either existing (case-insensitive where
// the usual file system is).
func samePath(a, b string) bool {
	if abs, err := filepath.Abs(a); err == nil {
		a = abs
	}
	if abs, err := filepath.Abs(b); err == nil {
		b = abs
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

const (
	// staleMargin is added to the longest call budget to get the sweep's threshold.
	staleMargin = time.Hour
	// sweepScan bounds how many directory entries one sweep looks at, and sweepRemove how many files it removes.
	sweepScan, sweepRemove = 4096, 256
)

// staleAfter is how old a fetch leftover must be before the sweep takes it: the longest call budget (the
// same Budgets the client runs a call under, 1 to 6 hours) plus an hour, so that no call another process
// is still running can have its claims or temps taken. A call also refreshes the mtime of everything it
// holds after each download (touch), so a long multi-output fetch never ages toward the threshold.
func staleAfter() time.Duration {
	var longest time.Duration
	for _, b := range Budgets {
		longest = max(longest, b)
	}
	return longest + staleMargin
}

// touch refreshes the modification time of the claims and the finished temps a call holds, best effort.
func touch(items []*staged) {
	now := time.Now()
	for _, it := range items {
		if it.tmp != "" {
			_ = os.Chtimes(it.tmp, now, now)
		}
		if it.reserved {
			_ = os.Chtimes(it.dst, now, now)
		}
	}
}

// claimedName matches the names claimName gives an output that took the remote job id as a prefix.
var claimedName = regexp.MustCompile(`^media-[0-9a-f]{16}-`)

// sweepStale removes what a fetch that never finished left in dir: staged `.media-fetch-*.part` temps and
// zero-byte job-prefixed claim files, both older than staleAfter(). A SIGINT, a kill or a crash between the
// claim and the rename skips the cleanup that error returns do, and an empty claim left behind pushes every
// later job onto a prefixed name. Bounded (entries scanned, files removed) and logged; a failure is only a
// log line, since the fetch it precedes does not depend on it.
//
// Two limits are deliberate. A crash mid-fetch can leave an empty claim that carries the node's bare name
// (the primary output takes it in media_dir): the sweep does not remove it, because an empty file with a
// plain name cannot be told from one the user made, and removing it could destroy theirs. And an empty
// `media-<id>-*` file older than the threshold is indistinguishable from a claim, so it is removed even if
// something else made it empty.
func sweepStale(dir string, now time.Time) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	entries, _ := d.ReadDir(sweepScan)
	threshold := staleAfter()
	var removed []string
	for _, e := range entries {
		if len(removed) >= sweepRemove {
			break
		}
		name := e.Name()
		stage := strings.HasPrefix(name, stagePrefix) && strings.HasSuffix(name, stageSuffix)
		if !stage && !claimedName.MatchString(name) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() || now.Sub(fi.ModTime()) < threshold {
			continue
		}
		if !stage && fi.Size() != 0 {
			continue // a job-prefixed name with content is a fetched file, not a claim
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			log.Printf("mediaremote: could not remove stale fetch leftover %s: %v", filepath.Join(dir, name), err)
			continue
		}
		removed = append(removed, name)
	}
	if len(removed) > 0 {
		log.Printf("mediaremote: removed %d stale fetch leftover(s) from %s: %s", len(removed), dir, strings.Join(removed, ", "))
	}
}

func getJSON[T any](ctx context.Context, cfg config.Config, u string) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return zero, err
	}
	auth(cfg, req)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return zero, err
	}
	if resp.StatusCode != http.StatusOK {
		return zero, &httpStatusError{code: resp.StatusCode, what: "GET " + u, body: truncate(body)}
	}
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return zero, fmt.Errorf("GET %s: not JSON: %w", u, err)
	}
	return v, nil
}

func auth(cfg config.Config, req *http.Request) {
	if cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.FleetAuthToken)
	}
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
