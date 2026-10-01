package mcpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// tools/list changed on EVERY box in 0.154.0: offload_classify and offload_extract gained a
// `route` property (the vision tools' shape). This pins that change deliberately, and pins that
// summarize and triage did NOT gain it: they are never routable to a node's text lane.
func TestTextToolsAdvertiseTheRoute(t *testing.T) {
	want := map[string]bool{"offload_classify": true, "offload_extract": true, "offload_summarize": false, "offload_triage": false}
	seen := 0
	for _, tool := range listTools(t, config.Default()) {
		expect, care := want[tool.Name]
		if !care {
			continue
		}
		seen++
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: schema not JSON: %v", tool.Name, err)
		}
		route, has := schema.Properties["route"]
		if has != expect {
			t.Fatalf("%s route param present = %v, want %v", tool.Name, has, expect)
		}
		if has && strings.Join(route.Enum, ",") != "local,auto,remote" {
			t.Fatalf("%s route enum = %v", tool.Name, route.Enum)
		}
		for _, r := range schema.Required {
			if r == "route" {
				t.Fatalf("%s: route must stay optional, every existing caller omits it", tool.Name)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d of %d text tools", seen, len(want))
	}
}

// An unknown route and route remote with no remotes are defers produced BEFORE the pipeline runs.
func TestTextRouteRefusalsNeverRunThePipeline(t *testing.T) {
	s := New(pipeline.New(config.Default(), nil, nil, nil))
	txt := callVision(t, s.handleClassify, map[string]any{"text": "an overdue invoice", "labels": []string{"a", "b"}, "route": "cloud"})
	if !strings.Contains(txt, `"deferred":true`) || !strings.Contains(txt, `"defer_class":"contract"`) || !strings.Contains(txt, "unrecognized route") {
		t.Fatalf("unknown route: %s", txt)
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}
	txt = callVision(t, s.handleExtract, map[string]any{"text": "Ada Lovelace", "schema": schema, "route": "remote"})
	if !strings.Contains(txt, `"deferred":true`) || !strings.Contains(txt, `"defer_class":"config"`) || !strings.Contains(txt, "delegate_remotes") {
		t.Fatalf("remote without remotes: %s", txt)
	}
}

// route omitted, "" and local all run the local cascade and carry no placement or node: the
// result is what offload_classify returned before the route existed.
func TestTextDefaultRouteRunsLocalUnstamped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": `{"label":"billing","confidence":0.97}`}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 40, "completion_tokens": 8},
		})
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.Model, cfg.TriageModel, cfg.EscalationModel, cfg.ReasoningModel = "m", "m", "", ""
	cfg.ThresholdsPath, cfg.RouterWeightsPath, cfg.TierOverridesPath, cfg.ConfHeadLabelsPath, cfg.CachePath = "", "", "", "", ""
	cfg.LedgerPath = ""
	s := New(pipeline.New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil))
	args := func(extra map[string]any) map[string]any {
		m := map[string]any{"text": "Invoice 4471 is overdue by 10 days, what is the late fee?", "labels": []string{"billing", "support"}}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for _, route := range []any{nil, "", "local"} {
		extra := map[string]any{}
		if route != nil {
			extra["route"] = route
		}
		txt := callVision(t, s.handleClassify, args(extra))
		if !strings.Contains(txt, `"label":"billing"`) {
			t.Fatalf("route %v: %s", route, txt)
		}
		if strings.Contains(txt, "placement") || strings.Contains(txt, `"node"`) {
			t.Fatalf("route %v: a local run must carry no placement or node: %s", route, txt)
		}
	}
}
