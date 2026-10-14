package main

import (
	"encoding/json"
	"errors"
	"fmt"
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

// offMatrixRender is the render the installer reaches through `install render --fallback-backend <b>`
// (install.ps1 passes it for a box that is not on the tier matrix). Before 0186 w4 the cuda one carried the
// literal `--cpu-moe -ngl 999` in its 26B entries; the installer no longer writes it, and the tests below
// that need the legacy shape inject it (withBareCPUMoe) instead of reading it from a render.
func offMatrixRender(t *testing.T, fallback string) renderResult {
	t.Helper()
	req := renderReq("", "linux", &pinnedVLLM{})
	req.Fallback = fallback
	res, err := deriveRender(embeddedProfiles, req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func offMatrixCUDARender(t *testing.T) renderResult { return offMatrixRender(t, "cuda") }

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

func TestRenderGateExemptsOnlyTheCPUBackend(t *testing.T) {
	off := offMatrixCUDARender(t)
	if err := renderGate(off); err != nil {
		t.Fatalf("the installer's own off-matrix render must pass the write gate with no exemption: %v", err)
	}

	// The flag in a table GPU tier's render is the defect: a backend with a card.
	gpu := tierRender(t, "blackwell-16")
	gpu.Config = withBareCPUMoe(t, gpu.Config, "-ngl 99 --parallel 1")
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
	// An off-matrix literal (vulkan still carries `-ngl 999` in moeLiteral) exempts nothing any more: it was
	// a concession to the installer writing `--cpu-moe -ngl 999`, and the installer stopped.
	vk := offMatrixRender(t, "vulkan")
	if vk.Profile.moeLiteral == "" || vk.Profile.Backend != "vulkan" {
		t.Fatalf("control: the vulkan off-matrix profile must carry a literal on a backend with a card, got %+v", vk.Profile)
	}
	lit := gpu
	lit.Profile = vk.Profile // moeLiteral set, backend vulkan: only the literal could be what exempts it
	if err := renderGate(lit); err == nil || !strings.Contains(err.Error(), "cpu-moe (") {
		t.Fatalf("an off-matrix literal on a backend with a card must not exempt the bare --cpu-moe, got %v", err)
	}
	vk.Config = withBareCPUMoe(t, vk.Config, "-ngl 999 --parallel 1")
	if err := renderGate(vk); err == nil || !strings.Contains(err.Error(), "cpu-moe (") {
		t.Fatalf("an off-matrix vulkan render that carries the bare --cpu-moe passed the write gate: %v", err)
	}

	// The shipped cpu tier renders no flag, so it passes by the rule and not by the exemption.
	if err := renderGate(tierRender(t, "cpu")); err != nil {
		t.Fatalf("the shipped cpu tier must pass the write gate: %v", err)
	}
}

// A partial placement that names no N used to render the every-expert form and be refused by the gate AFTER
// the flag was computed. It is refused at derive now, before any flag exists, and the specific refusal
// ("no N") is still the one the operator reads: the gate never sees a render with the flag in it.
func TestDeriveRenderKeepsTheSpecificNoNRefusal(t *testing.T) {
	doc := tableDoc(t)
	e := tierEntry(doc, "blackwell-16")
	e["moe_26b"] = "n_cpu_moe"
	e["n_cpu_moe_max"] = 14
	delete(e, "n_cpu_moe")
	res, err := deriveRender(marshalTable(t, doc), renderReq("blackwell-16", "linux", &pinnedVLLM{}))
	if err == nil || !strings.Contains(err.Error(), "no N") {
		t.Fatalf("moe_26b n_cpu_moe with no N must be refused by deriveRender with its own message, got err=%v", err)
	}
	if strings.Contains(res.Config, "--cpu-moe") {
		t.Fatalf("the refused tier still produced a config carrying the every-expert flag:\n%s", res.Config)
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
	// The renders of a box OFF the matrix are renders too: the installer reaches them with
	// --fallback-backend, and 0186 w4 made them clean (the full walk is install_nocpumoe_test.go).
	fallbacks := 0
	for _, fb := range []string{"cuda", "cuda-resident", "dual-cuda", "vulkan"} {
		for _, goos := range []string{"linux", "windows"} {
			req := renderReq("", goos, &pinnedVLLM{})
			req.Fallback = fb
			res, err := deriveRender(embeddedProfiles, req)
			if err != nil {
				if strings.Contains(err.Error(), "no serving template") {
					continue
				}
				t.Errorf("off-matrix %s (%s) does not render: %v", fb, goos, err)
				continue
			}
			fallbacks++
			if vs := servingtmpl.AuditCPUMoE(res.Config); len(vs) != 0 {
				t.Errorf("off-matrix %s (%s) renders the every-expert placement:\n%s", fb, goos, servingtmpl.Violations(vs))
			}
		}
	}
	if fallbacks < 6 {
		t.Fatalf("walked %d off-matrix renders — the guard went blind", fallbacks)
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

	t.Run("a stamped CLEAN off-matrix render prints OK, with no cpu-moe finding and no note", func(t *testing.T) {
		res := offMatrixCUDARender(t)
		if res.Basis.Render.FallbackBackend != "cuda" {
			t.Fatalf("control: the stamp records fallback %q", res.Basis.Render.FallbackBackend)
		}
		out, err := auditYAMLOn(t, stamp(t, res))
		if err != nil || !strings.Contains(out, ": OK") {
			t.Fatalf("a clean off-matrix render must print OK; err=%v:\n%s", err, out)
		}
		if strings.Contains(out, "cpu-moe") || strings.Contains(out, "note: ") {
			t.Errorf("a clean off-matrix render has no cpu-moe finding and nothing to note:\n%s", out)
		}
	})

	// What a binary <= 0.185.1 wrote for a box off the tier matrix: the 26B entries with the every-expert
	// literal, under a verbatim stamp that records the fallback. The installer stopped writing it, so the
	// stamp no longer excuses it: it is a finding that fails the audit and says how to fix it.
	t.Run("a legacy stamped off-matrix render that carries the flag is a violation that says to re-render", func(t *testing.T) {
		res := offMatrixCUDARender(t)
		res.Config = withBareCPUMoe(t, res.Config, "--n-gpu-layers 99 --parallel 1")
		out, err := auditYAMLOn(t, stamp(t, res))
		if err == nil {
			t.Fatalf("a legacy off-matrix render that parks every expert in RAM must fail the audit:\n%s", out)
		}
		if strings.Contains(out, ": OK") {
			t.Errorf("a refused file must not also print OK:\n%s", out)
		}
		var violation string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "note: ") && strings.Contains(l, "cpu-moe") {
				t.Errorf("the finding must not print as a note: %s", l)
			}
			if strings.Contains(l, "cpu-moe (") && !strings.HasPrefix(l, "note: ") {
				violation = l
			}
		}
		if violation == "" {
			t.Fatalf("the legacy off-matrix finding must be a `cpu-moe (` violation:\n%s", out)
		}
		for _, want := range []string{"re-render", "older binary"} {
			if !strings.Contains(violation, want) {
				t.Errorf("the violation must carry %q: %s", want, violation)
			}
		}
		if strings.Contains(violation, "unstamped file") || strings.Contains(violation, "no longer matches") {
			t.Errorf("a verbatim stamp is neither unstamped nor edited, and the text must not say so: %s", violation)
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

// auditYAMLWithTable runs `audit-yaml` over text with a given card table. Flags come before the file.
func auditYAMLWithTable(t *testing.T, devs []gpuprobe.Device, tableErr error, text string, flags ...string) (string, error) {
	t.Helper()
	withCardTable(t, devs, tableErr)
	path := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	var runErr error
	out := captureStdout(t, func() { runErr = runAuditYAML(append(flags, path)) })
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

// A pin that names no card leaves the seat on fewer cards than it was sized for, or on none. An index
// is the form a render writes, so it is judged in a verbatim render too (a seed's index pin that no
// card has is a render for a bigger box than this one, which a note would excuse into a dead seat).
func TestAuditYAMLRefusesAnIndexThatNamesNoCard(t *testing.T) {
	t.Run("a hand-maintained file", func(t *testing.T) {
		out, err := auditYAMLWithTable(t, threeCardTable(), nil, pinnedYAML("7"))
		if err == nil || !strings.Contains(out, "pin-unresolved (seat): ") || !strings.Contains(out, "index 7") {
			t.Fatalf("an index past the last card must fail and name the index; err=%v:\n%s", err, out)
		}
	})
	t.Run("a word the rules cannot read as a card", func(t *testing.T) {
		out, err := auditYAMLWithTable(t, threeCardTable(), nil, pinnedYAML("gpu_abc"))
		if err == nil || !strings.Contains(out, "pin-unresolved (seat): ") {
			t.Fatalf("a token that is neither an index nor a UUID must fail; err=%v:\n%s", err, out)
		}
	})
	t.Run("a verbatim render for three cards on a box with two", func(t *testing.T) {
		res, err := deriveRender(embeddedProfiles, renderReq("blackwell-3x16", "windows", &pinnedVLLM{}))
		if err != nil {
			t.Fatal(err)
		}
		stamped, err := servingtmpl.Stamp(res.Config, res.Basis, stampedAtFixed())
		if err != nil {
			t.Fatal(err)
		}
		out, runErr := auditYAMLWithTable(t, threeCardTable()[:2], nil, stamped)
		if runErr == nil || !strings.Contains(out, "pin-unresolved (") {
			t.Fatalf("a seed that pins index 2 on a two-card box leaves its seats on no card; err=%v:\n%s", runErr, out)
		}
	})
}

// The pin rules compare a file's pins with the box that runs the audit. A file that belongs to another
// machine has the other machine's UUIDs, so the comparison is wrong; the caller who knows says so.
func TestAuditYAMLOtherBoxSkipsThePinRules(t *testing.T) {
	foreign := pinnedYAML("GPU-dddd0000-0000-0000-0000-000000000000")
	t.Run("by default a pin that names no card on this box fails and says how to audit a foreign file", func(t *testing.T) {
		out, err := auditYAMLWithTable(t, threeCardTable(), nil, foreign)
		if err == nil || !strings.Contains(out, "pin-unresolved (seat): ") {
			t.Fatalf("the default is unchanged: this box's cards are the table; err=%v:\n%s", err, out)
		}
		if !strings.Contains(out, "--other-box") {
			t.Errorf("a finding that may be a foreign file must name the flag that says so:\n%s", out)
		}
	})
	t.Run("the flag skips the rules, says so and never reads the card table", func(t *testing.T) {
		prev := renderCardTable
		renderCardTable = func() ([]gpuprobe.Device, error) {
			t.Error("the card table was read for a file the caller said belongs to another machine")
			return nil, nil
		}
		t.Cleanup(func() { renderCardTable = prev })
		path := filepath.Join(t.TempDir(), "llama-swap.yaml")
		if err := os.WriteFile(path, []byte(foreign), 0o644); err != nil {
			t.Fatal(err)
		}
		var err error
		out := captureStdout(t, func() { err = runAuditYAML([]string{"--other-box", path}) })
		if err != nil || !strings.Contains(out, ": OK") {
			t.Fatalf("with the pin rules off the file is clean; err=%v:\n%s", err, out)
		}
		if !strings.Contains(out, "note: "+path+": ") || !strings.Contains(out, "pin rules not applied") || !strings.Contains(out, "--other-box") {
			t.Errorf("a silent pass would read as a clean audit; the output must say the pin rules did not run and why:\n%s", out)
		}
	})
	t.Run("the flag does not switch off any other rule", func(t *testing.T) {
		bad := strings.Replace(foreign, "ttl: 300", "ttl: 0", 1)
		bad = strings.Replace(bad, "-ngl 99", "-ngl 99 --cpu-moe", 1)
		out, err := auditYAMLWithTable(t, threeCardTable(), nil, bad, "--other-box")
		if err == nil || !strings.Contains(out, "ttl (seat): ") || !strings.Contains(out, "cpu-moe (seat): ") {
			t.Fatalf("--other-box skips the pin rules and nothing else; err=%v:\n%s", err, out)
		}
		if strings.Contains(out, "pin-unresolved") {
			t.Errorf("the pin rules were skipped:\n%s", out)
		}
	})
	t.Run("a file that pins nothing is silent about it", func(t *testing.T) {
		out, err := auditYAMLWithTable(t, threeCardTable(), nil, "models:\n  m:\n    ttl: 300\n    cmd: /bin/llama-server -m m.gguf --port ${PORT}\n", "--other-box")
		if err != nil || strings.Contains(out, "pin rules") {
			t.Fatalf("nothing pins, so there is nothing to skip; err=%v:\n%s", err, out)
		}
	})
}

// The pin rules read every token of a CUDA_VISIBLE_DEVICES value, and one they cannot read as a card is a
// violation. This is the guard that no shipped tier renders such a token: it walks every tier on both
// operating systems against a box with eight cards of eight different models, so the only findings
// possible are a pin that names no card or a token the rules cannot read.
func TestRenderedTiersPinOnlyWhatThePinRulesCanRead(t *testing.T) {
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	var cards []gpuprobe.Card
	for i := 0; i < 8; i++ {
		cards = append(cards, gpuprobe.Card{
			UUID: fmt.Sprintf("GPU-%08x-0000-0000-0000-000000000000", i+1), NvidiaIndex: i, Name: fmt.Sprintf("Model %d", i),
		})
	}
	pinRe := regexp.MustCompile(`CUDA_VISIBLE_DEVICES=`)
	walked, pins := 0, 0
	for _, tier := range sortedKeys(doc.Profiles) {
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
			pins += len(pinRe.FindAllString(res.Config, -1))
			if vs := servingtmpl.AuditPins(res.Config, cards); len(vs) != 0 {
				t.Errorf("tier %s (%s) renders a pin the pin rules cannot place on a box of eight distinct cards:\n%s", tier, goos, servingtmpl.Violations(vs))
			}
		}
	}
	if walked < 20 || pins < 20 {
		t.Fatalf("walked %d tier renders holding %d pins — the guard went blind", walked, pins)
	}
}
