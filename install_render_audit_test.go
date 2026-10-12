package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
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

// ---- A-87: a pin on identical cards is by UUID, and every UUID names a card -------------------------

const (
	boxCardA = "GPU-aaaa1111-0000-0000-0000-000000000001"
	boxCardB = "GPU-bbbb2222-0000-0000-0000-000000000002"
	boxCardC = "GPU-cccc3333-0000-0000-0000-000000000003"
)

// threeCardTable is a box with two identical cards and the display card between them on the bus.
func threeCardTable() []gpuprobe.Device {
	return []gpuprobe.Device{
		{Index: 0, UUID: boxCardA, Name: "NVIDIA GeForce RTX 5060 Ti"},
		{Index: 1, UUID: boxCardB, Name: "NVIDIA GeForce RTX 5070 Ti"},
		{Index: 2, UUID: boxCardC, Name: "NVIDIA GeForce RTX 5060 Ti"},
	}
}

// auditYAMLWithTable runs `audit-yaml` over text with a given card table.
func auditYAMLWithTable(t *testing.T, devs []gpuprobe.Device, tableErr error, text string) (string, error) {
	t.Helper()
	withCardTable(t, devs, tableErr)
	path := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	var runErr error
	out := captureStdout(t, func() { runErr = runAuditYAML([]string{path}) })
	return out, runErr
}

func pinnedYAML(pin string) string {
	return "models:\n  seat:\n    ttl: 300\n    env: [\"CUDA_VISIBLE_DEVICES=" + pin + "\", CUDA_DEVICE_ORDER=PCI_BUS_ID]\n    cmd: /bin/llama-server -m m.gguf -ngl 99 --port ${PORT}\n"
}

func TestAuditYAMLAppliesThePinRulesWithTheBoxCardTable(t *testing.T) {
	t.Run("an index pin on identical cards in a hand-maintained file fails", func(t *testing.T) {
		out, err := auditYAMLWithTable(t, threeCardTable(), nil, pinnedYAML("0,2"))
		if err == nil {
			t.Fatalf("audit-yaml passed CUDA_VISIBLE_DEVICES=0,2 on two identical cards:\n%s", out)
		}
		if n := strings.Count(out, "pin-by-index (seat): "); n != 1 {
			t.Errorf("want exactly one `pin-by-index (seat): ...` line, got %d:\n%s", n, out)
		}
		if !strings.Contains(out, "NVIDIA GeForce RTX 5060 Ti") {
			t.Errorf("the finding names the identical cards:\n%s", out)
		}
		if strings.Contains(out, ": OK") {
			t.Errorf("a refused file must not also print OK:\n%s", out)
		}
	})
	t.Run("the same file pinned by UUID prints OK", func(t *testing.T) {
		out, err := auditYAMLWithTable(t, threeCardTable(), nil, pinnedYAML(boxCardA+","+boxCardC))
		if err != nil || !strings.Contains(out, ": OK") {
			t.Fatalf("a UUID pin on identical cards is the sanctioned form; err=%v:\n%s", err, out)
		}
	})
	t.Run("three distinct card names: the index pin prints OK", func(t *testing.T) {
		devs := threeCardTable()
		devs[2].Name = "NVIDIA GeForce RTX 4090"
		out, err := auditYAMLWithTable(t, devs, nil, pinnedYAML("0,2"))
		if err != nil || !strings.Contains(out, ": OK") {
			t.Fatalf("no two cards share a name; err=%v:\n%s", err, out)
		}
	})
	t.Run("a UUID of a card that is not in the table fails", func(t *testing.T) {
		out, err := auditYAMLWithTable(t, threeCardTable(), nil, pinnedYAML("GPU-dddd0000-0000-0000-0000-000000000000"))
		if err == nil || !strings.Contains(out, "pin-unresolved (seat): ") {
			t.Fatalf("a pin that names no card must fail; err=%v:\n%s", err, out)
		}
	})
}

// A fresh render of a tier whose seeds pin by index is not the maintainer's choice: the seed cannot
// carry a UUID. It is reported as a note and never fails the audit, or the only way to satisfy the rule
// would be a hand edit, which turns the file HAND-EDITED and red under --against-render.
func TestAuditYAMLNotesASeededIndexPinOnAVerbatimRender(t *testing.T) {
	res, err := deriveRender(embeddedProfiles, renderReq("blackwell-3x16", "windows", &pinnedVLLM{}))
	if err != nil {
		t.Fatal(err)
	}
	stamped, err := servingtmpl.Stamp(res.Config, res.Basis, stampedAtFixed())
	if err != nil {
		t.Fatal(err)
	}
	// What the seeds pin by index, counted from the render itself.
	want := 0
	for _, m := range regexp.MustCompile(`CUDA_VISIBLE_DEVICES=([0-9,]+)`).FindAllStringSubmatch(res.Config, -1) {
		for _, idx := range strings.Split(m[1], ",") {
			if idx == "0" || idx == "2" {
				want++
				break
			}
		}
	}
	if want < 5 {
		t.Fatalf("control: the render pins only %d entries on the identical cards, so the rule would be tested against nothing", want)
	}

	out, runErr := auditYAMLWithTable(t, threeCardTable(), nil, stamped)
	if runErr != nil {
		t.Fatalf("a verbatim render must not fail the audit over its own seeds: %v\n%s", runErr, out)
	}
	notes := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "note: ") && strings.Contains(l, "pin-by-index (") {
			notes++
		}
	}
	if notes != want {
		t.Errorf("want one `note:` per entry pinned on the identical cards (%d), got %d:\n%s", want, notes, out)
	}
	if !strings.Contains(out, ": OK") {
		t.Errorf("the rule audit is clean and says so:\n%s", out)
	}

	// The same text with the stamp removed is a hand-maintained file: the pin is its maintainer's choice.
	if out, runErr := auditYAMLWithTable(t, threeCardTable(), nil, res.Config); runErr == nil || !strings.Contains(out, "pin-by-index (") {
		t.Errorf("the unstamped copy of the same render is judged as a hand-maintained file; err=%v:\n%s", runErr, out)
	}
	// A UUID that names no card is a violation in a verbatim render too: no seed ever writes one.
	bad := strings.Replace(res.Config, "CUDA_VISIBLE_DEVICES=2,", "CUDA_VISIBLE_DEVICES=GPU-dddd0000-0000-0000-0000-000000000000,", 1)
	if bad == res.Config {
		t.Fatal("the control edit found no CUDA_VISIBLE_DEVICES=2 to replace")
	}
	badStamped, err := servingtmpl.Stamp(bad, res.Basis, stampedAtFixed())
	if err != nil {
		t.Fatal(err)
	}
	if out, runErr := auditYAMLWithTable(t, threeCardTable(), nil, badStamped); runErr == nil || !strings.Contains(out, "pin-unresolved (") {
		t.Errorf("a UUID that names no card fails even a verbatim render; err=%v:\n%s", runErr, out)
	}
}

func TestAuditYAMLSaysWhenThePinRulesCouldNotRun(t *testing.T) {
	out, err := auditYAMLWithTable(t, nil, errors.New("nvidia-smi: not found"), pinnedYAML("0,2"))
	if err != nil {
		t.Fatalf("a missing card table must not change the exit status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no card table: pin rules not applied") {
		t.Errorf("a silent pass would read as a clean audit; the output must say the pin rules did not run:\n%s", out)
	}
}

// nvidia-smi is not run for a file that pins nothing: a CPU-only or Vulkan box has none to run.
func TestAuditYAMLDoesNotReadTheCardTableWhenNothingPins(t *testing.T) {
	prev := renderCardTable
	renderCardTable = func() ([]gpuprobe.Device, error) {
		t.Error("the card table was read for a file that pins no device")
		return nil, nil
	}
	t.Cleanup(func() { renderCardTable = prev })
	path := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(path, []byte("models:\n  m:\n    ttl: 300\n    cmd: /bin/llama-server -m m.gguf --port ${PORT}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var runErr error
	out := captureStdout(t, func() { runErr = runAuditYAML([]string{path}) })
	if runErr != nil || strings.Contains(out, "no card table") {
		t.Fatalf("a file with no pin needs no card table; err=%v:\n%s", runErr, out)
	}
}

// verbatimStamp is the yes/no form of the first checks of servingtmpl.AgainstRender, and both audits
// that exempt on a stamp (W4's off-matrix note, A-87's severity split) read it. If the two ever
// disagree, a file reported HAND-EDITED would still be trusted by the exemption.
func TestVerbatimStampAgreesWithTheProvenanceVerdict(t *testing.T) {
	res := tierRender(t, "cpu")
	stamped, err := servingtmpl.Stamp(res.Config, res.Basis, stampedAtFixed())
	if err != nil {
		t.Fatal(err)
	}
	edit := func(from, to string) string {
		out := strings.Replace(stamped, from, to, 1)
		if out == stamped {
			t.Fatalf("the control edit found no %q", from)
		}
		return out
	}
	for _, tc := range []struct {
		name      string
		text      string
		wantOK    bool
		wantState servingtmpl.State
	}{
		{"an intact render", stamped, true, servingtmpl.StateMatch},
		{"no stamp at all", res.Config, false, servingtmpl.StateUnstamped},
		{"a body edited after the stamp", edit("\nmodels:\n", "\nmodels:\n  # edited\n"), false, servingtmpl.StateHandEdited},
		{"a basis edited after the stamp", edit(`"goos":"linux"`, `"goos":"windows"`), false, servingtmpl.StateHandEdited},
		{"a spec hash edited", edit("# spec_sha256: ", "# spec_sha256: 0"), false, servingtmpl.StateHandEdited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := verbatimStamp(tc.text); ok != tc.wantOK {
				t.Errorf("verbatimStamp ok = %v, want %v", ok, tc.wantOK)
			}
			if got := provenanceOf(tc.text).State; got != tc.wantState {
				t.Errorf("the provenance verdict = %s, want %s: the two readers disagree about this file", got, tc.wantState)
			}
		})
	}
}
