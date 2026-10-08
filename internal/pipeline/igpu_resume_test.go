package pipeline

// The iGPU lanes (CT-49: sd.cpp video and animate, audio.cpp voice and music) take their media lease in one
// place, runIGPU, and it must take it the way the sdcpp image lane does: for the whole node, with the request's
// waiter_token and with its door's resumability. A caller from a door that can resume (the MCP server) that finds
// the node held is handed a place in line and takes it back with the token; a caller from a door that cannot
// (the CLI, a fleet dispatch) gets the plain busy answer and leaves no place behind.
//
// The merge of the media-remote and iGPU branches fixed runIGPU to do this (it had taken a bare whole-node
// lease), and nothing pinned it: TestEveryMediaDoorThreadsTheRequestsResumability scanned pipeline.go, and the
// iGPU lanes live in igpumedia.go. Replacing the need with wholeNeed("") and, separately, dropping
// .resumableBy(req) both left the pipeline, mcpserver and mediaremote suites green (release 0.173.0 review, REL3).

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// useLane makes lane (one of the iGPU lane configs the other tests build) the fixture's config, keeping the
// fixture's own lease host: the scratch state root with its green reader audit, the three-card table, the card-scoped
// switch and the wait window.
func (f *admitFixture) useLane(lane config.Config) {
	f.t.Helper()
	fx := f.p.cfg
	lane.StateDir, lane.GPULockPath = fx.StateDir, fx.GPULockPath
	lane.GPUCardScopedLeases, lane.GPUComfyOrder, lane.GPUWaitMs = fx.GPUCardScopedLeases, fx.GPUComfyOrder, fx.GPUWaitMs
	lane.Endpoint, lane.Layers, lane.ComfyDir = fx.Endpoint, fx.Layers, fx.ComfyDir
	f.cfg, f.p.cfg = lane, lane
}

// run starts one call in the background through a door that can (or cannot) resume a place in line.
func (f *admitFixture) run(req core.Request, resumable bool) <-chan core.Result {
	f.t.Helper()
	req.Resumable = resumable
	ch := make(chan core.Result, 1)
	go func() { ch <- f.p.Run(context.Background(), req) }()
	return ch
}

// igpuResumeLanes are the three runIGPU callers: the lane config, and the request for it (extra is merged into
// the params, which is where a resumed call carries its waiter_token).
var igpuResumeLanes = []struct {
	name string
	cfg  func(t *testing.T, dir string) config.Config
	req  func(dir string, extra map[string]any) core.Request
}{
	{"sdcpp video", sdcppVideoCfg, func(dir string, extra map[string]any) core.Request {
		return videoReq(dir, mergeParams(extra))
	}},
	{"sdcpp animate", animateCfg, func(dir string, extra map[string]any) core.Request {
		return animateReq(dir, mergeParams(extra))
	}},
	{"audio.cpp voice", audiocppCfg, func(dir string, extra map[string]any) core.Request {
		return core.Request{Task: core.TaskGenerateAudio, Input: "hola mundo",
			Params: mergeParams(extra, "kind", "voice", "seed", 5, "out", filepath.Join(dir, "v.wav"))}
	}},
}

// mergeParams builds a params map from extra and the key/value pairs after it.
func mergeParams(extra map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range extra {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func TestAnIGPULaneKeepsAPlaceInLineAndResumesIt(t *testing.T) {
	for _, lane := range igpuResumeLanes {
		t.Run(lane.name, func(t *testing.T) {
			f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
			f.useLane(lane.cfg(t, f.dir))
			holder, err := f.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another job", TTL: time.Hour, Devices: []string{leaseIDOf(admitUUIDA)}})
			if err != nil {
				t.Fatal(err)
			}
			released := false
			defer func() {
				if !released {
					_ = holder.Release()
				}
			}()

			// A door that can resume: the node is held, so the answer is a place in line with a token.
			res := f.await(f.run(lane.req(f.dir, nil), true))
			if res.OK || res.Meta.ErrClass != "gpu_queued" {
				t.Fatalf("a resumable call on a held node must be queued with a place, got ok=%v class=%q: %s", res.OK, res.Meta.ErrClass, res.Reason)
			}
			token := queuedData(t, res).Token
			if !strings.HasPrefix(token, "tk-") {
				t.Fatalf("the queued answer carries no waiter token: %q", token)
			}
			if _, ok := f.m.ResumeToken(token); !ok {
				t.Fatal("the token must be on disk, resumable")
			}

			// A door that cannot resume: the plain busy answer, and no second place left behind.
			busyNotQueued(t, f.await(f.run(lane.req(f.dir, nil), false)))
			if n := len(f.m.Tokens()); n != 1 {
				t.Fatalf("%d token(s) on disk after a call that can never resume one, want only the first call's", n)
			}

			// The node frees, and the token resumes the place: the lane runs and the place is spent.
			released = true
			if err := holder.Release(); err != nil {
				t.Fatal(err)
			}
			again := f.await(f.run(lane.req(f.dir, map[string]any{"waiter_token": token}), true))
			if !again.OK {
				t.Fatalf("the resumed call must run once the node is free, got class=%q: %s", again.Meta.ErrClass, again.Reason)
			}
			if _, ok := f.m.ResumeToken(token); ok {
				t.Error("a token whose call ran must be dropped")
			}
			if toks := f.m.Tokens(); len(toks) != 0 {
				t.Errorf("tokens left over: %+v", toks)
			}
		})
	}
}
