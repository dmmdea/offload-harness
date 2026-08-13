// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Wave D tests. Every fixture below is a verbatim excerpt of real llama-swap
// output from the reference deployment, not an invented shape.

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"llamaswap-pp-cli/internal/lsconfig"
	"llamaswap-pp-cli/internal/mirror"
)

// realLogFixture is copied from the live /logs buffer. It carries the two
// classes the buffer actually had (500 on /v1/embeddings and a 502), the
// access-plus-WARN double-line shape that makes naive counting wrong, and the
// startup banner the LAN-exposure check reads.
const realLogFixture = `[INFO] llama-swap is reachable by all hosts on the network, use -listen localhost:11436 to restrict to loopback only
[INFO] llama-swap listening on http://0.0.0.0:11436
[INFO] using nvidia-smi for GPU monitoring
[INFO] Request 127.0.0.1 "POST /v1/embeddings HTTP/1.1" 200 16342 "OpenAI/Python 2.45.0" 234.6843ms
[INFO] Request 127.0.0.1 "POST /v1/embeddings HTTP/1.1" 500 161 "Go-http-client/1.1" 124.6106ms
[WARN] non-200 response, recording partial metrics: status=500, path=/v1/embeddings
[INFO] Request 127.0.0.1 "POST /v1/embeddings HTTP/1.1" 502 0 "Go-http-client/1.1" 946.383ms
[WARN] non-200 response, recording partial metrics: status=502, path=/v1/embeddings
[INFO] <gemma-4-e2b> Unloading model, TTL of 300s reached
[INFO] Request 127.0.0.1 "GET /api/models HTTP/1.1" 404 19 "curl/8.21.0" 0s
`

func TestClassifyLogsCountsRequestsSeparatelyFromLines(t *testing.T) {
	rep := glueClassifyLogs(realLogFixture, 0, false)

	byClass := map[string]glueTriageFinding{}
	for _, f := range rep.Findings {
		byClass[f.Class] = f
	}

	// A single failed request writes BOTH an access line and a WARN
	// partial-metrics line. Reporting 2 as the number of failures would be
	// wrong; reporting only 1 line would hide half the evidence. Both numbers
	// are kept, and they must differ here.
	embed := byClass["500-embed-toolarge"]
	if embed.Lines != 2 {
		t.Errorf("500-embed-toolarge lines = %d, want 2 (access line + WARN line)", embed.Lines)
	}
	if embed.Requests != 1 {
		t.Errorf("500-embed-toolarge requests = %d, want 1 (one actual request)", embed.Requests)
	}

	unload := byClass["502-unload-midflight"]
	if unload.Lines != 2 || unload.Requests != 1 {
		t.Errorf("502-unload-midflight = %d lines / %d requests, want 2 / 1", unload.Lines, unload.Requests)
	}

	// 404 is not a taxonomy class: a probe for an endpoint this build does not
	// serve is not a failure, and classifying it as one would bury the real
	// findings in noise.
	if _, present := byClass["400-ctx-overflow"]; present {
		t.Errorf("404 lines must not register as 400-ctx-overflow: %+v", byClass["400-ctx-overflow"])
	}
	if rep.Clean {
		t.Error("report claims clean, but the fixture contains 500 and 502 classes")
	}
}

func TestClassifyLogsRefusesToPretendSinceWasApplied(t *testing.T) {
	// Current llama-swap builds stamp no timestamps into /logs. --since then
	// cannot be honored, and the report must SAY so rather than returning
	// filtered-looking output that was never filtered.
	rep := glueClassifyLogs(realLogFixture, 2*time.Hour, false)
	if rep.Timestamped {
		t.Error("fixture has no timestamps but the report claims it does")
	}
	if rep.SinceApplied {
		t.Error("since_applied must be false when the buffer carries no timestamps")
	}
	joined := strings.Join(rep.Notes, " | ")
	if !strings.Contains(joined, "--since was NOT applied") {
		t.Errorf("the report must state that --since was not applied; notes were: %s", joined)
	}
}

func TestClassifyLogsHonorsSinceWhenTimestamped(t *testing.T) {
	recent := time.Now().Add(-1 * time.Minute).Format(time.RFC3339)
	old := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	text := "[" + old + "] http: proxy error: dial tcp 127.0.0.1:9207: connect refused\n" +
		"[" + recent + "] failed reading from gpuCh\n"

	rep := glueClassifyLogs(text, time.Hour, false)
	if !rep.Timestamped || !rep.SinceApplied {
		t.Fatalf("timestamped=%v since_applied=%v, want both true", rep.Timestamped, rep.SinceApplied)
	}
	names := map[string]bool{}
	for _, f := range rep.Findings {
		names[f.Class] = true
	}
	if names["proxy-dial-swap-window"] {
		t.Error("a 48h-old line must be excluded by --since 1h")
	}
	if !names["gpuCh-monitor"] {
		t.Error("a 1-minute-old line must survive --since 1h")
	}
}

func TestClassifyLogsAllIncludesZeroClasses(t *testing.T) {
	rep := glueClassifyLogs("[INFO] nothing interesting here\n", 0, true)
	if len(rep.Findings) != len(glueLogTaxonomy) {
		t.Errorf("--all must emit every class for stable machine keys: got %d, want %d",
			len(rep.Findings), len(glueLogTaxonomy))
	}
	if !rep.Clean {
		t.Error("a buffer with no matches must report clean")
	}
}

// TestClassifyLogsAgainstHistoricalCorpus runs the taxonomy over the operator's
// full on-disk llama-swap log when one is present.
//
// The live /logs endpoint serves a RING that a restart empties, so the rare
// classes (proxy-dial, gpuCh, aborted-start, premature-exit) are usually absent
// from it. The append-only file on disk is the only corpus where all eight have
// ever co-occurred, which makes it the only place the taxonomy can be shown to
// discriminate rather than merely to compile.
//
// Skipped when the file is absent, so the suite stays portable.
func TestClassifyLogsAgainstHistoricalCorpus(t *testing.T) {
	path := os.Getenv("LLAMASWAP_HISTORICAL_LOG")
	if path == "" {
		cfg, err := lsconfig.DefaultConfigPath()
		if err != nil {
			t.Skip("no llama-swap config on this host; nothing to locate the historical log from")
		}
		path = filepath.Join(filepath.Dir(cfg), "llama-swap.log")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("historical log not present at %s: %v", path, err)
	}
	rep := glueClassifyLogs(string(raw), 0, true)

	byClass := map[string]glueTriageFinding{}
	for _, f := range rep.Findings {
		byClass[f.Class] = f
	}
	// Every class must be REPRESENTED (--all guarantees the key), and the two
	// that prove discrimination rather than a catch-all regex must be non-zero
	// on a corpus known to contain them.
	for _, c := range glueLogTaxonomy {
		if _, ok := byClass[c.Name]; !ok {
			t.Errorf("--all must emit class %q even at zero", c.Name)
		}
	}
	if byClass["proxy-dial-swap-window"].Lines == 0 {
		t.Error("proxy-dial-swap-window found nothing in a corpus that contains dial errors")
	}
	if byClass["500-embed-toolarge"].Requests == 0 {
		t.Error("500-embed-toolarge found no requests in a corpus that contains them")
	}
	// Disjointness: no line may be counted twice, so the class totals cannot
	// exceed the buffer.
	total := 0
	for _, f := range rep.Findings {
		total += f.Lines
	}
	if total > rep.BufferLines {
		t.Errorf("classes overlap: %d classified lines in a %d-line buffer", total, rep.BufferLines)
	}
	t.Logf("historical corpus %s: %d lines; %s", path, rep.BufferLines, glueClassSummary(rep))
}

// glueClassSummary renders a compact class:count list for test logging.
func glueClassSummary(rep glueTriageReport) string {
	parts := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		parts = append(parts, f.Class+"="+strconv.Itoa(f.Lines)+"/"+strconv.Itoa(f.Requests))
	}
	return strings.Join(parts, " ")
}

func TestResolveHostPrefersNamedRemoteAndRejectsUnknownName(t *testing.T) {
	table := remoteTable{
		Source:  "config.json",
		Remotes: map[string]string{"node-a": "http://127.0.0.1:11436", "node-b": "http://node-b-host:11436"},
	}

	// A MagicDNS hostname is a legitimate remote and must NOT be rewritten to
	// a loopback literal: the 127.0.0.1 rule is about the LOCAL proxy only.
	got, err := resolveHost("node-b", table)
	if err != nil {
		t.Fatalf("resolveHost(node-b): %v", err)
	}
	if got != "http://node-b-host:11436" {
		t.Errorf("resolveHost(node-b) = %q, want the MagicDNS URL untouched", got)
	}

	// A bare unknown name is a usage error listing what IS available, not a
	// silent http://typo guess that fails with a confusing dial error later.
	if _, err := resolveHost("nosuchbox", table); err == nil {
		t.Fatal("an unknown bare remote name must be a usage error")
	} else if !strings.Contains(err.Error(), "node-b") || !strings.Contains(err.Error(), "node-a") {
		t.Errorf("the error must list the available remotes; got %v", err)
	}

	// An explicit URL bypasses the table entirely.
	got, err = resolveHost("http://10.0.0.25:11436", table)
	if err != nil || got != "http://10.0.0.25:11436" {
		t.Errorf("explicit URL = %q, %v; want it passed through", got, err)
	}

	// host:port gains a scheme rather than being rejected.
	got, err = resolveHost("node-a.tailnet-example.ts.net:11436", table)
	if err != nil || got != "http://node-a.tailnet-example.ts.net:11436" {
		t.Errorf("host:port = %q, %v; want an http:// scheme added", got, err)
	}
}

func TestGlueEvictedNamesWhatTheLoadCost(t *testing.T) {
	before := []mirror.RunningEntry{{Model: "embeddinggemma"}, {Model: "gemma-4-26b"}}
	after := []mirror.RunningEntry{{Model: "embeddinggemma"}, {Model: "gemma-4-e2b"}}
	got := glueEvicted(before, after)
	if len(got) != 1 || got[0] != "gemma-4-26b" {
		t.Errorf("glueEvicted = %v, want [gemma-4-26b]", got)
	}
	if len(glueEvicted(before, before)) != 0 {
		t.Error("an unchanged residency set must report no evictions")
	}
}

func TestParseEventFilterRejectsSilentlyIgnorableForms(t *testing.T) {
	if _, err := glueParseEventFilter("logData"); err == nil {
		t.Error("a bare value must be a usage error, not a silently ignored filter")
	}
	if _, err := glueParseEventFilter("model=x"); err == nil {
		t.Error("an unsupported key must be a usage error")
	}
	got, err := glueParseEventFilter("type=modelStatus")
	if err != nil || got != "modelStatus" {
		t.Errorf("glueParseEventFilter(type=modelStatus) = %q, %v", got, err)
	}
	if got, err := glueParseEventFilter(""); err != nil || got != "" {
		t.Errorf("an empty filter must be accepted as no filter; got %q, %v", got, err)
	}
}

func TestConfigureSnippetsAreParseableAndCarryContext(t *testing.T) {
	// Sorted as the command sorts them, so the reranker really is first
	// alphabetically — which is exactly the wrong default and the reason
	// glueChatDefault exists.
	models := []glueConfigureModel{
		{ID: "bge-reranker-v2-m3", ContextLength: 8192, Role: glueRoleRerank, Loaded: true},
		{ID: "embeddinggemma", ContextLength: 2048, Role: glueRoleEmbedding, Loaded: true},
		{ID: "gemma-4-e2b", ContextLength: 131072, Role: glueRoleChat},
		{ID: "whisper-stt", Role: glueRoleTranscribe}, // no ctx: unknown, never defaulted
	}

	// A chat client aimed at a rerank seat fails on every request, so the
	// default must skip both non-chat seats even though both sort earlier and
	// both are already loaded.
	if got := glueChatDefault(models); got != "gemma-4-e2b" {
		t.Errorf("glueChatDefault = %q, want gemma-4-e2b (the only chat seat)", got)
	}

	generic := glueConfigureSnippet("generic", "http://127.0.0.1:11436/v1", models, "")
	// The JSON half must actually parse — a paste-ready snippet that is not
	// valid JSON is worse than no snippet.
	body := generic[strings.Index(generic, "{"):]
	if end := strings.Index(body, "\n#"); end > 0 {
		body = body[:end]
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &cfg); err != nil {
		t.Fatalf("generic snippet is not valid JSON: %v\n%s", err, generic)
	}
	if cfg["base_url"] != "http://127.0.0.1:11436/v1" {
		t.Errorf("base_url = %v, want the /v1 base", cfg["base_url"])
	}
	if !strings.Contains(generic, "(ctx unknown)") {
		t.Error("a seat with no -c must be reported as ctx unknown, never as a fabricated default")
	}

	cc := glueConfigureSnippet("claude-code", "http://127.0.0.1:11436/v1", models, "gemma-4-e2b")
	if !strings.Contains(cc, `ANTHROPIC_BASE_URL="http://127.0.0.1:11436"`) {
		t.Errorf("claude-code snippet must strip /v1 from the Anthropic base:\n%s", cc)
	}
	if !strings.Contains(cc, `ANTHROPIC_MODEL="gemma-4-e2b"`) {
		t.Errorf("--model must pin the emitted model:\n%s", cc)
	}
}

func TestDoctorWorseRanksSeverities(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{doctorOK, doctorInfo, doctorInfo},
		{doctorInfo, doctorWarn, doctorWarn},
		{doctorWarn, doctorInfo, doctorWarn},
		{doctorWarn, doctorError, doctorError},
		{doctorError, doctorOK, doctorError},
	}
	for _, c := range cases {
		if got := doctorWorse(c.a, c.b); got != c.want {
			t.Errorf("doctorWorse(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestRunDoctorExtrasFilesAStatusTheGeneratedGateCanRead(t *testing.T) {
	// doctorExitForFailOn reads map values looking for a "status" key. If the
	// section shape drifts, --fail-on silently stops seeing these findings —
	// which is exactly the class of silent regression this asserts against.
	report := map[string]any{}
	saved := doctorExtraChecks
	doctorExtraChecks = []doctorExtraCheck{
		func(_ context.Context, _ *rootFlags) doctorFinding {
			return doctorFinding{Check: "fixture", Severity: doctorWarn, Detail: "synthetic"}
		},
	}
	defer func() { doctorExtraChecks = saved }()

	runDoctorExtras(context.Background(), nil, report)
	section, ok := report["llamaswap"].(map[string]any)
	if !ok {
		t.Fatalf("report[llamaswap] is %T, want map[string]any", report["llamaswap"])
	}
	if section["status"] != doctorWarn {
		t.Errorf("status = %v, want %q so --fail-on warn trips", section["status"], doctorWarn)
	}
	if err := doctorExitForFailOn("warn", report); err == nil {
		t.Error("--fail-on warn must trip on a warn finding")
	}
	if err := doctorExitForFailOn("error", report); err != nil {
		t.Errorf("--fail-on error must NOT trip on a warn finding: %v", err)
	}
}
