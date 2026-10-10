// Package textremote is the CALLER side of the fleet text lane (0.154.0): it decides where ONE
// classify or extract call runs (this box's own cascade, or a fleet node's own pipeline) and
// drives the wire when the answer is a node. The node side is fleetnode's POST /fleet/text. It is
// visionremote's shape, for a text payload.
//
// The route vocabulary is visionremote's, and so is the quality-first rule:
//
//   - local (the default, and ""): run in-process, exactly as before the route existed; every
//     caller that never passes a route is byte-identical.
//   - auto: an idle local card ALWAYS runs the work; only when the machine-wide GPU lease is held
//     (delegate.LocalBusy) is a fleet node considered, and when no node is eligible the work still
//     runs local. Queued-local beats ineligible-remote.
//   - remote: force a fleet node; with none eligible the call DEFERS (defer_class capacity, or
//     config when no remotes are configured at all), never a silent local run.
//
// A node is eligible only when its health lists "text" AND the task in text_tasks. The lane ships
// dark (no shipped tier declares a text task until measured data passes), so on a fleet where no
// node advertises it, remote defers and auto stays local. A node that predates the lane is never
// picked. The node's answer is the full core.Result its own pipeline produced (an unconstrained
// seat validated strictly, defers included), stamped with the node id and the placement reason
// (Meta.Node / Meta.Placement). summarize and triage are not routable: the node refuses them.
package textremote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

// Route values. RouteLocal is the default: an empty route means local.
const (
	RouteLocal  = "local"
	RouteAuto   = "auto"
	RouteRemote = "remote"
)

// Budget bounds one remote call end to end: a cold seat load on the node plus a slow runtime's
// prefill and decode (an NPU node decodes at single-digit tokens per second), with room for a
// queued dispatch. It is the vision lane's figure, and sits above the node's own
// request_timeout_sec.
const Budget = 300 * time.Second

// healthTimeout is how long one call waits for one node's /fleet/health: the single-shot lanes'
// shared bound, probed concurrently and cached (internal/rosterprobe). A var so a test compresses
// it; production never mutates it.
var healthTimeout = rosterprobe.DefaultTimeout

const (
	dispatchTimeout = 20 * time.Second
	pollEvery       = 500 * time.Millisecond
	maxBody         = 4 << 20
	// maxPayload is the node's POST /fleet/text body cap (fleetnode.TextBodyCap, dispatch's 1 MiB);
	// a larger call is refused here, naming the limit, rather than as a 400 after placement.
	maxPayload = 1 << 20
	// maxPollFailures bounds consecutive failed polls before the call gives
	// up on the node: five refusals (a few seconds) or five 20 s timeouts.
	maxPollFailures = 5
)

// HTTPClient is the transport every request uses; tests swap it. It rides
// netguard.SafeTransport for the same reason the delegator's health client
// does: the lane may only ever reach loopback or the operator's tailnet
// (never-cloud, ADR 0001), enforced at dial time.
var HTTPClient = &http.Client{Transport: netguard.SafeTransport(nil), Timeout: Budget, CheckRedirect: rosterprobe.NoRedirect}

// localBusy is the auto route's trigger — the machine-wide GPU lease, read
// exactly as agent placement reads it. A seam so tests drive both branches
// without a lease directory.
//
// The reading is narrowed to the cards the cascade's workhorse seat is pinned to (plan P4): a
// render on another card is not a reason to send a classify off the box. A seat with no
// declared pin reads every card, as before.
var localBusy = func(cfg config.Config) bool {
	pins, _ := cfg.ModelPins(cfg.Model)
	return delegate.LocalBusyFor(cfg.GPULockPath, cfg.StateDir, pins)
}

// Runner runs one request in-process; *pipeline.Pipeline satisfies it.
type Runner interface {
	Run(ctx context.Context, req core.Request) core.Result
}

// NormalizeRoute maps the caller's route to one of the three values; ok is
// false for anything unrecognized. "" is local.
func NormalizeRoute(route string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(route)) {
	case "", RouteLocal:
		return RouteLocal, true
	case RouteAuto:
		return RouteAuto, true
	case RouteRemote:
		return RouteRemote, true
	}
	return "", false
}

// Run is THE entry point the MCP handlers share: it applies
// the route rule above to req and returns the result exactly as the pipeline
// would, plus Meta.Node / Meta.Placement when the route made a decision. A
// local run under the default route carries neither — byte-identical to
// before the route existed.
func Run(ctx context.Context, cfg config.Config, runner Runner, req core.Request, route string) core.Result {
	r, ok := NormalizeRoute(route)
	if !ok {
		res := core.Deferf(fmt.Sprintf("unrecognized route %q; use local, auto or remote", route), "", core.Meta{})
		res.DeferClass = core.DeferClassContract
		return res
	}
	switch r {
	case RouteLocal:
		return runner.Run(ctx, req)
	case RouteRemote:
		// A call sent to a node is the remote lane's own: it writes its ledger row and, once a node
		// is chosen, its one PAIR card (D5/D6).
		h := core.BeginRemote(runner, req, r)
		defer core.CloseOnPanic(h) // a panic in the dispatch, the poll or the fetch closes the card before the door dies of it
		res, err := callWith(ctx, cfg, req, h)
		if err != nil {
			res = placementDefer(err, "remote: forced")
		} else {
			res.Meta.Placement = "remote: forced"
		}
		h.Finish(res)
		return res
	}
	// auto
	if !localBusy(cfg) {
		res := runner.Run(ctx, req)
		res.Meta.Placement = "local: gpu idle"
		return res
	}
	h := core.BeginRemote(runner, req, r)
	defer core.CloseOnPanic(h) // as above, on the auto route
	res, err := callWith(ctx, cfg, req, h)
	if err == nil {
		res.Meta.Placement = "remote: local gpu busy"
		h.Finish(res)
		return res
	}
	// Busy local, no usable remote: queued-local beats ineligible-remote, the
	// same rule Place applies to a contract. The reason travels so a slow call
	// is attributable. The local run writes its own row; an attempt that never
	// reached a node leaves nothing behind, one that did leaves a failed card.
	h.Discard(err.Error())
	res = runner.Run(ctx, req)
	res.Meta.Placement = "local: gpu busy, " + err.Error()
	return res
}

// placementDefer renders a Call failure as the deferred result the route
// contract promises: deferred:true, a reason, and a defer_class a caller can
// branch on — capacity when the fleet has no eligible node right now, config
// when no remotes are configured, infrastructure when a node took the job
// and the wire or the node then failed.
func placementDefer(err error, placement string) core.Result {
	res := core.Deferf(err.Error(), "", core.Meta{Placement: placement})
	var pe *placementError
	if errors.As(err, &pe) {
		res.DeferClass = pe.class
	} else {
		res.DeferClass = core.DeferClassInfrastructure
	}
	return res
}

// placementError is a Call failure with the defer class it maps to.
type placementError struct {
	class string
	msg   string
}

func (e *placementError) Error() string { return e.msg }

// Call runs req on the best eligible fleet node. A placement or wire failure is an error the caller
// maps to a defer class; the node's own result, including its defers, comes back as the core.Result
// it produced.
func Call(ctx context.Context, cfg config.Config, req core.Request) (core.Result, error) {
	return callWith(ctx, cfg, req, core.NopAttribution{})
}

// callWith is Call reporting the dispatch and the node's progress to h (core.RemoteAttribution).
func callWith(ctx context.Context, cfg config.Config, req core.Request, h core.RemoteAttribution) (core.Result, error) {
	start := time.Now()
	if len(cfg.DelegateRemotes) == 0 {
		return core.Result{}, &placementError{core.DeferClassConfig, "no delegate_remotes configured — nothing to place the text task on"}
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Budget)
		defer cancel()
	}
	jobID, body, err := encode(req)
	if err != nil {
		return core.Result{}, err
	}
	base, node, err := pickNode(ctx, cfg, string(req.Task))
	if err != nil {
		return core.Result{}, err
	}
	// The call's one PAIR card opens here, on the node about to receive the job.
	h.Dispatched(base, node, jobID)
	if err := dispatch(ctx, cfg, base, body); err != nil {
		return core.Result{}, err
	}
	res, err := wait(ctx, cfg, base, jobID, h)
	if err != nil {
		return core.Result{}, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("node %s: %v", node, err)}
	}
	res.Meta.Node = node
	if res.Meta.LatencyMs == 0 {
		res.Meta.LatencyMs = time.Since(start).Milliseconds()
	}
	if bad := checkNodeResult(req, res); bad != "" {
		// The node's own pipeline validated this answer, but the delegator does not take a remote
		// answer on trust: one that fails the cheap shape check below is a defer naming the node
		// and the reason, never an accepted result.
		d := core.Deferf(fmt.Sprintf("node %s returned an answer that fails the delegator's check: %s", node, bad),
			string(res.Data), res.Meta)
		d.DeferClass = core.DeferClassInfrastructure
		return d, nil
	}
	return res, nil
}

// checkNodeResult is the delegator's cheap post-check of an OK result from a node, and "" when it
// holds. Classify: a JSON object whose label is one of the request's labels and whose confidence is
// a number in 0..1. Extract: one JSON object whose keys all lie within the requested schema's
// properties (a schema naming no properties constrains no key). A deferred or not-OK result is the
// node's own refusal and passes through.
func checkNodeResult(req core.Request, res core.Result) string {
	if !res.OK || res.Deferred {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(res.Data, &obj); err != nil || obj == nil {
		return "data is not one JSON object"
	}
	switch req.Task {
	case core.TaskClassify:
		var label string
		if raw, ok := obj["label"]; !ok || json.Unmarshal(raw, &label) != nil {
			return "no string label"
		}
		if !stringIn(label, req.Params["labels"]) {
			return fmt.Sprintf("label %q is not in the requested set", label)
		}
		var conf float64
		if raw, ok := obj["confidence"]; !ok || json.Unmarshal(raw, &conf) != nil {
			return "no numeric confidence"
		}
		if !(conf >= 0 && conf <= 1) {
			return fmt.Sprintf("confidence %v is outside 0..1", conf)
		}
	case core.TaskExtract:
		schema, _ := req.Params["schema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if len(props) == 0 {
			return ""
		}
		for k := range obj {
			if _, ok := props[k]; !ok {
				return fmt.Sprintf("key %q is not in the requested schema", k)
			}
		}
	}
	return ""
}

// stringIn reports whether s is one of the labels, which arrive as []string from a local caller
// and []any from a decoded one.
func stringIn(s string, labels any) bool {
	switch l := labels.(type) {
	case []string:
		for _, v := range l {
			if v == s {
				return true
			}
		}
	case []any:
		for _, v := range l {
			if vs, ok := v.(string); ok && vs == s {
				return true
			}
		}
	}
	return false
}

// pickNode probes delegate_remotes and hands the views to delegate.PlaceText. Every miss is named
// in the error so "no node" is never a mystery: an unreachable node, a node without the lane (every
// node that predates it, and every node whose tier declares no text task), a node whose lane does not
// serve this task, a leased card.
func pickNode(ctx context.Context, cfg config.Config, task string) (base, node string, err error) {
	var (
		bases  []string
		views  []delegate.NodeView
		misses []string
	)
	for _, r := range rosterprobe.Probe(ctx, cfg.DelegateRemotes, cfg.FleetAuthToken, healthTimeout) {
		b, v := r.Base, r.View
		if r.Err != nil {
			misses = append(misses, r.Miss())
			continue
		}
		switch {
		case !v.ServesText():
			misses = append(misses, fmt.Sprintf("%s (%s): no text lane (tasks %v)", r.Shown(), v.NodeID, v.Tasks))
			continue
		case !v.ServesTextTask(task):
			misses = append(misses, fmt.Sprintf("%s (%s): its text lane does not serve %s (text_tasks %v)", r.Shown(), v.NodeID, task, v.TextTasks))
			continue
		case v.LeasedText || v.LeaseBusy:
			misses = append(misses, fmt.Sprintf("%s (%s): card leased", r.Shown(), v.NodeID))
			continue
		}
		bases = append(bases, b)
		views = append(views, v)
	}
	i, ok := delegate.PlaceText(views, task)
	if !ok {
		return "", "", &placementError{core.DeferClassCapacity,
			"no fleet node is eligible for the text lane — probed " + strings.Join(misses, "; ")}
	}
	node = views[i].NodeID
	if node == "" {
		node = bases[i]
	}
	return bases[i], node, nil
}

// payload is the POST /fleet/text body (fleetnode.TextPayload's shape).
type payload struct {
	JobID  string         `json:"job_id"`
	Task   string         `json:"task"`
	Input  string         `json:"input"`
	Params map[string]any `json:"params,omitempty"`
}

// encode mints the job id and renders the request as the node's payload, mirroring the params the
// MCP handlers build (classify: labels; extract: schema). A request the lane cannot carry (another
// task, or a body over the node's cap) is a contract defer, never a wire attempt.
func encode(req core.Request) (jobID string, body []byte, err error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", nil, &placementError{core.DeferClassInfrastructure, "job id: " + err.Error()}
	}
	p := payload{JobID: "text-" + hex.EncodeToString(rnd[:]), Task: string(req.Task), Input: req.Input}
	switch req.Task {
	case core.TaskClassify:
		p.Params = map[string]any{"labels": req.Params["labels"]}
	case core.TaskExtract:
		p.Params = map[string]any{"schema": req.Params["schema"]}
	default:
		return "", nil, &placementError{core.DeferClassContract, fmt.Sprintf("the text lane serves classify and extract, not %s", req.Task)}
	}
	body, err = json.Marshal(p)
	if err != nil {
		return "", nil, &placementError{core.DeferClassInfrastructure, "encoding text job: " + err.Error()}
	}
	if len(body) > maxPayload {
		return "", nil, &placementError{core.DeferClassContract, fmt.Sprintf("the text job is %d bytes, over the node's %d-byte body cap", len(body), maxPayload)}
	}
	return p.JobID, body, nil
}

func dispatch(ctx context.Context, cfg config.Config, base string, body []byte) error {
	dctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(dctx, http.MethodPost, base+"/fleet/text", bytes.NewReader(body))
	if err != nil {
		return &placementError{core.DeferClassInfrastructure, err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	auth(cfg, hreq)
	// Who asked, and whether the serving node must card the job because this box will not (D7/D11).
	pairworkloads.WireHeadersFor(cfg, hreq.Header)
	resp, err := HTTPClient.Do(hreq)
	if err != nil {
		return &placementError{core.DeferClassInfrastructure, rosterprobe.Scrub(base, fmt.Errorf("dispatch %s: %w", base, err))}
	}
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		// A refusal is the node's own verdict: capacity when it is re-placeable (queue full,
		// draining), config when the node says this task or payload is not one it serves (its
		// health and its ack disagreed), infrastructure otherwise (401, 403, 5xx).
		class := core.DeferClassInfrastructure
		switch resp.StatusCode {
		case http.StatusServiceUnavailable, http.StatusTooManyRequests:
			class = core.DeferClassCapacity
		case http.StatusBadRequest:
			class = core.DeferClassConfig
		}
		return &placementError{class, rosterprobe.Scrub(base, fmt.Errorf("dispatch %s: status %d: %s", base, resp.StatusCode, truncate(rb)))}
	}
	// The node accepted the job: whatever the negative cache holds against it is out of date.
	rosterprobe.Default.Forget(base)
	return nil
}

type jobWire struct {
	State string          `json:"state"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

// wait polls the job until it is done or errored. A done job's data is the
// node's full core.Result (fleetnode.textJobData) — defers ride inside it.
// An error state is the node's answer too (the build refused it, or the run
// died): it comes back as a deferred result, never a transport error.
//
// A poll that fails is retried: a dropped connection, a node hiccup or one
// slow GET must not throw away a job the node is still running (review
// finding, 2026-09-12 — the first draft failed the whole call on the first
// bad poll while Budget promised room for a cold seat swap). Two things end
// the wait early: the caller's context, and a 404 — the node denies holding
// the job (evicted, or the node restarted), so no later poll can succeed.
// maxPollFailures consecutive failures give up so a dead node costs bounded
// time, not the whole budget.
func wait(ctx context.Context, cfg config.Config, base, jobID string, h core.RemoteAttribution) (core.Result, error) {
	failures := 0
	for {
		pctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
		j, err := getJSON[jobWire](pctx, cfg, base+"/fleet/jobs/"+jobID)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return core.Result{}, fmt.Errorf("job %s: %w", jobID, ctx.Err())
			}
			if strings.Contains(err.Error(), "status 404") {
				return core.Result{}, fmt.Errorf("job %s: the node denies holding it (evicted or restarted): %w", jobID, err)
			}
			failures++
			if failures >= maxPollFailures {
				return core.Result{}, fmt.Errorf("job %s: %d consecutive poll failures, last: %w", jobID, failures, err)
			}
			select {
			case <-ctx.Done():
				return core.Result{}, fmt.Errorf("job %s: %w", jobID, ctx.Err())
			case <-time.After(pollEvery):
			}
			continue
		}
		failures = 0
		switch j.State {
		case "running":
			h.Running()
		case "done":
			var res core.Result
			if len(j.Data) == 0 || json.Unmarshal(j.Data, &res) != nil {
				return core.Deferf("job done with an unreadable result: "+truncate(j.Data), "", core.Meta{}), nil
			}
			return res, nil
		case "error":
			res := core.Deferf(j.Error, "", core.Meta{})
			res.DeferClass = core.DeferClassInfrastructure
			return res, nil
		}
		select {
		case <-ctx.Done():
			return core.Result{}, fmt.Errorf("job %s still %s: %w", jobID, j.State, ctx.Err())
		case <-time.After(pollEvery):
		}
	}
}

func getJSON[T any](ctx context.Context, cfg config.Config, url string) (T, error) {
	var zero T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return zero, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, truncate(body))
	}
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return zero, fmt.Errorf("GET %s: not JSON: %w", url, err)
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
	if len(s) > 256 {
		return s[:256] + "…"
	}
	return s
}
