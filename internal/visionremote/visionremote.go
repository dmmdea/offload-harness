// Package visionremote is the CALLER side of the fleet vision lane (0.116.0):
// it decides where ONE single-image vision task (vqa / ocr / assess_image)
// runs — this box's own vision seat, or a fleet node's — and drives the wire
// when the answer is a node. The node side is fleetnode's POST /fleet/vision.
//
// The route vocabulary mirrors agent_delegate's, with the same quality-first
// rule Place applies to contracts:
//
//   - local (the default, and "" ): run in-process, exactly as before the route
//     existed — every caller that never passes a route is byte-identical.
//   - auto: an idle local card ALWAYS runs the work; only when the machine-wide
//     GPU lease is held (delegate.LocalBusy — a render in flight or a text
//     reservation) is a fleet node considered, and when no node is eligible
//     the work still runs local. Queued-local beats ineligible-remote.
//   - remote: force a fleet node; with none eligible the call DEFERS
//     (defer_class capacity — or config when no remotes are configured at
//     all), never a silent local run: "remote" is the caller saying its own
//     card must stay untouched.
//
// The image is read HERE, on the box that has it, through the same loader and
// the same vision_max_image_bytes cap the local path applies
// (imageio.LoadImageB64), and travels as a data URI inside the job; the node
// never sees a path that is not its own. The node's answer is the full
// core.Result it would have returned to a local caller, stamped with the node
// id and the placement reason (Meta.Node / Meta.Placement).
package visionremote

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
	"github.com/dmmdea/offload-harness/internal/imageio"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

// Route values. RouteLocal is the default: an empty route means local.
const (
	RouteLocal  = "local"
	RouteAuto   = "auto"
	RouteRemote = "remote"
)

// Budget bounds one remote call end to end: a cold seat swap on the node
// (llama.cpp vision seats load in 10-30 s; the node's own vision_gpu_wait_sec
// is 90 s by default when a render holds its card) plus a slow card's image
// prefill and decode, with room for a queued dispatch.
const Budget = 300 * time.Second

const (
	healthTimeout   = 5 * time.Second
	dispatchTimeout = 20 * time.Second // the body is an image, several MB
	pollEvery       = 500 * time.Millisecond
	maxBody         = 4 << 20
	// maxPollFailures bounds consecutive failed polls before the call gives
	// up on the node: five refusals (a few seconds) or five 20 s timeouts.
	maxPollFailures = 5
)

// HTTPClient is the transport every request uses; tests swap it. It rides
// netguard.SafeTransport for the same reason the delegator's health client
// does: the lane may only ever reach loopback or the operator's tailnet
// (never-cloud, ADR 0001), enforced at dial time.
var HTTPClient = &http.Client{Transport: netguard.SafeTransport(nil), Timeout: Budget}

// localBusy is the auto route's trigger — the machine-wide GPU lease, read
// exactly as agent placement reads it. A seam so tests drive both branches
// without a lease directory.
var localBusy = func(cfg config.Config) bool {
	return delegate.LocalBusy(cfg.GPULockPath, cfg.StateDir)
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

// Run is THE entry point the MCP handlers and the CLI verbs share: it applies
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
		res, err := Call(ctx, cfg, req)
		if err != nil {
			return placementDefer(err, "remote: forced")
		}
		res.Meta.Placement = "remote: forced"
		return res
	}
	// auto
	if !localBusy(cfg) {
		res := runner.Run(ctx, req)
		res.Meta.Placement = "local: gpu idle"
		return res
	}
	res, err := Call(ctx, cfg, req)
	if err == nil {
		res.Meta.Placement = "remote: local gpu busy"
		return res
	}
	// Busy local, no usable remote: queued-local beats ineligible-remote, the
	// same rule Place applies to a contract. The reason travels so a slow call
	// is attributable.
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

// Call runs req on the best eligible fleet node. The image is loaded here
// (a bad or oversize image is a DEFERRED result, not an error — the same
// shape the local loader produces); a placement or wire failure is an error
// the caller maps to a defer class; the node's own result — including its
// defers — comes back as the core.Result it produced.
func Call(ctx context.Context, cfg config.Config, req core.Request) (core.Result, error) {
	start := time.Now()
	if len(cfg.DelegateRemotes) == 0 {
		return core.Result{}, &placementError{core.DeferClassConfig, "no delegate_remotes configured — nothing to place the vision task on"}
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Budget)
		defer cancel()
	}
	dataURI, err := imageio.LoadImageB64(req.Image, cfg.VisionMaxImageBytes)
	if err != nil {
		return core.Deferf("image load: "+err.Error(), "", core.Meta{}), nil
	}
	base, node, err := pickNode(ctx, cfg)
	if err != nil {
		return core.Result{}, err
	}
	jobID, err := dispatch(ctx, cfg, base, req, dataURI)
	if err != nil {
		return core.Result{}, err
	}
	res, err := wait(ctx, cfg, base, jobID)
	if err != nil {
		return core.Result{}, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("node %s: %v", node, err)}
	}
	res.Meta.Node = node
	if res.Meta.LatencyMs == 0 {
		res.Meta.LatencyMs = time.Since(start).Milliseconds()
	}
	return res, nil
}

// pickNode probes delegate_remotes and hands the views to
// delegate.PlaceVision. Every miss is named in the error so "no node" is never
// a mystery: an unreachable node, a node without the lane, a leased card.
func pickNode(ctx context.Context, cfg config.Config) (base, node string, err error) {
	var (
		bases  []string
		views  []delegate.NodeView
		misses []string
	)
	for _, b := range cfg.DelegateRemotes {
		b = strings.TrimRight(strings.TrimSpace(b), "/")
		if b == "" {
			continue
		}
		hctx, cancel := context.WithTimeout(ctx, healthTimeout)
		v, herr := delegate.FetchNodeView(hctx, b, cfg.FleetAuthToken)
		cancel()
		if herr != nil {
			misses = append(misses, b+": "+herr.Error())
			continue
		}
		switch {
		case !v.ServesVision():
			misses = append(misses, fmt.Sprintf("%s (%s): no vision lane (tasks %v)", b, v.NodeID, v.Tasks))
			continue
		case v.LeasedText || v.LeaseBusy:
			misses = append(misses, fmt.Sprintf("%s (%s): card leased", b, v.NodeID))
			continue
		}
		bases = append(bases, b)
		views = append(views, v)
	}
	i, ok := delegate.PlaceVision(views)
	if !ok {
		return "", "", &placementError{core.DeferClassCapacity,
			"no fleet node is eligible for the vision lane — probed " + strings.Join(misses, "; ")}
	}
	node = views[i].NodeID
	if node == "" {
		node = bases[i]
	}
	return bases[i], node, nil
}

// payload is the POST /fleet/vision body (fleetnode.VisionPayload's shape).
type payload struct {
	JobID    string `json:"job_id"`
	Task     string `json:"task"`
	Image    string `json:"image"`
	Question string `json:"question,omitempty"`
	Brief    string `json:"brief,omitempty"`
}

func dispatch(ctx context.Context, cfg config.Config, base string, req core.Request, dataURI string) (string, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", &placementError{core.DeferClassInfrastructure, "job id: " + err.Error()}
	}
	p := payload{JobID: "vision-" + hex.EncodeToString(rnd[:]), Task: string(req.Task), Image: dataURI}
	if q, ok := req.Params["question"].(string); ok {
		p.Question = q
	}
	if b, ok := req.Params["brief"].(string); ok {
		p.Brief = b
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", &placementError{core.DeferClassInfrastructure, "encoding vision job: " + err.Error()}
	}
	dctx, cancel := context.WithTimeout(ctx, dispatchTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(dctx, http.MethodPost, base+"/fleet/vision", bytes.NewReader(body))
	if err != nil {
		return "", &placementError{core.DeferClassInfrastructure, err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	auth(cfg, hreq)
	resp, err := HTTPClient.Do(hreq)
	if err != nil {
		return "", &placementError{core.DeferClassInfrastructure, fmt.Sprintf("dispatch %s: %v", base, err)}
	}
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		// A refusal is the node's own verdict (queue full, leased, draining,
		// 401): capacity when it is re-placeable, infrastructure otherwise —
		// the delegator's replaceableRefusal split, without the re-placement
		// (one image, one node; the caller retries or runs local).
		class := core.DeferClassInfrastructure
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
			class = core.DeferClassCapacity
		}
		return "", &placementError{class, fmt.Sprintf("dispatch %s: status %d: %s", base, resp.StatusCode, truncate(rb))}
	}
	return p.JobID, nil
}

type jobWire struct {
	State string          `json:"state"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

// wait polls the job until it is done or errored. A done job's data is the
// node's full core.Result (fleetnode.visionJobData) — defers ride inside it.
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
func wait(ctx context.Context, cfg config.Config, base, jobID string) (core.Result, error) {
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
