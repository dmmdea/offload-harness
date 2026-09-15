package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
)

// ampere16Replay is the render request the reference Linux node installs with
// (setup/install.sh step 5). The vLLM half is PINNED to nothing on purpose: this
// package's tests must not depend on whether the machine running them happens to
// have a hand-built venv, and the seed drift under test is the SERVED WINDOW,
// not the seat deployment.
func ampere16Replay(tier string) renderRequest {
	return renderRequest{
		TierID: tier, RAMTier: "mid", GOOS: "linux",
		LlamaBin: "/srv/offload/build/llamacpp/build/bin", ModelsDir: "/srv/offload/models",
		Listen: "127.0.0.1:11436", Home: "/srv/offload", Threads: 8,
		PinnedVLLM: &pinnedVLLM{},
	}
}

func stampedAtFixed() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }

// TestPreA39AmpereConfigIsReportedStaleNamingCtxSize is register K-02's KILL
// CRITERION, and it is a POSITIVE control: an audit that cannot be made to say
// STALE proves nothing by saying MATCH.
//
// The case is the real one. Register A-39 raised ampere-16's served window from
// 32768 to 131072 (commit 5e249dd, 0.113.32, 2026-09-07, "install defaults
// follow the measurement upward"). The Lenovo's rendered llama-swap config was
// never re-rendered, so it kept serving 32768 — and `local-offload audit-yaml`
// reported OK the entire time, because a config a tier revision behind breaks no
// operator rule. testdata/profiles-pre-a39-ampere-16.json is that tier's entry
// as it stood at 549f360, the commit before the raise, extracted verbatim from
// git history.
//
// The assertion is not merely "STALE": it is that the report NAMES ctx_size. A
// verdict that cannot say WHICH input moved sends an operator to diff two
// configs by eye, which is the work this row exists to remove.
func TestPreA39AmpereConfigIsReportedStaleNamingCtxSize(t *testing.T) {
	historical, err := os.ReadFile("testdata/profiles-pre-a39-ampere-16.json")
	if err != nil {
		t.Fatal(err)
	}
	old, err := deriveRender(historical, ampere16Replay("ampere-16"))
	if err != nil {
		t.Fatalf("rendering the pre-A-39 seed: %v", err)
	}
	if old.Params.Ctx != 32768 {
		t.Fatalf("the reconstructed pre-A-39 seed renders ctx %d, want 32768 — the control does not reproduce the defect", old.Params.Ctx)
	}
	if !strings.Contains(old.Config, "--ctx-size 32768") {
		t.Fatal("the pre-A-39 render does not carry --ctx-size 32768 — the control does not reproduce the defect")
	}
	stamped, err := servingtmpl.Stamp(old.Config, old.Basis, stampedAtFixed())
	if err != nil {
		t.Fatal(err)
	}

	// The rule audit is the incumbent gate, and it must still pass: that is the
	// whole reason a rule gate could not catch this.
	if vs := servingtmpl.Audit(stamped); len(vs) != 0 {
		t.Fatalf("the stale config breaks an operator rule, so this is not the case under test:\n%s", servingtmpl.Violations(vs))
	}

	rep := provenanceOf(stamped)
	t.Logf("%s", rep.Line("<pre-A-39 ampere-16 llama-swap.yaml>"))
	if rep.State != servingtmpl.StateStale {
		t.Fatalf("state = %s, want STALE (%s)", rep.State, rep.Detail)
	}
	if !containsKey(rep.Keys, "params.ctx_size") {
		t.Fatalf("STALE keys %v do not name params.ctx_size", rep.Keys)
	}
	if !containsKey(rep.Keys, "profiles_entry_sha256") {
		t.Errorf("STALE keys %v do not name profiles_entry_sha256 — the tier entry changed and the report should say so", rep.Keys)
	}
	// And the current seed must be the 131072 the register raised it to; if the
	// table were rolled back this control would pass for the wrong reason.
	now, err := deriveRender(embeddedProfiles, ampere16Replay("ampere-16"))
	if err != nil {
		t.Fatal(err)
	}
	if now.Params.Ctx != 131072 {
		t.Fatalf("this binary's ampere-16 seed renders ctx %d, want 131072 (register A-39) — the control is comparing against the wrong table", now.Params.Ctx)
	}
}

// TestFreshRenderVerifiesAgainstItsOwnBinary is the negative control the
// positive one needs: the SAME machinery must report MATCH for a config this
// binary just rendered, or STALE means nothing.
func TestFreshRenderVerifiesAgainstItsOwnBinary(t *testing.T) {
	for _, tc := range []struct{ tier, goos string }{
		{"ampere-16", "linux"},
		{"blackwell-2x16", "windows"}, // the Qube pair: a Windows template, a vLLM seat, two cards
	} {
		t.Run(tc.tier, func(t *testing.T) {
			req := ampere16Replay(tc.tier)
			req.GOOS = tc.goos
			res, err := deriveRender(embeddedProfiles, req)
			if err != nil {
				t.Fatalf("tier %s does not render for %s: %v", tc.tier, tc.goos, err)
			}
			stamped, err := servingtmpl.Stamp(res.Config, res.Basis, stampedAtFixed())
			if err != nil {
				t.Fatal(err)
			}
			rep := provenanceOf(stamped)
			if rep.State != servingtmpl.StateMatch {
				t.Fatalf("a config this binary just rendered for %s reports %s: %s (keys %v)", tc.goos, rep.State, rep.Detail, rep.Keys)
			}
			if rep.RenderedBy != buildinfo.Version {
				t.Errorf("rendered_by = %q, want %q", rep.RenderedBy, buildinfo.Version)
			}
			if rep.TierID != tc.tier {
				t.Errorf("tier = %q, want %q", rep.TierID, tc.tier)
			}
		})
	}
}

// TestLiveUnstampedConfigIsUnstamped: every config on the fleet today predates
// stamping. It must report UNSTAMPED — a finding — and never STALE, which would
// send an operator re-rendering a config nothing has shown to be wrong.
func TestLiveUnstampedConfigIsUnstamped(t *testing.T) {
	res, err := deriveRender(embeddedProfiles, ampere16Replay("ampere-16"))
	if err != nil {
		t.Fatal(err)
	}
	rep := provenanceOf(res.Config) // rendered, never stamped
	if rep.State != servingtmpl.StateUnstamped {
		t.Fatalf("state = %s, want UNSTAMPED for a config with no provenance block", rep.State)
	}
}

// TestReplayRefusesToClassifyTheAuditingMachine pins the one way this audit
// could produce a WRONG answer instead of no answer: a stamp naming no tier
// would make deriveRender classify whatever box is running the audit, and the
// report would then compare a node's config against a different machine's tier.
func TestReplayRefusesToClassifyTheAuditingMachine(t *testing.T) {
	if _, ok := replayRequest(servingtmpl.SpecBasis{}); ok {
		t.Error("a stamp with no tier and no fallback backend was accepted for replay")
	}
	if _, ok := replayRequest(servingtmpl.SpecBasis{TierID: "ampere-16"}); ok {
		t.Error("a stamp with no target OS was accepted for replay — the render would follow the auditing machine")
	}
	ok := false
	if _, ok = replayRequest(servingtmpl.SpecBasis{
		TierID: "ampere-16",
		Params: servingtmpl.ParamsBasis{GOOS: "linux"},
	}); !ok {
		t.Error("a complete stamp was refused for replay")
	}
	// Off-matrix: the tier id is a human label, so the fallback backend carries
	// the replay and the label must not be looked up as a table key.
	req, ok := replayRequest(servingtmpl.SpecBasis{
		TierID: "(off-matrix: cuda defaults)",
		Render: servingtmpl.RenderBasis{FallbackBackend: "cuda"},
		Params: servingtmpl.ParamsBasis{GOOS: "linux"},
	})
	if !ok {
		t.Fatal("an off-matrix stamp was refused for replay")
	}
	if req.TierID != "" || req.Fallback != "cuda" {
		t.Errorf("off-matrix replay = tier %q fallback %q, want empty tier and cuda", req.TierID, req.Fallback)
	}
}

// TestOffMatrixRenderStampsAndVerifies: a box that is not on the tier matrix
// still gets a stamp, and it still verifies. An off-matrix install is the one
// that most needs a provenance record, because no tier page describes it.
func TestOffMatrixRenderStampsAndVerifies(t *testing.T) {
	req := ampere16Replay("")
	req.Fallback = "cuda"
	res, err := deriveRender(embeddedProfiles, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.TierID != "(off-matrix: cuda defaults)" {
		t.Fatalf("tier id = %q", res.TierID)
	}
	if res.Basis.ProfilesEntrySHA256 != "" {
		t.Errorf("an off-matrix render has no tier entry; its hash must stay empty, got %q", res.Basis.ProfilesEntrySHA256)
	}
	stamped, err := servingtmpl.Stamp(res.Config, res.Basis, stampedAtFixed())
	if err != nil {
		t.Fatal(err)
	}
	if rep := provenanceOf(stamped); rep.State != servingtmpl.StateMatch {
		t.Fatalf("an off-matrix config does not verify against its own binary: %s -- %s (keys %v)", rep.State, rep.Detail, rep.Keys)
	}
}

func containsKey(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestServingConfigReporterCachesOnTheFile pins the two properties the health
// path needs: an unconfigured node reports nothing at all (nil, so both keys are
// omitted), and the verdict is not recomputed on every poll. Health is polled by
// every delegator on the fleet every few seconds, and the verdict costs a
// re-render -- an uncached reporter would spend the node's CPU answering a
// question whose answer only changes when a human re-renders the file.
func TestServingConfigReporterCachesOnTheFile(t *testing.T) {
	if servingConfigReporter("  ") != nil {
		t.Fatal("an unset serving_config_path must yield no reporter, so health omits both keys")
	}

	res, err := deriveRender(embeddedProfiles, ampere16Replay("ampere-16"))
	if err != nil {
		t.Fatal(err)
	}
	stamped, err := servingtmpl.Stamp(res.Config, res.Basis, stampedAtFixed())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(path, []byte(stamped), 0o644); err != nil {
		t.Fatal(err)
	}
	report := servingConfigReporter(path)
	sha, state := report()
	if state != string(servingtmpl.StateMatch) {
		t.Fatalf("state = %q, want MATCH", state)
	}
	if sha == "" {
		t.Fatal("a MATCH must publish the spec hash")
	}

	// Rewrite the file with a hand edit AND a new mtime: the cache key must move
	// with the file, or a node would publish MATCH over a config someone edited.
	edited := strings.Replace(stamped, "ttl: 300", "ttl: 1800", 1)
	if edited == stamped {
		t.Fatal("the rendered config carries no ttl: 300 -- the edit under test did not happen")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, state = report(); state != string(servingtmpl.StateHandEdited) {
		t.Errorf("state after a hand edit = %q, want HAND-EDITED", state)
	}

	// A missing file reports nothing rather than a guess: a node that cannot
	// read its own config must not publish a verdict about it.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if sha, state = report(); sha != "" || state != "" {
		t.Errorf("a missing config published sha=%q state=%q; both must be empty so health omits them", sha, state)
	}
}
