package pipeline

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// fakeSidecar speaks the accelerator wire contract: /health, /v1/<tool>.
func fakeSidecar(t *testing.T, handle func(tool string, args map[string]any) map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"enabled": true, "device": "coral-edgetpu", "status": "ALIVE"})
	})
	mux.HandleFunc("POST /v1/{tool}", func(w http.ResponseWriter, r *http.Request) {
		var args map[string]any
		json.NewDecoder(r.Body).Decode(&args)
		json.NewEncoder(w).Encode(handle(r.PathValue("tool"), args))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func accelReq(jobDir string, args map[string]any) core.Request {
	return core.Request{Task: core.TaskAccel, Input: "semantic_segment", Params: map[string]any{
		"accelerator": "coral-edgetpu", "tool": "semantic_segment", "args": args, "job_dir": jobDir}}
}

func TestRunAccelTaskShipsAMaskWrittenInsideTheJobDir(t *testing.T) {
	jobDir := t.TempDir()
	var seen map[string]any
	sc := fakeSidecar(t, func(tool string, args map[string]any) map[string]any {
		seen = args
		mask := filepath.Join(jobDir, "parrot.segmask.png")
		os.WriteFile(mask, []byte("PNG-ish"), 0o644)
		return map[string]any{"mask_path": mask, "classes": []any{"bird"}, "width": 640, "height": 480}
	})
	cfg := config.Default()
	cfg.Accelerators = []string{"coral-edgetpu"}
	cfg.CoralEndpoint = sc.URL + "-" + t.Name() // unique key so the shared sidecar map does not reuse another test's client
	cfg.CoralEndpoint = sc.URL
	p := New(cfg, nil, nil, nil)
	res := p.Run(context.Background(), accelReq(jobDir, map[string]any{"image_path": filepath.Join(jobDir, "parrot.jpg")}))
	if !res.OK || res.Deferred {
		t.Fatalf("result = %+v", res)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out["accelerator"] != "coral-edgetpu" || out["tool"] != "semantic_segment" {
		t.Fatalf("out = %v", out)
	}
	raw, _ := base64.StdEncoding.DecodeString(out["mask_b64"].(string))
	if string(raw) != "PNG-ish" || out["mask_name"] != "parrot.segmask.png" {
		t.Fatalf("mask not shipped: %v", out)
	}
	if seen["image_path"] != filepath.Join(jobDir, "parrot.jpg") {
		t.Fatalf("sidecar saw args %v", seen)
	}
	if res.Meta.Model != "coral-edgetpu" || res.Meta.LatencyMs < 0 {
		t.Fatalf("meta = %+v", res.Meta)
	}
}

func TestRunAccelTaskNeverShipsAFileOutsideTheJobDir(t *testing.T) {
	jobDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.png")
	os.WriteFile(outside, []byte("secret"), 0o644)
	sc := fakeSidecar(t, func(string, map[string]any) map[string]any {
		return map[string]any{"mask_path": outside}
	})
	cfg := config.Default()
	cfg.Accelerators = []string{"coral-edgetpu"}
	cfg.CoralEndpoint = sc.URL
	res := New(cfg, nil, nil, nil).Run(context.Background(), accelReq(jobDir, map[string]any{}))
	var out map[string]any
	json.Unmarshal(res.Data, &out)
	if _, shipped := out["mask_b64"]; shipped {
		t.Fatal("a file outside the job dir was shipped")
	}
}

func TestRunAccelTaskDefersOnAnUnlistedLaneAndOnASidecarDefer(t *testing.T) {
	cfg := config.Default() // no accelerators: nothing local to run on
	res := New(cfg, nil, nil, nil).Run(context.Background(), accelReq(t.TempDir(), map[string]any{}))
	if !res.Deferred || !strings.Contains(res.Reason, "not a local lane") {
		t.Fatalf("unlisted lane: %+v", res)
	}
	// A remote-only lane must NOT be eligible either (no forward-of-a-forward).
	cfg.FleetAccelerators = []string{"coral-edgetpu"}
	cfg.DelegateRemotes = []string{"http://127.0.0.1:1"}
	res = New(cfg, nil, nil, nil).Run(context.Background(), accelReq(t.TempDir(), map[string]any{}))
	if !res.Deferred || !strings.Contains(res.Reason, "not a local lane") {
		t.Fatalf("fleet-only lane: %+v", res)
	}
	// The sidecar's own structured defer passes through as a defer.
	sc := fakeSidecar(t, func(string, map[string]any) map[string]any {
		return map[string]any{"deferred": true, "reason": "model_missing"}
	})
	cfg = config.Default()
	cfg.Accelerators = []string{"coral-edgetpu"}
	cfg.CoralEndpoint = sc.URL
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res = New(cfg, nil, nil, nil).Run(ctx, accelReq(t.TempDir(), map[string]any{}))
	if !res.Deferred || res.Reason != "model_missing" {
		t.Fatalf("sidecar defer: %+v", res)
	}
}
