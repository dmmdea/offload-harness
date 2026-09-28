package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// browseHelperArg is how the fake sidecar is reached: the test binary re-executes
// itself with only this argument, so TestBrowseHelperProcess knows it is the child.
const browseHelperArg = "-test.run=^TestBrowseHelperProcess$"

// TestBrowseHelperProcess is the fake sidecar. It speaks the stdio protocol and
// picks its script from the start line's goal. In a normal test run it skips.
func TestBrowseHelperProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[1] != browseHelperArg {
		t.Skip("fake browse sidecar; runs only as a child")
	}
	in := bufio.NewReader(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	readLine := func() map[string]any {
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			os.Exit(7)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(line), &m)
		return m
	}
	start := readLine()
	result := func(status, class, reason string, extra map[string]any) {
		m := map[string]any{"type": "result", "status": status, "class": class, "reason": reason,
			"model_done": status == "done", "final": map[string]any{"url": start["url"], "title": "T", "text": "page text"},
			"actions": []any{}, "decisions": 1, "text_calls": 1, "captured": 0}
		for k, v := range extra {
			m[k] = v
		}
		_ = out.Encode(m)
		os.Exit(0)
	}
	decideBody := map[string]any{"model": "jev-latest", "state": map[string]any{"page": map[string]any{"url": start["url"]}},
		"questions": map[string]any{"operation": map[string]any{"type": "choice", "criteria": map[string]any{"DONE": "d", "BLOCKED": "b"}}}}
	switch start["goal"] {
	case "done":
		_ = out.Encode(map[string]any{"type": "decide", "id": 1, "body": decideBody})
		d := readLine()
		if d["type"] != "decision" || d["ok"] != true {
			result("error", "DECISION_UNAVAILABLE", fmt.Sprint(d["error"]), nil)
		}
		_ = out.Encode(map[string]any{"type": "text", "id": 2, "context": map[string]any{"goal": "done", "field": map[string]any{"label": "Title"}}})
		tr := readLine()
		if tr["type"] != "text_result" || tr["ok"] != true || tr["text"] != "Capture probe" {
			result("error", "TEXT_UNAVAILABLE", fmt.Sprint(tr), nil)
		}
		_ = out.Encode(map[string]any{"type": "step", "n": 1, "op": "TYPE_TEXT", "label": "Title", "url": start["url"]})
		result("done", "", "", map[string]any{"decisions": 1, "text_calls": 1})
	case "decision-error":
		_ = out.Encode(map[string]any{"type": "decide", "id": 1, "body": decideBody})
		d := readLine()
		if d["ok"] == true {
			result("done", "", "", nil)
		}
		result("error", "DECISION_UNAVAILABLE", fmt.Sprint(d["error"]), nil)
	case "oversized":
		big := strings.Repeat("x", browseMaxDecideBytes+1)
		_ = out.Encode(map[string]any{"type": "decide", "id": 1, "body": map[string]any{"state": big, "questions": map[string]any{"q": 1}}})
		d := readLine()
		if d["ok"] == true {
			result("done", "", "", nil)
		}
		result("error", "DECISION_UNAVAILABLE", fmt.Sprint(d["error"]), nil)
	case "blocked":
		result("blocked", "", "no supported operation can progress", nil)
	case "denied":
		result("denied", "DENIED", "the model chose \"Publish\"", nil)
	case "no-result":
		os.Exit(3)
	case "hang":
		time.Sleep(60 * time.Second)
		os.Exit(0)
	case "echo-start":
		// Report the start line back in the reason so the parent can assert on it.
		b, _ := json.Marshal(start)
		result("blocked", "", string(b), nil)
	}
	os.Exit(9)
}

type browseFixture struct {
	p          *Pipeline
	decisions  atomic.Int32
	llamaCalls atomic.Int32
	lastAuth   atomic.Value
}

// newBrowseFixture wires a pipeline to a fake decision endpoint (loopback, as the
// config requires) and a fake llama chat route that always writes "Capture probe".
func newBrowseFixture(t *testing.T, decision http.HandlerFunc) *browseFixture {
	t.Helper()
	f := &browseFixture{}
	if decision == nil {
		decision = func(w http.ResponseWriter, r *http.Request) {
			f.lastAuth.Store(r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"answers":{"operation":{"choice":"DONE","confidence":0.9,"probabilities":{"DONE":0.9,"BLOCKED":0.1}}},"model":"typesafe/jev-test","usage":{"cost":0.00002}}`)
		}
	}
	dsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.decisions.Add(1)
		decision(w, r)
	}))
	t.Cleanup(dsrv.Close)
	lsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.llamaCalls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, bad := body["response_format"]; bad {
			http.Error(w, "response_format crashes the model (Invariant 1)", 500)
			return
		}
		if g, _ := body["grammar"].(string); !strings.Contains(g, `"\"text\""`) {
			http.Error(w, "no text grammar", 500)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"text\": \"Capture probe\"}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	t.Cleanup(lsrv.Close)

	dir := t.TempDir()
	script := filepath.Join(dir, "runner.py")
	if err := os.WriteFile(script, []byte("# fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Endpoint = lsrv.URL
	cfg.Model = "gemma4-e4b"
	cfg.ThresholdsPath, cfg.RouterWeightsPath, cfg.TierOverridesPath, cfg.ConfHeadLabelsPath, cfg.CachePath = "", "", "", "", ""
	cfg.BrowsePython = os.Args[0]
	cfg.BrowseScript = script
	cfg.BrowseDecisionURL = dsrv.URL + "/v1/systemone"
	cfg.BrowseTimeoutSec = 20
	cfg.StateDir = dir
	f.p = New(cfg, llamaclient.New(lsrv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)

	old := browseCommand
	browseCommand = func(python, script string) (string, []string) { return os.Args[0], []string{browseHelperArg} }
	t.Cleanup(func() { browseCommand = old })
	return f
}

func runBrowseReq(p *Pipeline, params map[string]any) core.Result {
	return p.Run(context.Background(), core.Request{Task: core.TaskBrowse, Door: "offload_browse", Params: params})
}

func TestBrowseHappyPathProxiesDecisionAndGeneratesText(t *testing.T) {
	t.Setenv("LOCAL_OFFLOAD_BROWSE_BEARER", "local-nonce")
	f := newBrowseFixture(t, nil)
	res := runBrowseReq(f.p, map[string]any{"url": "https://example.com/", "goal": "done"})
	if !res.OK || res.Deferred {
		t.Fatalf("want OK, got deferred=%v reason=%q", res.Deferred, res.Reason)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "done" || out["decision_model"] != "typesafe/jev-test" {
		t.Errorf("result = %v", out)
	}
	if f.decisions.Load() != 1 || f.llamaCalls.Load() != 1 {
		t.Errorf("decisions=%d llama=%d, want 1 and 1", f.decisions.Load(), f.llamaCalls.Load())
	}
	if got, _ := f.lastAuth.Load().(string); got != "Bearer local-nonce" {
		t.Errorf("the bearer must come from LOCAL_OFFLOAD_BROWSE_BEARER, got %q", got)
	}
	if steps, _ := out["steps"].(float64); steps != 1 {
		t.Errorf("steps = %v, want 1", out["steps"])
	}
}

func TestBrowseNotConfiguredDefersWithoutSpawning(t *testing.T) {
	f := newBrowseFixture(t, nil)
	f.p.cfg.BrowseDecisionURL = "https://api.typesafe.ai/v1/systemone" // a provider: never
	res := runBrowseReq(f.p, map[string]any{"url": "https://example.com/", "goal": "done"})
	if !res.Deferred || !strings.Contains(res.Reason, "not configured") {
		t.Fatalf("want a not-configured defer, got ok=%v reason=%q", res.OK, res.Reason)
	}
	if f.decisions.Load() != 0 {
		t.Error("an unconfigured lane must not call any decision endpoint")
	}
}

func TestBrowseBadInputDefersBeforeSpawn(t *testing.T) {
	f := newBrowseFixture(t, nil)
	for name, params := range map[string]map[string]any{
		"no goal":             {"url": "https://example.com/"},
		"file url":            {"url": "file:///etc/passwd", "goal": "x"},
		"javascript url":      {"url": "javascript:alert(1)", "goal": "x"},
		"too many actions":    {"url": "https://example.com/", "goal": "x", "max_actions": 61},
		"host not allowed":    {"url": "https://example.com/", "goal": "x", "allow_hosts": []any{"substack.com"}},
		"unattended no hosts": {"url": "https://example.com/", "goal": "x", "unattended": true},
		"unattended allow_labels": {"url": "https://example.com/", "goal": "x", "unattended": true,
			"allow_hosts": []any{"example.com"}, "allow_labels": []any{"Publish"}},
		"capture off-host": {"url": "https://example.com/", "goal": "x", "allow_hosts": []any{"example.com"},
			"capture": []any{"https://other.test/api/"}},
	} {
		res := runBrowseReq(f.p, params)
		if !res.Deferred || !strings.Contains(res.Reason, "BAD_INPUT") {
			t.Errorf("%s: want BAD_INPUT defer, got ok=%v reason=%q", name, res.OK, res.Reason)
		}
	}
	if f.decisions.Load() != 0 {
		t.Error("bad input must be refused before the sidecar runs")
	}
}

func TestBrowseDecisionEndpointFailureDefersTyped(t *testing.T) {
	f := newBrowseFixture(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) })
	res := runBrowseReq(f.p, map[string]any{"url": "https://example.com/", "goal": "decision-error"})
	if !res.Deferred || !strings.Contains(res.Reason, "DECISION_UNAVAILABLE") {
		t.Fatalf("want DECISION_UNAVAILABLE defer, got %q", res.Reason)
	}
}

// A 3xx from the loopback decision service must never be followed: a 307/308 keeps
// the POST body, so following it would carry the page state to wherever Location
// points and defeat the loopback-only rule (review finding security:F1).
func TestBrowseDecisionRedirectIsNeverFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	far := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = io.WriteString(w, `{"answers":{"operation":{"choice":"DONE","confidence":1,"probabilities":{"DONE":1}}},"model":"x"}`)
	}))
	defer far.Close()
	f := newBrowseFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, far.URL+"/v1/systemone", http.StatusTemporaryRedirect)
	})
	res := runBrowseReq(f.p, map[string]any{"url": "https://example.com/", "goal": "decision-error"})
	if !res.Deferred || !strings.Contains(res.Reason, "DECISION_UNAVAILABLE") {
		t.Fatalf("a redirecting decision endpoint must defer DECISION_UNAVAILABLE, got %q", res.Reason)
	}
	if elsewhere.Load() != 0 {
		t.Fatalf("the redirect target received %d request(s): page state left the loopback endpoint", elsewhere.Load())
	}
}

func TestBrowseOversizedDecideBodyIsRefusedNotForwarded(t *testing.T) {
	f := newBrowseFixture(t, nil)
	res := runBrowseReq(f.p, map[string]any{"url": "https://example.com/", "goal": "oversized"})
	if !res.Deferred || !strings.Contains(res.Reason, "DECISION_UNAVAILABLE") {
		t.Fatalf("want DECISION_UNAVAILABLE defer, got %q", res.Reason)
	}
	if f.decisions.Load() != 0 {
		t.Error("an oversized decision body must never reach the endpoint")
	}
}

func TestBrowseBlockedAndDeniedAreDefersWithPartial(t *testing.T) {
	f := newBrowseFixture(t, nil)
	for goal, want := range map[string]string{"blocked": "blocked", "denied": "DENIED"} {
		res := runBrowseReq(f.p, map[string]any{"url": "https://example.com/", "goal": goal})
		if !res.Deferred || !strings.Contains(res.Reason, want) {
			t.Errorf("%s: want a defer naming %s, got %q", goal, want, res.Reason)
		}
		if !strings.Contains(res.Partial, `"final"`) {
			t.Errorf("%s: the defer must carry the run so far as partial, got %q", goal, res.Partial)
		}
	}
}

func TestBrowseSidecarWithoutResultIsRunnerFailed(t *testing.T) {
	f := newBrowseFixture(t, nil)
	res := runBrowseReq(f.p, map[string]any{"url": "https://example.com/", "goal": "no-result"})
	if !res.Deferred || !strings.Contains(res.Reason, "RUNNER_FAILED") {
		t.Fatalf("want RUNNER_FAILED, got %q", res.Reason)
	}
}

func TestBrowseHangIsKilledAtTheTimeout(t *testing.T) {
	f := newBrowseFixture(t, nil)
	f.p.cfg.BrowseTimeoutSec = 2
	started := time.Now()
	res := runBrowseReq(f.p, map[string]any{"url": "https://example.com/", "goal": "hang"})
	if !res.Deferred || !strings.Contains(res.Reason, "TIMEOUT") {
		t.Fatalf("want TIMEOUT, got %q", res.Reason)
	}
	if el := time.Since(started); el > 20*time.Second {
		t.Errorf("a hung sidecar must be killed near the 2s budget, took %s", el)
	}
}

func TestBrowseStartLineCarriesTheValidatedRequest(t *testing.T) {
	f := newBrowseFixture(t, nil)
	f.p.cfg.BrowseBrowser = "brave"
	res := runBrowseReq(f.p, map[string]any{"url": "https://pub.example.com/publish", "goal": "echo-start",
		"allow_hosts": []any{"Example.COM"}, "capture": []any{"https://pub.example.com/api/v1/"}, "max_actions": 5})
	var start map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(res.Reason, "browse: blocked: ")), &start); err != nil {
		t.Fatalf("reason %q did not carry the start line: %v", res.Reason, err)
	}
	if start["browser"] != "brave" || start["max_actions"] != float64(5) || start["unattended"] != false {
		t.Errorf("start = %v", start)
	}
	if hosts, _ := start["allow_hosts"].([]any); len(hosts) != 1 || hosts[0] != "example.com" {
		t.Errorf("hosts must be normalized to lowercase, got %v", start["allow_hosts"])
	}
	if cp, _ := start["capture_path"].(string); !strings.HasSuffix(cp, ".jsonl") || !strings.Contains(cp, "browse-captures") {
		t.Errorf("capture_path = %q", start["capture_path"])
	}
}

func TestBrowseRunnerEnvIsAnAllowlistWithTelemetryOff(t *testing.T) {
	env := browseRunnerEnv([]string{"PATH=/bin", "OPENROUTER_API_KEY=sk-x", "LOCAL_OFFLOAD_BROWSE_BEARER=n", "HOME=/h", "GPU_LEASE_TOKEN=t"}, "linux")
	joined := strings.Join(env, "\n")
	for _, bad := range []string{"OPENROUTER_API_KEY", "LOCAL_OFFLOAD_BROWSE_BEARER", "GPU_LEASE_TOKEN"} {
		if strings.Contains(joined, bad) {
			t.Errorf("%s must never reach the sidecar", bad)
		}
	}
	for _, want := range []string{"PATH=/bin", "HOME=/h", "BH_TELEMETRY=0", "ANONYMIZED_TELEMETRY=0", "BU_NAME=offload-browse"} {
		if !strings.Contains(joined, want) {
			t.Errorf("sidecar env must carry %s, got %v", want, env)
		}
	}
}
