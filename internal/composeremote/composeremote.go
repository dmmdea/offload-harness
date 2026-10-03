// Package composeremote places a composition on a fleet node and brings the video back, so a machine
// with no composition lane of its own (a thin client) calls offload_compose_video exactly as a render
// node does (ADR 0070). It mirrors visionremote: probe delegate_remotes through /fleet/health, pick a
// node that advertises the task, dispatch with the fleet bearer, poll the job, and map every failure
// to a typed defer.
//
// Two kinds of request travel:
//   - a vetted template (template + variables) goes through the fleet's template door, compose-video
//     over /fleet/dispatch, which every node with the lane serves;
//   - a project (project_dir, or an inline html composition) is packed into a bundle, checked here with
//     the same confinement rules the node applies (composebundle), and sent to the token-gated
//     compose-project door, which only nodes that opened it advertise.
//
// The node writes the video (and any snapshots) into its own media dir; they are fetched back by name
// from /fleet/media into the caller's `out` or this box's media dir, and the result's paths are
// rewritten to the local copies. The node's typed failure reason (LINT_ERRORS, CHECK_FAILED, ...) is
// passed through unchanged.
package composeremote

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
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

const (
	RouteLocal  = "local"
	RouteAuto   = "auto"
	RouteRemote = "remote"

	// The fleet task types (fleetnode.ComposeTask / ComposeProjectTask).
	taskTemplate = "compose-video"
	taskProject  = "compose-project"
)

const (
	healthTimeout   = 5 * time.Second
	dispatchTimeout = 10 * time.Minute // a project bundle may be tens of MB
	pollTimeout     = 20 * time.Second
	fetchTimeout    = 15 * time.Minute
	pollEvery       = 1 * time.Second
	maxBody         = 4 << 20
	maxPollFailures = 5
	// Budget bounds one remote composition end to end when the caller set no deadline: a 30-minute
	// render (compose_timeout_sec's default) plus the transfers.
	Budget = 45 * time.Minute
)

// HTTPClient is the transport every request uses; tests swap it. netguard.SafeTransport limits the lane to
// loopback and the operator's tailnet at dial time (never cloud, ADR 0001).
var HTTPClient = &http.Client{Transport: netguard.SafeTransport(nil)}

// Runner runs one request in-process; *pipeline.Pipeline satisfies it.
type Runner interface {
	Run(ctx context.Context, req core.Request) core.Result
}

// NormalizeRoute maps the caller's route to one of the three values; "" is auto.
func NormalizeRoute(route string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(route)) {
	case "", RouteAuto:
		return RouteAuto, true
	case RouteLocal:
		return RouteLocal, true
	case RouteRemote:
		return RouteRemote, true
	}
	return "", false
}

// Run is the entry point the MCP tool and the CLI verb share. local runs here; remote runs on a fleet
// node; auto runs here when this box has the composition lane (byte-identical to before the route
// existed) and on a fleet node when it has none.
func Run(ctx context.Context, cfg config.Config, runner Runner, req core.Request, route string) core.Result {
	r, ok := NormalizeRoute(route)
	if !ok {
		res := core.Deferf(fmt.Sprintf("unrecognized route %q; use local, auto or remote", route), "", core.Meta{})
		res.DeferClass = core.DeferClassContract
		return res
	}
	if r == RouteLocal || (r == RouteAuto && cfg.ComposeRouteConfigured()) {
		return runner.Run(ctx, req)
	}
	placement := "remote: forced"
	if r == RouteAuto {
		placement = "remote: no composition lane on this machine"
		if len(cfg.DelegateRemotes) == 0 {
			// Neither door is open: say both ways to open one, not only the fleet's.
			res := core.Deferf("compose_video: no composition route on this machine (bind compose_script, hyperframes_dir and hyperframes_browser_path: local-offload install hyperframes) and no delegate_remotes to render it on a fleet node", "", core.Meta{Placement: placement})
			res.DeferClass = core.DeferClassConfig
			return res
		}
	}
	res, err := Call(ctx, cfg, req)
	if err != nil {
		return placementDefer(err, placement)
	}
	res.Meta.Placement = placement
	return res
}

type placementError struct {
	class string
	msg   string
}

func (e *placementError) Error() string { return e.msg }

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

// Call runs req on the best eligible fleet node and fetches its outputs. A placement, transfer or
// bundle problem is an error the caller maps to a defer class; the node's own verdict (a render that
// deferred) comes back as a deferred core.Result.
func Call(ctx context.Context, cfg config.Config, req core.Request) (core.Result, error) {
	start := time.Now()
	if len(cfg.DelegateRemotes) == 0 {
		return core.Result{}, &placementError{core.DeferClassConfig, "no delegate_remotes configured: there is no fleet node to render the composition on"}
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Budget)
		defer cancel()
	}
	p := req.Params
	str := func(k string) string { s, _ := p[k].(string); return strings.TrimSpace(s) }
	if str("format") == "png-sequence" {
		return core.Deferf("png-sequence writes a directory, which a fleet node cannot send back; render mp4, webm, mov or gif remotely", "", core.Meta{}), nil
	}
	template, html, projectDir := str("template"), stringParam(p, "html"), str("project_dir")
	n := 0
	for _, v := range []string{template, html, projectDir} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		return core.Deferf("give exactly one of template, html, project_dir", "", core.Meta{}), nil
	}

	var (
		task string
		body []byte
		err  error
	)
	jobID, err := newJobID()
	if err != nil {
		return core.Result{}, &placementError{core.DeferClassInfrastructure, "job id: " + err.Error()}
	}
	if template != "" {
		task = taskTemplate
		payload := map[string]any{"template": template}
		copyOptions(p, payload)
		if v, ok := p["variables"]; ok {
			payload["variables"] = v
		}
		raw, _ := json.Marshal(payload)
		body, _ = json.Marshal(map[string]any{"job_id": jobID, "task_type": taskTemplate, "payload": json.RawMessage(raw)})
	} else {
		task = taskProject
		bundle, berr := buildBundle(cfg, projectDir, html, str("composition"))
		if berr != nil {
			return core.Deferf("compose-project bundle: "+berr.Error(), "", core.Meta{}), nil
		}
		sum := sha256.Sum256(bundle)
		payload := map[string]any{"job_id": jobID, "bundle": base64.StdEncoding.EncodeToString(bundle), "bundle_sha256": hex.EncodeToString(sum[:])}
		if c := str("composition"); c != "" && projectDir != "" {
			payload["composition"] = c
		}
		copyOptions(p, payload)
		body, _ = json.Marshal(payload)
	}

	base, node, err := pickNode(ctx, cfg, task)
	if err != nil {
		return core.Result{}, err
	}
	route := "/fleet/dispatch"
	if task == taskProject {
		route = "/fleet/compose-project"
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
	local, err := fetchOutputs(ctx, cfg, base, data, str("out"))
	if err != nil {
		return core.Result{}, &placementError{core.DeferClassInfrastructure, fmt.Sprintf("node %s rendered the composition but fetching it failed: %v", node, err)}
	}
	return core.Result{OK: true, Data: local, Meta: core.Meta{Node: node, LatencyMs: time.Since(start).Milliseconds(), Model: "hyperframes"}}, nil
}

// stringParam reads a string parameter without trimming (an html body keeps its whitespace).
func stringParam(p map[string]any, k string) string {
	s, _ := p[k].(string)
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

// copyOptions carries the render options a node accepts; `out` never travels (the node writes into its
// own media dir and the file is fetched back).
func copyOptions(from, to map[string]any) {
	for _, k := range []string{"format", "quality", "resolution", "fps", "workers", "strict", "snapshots"} {
		if v, ok := from[k]; ok && v != nil && v != "" {
			to[k] = v
		}
	}
}

// buildBundle packs a project (or wraps an inline composition) and runs the node's own checks on it
// here, so a project the node would refuse fails before any upload.
func buildBundle(cfg config.Config, projectDir, html, composition string) ([]byte, error) {
	var (
		bundle []byte
		err    error
	)
	if projectDir != "" {
		bundle, err = composebundle.Pack(projectDir, composebundle.Limits{})
	} else {
		bundle, err = composebundle.PackHTML(html)
		composition = ""
	}
	if err != nil {
		return nil, err
	}
	if max := cfg.EffectiveComposeBundleMaxBytes(); int64(len(bundle)) > max {
		return nil, fmt.Errorf("the bundle is %d bytes, over fleet_compose_bundle_max_mb (%d MiB); the node's own cap may differ", len(bundle), max>>20)
	}
	tmp, err := os.MkdirTemp("", "compose-bundle-check-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := composebundle.Extract(bundle, tmp, composebundle.Limits{}); err != nil {
		return nil, err
	}
	if err := composebundle.Confine(tmp, composition); err != nil {
		return nil, err
	}
	return bundle, nil
}

func newJobID() (string, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	return "compose-" + hex.EncodeToString(rnd[:]), nil
}

// pickNode probes delegate_remotes and returns the eligible node with the shortest queue (config
// order breaks ties). Every miss is named in the error.
func pickNode(ctx context.Context, cfg config.Config, task string) (base, node string, err error) {
	type cand struct {
		base, node string
		queue, i   int
	}
	var (
		cands  []cand
		misses []string
	)
	for i, b := range cfg.DelegateRemotes {
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
		if !contains(v.Tasks, task) {
			misses = append(misses, fmt.Sprintf("%s (%s): does not serve %s", b, v.NodeID, task))
			continue
		}
		id := v.NodeID
		if id == "" {
			id = b
		}
		cands = append(cands, cand{b, id, v.QueueDepth + v.JobsRunning, i})
	}
	if len(cands) == 0 {
		why := "no fleet node serves " + task
		if task == taskProject {
			why += " (a node opens that door with fleet_compose_projects and a fleet_auth_token)"
		}
		return "", "", &placementError{core.DeferClassCapacity, why + " — probed " + strings.Join(misses, "; ")}
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].queue != cands[b].queue {
			return cands[a].queue < cands[b].queue
		}
		return cands[a].i < cands[b].i
	})
	return cands[0].base, cands[0].node, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
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

// wait polls the job until it is done or errored. A done media job's data is the pipeline's result
// object; an errored one carries the node's typed reason, which comes back as a deferred result.
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

// fetchOutputs downloads the video and every snapshot the node reported and returns the result object
// with those paths rewritten to the local copies. A file lands at `out` when the caller named one
// (snapshots beside it), else in this box's media dir under the node's file name.
func fetchOutputs(ctx context.Context, cfg config.Config, base string, data json.RawMessage, out string) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("the node's result is not an object: %w", err)
	}
	remote, _ := m["video_path"].(string)
	if remote == "" {
		return nil, errors.New("the node's result names no video_path")
	}
	name, err := nodeName(remote)
	if err != nil {
		return nil, err
	}
	dir := cfg.MediaDir
	video := filepath.Join(dir, name)
	if out != "" {
		video = out
		dir = filepath.Dir(out)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := download(ctx, cfg, base, name, video); err != nil {
		return nil, err
	}
	m["video_path"] = video
	if snaps, ok := m["snapshots"].([]any); ok {
		local := make([]any, 0, len(snaps))
		for _, s := range snaps {
			rs, _ := s.(string)
			if rs == "" {
				continue
			}
			sn, err := nodeName(rs)
			if err != nil {
				return nil, err
			}
			dst := filepath.Join(dir, sn)
			if err := download(ctx, cfg, base, sn, dst); err != nil {
				return nil, err
			}
			local = append(local, dst)
		}
		m["snapshots"] = local
	}
	return json.Marshal(m)
}

// nodeName is the bare file name of a path the node reported, whichever OS wrote it. The path is the
// node's, but the name becomes a file in this machine's media dir, so it must be a plain name: never
// ".", "..", a root, or a name with a colon (a drive or an NTFS stream).
func nodeName(p string) (string, error) {
	n := path.Base(strings.ReplaceAll(p, `\`, "/"))
	if n == "." || n == ".." || n == "/" || strings.ContainsAny(n, ":\x00") {
		return "", fmt.Errorf("the node named an output %q that is not a plain file name", p)
	}
	return n, nil
}

func download(ctx context.Context, cfg config.Config, base, name, dst string) error {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, base+"/fleet/media/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	auth(cfg, req)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s: status %d: %s", name, resp.StatusCode, truncate(b))
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("GET %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
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
