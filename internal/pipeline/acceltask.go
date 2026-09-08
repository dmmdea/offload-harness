package pipeline

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
)

// runAccelTask executes one fleet "accel" job: the named tool on the named
// LOCAL lane (fleetnode.buildAccel owns the payload and the job dir). Only
// local lanes are eligible — a node never forwards a forwarded call — and a
// lane the node does not list is a defer, not an error: the caller's placement
// probe was wrong or stale, and the caller does the work another way.
//
// A file the tool writes inside the job dir (the Coral's semantic mask) is
// unreadable by the caller once the job is cleaned up, so its bytes ride back
// in the result as mask_b64 (cap core.AccelImageCap).
func (p *Pipeline) runAccelTask(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	id, _ := req.Params["accelerator"].(string)
	tool, _ := req.Params["tool"].(string)
	args, _ := req.Params["args"].(map[string]any)
	jobDir, _ := req.Params["job_dir"].(string)
	meta.Model = id
	finish := func(r core.Result) core.Result {
		r.Meta.LatencyMs = time.Since(start).Milliseconds()
		return r
	}
	var lane *agent.AccelLane
	for _, l := range localAccelLanes(p.cfg) {
		if l.ID == id {
			ll := l
			lane = &ll
			break
		}
	}
	if lane == nil {
		return finish(core.Deferf(fmt.Sprintf("accel: %s is not a local lane on this node (accelerators: %v)", id, p.cfg.Accelerators), "", meta))
	}
	if args == nil {
		args = map[string]any{}
	}
	out, err := lane.Call(ctx, tool, args)
	if err != nil {
		return finish(core.Deferf("accel: "+id+": "+err.Error(), "", meta))
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil || m == nil {
		return finish(core.Deferf("accel: "+id+" returned a non-object result", out, meta))
	}
	if d, _ := m["deferred"].(bool); d {
		reason, _ := m["reason"].(string)
		return finish(core.Deferf(reason, "", meta))
	}
	if mp, ok := m["mask_path"].(string); ok && jobDir != "" {
		if abs, aerr := filepath.Abs(mp); aerr == nil && strings.HasPrefix(abs, filepath.Clean(jobDir)+string(filepath.Separator)) {
			if b, rerr := os.ReadFile(abs); rerr == nil && len(b) <= core.AccelImageCap {
				m["mask_b64"] = base64.StdEncoding.EncodeToString(b)
				m["mask_name"] = filepath.Base(abs)
			}
		}
	}
	m["accelerator"] = id
	m["tool"] = tool
	b, merr := json.Marshal(m)
	if merr != nil {
		return finish(core.Deferf("accel: non-serializable result: "+merr.Error(), "", meta))
	}
	return finish(core.Result{OK: true, Data: b, Meta: meta})
}
