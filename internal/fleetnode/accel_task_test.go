package fleetnode

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func accelCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Accelerators = []string{"coral-edgetpu"}
	cfg.Home = t.TempDir() // BaseDir() = Home; keep job dirs in the test's tmp
	return cfg
}

func TestAccelTaskIsServedExactlyWhenADeviceIsListed(t *testing.T) {
	off := config.Default()
	for _, tt := range SupportedTasksFor(off, true) {
		if tt == "accel" {
			t.Fatal("accel advertised with no accelerator listed")
		}
	}
	on := accelCfg(t)
	found := false
	for _, tt := range SupportedTasksFor(on, true) {
		if tt == "accel" {
			found = true
		}
	}
	if !found {
		t.Fatalf("accel not advertised with an accelerator listed: %v", SupportedTasksFor(on, true))
	}
}

func TestBuildAccelWritesTheShippedImageIntoTheJobDir(t *testing.T) {
	cfg := accelCfg(t)
	payload, _ := json.Marshal(map[string]any{
		"accelerator": "coral-edgetpu", "tool": "classify",
		"args":      map[string]any{"domain": "birds", "image_path": "/caller/box/parrot.jpg", "out_path": "/caller/box/mask.png"},
		"image_b64": base64.StdEncoding.EncodeToString([]byte("jpeg bytes")), "image_name": "parrot.jpg",
	})
	req, cleanup, err := BuildRequest(t.Context(), cfg, true, "accel", payload)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if req.Task != core.TaskAccel || req.Params["accelerator"] != "coral-edgetpu" || req.Params["tool"] != "classify" {
		t.Fatalf("request = %+v", req)
	}
	args := req.Params["args"].(map[string]any)
	jobDir := req.Params["job_dir"].(string)
	ip, _ := args["image_path"].(string)
	if filepath.Dir(ip) != jobDir || filepath.Base(ip) != "parrot.jpg" {
		t.Fatalf("image_path %q not inside job dir %q", ip, jobDir)
	}
	if b, rerr := os.ReadFile(ip); rerr != nil || string(b) != "jpeg bytes" {
		t.Fatalf("shipped image not written: %v %q", rerr, b)
	}
	if _, leaked := args["out_path"]; leaked {
		t.Fatal("caller-side out_path survived into the node's args")
	}
	if args["domain"] != "birds" {
		t.Fatalf("args = %v", args)
	}
	cleanup()
	if _, serr := os.Stat(jobDir); !os.IsNotExist(serr) {
		t.Fatal("cleanup left the job dir behind")
	}
}

func TestBuildAccelRefusals(t *testing.T) {
	cfg := accelCfg(t)
	cases := map[string]map[string]any{
		"unlisted accelerator": {"accelerator": "hailo-8l", "tool": "classify"},
		"missing tool":         {"accelerator": "coral-edgetpu"},
		"bad image name":       {"accelerator": "coral-edgetpu", "tool": "classify", "image_b64": "aGk=", "image_name": "../escape.jpg"},
		"not base64":           {"accelerator": "coral-edgetpu", "tool": "classify", "image_b64": "%%%"},
		"unknown field":        {"accelerator": "coral-edgetpu", "tool": "classify", "surprise": 1},
	}
	for name, p := range cases {
		payload, _ := json.Marshal(p)
		if _, cleanup, err := BuildRequest(t.Context(), cfg, true, "accel", payload); err == nil {
			cleanup()
			t.Errorf("%s: accepted", name)
		}
	}
	// Over the cap: refused, and nothing left on disk.
	big := map[string]any{"accelerator": "coral-edgetpu", "tool": "classify",
		"image_b64": base64.StdEncoding.EncodeToString(make([]byte, core.AccelImageCap+1))}
	payload, _ := json.Marshal(big)
	if _, cleanup, err := BuildRequest(t.Context(), cfg, true, "accel", payload); err == nil {
		cleanup()
		t.Fatal("oversized image accepted")
	} else if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("oversized image: err = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(cfg.BaseDir(), "pipeline-jobs"))
	if len(entries) != 0 {
		t.Fatalf("refused jobs left dirs behind: %v", entries)
	}
}

// accel drives a loopback sidecar, never the text endpoint: it must not hold a
// fleet execution slot behind a five-minute digest contract.
func TestAccelIsNotConcurrencyCapped(t *testing.T) {
	s, _ := newTestServer(t, accelCfg(t), nil, nil)
	if s.concurrencyCapped("accel") {
		t.Fatal("accel counted against fleet_max_concurrent_jobs")
	}
	if !s.concurrencyCapped("agent") {
		t.Fatal("control: agent must stay capped")
	}
}
