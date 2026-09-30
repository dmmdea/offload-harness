package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// rknpuRoutes maps each RKNPU tool to the sidecar route it drives. The routes are the contract with
// accelerators/rknpu/server.py (POST /v1/<tool>), so a renamed row would call a route that 404s.
var rknpuRoutes = map[string]string{"offload_classify_image": "classify", "offload_object_detect": "object_detect", "offload_image_embed": "embed"}

// Registration is gated on the device being LISTED, like every accelerator: none of the three tools
// without it, all three with it, and the rest of tools/list byte-identical.
func TestRknpuToolsRegistrationGatedOnAccelerator(t *testing.T) {
	off := listTools(t, config.Default())
	for _, tool := range off {
		if _, ok := rknpuRoutes[tool.Name]; ok {
			t.Fatalf("%s advertised with no accelerator", tool.Name)
		}
	}
	cfgOn := config.Default()
	cfgOn.Accelerators = []string{"rknpu"}
	found := map[string]bool{}
	var stripped []*mcp.Tool
	for _, tool := range listTools(t, cfgOn) {
		if _, ok := rknpuRoutes[tool.Name]; ok {
			found[tool.Name] = true
			continue
		}
		stripped = append(stripped, tool)
	}
	if len(found) != len(rknpuRoutes) {
		t.Fatalf("expected all %d RKNPU tools, found %v", len(rknpuRoutes), found)
	}
	offJSON, _ := json.Marshal(off)
	strippedJSON, _ := json.Marshal(stripped)
	if !bytes.Equal(offJSON, strippedJSON) {
		t.Fatal("the RKNPU accelerator changed the tool list beyond adding its own tools")
	}
}

// Each tool drives its own sidecar route and the sidecar's dict comes back verbatim, with the caller's
// arguments (the optional ones included) reaching the route unchanged. A down sidecar with no launcher
// defers with the DEVICE named — never a tool error.
func TestRknpuToolRoutesPassThroughAndDefer(t *testing.T) {
	var mu sync.Mutex
	received := map[string]map[string]any{}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"enabled":true,"models_missing":[]}`)) })
	for _, route := range rknpuRoutes {
		mux.HandleFunc("/v1/"+route, func(w http.ResponseWriter, r *http.Request) {
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			mu.Lock()
			received[strings.TrimPrefix(r.URL.Path, "/v1/")] = in
			mu.Unlock()
			w.Write([]byte(`{"served_by":"` + strings.TrimPrefix(r.URL.Path, "/v1/") + `"}`))
		})
	}
	fake := httptest.NewServer(mux)
	defer fake.Close()

	cfg := config.Default()
	cfg.Accelerators = []string{"rknpu"}
	cfg.RknpuEndpoint = fake.URL
	s := New(pipeline.New(cfg, nil, nil, nil))
	call := func(s *Server, row accelMCPTool) string {
		res, err := s.handleAccelTool("rknpu", row.sidecar, row.arg)(context.Background(), &mcp.CallToolRequest{
			Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"image_path":"/tmp/x.jpg","top_k":3,"score_threshold":0.4}`)}})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(*mcp.TextContent).Text
	}

	rows := accelMCPTools("rknpu")
	if len(rows) != len(rknpuRoutes) {
		t.Fatalf("the RKNPU table has %d rows, want %d", len(rows), len(rknpuRoutes))
	}
	for _, row := range rows {
		if rknpuRoutes[row.name] != row.sidecar || row.arg != "image_path" {
			t.Errorf("%s maps to sidecar tool %q on arg %q, want %q on image_path", row.name, row.sidecar, row.arg, rknpuRoutes[row.name])
		}
		if text := call(s, row); text != `{"served_by":"`+row.sidecar+`"}` {
			t.Errorf("%s: the sidecar's dict did not pass through verbatim: %s", row.name, text)
		}
		mu.Lock()
		in := received[row.sidecar]
		mu.Unlock()
		if in["image_path"] != "/tmp/x.jpg" || in["top_k"] != float64(3) || in["score_threshold"] != 0.4 {
			t.Errorf("%s: arguments did not reach /v1/%s unchanged: %v", row.name, row.sidecar, in)
		}
	}

	// Down and no rknpu_sidecar_cmd: a defer naming the device.
	fake.Close()
	cfg2 := config.Default()
	cfg2.Accelerators = []string{"rknpu"}
	cfg2.RknpuEndpoint = fake.URL
	cfg2.RknpuSidecarCmd = ""
	if text := call(New(pipeline.New(cfg2, nil, nil, nil)), rows[0]); !strings.Contains(text, `"deferred":true`) || !strings.Contains(text, "rknpu:") {
		t.Fatalf("down sidecar did not defer with the device named: %s", text)
	}
}

// The lane config reads the RKNPU's own four keys — endpoint, launcher, call bound, idle window — and a
// zero timeout falls back to 60 s, like the Hailo's (the Coral's is 30).
func TestRknpuLaneConfigReadsItsOwnKeys(t *testing.T) {
	cfg := config.Default()
	cfg.RknpuEndpoint, cfg.RknpuSidecarCmd, cfg.RknpuTimeoutSec, cfg.RknpuIdleSec = "http://127.0.0.1:1", "/x/rknpu-http.sh", 42, 77
	cfg.HailoEndpoint, cfg.CoralEndpoint = "http://127.0.0.1:2", "http://127.0.0.1:3"
	lc, ok := accelLaneConfigFor(cfg, "rknpu")
	if !ok || lc.endpoint != "http://127.0.0.1:1" || lc.cmd != "/x/rknpu-http.sh" || lc.timeout != 42*time.Second || lc.idleSec != 77 {
		t.Fatalf("lane config = %+v (ok %v)", lc, ok)
	}
	cfg.RknpuTimeoutSec = 0
	if lc, _ := accelLaneConfigFor(cfg, "rknpu"); lc.timeout != 60*time.Second {
		t.Errorf("a zero rknpu_timeout_sec must fall back to 60 s, got %s", lc.timeout)
	}
}

// The status block lists the RKNPU with its lane config, owned capabilities and a live health probe —
// and never spawns the sidecar.
func TestStatusListsRknpu(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"enabled":true,"loaded":[],"models_missing":[],"uptime_sec":4}`))
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()
	cfg := config.Default()
	cfg.Accelerators = []string{"rknpu"}
	cfg.RknpuEndpoint = fake.URL
	st := accelStatus(context.Background(), cfg)
	entry, ok := st["rknpu"].(map[string]any)
	if !ok {
		t.Fatalf("status has no rknpu entry: %v", st)
	}
	if entry["endpoint"] != fake.URL || entry["sidecar_cmd_configured"] != false || !reflect.DeepEqual(entry["owns"], []string{"classify_image", "object_detect", "image_embed"}) {
		t.Errorf("entry = %v", entry)
	}
	if h, ok := entry["health"].(map[string]any); !ok || h["enabled"] != true {
		t.Errorf("live health not reported: %v", entry)
	}
}

// schemaShape is what a caller can rely on in a tool's input schema, wording aside: each property's type
// (and enum) and the required list.
func schemaShape(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s struct {
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("schema %s: %v", raw, err)
	}
	var props []string
	for name, p := range s.Properties {
		props = append(props, name+":"+p.Type+strings.Join(p.Enum, "|"))
	}
	sort.Strings(props)
	sort.Strings(s.Required)
	return strings.Join(props, ",") + " required=" + strings.Join(s.Required, ",")
}

// The agent loop advertises the SAME RKNPU tools as the MCP surface — names and input schemas — and
// every description on both names the device, so a contract landing on the node and a Claude session on
// it see one tool set. The embed description says its space is no other device's.
func TestRknpuLoopAndMCPToolParity(t *testing.T) {
	mcpTools := map[string]accelMCPTool{}
	for _, tl := range accelMCPTools("rknpu") {
		mcpTools[tl.name] = tl
	}
	if len(mcpTools) != len(rknpuRoutes) {
		t.Fatalf("the MCP table has %d RKNPU tools, want %d", len(mcpTools), len(rknpuRoutes))
	}
	lanes := []agent.AccelLane{{ID: "rknpu", Call: func(context.Context, string, map[string]any) (string, error) { return "{}", nil }}}
	loop, err := agent.ReadOnlyToolsWithLanes(t.TempDir(), nil, nil, lanes)
	if err != nil {
		t.Fatal(err)
	}
	loopTools := map[string]agent.Tool{}
	for _, tl := range loop {
		if _, ok := mcpTools[tl.Name]; ok {
			loopTools[tl.Name] = tl
		}
	}
	if len(loopTools) != len(mcpTools) {
		t.Fatalf("loop advertises %d of the %d MCP tools", len(loopTools), len(mcpTools))
	}
	for name, m := range mcpTools {
		l := loopTools[name]
		if got, want := schemaShape(t, l.Schema), schemaShape(t, json.RawMessage(m.schema)); got != want {
			t.Errorf("%s: loop schema %q != MCP schema %q", name, got, want)
		}
		for surface, desc := range map[string]string{"MCP": m.desc, "loop": l.Description} {
			if !strings.Contains(desc, "Rockchip RK3588 NPU") {
				t.Errorf("%s description of %s does not name the device: %s", surface, name, desc)
			}
			if name == "offload_image_embed" && !strings.Contains(desc, "NOT the Hailo's tinyclip") {
				t.Errorf("%s description of %s does not say its space is not the other devices': %s", surface, name, desc)
			}
		}
	}
}

// The loop's RKNPU lane is the pipeline's own table row, so it must drive the sidecar the config names —
// the same one the MCP surface drives — and defer with the device named when it is down.
func TestRknpuLoopLaneReachesTheConfiguredSidecar(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"enabled":true}`)) })
	mux.HandleFunc("/v1/classify", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"best":{"label":"tabby","score":0.9}}`))
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()

	cfg := config.Default()
	cfg.Accelerators = []string{"rknpu"}
	cfg.RknpuEndpoint = fake.URL
	lanes := pipeline.NewLoopAccel(cfg)
	if len(lanes) != 1 || lanes[0].ID != "rknpu" {
		t.Fatalf("lanes = %v, want the one rknpu lane", lanes)
	}
	out, err := lanes[0].Call(context.Background(), "classify", map[string]any{"image_path": "/tmp/x.jpg"})
	if err != nil || !strings.Contains(out, "tabby") {
		t.Fatalf("the loop lane did not reach the configured sidecar: %q, %v", out, err)
	}

	fake.Close()
	cfg2 := config.Default()
	cfg2.Accelerators = []string{"rknpu"}
	cfg2.RknpuEndpoint = fake.URL
	cfg2.RknpuSidecarCmd = ""
	out, _ = pipeline.NewLoopAccel(cfg2)[0].Call(context.Background(), "classify", map[string]any{"image_path": "/tmp/x.jpg"})
	if !strings.Contains(out, `"deferred":true`) || !strings.Contains(out, "rknpu:") {
		t.Fatalf("a down sidecar did not defer with the device named: %s", out)
	}
}

// The shared-name rule over all three devices, in every order, on the MCP surface. classify_image is owned
// by the Coral and the RKNPU only, object_detect and image_embed by all three, so a name goes to the FIRST
// listed device that owns it — which is not always the first device listed (a Hailo listed first owns no
// classify). Each shared name is advertised exactly once.
func TestSharedNameRuleOverThreeDevices(t *testing.T) {
	owners := map[string][]string{
		"offload_classify_image": {"coral-edgetpu", "rknpu"},
		"offload_object_detect":  {"hailo-8l", "coral-edgetpu", "rknpu"},
		"offload_image_embed":    {"hailo-8l", "coral-edgetpu", "rknpu"},
	}
	orders := [][]string{
		{"hailo-8l", "coral-edgetpu", "rknpu"}, {"hailo-8l", "rknpu", "coral-edgetpu"},
		{"coral-edgetpu", "hailo-8l", "rknpu"}, {"coral-edgetpu", "rknpu", "hailo-8l"},
		{"rknpu", "hailo-8l", "coral-edgetpu"}, {"rknpu", "coral-edgetpu", "hailo-8l"},
	}
	for _, order := range orders {
		cfg := config.Default()
		cfg.Accelerators = order
		s := New(pipeline.New(cfg, nil, nil, nil))
		owner := s.registerAccelTools(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil), cfg)
		count := map[string]int{}
		for _, tool := range listTools(t, cfg) {
			count[tool.Name]++
		}
		for name, owning := range owners {
			want := ""
			for _, id := range order {
				if slices.Contains(owning, id) {
					want = id
					break
				}
			}
			if owner[name] != want {
				t.Errorf("order %v: %s owned by %q, want %q", order, name, owner[name], want)
			}
			if count[name] != 1 {
				t.Errorf("order %v: %s advertised %d times, want exactly 1", order, name, count[name])
			}
		}
		for _, name := range []string{"offload_face_detect", "offload_zero_shot", "offload_semantic_segment"} {
			if count[name] != 1 {
				t.Errorf("order %v: device-unique %s advertised %d times", order, name, count[name])
			}
		}
	}
}

// A device is spelled by hand in the MCP table, the agent loop's table and the pipeline's lane table, with
// no registry to keep them level — a missed site registers nothing and fails without a word. Every device
// profiles.json declares must therefore have all three: the owned capabilities the seed lists are the
// tools the MCP table serves (offload_<capability>), the loop advertises the same tools, and the pipeline
// wires a lane for it.
func TestEveryDeclaredAcceleratorHasItsAdapters(t *testing.T) {
	doc, err := tierseed.LoadDoc("../..")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Accelerators) < 3 {
		t.Fatalf("profiles.json declares %d accelerators, want at least the Hailo, the Coral and the RKNPU", len(doc.Accelerators))
	}
	sorted := func(xs []string) []string {
		out := slices.Clone(xs)
		sort.Strings(out)
		return out
	}
	for id, acc := range doc.Accelerators {
		owns := sorted(acc.Owns)
		if got := sorted(accelOwns(id)); !reflect.DeepEqual(got, owns) {
			t.Errorf("%s: accelOwns = %v, profiles.json owns %v", id, got, owns)
		}
		var served []string
		for _, tl := range accelMCPTools(id) {
			served = append(served, strings.TrimPrefix(tl.name, "offload_"))
		}
		if got := sorted(served); !reflect.DeepEqual(got, owns) {
			t.Errorf("%s: the MCP table serves %v, profiles.json owns %v", id, got, owns)
		}
		if _, ok := accelLaneConfigFor(config.Default(), id); !ok {
			t.Errorf("%s: no lane config in mcpserver", id)
		}

		cfg := config.Default()
		cfg.Accelerators = []string{id}
		lanes := pipeline.NewLoopAccel(cfg)
		if len(lanes) != 1 || lanes[0].ID != id {
			t.Errorf("%s: the pipeline wires no lane for it (%d lanes)", id, len(lanes))
			continue
		}
		loop, err := agent.ReadOnlyToolsWithLanes(t.TempDir(), nil, nil, lanes)
		if err != nil {
			t.Fatal(err)
		}
		var loopServed []string
		for _, tl := range loop {
			for _, m := range accelMCPTools(id) {
				if tl.Name == m.name {
					loopServed = append(loopServed, strings.TrimPrefix(tl.Name, "offload_"))
				}
			}
		}
		if got := sorted(loopServed); !reflect.DeepEqual(got, owns) {
			t.Errorf("%s: the agent loop serves %v, profiles.json owns %v", id, got, owns)
		}
	}
}
