package mediaremote

// Fixtures for the overflow tests (ADR 0082): real fleet nodes (the server, its doors, its job store, its recipe
// advertisement) whose ComfyUI tree holds the weight files of the families they bind, a delegator that has the same
// files, and a runner that stands in for the delegator's pipeline and says whether its lane is free.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// treeFiles are the weight files every fixture machine holds, at one size per name: a copy of a file is the same file.
var treeFiles = map[string]int{
	"models/diffusion_models/krea2_turbo_bf16.safetensors":               111,
	"models/checkpoints/hidream_o1_image_bf16.safetensors":               333,
	"models/diffusion_models/qwen_image_2.1_uc_bf16.safetensors":         222,
	"models/diffusion_models/qwen-image-2.1-UC-int8_convrot.safetensors": 177,
	"models/diffusion_models/qwen-image-2.1-UC-NVFP4.safetensors":        99,
	"models/text_encoders/qwen3vl_8b_bf16.safetensors":                   88,
	"models/text_encoders/qwen3vl_4b_bf16.safetensors":                   44,
	"models/vae/qwen_image_2.1_vae_bf16.safetensors":                     6,
	"models/vae/qwen_image_vae.safetensors":                              5,
}

// comfyTree writes treeFiles into a fresh directory and returns it.
func comfyTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for rel, n := range treeFiles {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// block is a Qwen-Image-2.1 family overlay over the given checkpoint, in the shape the live nodes' blocks have; extra is
// spliced in as more JSON members ("" for none).
func block(ckpt, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return `{"license":"Qwen Research License","commercial_use":false,"imagegen_family":"qwen-image-2.1",
		"imagegen_ckpt":"` + ckpt + `","imagegen_clip":"qwen3vl_8b_bf16.safetensors",
		"imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors","imagegen_steps":40,"imagegen_cfg":1,"imagegen_sampler":"euler",
		"imagegen_schedule":"official"` + extra + `}`
}

const (
	bf16Ckpt = "qwen_image_2.1_uc_bf16.safetensors"
	int8Ckpt = "qwen-image-2.1-UC-int8_convrot.safetensors"
	fp4Ckpt  = "qwen-image-2.1-UC-NVFP4.safetensors"
)

// binding binds a ComfyUI tree, a default family and named families (name -> overlay JSON) on a config.
func binding(tree, defaultFamily string, families map[string]string) func(*config.Config) {
	return func(c *config.Config) {
		c.ComfyDir = tree
		c.ImageGenScript = "render/comfy-generate.mjs"
		switch defaultFamily {
		case "krea2":
			c.ImageGenFamily, c.ImageGenCkpt, c.ImageGenVAE = "krea2", "krea2_turbo_bf16.safetensors", "qwen_image_vae.safetensors"
			c.ImageGenSteps, c.ImageGenCFG = 8, 1
		case "hidream-o1":
			c.ImageGenFamily, c.ImageGenCkpt, c.ImageGenVAE = "hidream-o1", "hidream_o1_image_bf16.safetensors", "builtin"
		case "qwen-image-2.1-int8":
			// the default binding IS the int8 build (a box whose house family is the 2.1 one)
			c.ImageGenFamily, c.ImageGenCkpt, c.ImageGenCLIP, c.ImageGenVAE = "qwen-image-2.1", int8Ckpt, "qwen3vl_8b_bf16.safetensors", "qwen_image_2.1_vae_bf16.safetensors"
			c.ImageGenSteps, c.ImageGenCFG, c.ImageGenSampler, c.ImageGenSchedule = 40, 1, "euler", "official"
			c.ImageGenLicense, c.ImageGenCommercialUse = "Qwen Research License", boolPtr(false)
		}
		c.ImageGenFamilies = map[string]config.FamilyOverlay{}
		for name, js := range families {
			var ov config.FamilyOverlay
			if err := json.Unmarshal([]byte(js), &ov); err != nil {
				panic(err)
			}
			c.ImageGenFamilies[name] = ov
		}
	}
}

func boolPtr(b bool) *bool { return &b }

// useVersion makes this machine's release the one the fixture nodes publish.
const fixtureVersion = "0.179.0-test"

func useVersion(t *testing.T) {
	t.Helper()
	prev := localVersion
	localVersion = fixtureVersion
	t.Cleanup(func() { localVersion = prev })
}

// startImageNode starts a fleet node with the given image binding.
func startImageNode(t *testing.T, id, defaultFamily string, families map[string]string, o nodeOpts) *node {
	t.Helper()
	o.id, o.version = id, fixtureVersion
	tree := comfyTree(t)
	bind := binding(tree, defaultFamily, families)
	prev := o.cfg
	o.cfg = func(c *config.Config) {
		bind(c)
		if prev != nil {
			prev(c)
		}
	}
	return startNode(t, o)
}

// overflowClient is a delegator that holds the same files as its nodes and binds the given default and families. Its
// image lane exists (the route derivation is stubbed to say so for the default and every family), so route auto
// considers it before the fleet.
func overflowClient(t *testing.T, defaultFamily string, families map[string]string, nodes ...*node) config.Config {
	t.Helper()
	useVersion(t)
	resetPlacer()
	t.Cleanup(resetPlacer)
	cfg := clientCfg(t, nodes...)
	binding(comfyTree(t), defaultFamily, families)(&cfg)
	names := []string{"generate_image"}
	for name := range families {
		names = append(names, mediacap.ImageFamilyRoute(name))
	}
	stubLocalRoutes(t, configured(names...))
	return cfg
}

// laneRunner stands in for the delegator's pipeline: it answers the lane question from verdict, counts what it was
// asked and what it ran, and hands out the rig's attribution handle when it has one.
type laneRunner struct {
	mu      sync.Mutex
	verdict core.LaneVerdict
	probes  int
	probed  []core.Request
	runs    int
	local   core.Result
	rig     *pairRig
}

func (l *laneRunner) MediaLaneFree(_ context.Context, req core.Request) core.LaneVerdict {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.probes++
	l.probed = append(l.probed, req)
	return l.verdict
}

func (l *laneRunner) Run(context.Context, core.Request) core.Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs++
	if l.local.OK || l.local.Deferred {
		return l.local
	}
	return core.Result{OK: true, Data: json.RawMessage(`{"local":true}`)}
}

func (l *laneRunner) BeginRemote(req core.Request, route string) core.RemoteAttribution {
	if l.rig == nil {
		return nil
	}
	return l.rig.p.BeginRemote(req, route)
}

func (l *laneRunner) counts() (probes, runs int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.probes, l.runs
}

// heldLane is the verdict of a lane a bench render holds.
func heldLane() core.LaneVerdict {
	return core.LaneVerdict{
		Free:    false,
		Why:     `card(s) c2 held by a media-class lease ("bench render of the spring set"), about 300s of its declared term left`,
		Holders: []core.LaneHolder{{Epoch: 41, Class: "media", Reason: "bench render of the spring set", RemainingSec: 300, Devices: []string{"c2"}}},
	}
}

// queuedLocal is what the pipeline answers when the lane stays busy for the whole wait: a place in line.
func queuedLocal() core.Result {
	res := core.Deferf("gpu queued: card(s) c2 held by a media-class lease (\"bench render of the spring set\"); your place in line is #1 (token tk-abc12345), at most 300s until the lease(s) in the way end; call again with waiter_token=tk-abc12345 to keep it", "",
		core.Meta{ErrClass: core.ErrClassGPUQueued})
	res.DeferClass = core.DeferClassCapacity
	res.Data = json.RawMessage(`{"queued":true,"waiter_token":"tk-abc12345","queue_position":1,"eta_s":300,"devices":["c2"]}`)
	return res
}

func overflowReq(t *testing.T, family string, extra map[string]any) core.Request {
	t.Helper()
	params := map[string]any{"out": filepath.Join(t.TempDir(), "door.png"), "seed": 424242, "width": 2048, "height": 2048}
	if family != "" {
		params["family"] = family
	}
	for k, v := range extra {
		params[k] = v
	}
	return core.Request{Task: core.TaskGenerateImage, Door: "offload_generate_image", Input: "a red door", Params: params, Resumable: true}
}

// posted decodes the dispatch the node received: the job id it carried and its payload.
func posted(t *testing.T, n *node) (jobID string, payload map[string]any) {
	t.Helper()
	ps := n.posts()
	if len(ps) == 0 {
		t.Fatal("the node received no POST")
	}
	var wire struct {
		JobID   string         `json:"job_id"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(ps[len(ps)-1].body, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.JobID, wire.Payload
}

// jobIDs lists the job id of every POST the node received.
func jobIDs(t *testing.T, n *node) []string {
	t.Helper()
	var out []string
	for _, p := range n.posts() {
		var wire struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(p.body, &wire); err != nil {
			t.Fatal(err)
		}
		out = append(out, wire.JobID)
	}
	return out
}

// untouched asserts the node saw no request at all (not even a health read).
func untouched(t *testing.T, name string, n *node) {
	t.Helper()
	if got := n.requests(); len(got) != 0 {
		t.Errorf("%s was contacted: %+v", name, got)
	}
}

// noPosts asserts the node was read but never sent a job.
func noPosts(t *testing.T, name string, n *node) {
	t.Helper()
	if got := n.posts(); len(got) != 0 {
		t.Errorf("%s was sent a job: %d POST(s)", name, len(got))
	}
}

// leased is a node lease reader that reports a held media lease with a reason and 5 minutes left.
func leased() func() gpulease.Info {
	return func() gpulease.Info {
		return gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 9, PID: 4242, Reason: "a bench render", ExpiresAt: time.Now().Add(5 * time.Minute)}
	}
}

// without removes keys from the health body a node serves.
func without(keys ...string) func(map[string]any) {
	return func(m map[string]any) {
		for _, k := range keys {
			delete(m, k)
		}
	}
}

// clusterOf decodes the cluster[] block of a result's data.
func clusterOf(t *testing.T, res core.Result) []ClusterRow {
	t.Helper()
	var d struct {
		Cluster []ClusterRow `json:"cluster"`
	}
	if err := json.Unmarshal(res.Data, &d); err != nil {
		t.Fatalf("result data %s: %v", res.Data, err)
	}
	return d.Cluster
}

func rowFor(rows []ClusterRow, nodeSubstr string) (ClusterRow, bool) {
	for _, r := range rows {
		if strings.Contains(r.Node, nodeSubstr) {
			return r, true
		}
	}
	return ClusterRow{}, false
}
