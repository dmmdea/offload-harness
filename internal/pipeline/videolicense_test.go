package pipeline

// CT-47: a video render is tagged with its family's license exactly as an image
// render is (ADR 0058, operator order 2026-10-01: license and commercial_use only,
// no warning text). The tag follows the family that ACTUALLY rendered, not the seat.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func videoLicenseCfg(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VideoGenScript = writeStub(t, dir)
	cfg.MediaDir = dir
	cfg.VideoGenFamily = "ltx25"
	no, yes := false, true
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{
		"hunyuan": {License: "Example Community License", CommercialUse: &no},
		"ltx25":   {License: "Example Open License", CommercialUse: &yes},
	}
	return cfg
}

func TestGenerateVideoResultCarriesTheRenderingFamilysLicense(t *testing.T) {
	requireNodePipeline(t)
	cfg := videoLicenseCfg(t)
	p := footprintTestPipeline(t, cfg, 5.5)

	// An override renders hunyuan on an ltx25 seat: the tag is hunyuan's, not the seat's.
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "a calm ocean at dawn",
		Params: map[string]any{"model": "hunyuan"}})
	if !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	var payload map[string]any
	if err := json.Unmarshal(res.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["license"] != "Example Community License" || payload["commercial_use"] != false || hasJSONKey(payload, "license_note") {
		t.Errorf("hunyuan payload = %v, want license + commercial_use:false and no license_note", payload)
	}
	if res.Meta.License != "Example Community License" {
		t.Errorf("meta.License = %q, want the hunyuan license (it is what the ledger row records)", res.Meta.License)
	}

	// The seated family (no override) is tagged with its own pair, including true.
	res = p.Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "a calm ocean at dawn"})
	if !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	payload = map[string]any{}
	if err := json.Unmarshal(res.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["license"] != "Example Open License" || payload["commercial_use"] != true || res.Meta.License != "Example Open License" {
		t.Errorf("seated ltx25 payload = %v meta.License = %q", payload, res.Meta.License)
	}
}

// A family that declares no license adds neither key: the result is byte-identical to
// what it was before the fields existed, and a reader treats the absence as UNKNOWN.
func TestGenerateVideoUndeclaredFamilyAddsNoLicenseKeys(t *testing.T) {
	requireNodePipeline(t)
	cfg := videoLicenseCfg(t)
	cfg.VideoGenFamilies = nil
	p := footprintTestPipeline(t, cfg, 5.5)
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "a calm ocean at dawn"})
	if !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	var payload map[string]any
	if err := json.Unmarshal(res.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if hasJSONKey(payload, "license") || hasJSONKey(payload, "commercial_use") || res.Meta.License != "" {
		t.Errorf("an undeclared family must carry no license keys: %v meta.License=%q", payload, res.Meta.License)
	}
}
