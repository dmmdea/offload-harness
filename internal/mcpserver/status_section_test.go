package mcpserver

// status_section_test.go: offload_status takes ONE optional argument, section.
//
// The full answer is a 5.5-6k-token dump and it is the usual FIRST call of a
// delegating session, while sizing a contract needs only the fleet block (~1.7k
// tokens). So a caller can ask for one block, or for "brief" (the fleet block plus
// one-line lease and local verdicts). Three properties are pinned here:
//
//  1. the default answer is byte-for-byte the answer before the argument existed
//     (a golden captured from the pre-change handler on a fixture that owns every
//     machine-dependent input);
//  2. a section returns exactly its own block and nothing else — and does not pay
//     for the blocks it skips (the GPU sample);
//  3. an unknown section or argument is a clear defer, never a silent fall-back
//     to the full dump (a fall-back would cost the very tokens this exists to save,
//     and the caller would not notice).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// statusBlocks is every top-level block of the full payload, written out here
// rather than imported so a wrong implementation list cannot make these tests
// pass by agreeing with itself.
var statusBlocks = []string{"local", "media", "remote", "accelerators", "reuse", "fleet", "kv_cache_server", "gpu_lease"}

const statusGoldenPath = "testdata/offload_status_default.golden.json"

// statusFixture is a server whose status answer depends on nothing but this
// function: a fake llama-swap, one fake fleet node, a temp state dir, every media
// binding either unset or pointed at a temp file, no NIM key, no GPU sample. The
// returned normalizer replaces the three run-specific strings (two loopback URLs
// and the temp dir) so the answer can be compared across runs and machines.
func statusFixture(t *testing.T) (*Server, config.Config, func(string) string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data": []map[string]any{
					{"id": "offload-e4b"}, {"id": "gemma4-e2b"}, {"id": "gemma4-26b-a4b"}, {"id": "embeddinggemma"}, {"id": "agent-seat"},
				},
			})
		case "/running":
			_ = json.NewEncoder(w).Encode(map[string]any{"running": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fleet/health" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "node-fixture", "agent_enabled": true, "agent_seat": "seat-b",
			"agent_seat_resident": true, "agent_ctx_tokens": 32768, "queue_depth": 0,
			"jobs_running": 0, "jobs_admitting": 0, "seat_loaded": false, "max_concurrent_jobs": 4,
			"served_models": []string{"seat-b", "offload-e4b"},
		})
	}))
	t.Cleanup(node.Close)

	tmp := t.TempDir()
	for _, f := range []string{"video.mjs", "node"} {
		if err := os.WriteFile(filepath.Join(tmp, f), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("NVIDIA_API_KEY", "")
	t.Setenv("NGC_API_KEY", "")
	t.Setenv("GPU_LOCK", "")
	statusSamplesGPU = false
	t.Cleanup(func() { statusSamplesGPU = true })

	cfg := config.Default()
	cfg.Endpoint = upstream.URL
	cfg.AgentModel = "agent-seat"
	cfg.AgentDelegationEnabled = true
	cfg.DelegateRemotes = []string{node.URL}
	cfg.StateDir = tmp
	cfg.GPULockPath = ""
	cfg.CachePath = tmp + "/cache.db"
	cfg.EmbedMemoPath = tmp + "/embed-memo.db"
	cfg.VLLMSeats = []string{"seat-a"}
	cfg.KVCacheServers = config.KVCacheServers{{Seat: "seat-a", Storeless: true, Reason: "fixture: VRAM only"}}
	// Media: one CONFIGURED script route (plus the node + ComfyUI prereqs it
	// pulls in), one URL route, everything else unset — never a PATH lookup or a
	// default path whose existence depends on the machine running the test.
	cfg.ImageGenEngine, cfg.ImageGenScript = "", ""
	cfg.InpaintScript, cfg.GenEditScript, cfg.UpscaleScript = "", "", ""
	cfg.VideoGenScript = tmp + "/video.mjs"
	cfg.AnimateGenScript, cfg.VoiceGenScript, cfg.MusicGenScript, cfg.RunGraphScript = "", "", "", ""
	cfg.TTSEndpoint, cfg.TTSModel, cfg.TTSVoice = "http://tts.invalid:1", "", ""
	cfg.EditPython, cfg.GimpConsolePath, cfg.FFmpegPath = "", "", ""
	cfg.NodePath = tmp + "/node"
	cfg.ComfyDir = tmp

	escaped, _ := json.Marshal(tmp)
	tmpJSON := strings.Trim(string(escaped), `"`)
	norm := func(s string) string {
		s = strings.ReplaceAll(s, upstream.URL, "@ENDPOINT@")
		s = strings.ReplaceAll(s, node.URL, "@NODE@")
		return strings.ReplaceAll(s, tmpJSON, "@TMP@")
	}
	return New(pipeline.New(cfg, nil, nil, nil)), cfg, norm
}

// statusCall runs offload_status over an in-memory MCP transport — the path a
// real client takes — and returns the raw result text. args nil sends a call
// with no arguments member at all.
func statusCall(t *testing.T, s *Server, args any) string {
	t.Helper()
	srv := s.buildServer("test")
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "status", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()
	params := &mcp.CallToolParams{Name: "offload_status"}
	if args != nil {
		params.Arguments = args
	}
	res, err := cs.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("tools/call offload_status: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("offload_status returned %d content items, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("offload_status returned %T, want text", res.Content[0])
	}
	return text.Text
}

func decodeObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("result is not a JSON object: %v\n%s", err, raw)
	}
	return m
}

func sortedKeys(m map[string]any) []string {
	out := keysOf(m)
	sort.Strings(out)
	return out
}

// TestStatusDefaultIsByteIdenticalToTheGolden pins the default answer to the
// bytes the handler produced BEFORE the section argument existed. The golden was
// captured from that handler on this fixture (OFFLOAD_UPDATE_GOLDEN=1 rewrites
// it; only do that for a deliberate change to the full payload, and say so in
// the changelog). No arguments, empty arguments and section "all" are the same
// answer.
func TestStatusDefaultIsByteIdenticalToTheGolden(t *testing.T) {
	s, _, norm := statusFixture(t)
	got := norm(statusCall(t, s, nil))
	if os.Getenv("OFFLOAD_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(statusGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statusGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s (%d bytes)", statusGoldenPath, len(got))
	}
	raw, err := os.ReadFile(statusGoldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	want := string(bytes.TrimSpace(raw))
	for _, tc := range []struct {
		name string
		args any
	}{
		{"no arguments", nil},
		{"empty object", json.RawMessage(`{}`)},
		{"section all", json.RawMessage(`{"section":"all"}`)},
		{"section empty string", json.RawMessage(`{"section":""}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := norm(statusCall(t, s, tc.args))
			if got != want {
				t.Fatalf("default offload_status answer drifted from the golden (%d vs %d bytes).\n got: %s\nwant: %s", len(got), len(want), got, want)
			}
		})
	}
}

// TestStatusSectionReturnsOnlyItsBlock: every section answers {<key>: <block>}
// and nothing else, and the block is the same one the full answer carries.
func TestStatusSectionReturnsOnlyItsBlock(t *testing.T) {
	s, _, norm := statusFixture(t)
	full := decodeObject(t, norm(statusCall(t, s, nil)))
	for _, key := range statusBlocks {
		t.Run(key, func(t *testing.T) {
			got := decodeObject(t, norm(statusCall(t, s, json.RawMessage(`{"section":"`+key+`"}`))))
			if keys := sortedKeys(got); len(keys) != 1 || keys[0] != key {
				t.Fatalf("section %q returned keys %v, want exactly [%s]", key, keys, key)
			}
			want, present := full[key]
			if !present {
				// accelerators is absent from the full answer on a box that lists
				// none; asked for by name, the block is an empty object — zero
				// devices, never null (which would read as "unknown").
				if key != "accelerators" {
					t.Fatalf("fixture's full answer has no %q block", key)
				}
				want = map[string]any{}
			}
			if !reflect.DeepEqual(got[key], want) {
				t.Fatalf("section %q block differs from the full answer's.\n got: %v\nwant: %v", key, got[key], want)
			}
		})
	}
}

// TestStatusSectionIsCaseAndSpaceTolerant: " Fleet " is the fleet section, not an
// unknown one — normalizing the spelling is not a fall-back to the full dump.
func TestStatusSectionIsCaseAndSpaceTolerant(t *testing.T) {
	s, _, _ := statusFixture(t)
	got := decodeObject(t, statusCall(t, s, json.RawMessage(`{"section":" Fleet "}`)))
	if keys := sortedKeys(got); len(keys) != 1 || keys[0] != "fleet" {
		t.Fatalf(`section " Fleet " returned keys %v, want [fleet]`, keys)
	}
}

// TestStatusBriefIsTheFleetPlusTwoVerdictLines: brief is the sizing answer — the
// whole fleet block (seats, ctx ceilings, queues) plus ONE line each for this
// box's lease and its local serving state.
func TestStatusBriefIsTheFleetPlusTwoVerdictLines(t *testing.T) {
	s, cfg, norm := statusFixture(t)
	fullRaw := norm(statusCall(t, s, nil))
	full := decodeObject(t, fullRaw)
	briefRaw := norm(statusCall(t, s, json.RawMessage(`{"section":"brief"}`)))
	brief := decodeObject(t, briefRaw)

	if keys := sortedKeys(brief); !reflect.DeepEqual(keys, []string{"fleet", "gpu_lease_verdict", "local_verdict"}) {
		t.Fatalf("brief keys = %v, want [fleet gpu_lease_verdict local_verdict]", keys)
	}
	if !reflect.DeepEqual(brief["fleet"], full["fleet"]) {
		t.Fatalf("brief.fleet must be the full fleet block.\n got: %v\nwant: %v", brief["fleet"], full["fleet"])
	}
	if len(briefRaw) >= len(fullRaw) {
		t.Fatalf("brief (%d bytes) is not smaller than the full answer (%d bytes)", len(briefRaw), len(fullRaw))
	}

	gl, _ := brief["gpu_lease_verdict"].(string)
	verdict, _ := full["gpu_lease"].(map[string]any)["verdict"].(string)
	if verdict == "" || !strings.HasPrefix(gl, verdict) {
		t.Errorf("gpu_lease_verdict %q must lead with the lease verdict %q", gl, verdict)
	}
	if !strings.Contains(gl, "local-offload gpu reserve --wait") {
		t.Errorf("gpu_lease_verdict %q must carry the queue command — a held card is a place in line", gl)
	}

	lv, _ := brief["local_verdict"].(string)
	seat := full["fleet"].(map[string]any)["local_agent_seat"].(map[string]any)
	for _, want := range []string{"5 models served", cfg.AgentPlannerModel(""), seat["verdict"].(string), "stt_hq"} {
		if !strings.Contains(lv, want) {
			t.Errorf("local_verdict %q must name %q", lv, want)
		}
	}
	for name, line := range map[string]string{"gpu_lease_verdict": gl, "local_verdict": lv} {
		if line == "" || strings.ContainsAny(line, "\r\n") {
			t.Errorf("%s must be one non-empty line, got %q", name, line)
		}
	}
}

// TestStatusBriefNamesTheHolderOfAHeldLease: held, the one line says who holds
// the cards, why, and still ends in the queue command.
func TestStatusBriefNamesTheHolderOfAHeldLease(t *testing.T) {
	s, cfg, _ := statusFixture(t)
	m, err := gpulease.OpenAt("", cfg.StateDir)
	if err != nil {
		t.Fatalf("open lease: %v", err)
	}
	// A TEXT lease: Exclusive is stamped only on that class (gpulease TryAcquire).
	l, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "fixture bench", Exclusive: true, TTL: time.Hour})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = l.Release() }()

	brief := decodeObject(t, statusCall(t, s, json.RawMessage(`{"section":"brief"}`)))
	gl, _ := brief["gpu_lease_verdict"].(string)
	for _, want := range []string{"held", "fixture bench", "text", "exclusive", "local-offload gpu reserve --wait"} {
		if !strings.Contains(gl, want) {
			t.Errorf("held gpu_lease_verdict %q must name %q", gl, want)
		}
	}
}

// TestStatusRejectsAnUnknownSectionOrArgument: a typo or a guessed argument gets
// a defer that names the valid values — never the full 6k-token dump.
func TestStatusRejectsAnUnknownSectionOrArgument(t *testing.T) {
	s, _, _ := statusFixture(t)
	for _, tc := range []struct {
		name, args string
		want       []string
	}{
		{"unknown section", `{"section":"gpu"}`, []string{`"gpu"`, "gpu_lease", "brief", "fleet"}},
		{"unknown argument", `{"brief":true}`, []string{"brief", "section"}},
		{"wrong type", `{"section":5}`, []string{"bad arguments", "section"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeObject(t, statusCall(t, s, json.RawMessage(tc.args)))
			if got["deferred"] != true {
				t.Fatalf("%s must defer, got keys %v", tc.args, sortedKeys(got))
			}
			for _, k := range statusBlocks {
				if _, leaked := got[k]; leaked {
					t.Fatalf("%s must not fall back to any status block; got %q", tc.args, k)
				}
			}
			reason, _ := got["reason"].(string)
			for _, w := range tc.want {
				if !strings.Contains(reason, w) {
					t.Errorf("reason %q must name %q", reason, w)
				}
			}
		})
	}
}

// TestStatusSectionKeepsConfigErrorFirst: on a config that failed validation the
// reason is the first key of EVERY answer, a section included — this is the one
// tool still answering while the others defer.
func TestStatusSectionKeepsConfigErrorFirst(t *testing.T) {
	s, _, _ := statusFixture(t)
	s = s.WithConfigError(badConfig)
	for _, section := range []string{"fleet", "brief"} {
		raw := statusCall(t, s, json.RawMessage(`{"section":"`+section+`"}`))
		if !strings.HasPrefix(raw, `{"config_error":`) {
			t.Errorf("section %s: config_error must be the first key; got %.80s", section, raw)
		}
		if got := decodeObject(t, raw); got["fleet"] == nil {
			t.Errorf("section %s must still answer with the fleet block; keys %v", section, sortedKeys(got))
		}
	}
}

// TestStatusSectionSkipsTheBlocksItDoesNotReturn: a section computes only its
// own block. The fleet section must not sample the GPUs (an nvidia-smi run per
// call for a block it then throws away); gpu_lease and brief must.
func TestStatusSectionSkipsTheBlocksItDoesNotReturn(t *testing.T) {
	s, _, _ := statusFixture(t)
	calls := 0
	statusSamplesGPU = true
	statusGPUSampler = func(context.Context) ([]gpuactivity.GPU, error) {
		calls++
		return nil, os.ErrNotExist // an error return also skips the process sample
	}
	t.Cleanup(func() { statusGPUSampler = nil })
	for _, tc := range []struct {
		section string
		want    int
	}{{"fleet", 0}, {"local", 0}, {"gpu_lease", 1}, {"brief", 1}, {"all", 1}} {
		calls = 0
		_ = statusCall(t, s, json.RawMessage(`{"section":"`+tc.section+`"}`))
		if calls != tc.want {
			t.Errorf("section %s sampled the GPUs %d times, want %d", tc.section, calls, tc.want)
		}
	}
}

// TestStatusSchemaPublishesTheSectionEnum: tools/list advertises section with
// exactly the values the handler accepts, and the delegation door's sizing
// guidance points at the brief form.
func TestStatusSchemaPublishesTheSectionEnum(t *testing.T) {
	s, _, _ := statusFixture(t)
	srv := s.buildServer("test")
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "status", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var status, delegate *mcp.Tool
	for _, tool := range list.Tools {
		switch tool.Name {
		case "offload_status":
			status = tool
		case "agent_delegate":
			delegate = tool
		}
	}
	if status == nil || delegate == nil {
		t.Fatalf("tools/list must carry offload_status and agent_delegate (delegation is on in the fixture)")
	}
	raw, _ := json.Marshal(status.InputSchema)
	var schema struct {
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("offload_status inputSchema: %v", err)
	}
	if len(schema.Required) != 0 {
		t.Errorf("section must stay optional; required = %v", schema.Required)
	}
	sec, ok := schema.Properties["section"]
	if !ok || sec.Type != "string" {
		t.Fatalf("offload_status must advertise a string section argument; schema = %s", raw)
	}
	want := append([]string{"all", "brief"}, statusBlocks...)
	got := append([]string(nil), sec.Enum...)
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("section enum = %v, want %v", sec.Enum, want)
	}
	if len(schema.Properties) != 1 {
		t.Errorf("offload_status takes exactly one argument; schema = %s", raw)
	}
	if !strings.Contains(delegate.Description, `offload_status {section:"brief"}`) {
		t.Errorf("agent_delegate's sizing guidance must name the brief form of offload_status")
	}
	if !strings.Contains(status.Description, `section:"brief"`) {
		t.Errorf("offload_status's description must name the brief form")
	}
}
