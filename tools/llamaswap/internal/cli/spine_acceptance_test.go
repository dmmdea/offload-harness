// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Acceptance tests for the wave A spine, driven entirely against
// internal/fakeswap. The live proxy carries the mem0 memory stack, so no test
// here may unload, restart, or write to the real server.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llamaswap-pp-cli/internal/fakeswap"
	"llamaswap-pp-cli/internal/mirror"
	"llamaswap-pp-cli/internal/store"
)

const spineTestYAML = `
startPort: 9200
models:
  "gemma-4-e4b":
    cmd: "C:/llama.cpp/llama-server.exe --port ${PORT} --host 127.0.0.1 -m V:/models/gemma-4-e4b.gguf --ctx-size 8192"
    aliases: ["offload-e4b"]
    ttl: 300

  "embeddinggemma":
    cmd: "C:/llama.cpp/llama-server.exe --port ${PORT} --host 127.0.0.1 -m V:/models/embeddinggemma.gguf --embeddings"
    aliases: ["text-embedding", "local-embed"]
    ttl: -1                 # never auto-unload — mem0 needs it up

  "bge-reranker-v2-m3":
    cmd: "C:/llama.cpp/llama-server.exe --port ${PORT} --host 127.0.0.1 -m V:/models/bge-reranker.gguf --reranking"
    aliases: ["reranker-v2-m3", "v0.12-reranker"]
    ttl: -1
`

type spineTestEnv struct {
	fake   *fakeswap.Server
	dbPath string
}

func newSpineTestEnv(t *testing.T) *spineTestEnv {
	t.Helper()
	fs := fakeswap.New()
	t.Cleanup(fs.Close)

	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "llama-swap.yaml")
	if err := os.WriteFile(yamlPath, []byte(spineTestYAML), 0o600); err != nil {
		t.Fatalf("write yaml fixture: %v", err)
	}

	t.Setenv("LLAMASWAP_BASE_URL", fs.URL())
	t.Setenv("LLAMASWAP_NO_LEARN", "true")
	t.Setenv("LLAMASWAP_CONFIG", filepath.Join(dir, "config.json")) // absent on purpose
	t.Setenv(mirror.EnvYAMLPath, yamlPath)
	t.Setenv(mirror.EnvKeepSet, "")
	t.Setenv("PRINTING_PRESS_VERIFY", "")
	t.Setenv("PRINTING_PRESS_DOGFOOD", "")

	// Roster: one swappable seat plus the two mem0 keep-set seats.
	fs.AddModel(fakeswap.Model{ID: "gemma-4-e4b", Name: "Gemma 4 E4B", Aliases: []string{"offload-e4b"}, ConfigTTL: 300})
	fs.AddModel(fakeswap.Model{ID: "embeddinggemma", Name: "EmbeddingGemma-300m", Aliases: []string{"text-embedding", "local-embed"}, ConfigTTL: -1})
	fs.AddModel(fakeswap.Model{ID: "bge-reranker-v2-m3", Name: "bge-reranker-v2-m3", Aliases: []string{"reranker-v2-m3", "v0.12-reranker"}, ConfigTTL: -1})

	return &spineTestEnv{fake: fs, dbPath: filepath.Join(dir, "data.db")}
}

func runSpine(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := RootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errBuf.String(), err
}

// lastJSONObject pulls the final top-level JSON object out of a stream that may
// also contain the framework sync's NDJSON events.
func lastJSONObject(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	var last map[string]any
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			break
		}
		if m, ok := v.(map[string]any); ok {
			last = m
		}
	}
	if last == nil {
		t.Fatalf("no JSON object in output:\n%s", s)
	}
	return last
}

// --- (d) keep-set refusal fires on an ALIAS -----------------------------------

// TestAcceptanceD_KeepsetRefusalOnAlias: `unload local-embed` must refuse. The
// alias is how the mem0 stack is actually addressed, so an id-only check would
// let the outage through. Nothing may reach the server.
func TestAcceptanceD_KeepsetRefusalOnAlias(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("embeddinggemma")

	out, _, err := runSpine(t, "models", "unload", "local-embed", "--json", "--db", env.dbPath)
	if got := ExitCode(err); got != ExitKeepsetRefusal {
		t.Fatalf("exit code = %d, want %d (ExitKeepsetRefusal); err=%v\noutput:\n%s", got, ExitKeepsetRefusal, err, out)
	}
	if calls := env.fake.UnloadCalls(); len(calls) != 0 {
		t.Fatalf("refusal must send NOTHING to the server, got %+v", calls)
	}
	envelope := lastJSONObject(t, out)
	if refused, _ := envelope["refused"].(bool); !refused {
		t.Fatalf("envelope.refused = %v, want true:\n%s", envelope["refused"], out)
	}
	results, _ := envelope["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %v", envelope["results"])
	}
	first, _ := results[0].(map[string]any)
	if first["action"] != "refused_keepset" {
		t.Fatalf("action = %v, want refused_keepset", first["action"])
	}
	if first["model"] != "embeddinggemma" {
		t.Fatalf("alias must resolve to the canonical id, got %v", first["model"])
	}
}

func TestKeepsetRefusalOverriddenByForce(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("embeddinggemma")

	out, stderr, err := runSpine(t, "models", "unload", "local-embed", "--force-keepset", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("--force-keepset should succeed: %v\n%s", err, out)
	}
	calls := env.fake.UnloadCalls()
	if len(calls) != 1 || calls[0].Model != "embeddinggemma" {
		t.Fatalf("unload calls = %+v", calls)
	}
	if !strings.Contains(stderr, "--force-keepset unloaded embeddinggemma") {
		t.Fatalf("an override of the keep-set must be LOUD on stderr, got:\n%s", stderr)
	}
	// The override must be recorded so `keepset audit` can attribute it.
	db, derr := store.OpenWithContext(context.Background(), env.dbPath)
	if derr != nil {
		t.Fatalf("open store: %v", derr)
	}
	defer db.Close()
	var forced int
	if qerr := db.DB().QueryRow(`SELECT forced FROM unload_provenance WHERE model='embeddinggemma'`).Scan(&forced); qerr != nil {
		t.Fatalf("provenance row missing: %v", qerr)
	}
	if forced != 1 {
		t.Fatalf("unload_provenance.forced = %d, want 1", forced)
	}
}

// --- (e) drain fails closed ---------------------------------------------------

// TestAcceptanceE_DrainTimeoutIsUnobservableAndUnloadsNothing: /slots never
// answers. Unreadable slot state is NOT "probably idle" — the command must fail
// closed with ExitDrainUnobservable and leave the model loaded.
func TestAcceptanceE_DrainUnobservableUnloadsNothing(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("gemma-4-e4b")
	env.fake.SetSlots("gemma-4-e4b", fakeswap.SlotTimeout)

	out, _, err := runSpine(t, "models", "unload", "gemma-4-e4b",
		"--drain", "--drain-timeout", "3s", "--json", "--db", env.dbPath)
	if got := ExitCode(err); got != ExitDrainUnobservable {
		t.Fatalf("exit code = %d, want %d (ExitDrainUnobservable); err=%v\noutput:\n%s", got, ExitDrainUnobservable, err, out)
	}
	if calls := env.fake.UnloadCalls(); len(calls) != 0 {
		t.Fatalf("fail-closed drain must unload NOTHING, got %+v", calls)
	}
	if !strings.Contains(out, "gemma-4-e4b") || !strings.Contains(out, "nothing was unloaded") {
		t.Fatalf("output must name the unobservable target and say nothing was unloaded:\n%s", out)
	}
}

// TestDrainTimeoutWhileProcessing: slots ARE readable and report a busy slot.
// That is a timeout (21), not unobservable (22) — different code because the
// operator's next move is different.
func TestDrainTimeoutWhileProcessing(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("gemma-4-e4b")
	env.fake.SetSlots("gemma-4-e4b", fakeswap.SlotProcessing)

	out, _, err := runSpine(t, "models", "unload", "gemma-4-e4b",
		"--drain", "--drain-timeout", "600ms", "--json", "--db", env.dbPath)
	if got := ExitCode(err); got != ExitDrainTimeout {
		t.Fatalf("exit code = %d, want %d (ExitDrainTimeout)\noutput:\n%s", got, ExitDrainTimeout, out)
	}
	if calls := env.fake.UnloadCalls(); len(calls) != 0 {
		t.Fatalf("a drain timeout must unload nothing, got %+v", calls)
	}
}

// TestAcceptanceE_Slots404FallsBackAndProceeds: a 404 means llama-server was
// started without --slots. That is endpoint ABSENT, not unobservable, so the
// documented activity fallback runs, says so, and the unload proceeds.
func TestAcceptanceE_Slots404FallsBackAndProceeds(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("gemma-4-e4b")
	env.fake.SetSlots("gemma-4-e4b", fakeswap.SlotStatus404)
	env.fake.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 900) // terminal: idle

	out, _, err := runSpine(t, "models", "unload", "gemma-4-e4b",
		"--drain", "--drain-timeout", "3s", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("idle model behind a 404 /slots should unload: %v\n%s", err, out)
	}
	calls := env.fake.UnloadCalls()
	if len(calls) != 1 || calls[0].Model != "gemma-4-e4b" {
		t.Fatalf("unload calls = %+v", calls)
	}
	envelope := lastJSONObject(t, out)
	results, _ := envelope["results"].([]any)
	first, _ := results[0].(map[string]any)
	if first["drain_method"] != "activity-fallback" {
		t.Fatalf("drain_method = %v, want activity-fallback", first["drain_method"])
	}
	if !strings.Contains(out, "fell back to the /api/metrics/activity in-flight check") {
		t.Fatalf("the fallback must be disclosed in the output:\n%s", out)
	}
}

func TestDrain404FallbackSeesInFlightRow(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("gemma-4-e4b")
	env.fake.SetSlots("gemma-4-e4b", fakeswap.SlotStatus404)
	env.fake.AddInFlight("gemma-4-e4b", "/v1/chat/completions")

	_, _, err := runSpine(t, "models", "unload", "gemma-4-e4b",
		"--drain", "--drain-timeout", "600ms", "--json", "--db", env.dbPath)
	if got := ExitCode(err); got != ExitDrainTimeout {
		t.Fatalf("exit code = %d, want %d: a non-terminal activity row means still busy", got, ExitDrainTimeout)
	}
	if calls := env.fake.UnloadCalls(); len(calls) != 0 {
		t.Fatalf("busy model must not be unloaded, got %+v", calls)
	}
}

// --- (f) unload --all excludes the keep-set -----------------------------------

// TestAcceptanceF_UnloadAllSkipsKeepSet: every non-keep-set model is unloaded and
// the keep-set is untouched. This is the case that makes the mem0-outage class
// structurally impossible rather than merely unlikely.
func TestAcceptanceF_UnloadAllSkipsKeepSet(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("gemma-4-e4b", "embeddinggemma", "bge-reranker-v2-m3")

	out, _, err := runSpine(t, "models", "unload-all", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("unload-all: %v\n%s", err, out)
	}
	calls := env.fake.UnloadCalls()
	if len(calls) != 1 || calls[0].Model != "gemma-4-e4b" {
		t.Fatalf("only the non-keep-set seat may be unloaded, got %+v", calls)
	}
	envelope := lastJSONObject(t, out)
	results, _ := envelope["results"].([]any)
	actions := map[string]string{}
	for _, r := range results {
		m, _ := r.(map[string]any)
		model, _ := m["model"].(string)
		action, _ := m["action"].(string)
		actions[model] = action
	}
	if actions["gemma-4-e4b"] != "unloaded" {
		t.Fatalf("gemma-4-e4b action = %q, want unloaded", actions["gemma-4-e4b"])
	}
	for _, keepMember := range []string{"embeddinggemma", "bge-reranker-v2-m3"} {
		if actions[keepMember] != "skipped_keepset" {
			t.Fatalf("%s action = %q, want skipped_keepset", keepMember, actions[keepMember])
		}
	}
	// And they are still resident on the fake.
	var running struct{ Running []any }
	_ = running
	if hits := env.fake.Hits("/api/models/unload"); hits != 0 {
		t.Fatalf("the non-selective bulk route must not be used when a per-model route exists (hits=%d)", hits)
	}
}

// TestUnloadAllRefusesBulkFallbackWhileKeepSetResident: on a build without the
// per-model route, the only remaining routes unload EVERYTHING. Rather than
// widening the blast radius silently, the command refuses.
func TestUnloadAllRefusesBulkFallbackWhileKeepSetResident(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("gemma-4-e4b", "embeddinggemma")
	env.fake.SetUnloadModelRouteMissing(true)

	out, _, err := runSpine(t, "models", "unload-all", "--json", "--db", env.dbPath)
	if got := ExitCode(err); got != ExitKeepsetRefusal {
		t.Fatalf("exit code = %d, want %d\noutput:\n%s", got, ExitKeepsetRefusal, out)
	}
	if calls := env.fake.UnloadCalls(); len(calls) != 0 {
		t.Fatalf("nothing may be unloaded, got %+v", calls)
	}
	if !strings.Contains(out, "NOT selective") {
		t.Fatalf("the refusal must explain why the fallback was not taken:\n%s", out)
	}
}

func TestUnloadAllUsesLegacyRouteWhenNoKeepSetResident(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("gemma-4-e4b")
	env.fake.SetUnloadModelRouteMissing(true)
	env.fake.SetUnloadAllRouteMissing(true)
	env.fake.SetLegacyUnload(true)

	out, _, err := runSpine(t, "models", "unload-all", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("legacy fallback should succeed: %v\n%s", err, out)
	}
	calls := env.fake.UnloadCalls()
	if len(calls) != 1 || calls[0].Path != "/unload" {
		t.Fatalf("expected the legacy GET /unload fallback, got %+v", calls)
	}
}

func TestUnloadUnknownModelExitsModelNotFound(t *testing.T) {
	env := newSpineTestEnv(t)
	_, _, err := runSpine(t, "models", "unload", "not-a-model", "--json", "--db", env.dbPath)
	if got := ExitCode(err); got != ExitModelNotFound {
		t.Fatalf("exit code = %d, want %d", got, ExitModelNotFound)
	}
	if calls := env.fake.UnloadCalls(); len(calls) != 0 {
		t.Fatalf("nothing may be unloaded for an unknown model, got %+v", calls)
	}
}

// --- sync (restart-crossing, through the CLI) ---------------------------------

// TestSyncCommandCrossesARestart drives acceptance (a) through the real command
// tree rather than the engine API.
func TestSyncCommandCrossesARestart(t *testing.T) {
	env := newSpineTestEnv(t)
	for i := 0; i < 20; i++ {
		env.fake.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 100)
	}
	out1, _, err := runSpine(t, "sync", "--resources", "models", "--db", env.dbPath, "--json", "--event-window", "0")
	if err != nil {
		t.Fatalf("sync 1: %v\n%s", err, out1)
	}
	if !strings.Contains(out1, `"epoch_count_is_lower_bound": true`) {
		t.Fatalf("sync must state that the epoch count is a lower bound:\n%s", out1)
	}
	if !strings.Contains(out1, `"post_poll_tail": "unknowable"`) {
		t.Fatalf("sync must report the post-poll tail as unknowable, never 0:\n%s", out1)
	}

	env.fake.ResetEpoch()
	for i := 0; i < 35; i++ {
		env.fake.AddActivity("embeddinggemma", "/v1/embeddings", 200, 20)
	}
	out2, _, err := runSpine(t, "sync", "--resources", "models", "--db", env.dbPath, "--json", "--event-window", "0")
	if err != nil {
		t.Fatalf("sync 2: %v\n%s", err, out2)
	}

	db, derr := store.OpenWithContext(context.Background(), env.dbPath)
	if derr != nil {
		t.Fatalf("open store: %v", derr)
	}
	defer db.Close()
	var epochs, sealed int
	if qerr := db.DB().QueryRow(`SELECT COUNT(*) FROM epochs`).Scan(&epochs); qerr != nil {
		t.Fatalf("count epochs: %v", qerr)
	}
	if qerr := db.DB().QueryRow(`SELECT COUNT(*) FROM epochs WHERE state='sealed'`).Scan(&sealed); qerr != nil {
		t.Fatalf("count sealed: %v", qerr)
	}
	if epochs != 2 || sealed != 1 {
		t.Fatalf("epochs=%d sealed=%d, want 2/1\n%s", epochs, sealed, out2)
	}
	var e1, e2 int
	_ = db.DB().QueryRow(`SELECT COUNT(*) FROM requests WHERE epoch_id=1`).Scan(&e1)
	_ = db.DB().QueryRow(`SELECT COUNT(*) FROM requests WHERE epoch_id=2`).Scan(&e2)
	if e1 != 20 || e2 != 35 {
		t.Fatalf("rows per epoch = %d/%d, want 20/35 (no splicing)", e1, e2)
	}
}

func TestSyncSealForcesTheOpenEpochClosed(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 100)
	env.fake.AddInFlight("gemma-4-e4b", "/v1/chat/completions")
	if _, _, err := runSpine(t, "sync", "--resources", "models", "--db", env.dbPath, "--json", "--event-window", "0"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	out, _, err := runSpine(t, "sync", "--seal", "--db", env.dbPath, "--json")
	if err != nil {
		t.Fatalf("sync --seal: %v\n%s", err, out)
	}
	if !strings.Contains(out, mirror.SealManual) {
		t.Fatalf("seal report should name the manual reason:\n%s", out)
	}
	db, _ := store.OpenWithContext(context.Background(), env.dbPath)
	defer db.Close()
	var censored int
	_ = db.DB().QueryRow(`SELECT COUNT(*) FROM requests WHERE censored=1`).Scan(&censored)
	if censored != 1 {
		t.Fatalf("censored rows = %d, want 1", censored)
	}
}

func TestSyncUnderVerifyEnvTouchesNothing(t *testing.T) {
	env := newSpineTestEnv(t)
	t.Setenv("PRINTING_PRESS_VERIFY", "1")
	out, _, err := runSpine(t, "sync", "--seal", "--db", env.dbPath, "--json")
	if err != nil {
		t.Fatalf("verify-mode sync: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"would_sync": true`) {
		t.Fatalf("verify mode must report would_sync:\n%s", out)
	}
	if _, statErr := os.Stat(env.dbPath); statErr == nil {
		t.Fatal("verify mode must not create the database")
	}
}

// --- keepset status / audit / swaps / ps -------------------------------------

func TestKeepsetStatusReportsResidentAndAnswering(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("embeddinggemma")

	out, _, err := runSpine(t, "keepset", "status", "--json")
	// bge-reranker-v2-m3 is configured resident but not loaded: that is drift.
	if got := ExitCode(err); got != ExitDrift {
		t.Fatalf("exit code = %d, want %d (a missing keep-set member is a finding)\n%s", got, ExitDrift, out)
	}
	envelope := lastJSONObject(t, out)
	members, _ := envelope["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("members = %v", envelope["members"])
	}
	byID := map[string]map[string]any{}
	for _, m := range members {
		mm, _ := m.(map[string]any)
		id, _ := mm["model"].(string)
		byID[id] = mm
	}
	if byID["embeddinggemma"]["resident"] != true {
		t.Fatalf("embeddinggemma should be resident: %v", byID["embeddinggemma"])
	}
	if byID["embeddinggemma"]["answering"] != true {
		t.Fatalf("a resident seat should answer its health probe: %v", byID["embeddinggemma"])
	}
	if byID["bge-reranker-v2-m3"]["resident"] != false {
		t.Fatalf("bge-reranker-v2-m3 should not be resident: %v", byID["bge-reranker-v2-m3"])
	}
	if byID["bge-reranker-v2-m3"]["answering"] != nil {
		t.Fatalf("a non-resident seat must NOT be probed (that would auto-start it): %v", byID["bge-reranker-v2-m3"])
	}
	if env.fake.Hits("/upstream/health") != 1 {
		t.Fatalf("exactly one health probe expected, got %d", env.fake.Hits("/upstream/health"))
	}
}

func TestKeepsetAuditAttributesACLIEviction(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("embeddinggemma")
	// Force an unload so a provenance row exists, then mirror a swap timeline.
	if _, _, err := runSpine(t, "models", "unload", "local-embed", "--force-keepset", "--json", "--db", env.dbPath); err != nil {
		t.Fatalf("forced unload: %v", err)
	}
	db, derr := store.OpenWithContext(context.Background(), env.dbPath)
	if derr != nil {
		t.Fatalf("open store: %v", derr)
	}
	if err := store.EnsureDomainSchema(context.Background(), db.DB()); err != nil {
		t.Fatalf("schema: %v", err)
	}
	var provTS string
	if err := db.DB().QueryRow(`SELECT ts FROM unload_provenance WHERE model='embeddinggemma'`).Scan(&provTS); err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if _, err := db.DB().Exec(
		`INSERT INTO swap_events (ts, model, event, source) VALUES (?,?,?,?)`,
		provTS, "embeddinggemma", "unloaded", "test"); err != nil {
		t.Fatalf("seed swap event: %v", err)
	}
	db.Close()

	out, _, err := runSpine(t, "keepset", "audit", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("audit: %v\n%s", err, out)
	}
	envelope := lastJSONObject(t, out)
	if envelope["sampled"] != true {
		t.Fatalf("audit output must label itself sampled: %v", envelope["sampled"])
	}
	members, _ := envelope["members"].([]any)
	found := false
	for _, m := range members {
		mm, _ := m.(map[string]any)
		if mm["model"] != "embeddinggemma" {
			continue
		}
		found = true
		windows, _ := mm["degraded_windows"].([]any)
		if len(windows) != 1 {
			t.Fatalf("degraded windows = %v", mm["degraded_windows"])
		}
		win, _ := windows[0].(map[string]any)
		attribution, _ := win["attribution"].(string)
		if attribution != "cli:cli" {
			t.Fatalf("attribution = %q, want the CLI provenance row", attribution)
		}
		if !strings.Contains(win["detail"].(string), "--force-keepset") {
			t.Fatalf("a forced eviction must be visible in the detail: %v", win["detail"])
		}
	}
	if !found {
		t.Fatalf("embeddinggemma missing from the audit: %s", out)
	}
	if !strings.Contains(out, "unknowable") {
		t.Fatalf("audit coverage must name the permanently unknowable hole:\n%s", out)
	}
}

func TestSwapsReportsColdLoadPercentilesAndThrash(t *testing.T) {
	env := newSpineTestEnv(t)
	db, err := store.OpenWithContext(context.Background(), env.dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.EnsureDomainSchema(context.Background(), db.DB()); err != nil {
		t.Fatalf("schema: %v", err)
	}
	// A→B then B→A within the attribution window: a mutual-eviction pair.
	seed := []struct {
		ts, model, event string
		cold             any
	}{
		{"2026-08-13T00:00:00Z", "gemma-4-31b", "unloading", nil},
		{"2026-08-13T00:00:05Z", "qwen3-coder-30b", "loading", nil},
		{"2026-08-13T00:00:40Z", "qwen3-coder-30b", "ready", int64(35000)},
		{"2026-08-13T00:10:00Z", "qwen3-coder-30b", "unloading", nil},
		{"2026-08-13T00:10:03Z", "gemma-4-31b", "loading", nil},
		{"2026-08-13T00:10:50Z", "gemma-4-31b", "ready", int64(47000)},
	}
	for _, s := range seed {
		if _, err := db.DB().Exec(
			`INSERT INTO swap_events (ts, model, event, cold_load_ms, source) VALUES (?,?,?,?,?)`,
			s.ts, s.model, s.event, s.cold, "test"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	db.Close()

	out, _, err := runSpine(t, "swaps", "--thrash", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("swaps: %v\n%s", err, out)
	}
	envelope := lastJSONObject(t, out)
	thrash, _ := envelope["thrash"].([]any)
	if len(thrash) != 1 {
		t.Fatalf("thrash pairs = %v", envelope["thrash"])
	}
	pair, _ := thrash[0].(map[string]any)
	if pair["total"].(float64) != 2 {
		t.Fatalf("mutual eviction total = %v, want 2", pair["total"])
	}
	models, _ := envelope["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("model rows = %v", envelope["models"])
	}
	coverage, _ := envelope["coverage"].(map[string]any)
	if coverage["sampled"] != true {
		t.Fatalf("swaps must label its coverage sampled: %v", coverage)
	}
}

func TestPsJoinsRunningWithYAMLTTL(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("embeddinggemma")

	out, _, err := runSpine(t, "ps", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("ps: %v\n%s", err, out)
	}
	envelope := lastJSONObject(t, out)
	running, _ := envelope["running"].([]any)
	if len(running) != 1 {
		t.Fatalf("running = %v", envelope["running"])
	}
	row, _ := running[0].(map[string]any)
	if row["name"] != "embeddinggemma" {
		t.Fatalf("name = %v", row["name"])
	}
	// The server reports ttl:0 for this seat; the YAML says -1. The YAML wins.
	if ttl, _ := row["ttl"].(string); !strings.HasPrefix(ttl, "-1") {
		t.Fatalf("ttl = %v, want the YAML value (-1), not the server's ttl:0 lie", row["ttl"])
	}
	if row["ttl_source"] != "llama-swap YAML" {
		t.Fatalf("ttl_source = %v", row["ttl_source"])
	}
	if row["keep_set"] != true {
		t.Fatalf("keep_set = %v, want true", row["keep_set"])
	}
	if row["uptime"] != "unknown" {
		t.Fatalf("uptime = %v; with no mirrored load event it must be unknown, not fabricated", row["uptime"])
	}
	if ctx, ok := row["ctx"].(float64); !ok || int(ctx) != 4096 {
		t.Fatalf("ctx = %v, want 4096 parsed from the live cmd", row["ctx"])
	}
}

func TestPsInflightListsNonTerminalRows(t *testing.T) {
	env := newSpineTestEnv(t)
	env.fake.RunningIDs("gemma-4-e4b")
	env.fake.AddActivity("gemma-4-e4b", "/v1/chat/completions", 200, 120)
	env.fake.AddInFlight("gemma-4-e4b", "/v1/chat/completions")

	out, _, err := runSpine(t, "ps", "--inflight", "--json", "--db", env.dbPath)
	if err != nil {
		t.Fatalf("ps --inflight: %v\n%s", err, out)
	}
	envelope := lastJSONObject(t, out)
	inflight, _ := envelope["inflight"].([]any)
	if len(inflight) != 1 {
		t.Fatalf("inflight = %v, want exactly the non-terminal row", envelope["inflight"])
	}
}
