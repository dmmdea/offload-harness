package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/servingtmpl"
)

// W4 at its two callers. servingtmpl.AuditCPUMoE is pure and exemption-free; the exemption belongs to
// whoever holds the evidence. `install render` holds the resolved profile and runs BEFORE the stamp
// exists (the stamp is prepended after the rule audit), so renderGate reads the profile. `audit-yaml`
// holds a file and a stamp, so it reads the stamp. A gate that read the stamp at render time would
// exempt nothing, and one that read the profile at audit time would have no profile.

// offMatrixCUDARender is the render the installer reaches through `install render --fallback-backend
// cuda` (install.ps1 passes it for a box that is not on the tier matrix). It carries the literal
// `--cpu-moe -ngl 999` in its 26B entries.
func offMatrixCUDARender(t *testing.T) renderResult {
	t.Helper()
	req := renderReq("", "linux", &pinnedVLLM{})
	req.Fallback = "cuda"
	res, err := deriveRender(embeddedProfiles, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Config, "--cpu-moe -ngl 999") {
		t.Fatalf("the off-matrix cuda render carries no `--cpu-moe -ngl 999`, so the exemption would be tested against nothing:\n%s", res.Config)
	}
	return res
}

func tierRender(t *testing.T, tier string) renderResult {
	t.Helper()
	res, err := deriveRender(embeddedProfiles, renderReq(tier, "linux", &pinnedVLLM{}))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// withBareCPUMoe puts the every-expert flag into a render's config the way a hand edit (or the retired
// selftest remediation) would, and fails when the anchor is gone.
func withBareCPUMoe(t *testing.T, cfg, anchor string) string {
	t.Helper()
	out := strings.Replace(cfg, anchor, "--cpu-moe -ngl 999 "+anchor, 1)
	if out == cfg {
		t.Fatalf("the control edit found no %q to put the flag before", anchor)
	}
	return out
}

func TestRenderGateExemptsOnlyTheCPUBackendAndTheOffMatrixLiteral(t *testing.T) {
	off := offMatrixCUDARender(t)
	if err := renderGate(off); err != nil {
		t.Fatalf("the installer's own off-matrix render must pass the write gate until the installer stops writing it: %v", err)
	}

	// The same text on a table GPU tier is the defect: no moeLiteral, a backend with a card.
	gpu := tierRender(t, "blackwell-16")
	gpu.Config = off.Config
	err := renderGate(gpu)
	if err == nil {
		t.Fatal("a table GPU tier whose render carries the bare --cpu-moe passed the write gate")
	}
	for _, want := range []string{"blackwell-16", "cpu-moe (", "INV-1", "not written"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must carry %q, got: %v", want, err)
		}
	}

	// The cpu backend has no card to overflow from. Same text, same tier otherwise: only the backend moves.
	cpu := gpu
	cpu.Profile.Backend = "cpu"
	if err := renderGate(cpu); err != nil {
		t.Fatalf("the cpu backend has no card to overflow from and must not be refused: %v", err)
	}
	// And a literal alone (no cpu backend) is the other half of the exemption.
	lit := gpu
	lit.Profile = off.Profile // backend cuda, moeLiteral set: only the literal can be what exempts it
	if err := renderGate(lit); err != nil {
		t.Fatalf("the off-matrix literal must stay exempt: %v", err)
	}

	// The shipped cpu tier renders no flag, so it passes by the rule and not by the exemption.
	if err := renderGate(tierRender(t, "cpu")); err != nil {
		t.Fatalf("the shipped cpu tier must pass the write gate: %v", err)
	}
}

// A partial placement that names no N renders the every-expert form; the existing, more specific
// refusal ("no N") must still be the one the operator reads.
func TestRenderGateKeepsTheMoreSpecificNoNRefusalFirst(t *testing.T) {
	doc := tableDoc(t)
	e := tierEntry(doc, "blackwell-16")
	e["moe_26b"] = "n_cpu_moe"
	e["n_cpu_moe_max"] = 14
	delete(e, "n_cpu_moe")
	res, err := deriveRender(marshalTable(t, doc), renderReq("blackwell-16", "linux", &pinnedVLLM{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Config, "--cpu-moe -ngl 999") {
		t.Fatalf("the control render carries no bare --cpu-moe:\n%s", res.Config)
	}
	if err := renderGate(res); err == nil || !strings.Contains(err.Error(), "no N") {
		t.Fatalf("moe_26b n_cpu_moe with no N must keep its own refusal, got %v", err)
	}
}

// Every table tier but cpu renders without the bare flag today; this is the guard that keeps the new
// write-gate rule from refusing a shipped install, and the one that goes red when a tier regresses.
func TestRenderedTiersCarryNoBareCPUMoE(t *testing.T) {
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	walked := 0
	for _, tier := range sortedKeys(doc.Profiles) {
		if doc.Profiles[tier].Backend == "cpu" {
			continue
		}
		for _, goos := range []string{"linux", "windows"} {
			res, err := deriveRender(embeddedProfiles, renderReq(tier, goos, &pinnedVLLM{}))
			if err != nil {
				if strings.Contains(err.Error(), "no serving template") {
					continue
				}
				t.Errorf("tier %s (%s) does not render: %v", tier, goos, err)
				continue
			}
			walked++
			if vs := servingtmpl.AuditCPUMoE(res.Config); len(vs) != 0 {
				t.Errorf("tier %s (%s) renders the every-expert placement:\n%s", tier, goos, servingtmpl.Violations(vs))
			}
		}
	}
	if walked < 20 {
		t.Fatalf("walked %d tier renders — the guard went blind", walked)
	}
}

// auditYAMLOn writes text to a scratch file and runs `audit-yaml` over it with no card table, so a test
// of the stamp rules never shells out to nvidia-smi.
func auditYAMLOn(t *testing.T, text string, flags ...string) (string, error) {
	t.Helper()
	withCardTable(t, nil, errors.New("nvidia-smi: not found"))
	path := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	var runErr error
	out := captureStdout(t, func() { runErr = runAuditYAML(append(flags, path)) })
	return out, runErr
}

func TestAuditYAMLRefusesAFileWithABareCPUMoE(t *testing.T) {
	out, err := auditYAMLOn(t, "models:\n  m26:\n    ttl: 300\n    cmd: >-\n      /bin/llama-server -m m.gguf --cpu-moe -ngl 999 --port ${PORT}\n")
	if err == nil {
		t.Fatalf("audit-yaml passed a file whose m26 entry parks every expert in RAM:\n%s", out)
	}
	if strings.Contains(out, ": OK") {
		t.Errorf("a refused file must not also print OK:\n%s", out)
	}
	if n := strings.Count(out, "cpu-moe (m26): "); n != 1 {
		t.Errorf("want exactly one `cpu-moe (m26): ...` line, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "unstamped") {
		t.Errorf("an unstamped file is judged as a box with a card, and the finding says so:\n%s", out)
	}
}

// The acceptance read-back: the sanctioned partial spill prints OK and exits clean.
func TestAuditYAMLLeavesThePartialSpillClean(t *testing.T) {
	out, err := auditYAMLOn(t, "models:\n  m26:\n    ttl: 300\n    cmd: >-\n      /bin/llama-server -m m.gguf --n-cpu-moe 14 -ngl 999 --port ${PORT}\n")
	if err != nil || !strings.Contains(out, ": OK") || strings.Contains(out, "cpu-moe") {
		t.Fatalf("a measured partial spill must print OK, got err=%v:\n%s", err, out)
	}
}

func TestAuditYAMLReadsTheExemptionFromTheStamp(t *testing.T) {
	stamp := func(t *testing.T, res renderResult) string {
		t.Helper()
		s, err := servingtmpl.Stamp(res.Config, res.Basis, stampedAtFixed())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	t.Run("a stamped cpu render is clean", func(t *testing.T) {
		res := tierRender(t, "cpu")
		if res.Basis.Params.Backend != "cpu" {
			t.Fatalf("control: the cpu tier's stamp records backend %q", res.Basis.Params.Backend)
		}
		res.Config = withBareCPUMoe(t, res.Config, "--port ${PORT} --host 127.0.0.1")
		out, err := auditYAMLOn(t, stamp(t, res))
		if err != nil || strings.Contains(out, "cpu-moe") {
			t.Fatalf("the cpu backend has no card to overflow from; err=%v:\n%s", err, out)
		}
	})

	t.Run("a stamped off-matrix render is a note and does not fail the audit", func(t *testing.T) {
		res := offMatrixCUDARender(t)
		if res.Basis.Render.FallbackBackend != "cuda" {
			t.Fatalf("control: the stamp records fallback %q", res.Basis.Render.FallbackBackend)
		}
		out, err := auditYAMLOn(t, stamp(t, res))
		if err != nil {
			t.Fatalf("an off-matrix render must not fail the audit: %v\n%s", err, out)
		}
		if !strings.Contains(out, ": OK") {
			t.Errorf("the rule audit is clean and says so:\n%s", out)
		}
		var note string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "note: ") && strings.Contains(l, "cpu-moe (") {
				note = l
			}
		}
		if note == "" {
			t.Fatalf("the off-matrix finding must print as a `note:` line:\n%s", out)
		}
		if !strings.Contains(note, "off-matrix") {
			t.Errorf("the note says why it is not a failure: %s", note)
		}
	})

	t.Run("a stamped table GPU tier is not exempt", func(t *testing.T) {
		res := tierRender(t, "blackwell-16")
		res.Config = withBareCPUMoe(t, res.Config, "-ngl 99 --parallel 1")
		out, err := auditYAMLOn(t, stamp(t, res))
		if err == nil || !strings.Contains(out, "cpu-moe (") {
			t.Fatalf("a GPU tier's stamp exempts nothing; err=%v:\n%s", err, out)
		}
		if strings.Contains(out, "note: ") && strings.Contains(out, "cpu-moe") && !strings.Contains(out, "violation") {
			t.Errorf("the finding must be a violation, not a note:\n%s", out)
		}
	})

	t.Run("an unstamped GPU render is a violation that says it was judged as a GPU box", func(t *testing.T) {
		res := tierRender(t, "blackwell-16")
		cfg := withBareCPUMoe(t, res.Config, "-ngl 99 --parallel 1")
		out, err := auditYAMLOn(t, cfg)
		if err == nil || !strings.Contains(out, "cpu-moe (") || !strings.Contains(out, "unstamped") {
			t.Fatalf("an unstamped file is judged as a box with a card; err=%v:\n%s", err, out)
		}
	})

	t.Run("a stamp whose body was edited afterwards exempts nothing", func(t *testing.T) {
		res := tierRender(t, "cpu")
		s := stamp(t, res)
		edited := withBareCPUMoe(t, s, "--port ${PORT} --host 127.0.0.1")
		out, err := auditYAMLOn(t, edited)
		if err == nil || !strings.Contains(out, "cpu-moe (") {
			t.Fatalf("a hand-edited body is not the render its stamp describes; err=%v:\n%s", err, out)
		}
	})
}
