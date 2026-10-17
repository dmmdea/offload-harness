package fleetnode

// What /fleet/health says to a caller that does not hold the bearer. Health stays open on a node that has a
// token (a roster read has to work before it holds a credential, and a cascade lane reads it to learn
// whether a base is a fleet node), so it may carry only what describes the node: its identity, its
// versions, its capacity and its counts. Free text an operator typed, a path on the node's disk and the
// weight files its bindings name are for the holder of the bearer.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/storesteward"
)

const (
	healthLeaseReason = "arm B window"
	healthStoreRoot   = "/srv/kv"
)

func healthOpts(loopback bool) *Options {
	opts := leasedOpts(textLease())
	opts.LoopbackListener = loopback
	opts.Store = func() storesteward.Status {
		return storesteward.Status{Root: healthStoreRoot, UsedGB: 61.2, CapGB: 100, Files: 1203, Error: "cannot read " + healthStoreRoot}
	}
	return opts
}

func TestHealthWithoutTheBearerCarriesNoReasonPathOrWeightName(t *testing.T) {
	s, _ := newTestServer(t, tokenCfg("s3cret"), &fakeRunner{}, healthOpts(false))
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health without the bearer = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leaked := range []string{healthLeaseReason, healthStoreRoot} {
		if strings.Contains(body, leaked) {
			t.Errorf("health without the bearer carries %q: %s", leaked, body)
		}
	}
	m := decodeMap(t, rec)
	if lease, ok := m["lease"].(map[string]any); !ok {
		t.Errorf("the lease block (held, class, window) is a count and a state and must stay: %s", body)
	} else if _, has := lease["reason"]; has {
		t.Errorf("lease.reason is the operator's free text and must not be served without the bearer: %v", lease)
	}
	for _, key := range []string{"store", "image_recipes", "media_recipes"} {
		if _, has := m[key]; has {
			t.Errorf("health without the bearer carries %q", key)
		}
	}
	for _, key := range []string{"node_id", "supported_task_types", "queue_depth", "vram_total_gb", "saturation"} {
		if _, has := m[key]; !has {
			t.Errorf("health without the bearer lost %q, which describes the node and routes work", key)
		}
	}
}

func TestHealthWithTheBearerIsTheFullView(t *testing.T) {
	s, _ := newTestServer(t, tokenCfg("s3cret"), &fakeRunner{}, healthOpts(false))
	rec := do(t, s, http.MethodGet, "/fleet/health", "", map[string]string{"Authorization": "Bearer s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("health with the bearer = %d (body %s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	lease, _ := m["lease"].(map[string]any)
	if lease["reason"] != healthLeaseReason {
		t.Errorf("lease.reason = %v, want %q for a holder of the bearer", lease["reason"], healthLeaseReason)
	}
	store, _ := m["store"].(map[string]any)
	if store["root"] != healthStoreRoot {
		t.Errorf("store.root = %v, want %q for a holder of the bearer", store["root"], healthStoreRoot)
	}
	// A wrong bearer is a stranger.
	rec = do(t, s, http.MethodGet, "/fleet/health", "", map[string]string{"Authorization": "Bearer nope"})
	if strings.Contains(rec.Body.String(), healthStoreRoot) {
		t.Errorf("a wrong bearer read the store root: %s", rec.Body.String())
	}
}

func TestATokenlessLoopbackNodeKeepsItsFullHealth(t *testing.T) {
	s, _ := newTestServer(t, tokenCfg(""), &fakeRunner{}, healthOpts(true))
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if !strings.Contains(rec.Body.String(), healthLeaseReason) || !strings.Contains(rec.Body.String(), healthStoreRoot) {
		t.Errorf("a loopback node with no token is the local trust boundary and serves its full health: %s", rec.Body.String())
	}
}

// The weight-file rows are dropped from the stranger's view by the same function that drops the rest.
func TestTheStrangersViewDropsEveryPrivateField(t *testing.T) {
	full := healthPayload{
		NodeID:       "n",
		ImageRecipes: []ImageRecipeHealth{{Name: "default"}},
		MediaRecipes: []MediaRecipeHealth{{Lane: "video-gen", Files: []mediacap.RecipeFile{{Name: "wan/high.safetensors"}}}},
		Lease:        &LeaseHealth{Held: true, Reason: healthLeaseReason},
		Store:        &storesteward.Status{Root: healthStoreRoot},
	}
	view := full.withoutPrivateFields()
	if len(view.ImageRecipes) != 0 || len(view.MediaRecipes) != 0 || view.Store != nil {
		t.Errorf("the stranger's view kept private rows: %+v", view)
	}
	if view.Lease == nil || view.Lease.Reason != "" || !view.Lease.Held {
		t.Errorf("the stranger's lease = %+v, want held with no reason", view.Lease)
	}
	if full.Lease.Reason != healthLeaseReason || full.Store == nil || len(full.ImageRecipes) != 1 {
		t.Errorf("building the stranger's view changed the full one: %+v", full)
	}
	if view.NodeID != "n" {
		t.Errorf("the stranger's view lost the node id")
	}
}

// A rendered artifact is the node's output: on a node with a token it is read with the bearer, whatever
// task made it (before, only the outputs of the token-gated lanes were).
func TestMediaFilesNeedTheBearerOnATokenedNode(t *testing.T) {
	mediaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(mediaDir, "render-x.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := tokenCfg("s3cret")
	cfg.MediaDir = mediaDir
	s, _ := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))
	if rec := do(t, s, http.MethodGet, "/fleet/media/render-x.png", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("media file with no bearer = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, http.MethodGet, "/fleet/media/render-x.png", "", map[string]string{"Authorization": "Bearer s3cret"}); rec.Code != http.StatusOK || rec.Body.String() != "png" {
		t.Errorf("media file with the bearer = %d %q, want 200 with the bytes", rec.Code, rec.Body.String())
	}
	// A node with no token keeps serving its media as it always did.
	cfg.FleetAuthToken = ""
	s2, _ := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))
	if rec := do(t, s2, http.MethodGet, "/fleet/media/render-x.png", "", nil); rec.Code != http.StatusOK {
		t.Errorf("tokenless loopback media file = %d, want 200", rec.Code)
	}
}
