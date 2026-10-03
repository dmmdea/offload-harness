// Package mediaremote places one image, video, character-animation, audio or ComfyUI-graph job on a fleet
// node and brings the output back, so a machine with no render lane of its own (or one the caller names a
// node for) calls offload_generate_* exactly as a render node does (ADR 0072). It mirrors composeremote:
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
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/composebundle"
	"github.com/dmmdea/offload-harness/internal/composeremote"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

const (
	RouteLocal  = composeremote.RouteLocal
	RouteAuto   = composeremote.RouteAuto
	RouteRemote = composeremote.RouteRemote
)

const (
	healthTimeout   = 5 * time.Second
	dispatchTimeout = 20 * time.Minute // a bundle may be hundreds of MB
	pollTimeout     = 20 * time.Second
	fetchTimeout    = 30 * time.Minute
	maxBody         = 4 << 20
	maxPollFailures = 5
)

// pollEvery is the job poll interval (a var so tests need not wait out real seconds).
var pollEvery = 2 * time.Second

// Budgets bound one remote job end to end when the caller set no deadline: a render's own wall, its queue
// behind the node's media slot, and the transfers.
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

// LocalConfigured reports whether THIS machine has the lane req needs, by the same derivation the node's
// advertisement uses (internal/mediacap reads the files, not only the config): the named image family
// resolves, the video, animation or run-graph route is CONFIGURED, music has its script and weights, and
// voice has its script or a speech endpoint. A default config binds every script, so a thin client reads
// as having a lane only when the files behind the binding are really there.
func LocalConfigured(cfg config.Config, req core.Request) bool {
	routes := map[string]bool{}
	for _, r := range mediacap.Routes(cfg) {
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
	res, err := Call(ctx, cfg, req, remotes)
	if err != nil {
		return placementDefer(err, placement)
	}
	res.Meta.Placement = placement
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
	start := time.Now()
	bases, err := candidateBases(cfg, remotes)
	if err != nil {
		return core.Result{}, err
	}
	pl, err := plan(req)
	if err != nil {
		return core.Result{}, err
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

	// The wire body: with input files, the media-job door with a bundle; without, the plain dispatch.
	route, taskForNode := "/fleet/dispatch", pl.fleetTask
	rawPayload, _ := json.Marshal(pl.payload)
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
		body, _ = json.Marshal(map[string]any{
			"job_id": jobID, "task_type": pl.fleetTask, "payload": json.RawMessage(rawPayload),
			"bundle": base64.StdEncoding.EncodeToString(bundle), "bundle_sha256": hex.EncodeToString(sum[:]), "inputs": inputs,
		})
		route, taskForNode = "/fleet/media-job", taskMediaJob
	} else {
		body, _ = json.Marshal(map[string]any{"job_id": jobID, "task_type": pl.fleetTask, "payload": json.RawMessage(rawPayload)})
	}

	base, node, err := pickNode(ctx, cfg, bases, pl.fleetTask, taskForNode, nodeRoutes(req))
	if err != nil {
		return core.Result{}, err
	}
	if err := post(ctx, cfg, base+route, body); err != nil {
		return core.Result{}, err
	}
	res, data, err := wait(ctx, cfg, base, jobID)
	if err != nil {
		return core.Result{}, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("node %s: %v", node, err)}
	}
	if !res.OK {
		res.Meta.Node = node
		return res, nil
	}
	local, err := fetchOutputs(ctx, cfg, base, data, str(req.Params, "out"), jobID, node)
	if err != nil {
		var pe *placementError
		if errors.As(err, &pe) {
			return core.Result{}, &placementError{pe.class, fmt.Sprintf("node %s rendered the job but %s", node, pe.msg)}
		}
		return core.Result{}, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("node %s rendered the job but fetching it failed: %v", node, err)}
	}
	return core.Result{OK: true, Data: local, Meta: core.Meta{Node: node, LatencyMs: time.Since(start).Milliseconds()}}, nil
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

// buildBundle copies each input file into a temp directory under its bundle name and packs the directory
// (regular files only, within the node's cap as this machine knows it). names[i] is the name inputs[i]
// travels under.
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
			return nil, nil, &contractError{fmt.Sprintf("%s: %v", in.field, err)}
		}
	}
	max := cfg.EffectiveMediaInputsMaxBytes()
	var total int64
	for _, in := range inputs {
		if fi, err := os.Stat(in.path); err == nil {
			total += fi.Size()
		}
	}
	if total > max {
		return nil, nil, &contractError{fmt.Sprintf("the input files are %d bytes, over fleet_media_inputs_max_mb (%d MiB); the node's own cap may differ", total, max>>20)}
	}
	bundle, err := composebundle.Pack(tmp, composebundle.Limits{MaxFiles: 8, MaxFileBytes: max, MaxTotal: max})
	if err != nil {
		return nil, nil, &contractError{"media-job bundle: " + err.Error()}
	}
	if int64(len(bundle)) > max {
		return nil, nil, &contractError{fmt.Sprintf("the bundle is %d bytes, over fleet_media_inputs_max_mb (%d MiB); the node's own cap may differ", len(bundle), max>>20)}
	}
	return bundle, names, nil
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
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
	for i, b := range bases {
		hctx, cancel := context.WithTimeout(ctx, healthTimeout)
		v, herr := delegate.FetchNodeView(hctx, b, cfg.FleetAuthToken)
		cancel()
		if herr != nil {
			misses = append(misses, b+": "+herr.Error())
			continue
		}
		who := fmt.Sprintf("%s (%s)", b, v.NodeID)
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
			id = b
		}
		cands = append(cands, cand{b, id, v.LeaseHeld || v.LeaseBusy || v.LeaseOverdue, v.QueueDepth + v.JobsRunning, i})
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

func post(ctx context.Context, cfg config.Config, url string, body []byte) error {
	dctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(dctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &placementError{core.DeferClassInfrastructure, err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	auth(cfg, hreq)
	resp, err := HTTPClient.Do(hreq)
	if err != nil {
		return &placementError{core.DeferClassInfrastructure, fmt.Sprintf("dispatch %s: %v", url, err)}
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
		return &placementError{class, fmt.Sprintf("dispatch %s: status %d: %s", url, resp.StatusCode, truncate(rb))}
	}
	return nil
}

type jobWire struct {
	State string          `json:"state"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

// wait polls the job until it is done or errored. A done media job's data is the pipeline's result object;
// an errored one carries the node's typed reason, which comes back as a deferred result.
func wait(ctx context.Context, cfg config.Config, base, jobID string) (core.Result, json.RawMessage, error) {
	failures := 0
	for {
		pctx, cancel := context.WithTimeout(ctx, pollTimeout)
		j, err := getJSON[jobWire](pctx, cfg, base+"/fleet/jobs/"+jobID)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return core.Result{}, nil, fmt.Errorf("job %s: %w", jobID, ctx.Err())
			}
			if strings.Contains(err.Error(), "status 404") {
				return core.Result{}, nil, fmt.Errorf("job %s: the node denies holding it (evicted or restarted): %w", jobID, err)
			}
			failures++
			if failures >= maxPollFailures {
				return core.Result{}, nil, fmt.Errorf("job %s: %d consecutive poll failures, last: %w", jobID, failures, err)
			}
		} else {
			failures = 0
			switch j.State {
			case "done":
				if len(j.Data) == 0 {
					return core.Deferf("the node finished the job with no result", "", core.Meta{}), nil, nil
				}
				return core.Result{OK: true}, j.Data, nil
			case "error":
				return core.Deferf(j.Error, "", core.Meta{}), nil, nil
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

// fetched is one output downloaded to a temp name and verified, waiting to be moved into place.
type fetched struct {
	tmp, dst string
}

// fetchOutputs downloads every file the node's result names, verifies each against the sha256 the node
// published, and returns the result object with those paths rewritten to the local copies. Nothing lands
// until every file is downloaded and verified: a mismatch deletes what was fetched and leaves the caller's
// destination as it was. The primary output goes to `out` when the caller named one; the rest go to this
// box's media dir under the node's file names.
func fetchOutputs(ctx context.Context, cfg config.Config, base string, data json.RawMessage, out, jobID, node string) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("the node's result is not an object: %w", err)
	}
	expected := map[string]string{} // name -> sha256 the node published
	published := false
	if arts, ok := m["artifacts"].([]any); ok {
		for _, a := range arts {
			am, _ := a.(map[string]any)
			name, _ := am["name"].(string)
			sum, _ := am["sha256"].(string)
			if name != "" && sum != "" {
				expected[name] = strings.ToLower(sum)
				published = true
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

	// Primary = the first named output. Its destination is `out` when given.
	dest := map[string]string{} // node path -> local path
	taken := map[string]string{}
	for i, p := range named {
		name, err := nodeName(p)
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(cfg.MediaDir, name)
		if i == 0 && out != "" {
			dst = out
		}
		if prev, dup := taken[dst]; dup && prev != p {
			return nil, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("the node named two different outputs that map to %s", dst)}
		}
		taken[dst] = p
		dest[p] = dst
	}

	var pending []fetched
	cleanup := func() {
		for _, f := range pending {
			os.Remove(f.tmp)
		}
	}
	unverified := false
	for _, p := range named {
		name, _ := nodeName(p)
		dst := dest[p]
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			cleanup()
			return nil, err
		}
		tmp, got, err := download(ctx, cfg, base, name, dst)
		if err != nil {
			cleanup()
			return nil, err
		}
		pending = append(pending, fetched{tmp, dst})
		if want, ok := expected[name]; ok {
			if got != want {
				cleanup()
				return nil, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("the file %s arrived with sha256 %s but the node published %s (job %s): it was discarded", name, got, want, jobID)}
			}
		} else {
			unverified = true
		}
	}
	for i, f := range pending {
		if err := os.Rename(f.tmp, f.dst); err != nil {
			for _, g := range pending[i:] {
				os.Remove(g.tmp)
			}
			return nil, err
		}
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

// download fetches one file into <dst>.part and returns that temp path and the sha256 of the bytes written.
func download(ctx context.Context, cfg config.Config, base, name, dst string) (tmp, sum string, err error) {
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
		return "", "", fmt.Errorf("GET %s: status %d: %s", name, resp.StatusCode, truncate(b))
	}
	tmp = dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", "", fmt.Errorf("GET %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	return tmp, hex.EncodeToString(h.Sum(nil)), nil
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
		return zero, fmt.Errorf("GET %s: status %d: %s", u, resp.StatusCode, truncate(body))
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
