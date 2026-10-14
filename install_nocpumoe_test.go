package main

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/servingtmpl"
)

// 0186 w4: the installer stops writing the every-expert `--cpu-moe`. INV-1: the cards do the inference and
// host RAM is overflow only, so the one sanctioned expert placement in RAM is a MEASURED partial
// `--n-cpu-moe N` up to the tier's n_cpu_moe_max. The cpu backend has no card to overflow from and keeps its
// exemption. Everything below is the other half of the 0.184.0 audit (AuditCPUMoE): that rule refused the
// flag in a render; this one makes the installer stop producing it.

// offMatrixReq is a render request for a box that is not on the tier matrix: no tier, a fallback backend.
func offMatrixReq(goos, fallback, ramTier string) renderRequest {
	req := renderReq("", goos, &pinnedVLLM{})
	req.Fallback, req.RAMTier = fallback, ramTier
	return req
}

// modelKeys lists the entries of a rendered config's models map.
func modelKeys(t *testing.T, cfg string) map[string]bool {
	t.Helper()
	var doc struct {
		Models map[string]any `yaml:"models"`
	}
	if err := yaml.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatalf("the rendered config does not parse: %v", err)
	}
	keys := map[string]bool{}
	for k := range doc.Models {
		keys[k] = true
	}
	return keys
}

// G1. A box off the tier matrix has no tier, no measurement and no declared fit, so the CUDA family renders
// no 26B (the same outcome as every tier that cannot fit it) and no off-matrix render of any backend carries
// the every-expert placement. The walk covers the RAM tiers a caller may pass (empty = the caller does not
// know) and both operating systems, and it is the proof that dropping the 26B from the cuda-resident and
// dual-cuda templates renders at all: no table tier does that on those two templates.
func TestOffMatrixFallbacksRenderNoEveryExpertFlag(t *testing.T) {
	cudaFamily := map[string]bool{"cuda": true, "cuda-resident": true, "dual-cuda": true}
	walked := map[string]int{}
	for _, fb := range []string{"cuda", "cuda-resident", "dual-cuda", "vulkan"} {
		for _, goos := range []string{"linux", "windows"} {
			for _, ram := range []string{"", "min", "mid"} {
				name := goos + "/" + fb + "/ram=" + ram
				res, err := deriveRender(embeddedProfiles, offMatrixReq(goos, fb, ram))
				if err != nil {
					if strings.Contains(err.Error(), "no serving template") {
						continue
					}
					t.Errorf("%s does not render: %v", name, err)
					continue
				}
				walked[goos+"/"+fb]++
				if vs := servingtmpl.AuditCPUMoE(res.Config); len(vs) != 0 {
					t.Errorf("%s renders the every-expert placement:\n%s", name, servingtmpl.Violations(vs))
				}
				if err := renderGate(res); err != nil {
					t.Errorf("%s is refused by the write gate: %v", name, err)
				}
				if !cudaFamily[fb] {
					continue
				}
				if res.Include26B {
					t.Errorf("%s keeps the 26B, which no measurement says fits the card", name)
				}
				keys := modelKeys(t, res.Config)
				for _, m := range []string{"gemma4-26b-a4b", agentModel26BKey} {
					if keys[m] {
						t.Errorf("%s still defines the %s entry", name, m)
					}
				}
			}
		}
	}
	for _, k := range []string{"linux/cuda", "windows/cuda", "windows/cuda-resident", "windows/dual-cuda", "linux/vulkan", "windows/vulkan"} {
		if walked[k] != 3 {
			t.Fatalf("walked %d of 3 RAM tiers for %s: the guard went blind", walked[k], k)
		}
	}
}

// agentModel26BKey is the 26B reasoning-on twin that rides the 26B's gate (servingtmpl's agentModel26B).
const agentModel26BKey = "gemma-4-26b-agent"

// G3. The flag text the placement resolver returns is the last place a bare `--cpu-moe` could be computed.
// A tier that names the every-expert form on a backend with a card, or the partial form with no N, gets no
// flag and no 26B (and deriveRender refuses it earlier, G4); the cpu backend keeps its inert form and a
// measured partial spill is untouched.
func TestMoePlacementNeverReturnsTheEveryExpertFormOnABackendWithACard(t *testing.T) {
	bare := regexp.MustCompile(`(^|\s)--cpu-moe(\s|$)`)
	for _, backend := range []string{"cuda", "cuda-resident", "dual-cuda", "vulkan", "dual-blackwell", "triple-blackwell"} {
		for _, tc := range []struct {
			mode string
			n    int
		}{{"cpu_moe", 0}, {"n_cpu_moe", 0}, {"n_cpu_moe", -3}} {
			for _, ram := range []string{"", "mid", "high"} {
				p := servingProfile{Backend: backend, Include26B: true, MoE26B: tc.mode, NCPUMoE: tc.n}
				flag, include := moePlacement(p, ram)
				if bare.MatchString(flag) {
					t.Errorf("%s %s n=%d ram=%q: moePlacement returned the every-expert form %q", backend, tc.mode, tc.n, ram, flag)
				}
				if flag != "" || include {
					t.Errorf("%s %s n=%d ram=%q: want no flag and no 26B, got %q include=%v", backend, tc.mode, tc.n, ram, flag, include)
				}
			}
		}
	}
	// The one exemption, documented: the cpu backend has no card, and its templates carry the token in no cmd.
	cpu := servingProfile{Backend: "cpu", Include26B: true, MoE26B: "cpu_moe"}
	if flag, include := moePlacement(cpu, "mid"); flag != "--cpu-moe -ngl 999" || !include {
		t.Errorf("the cpu backend keeps its placement, got %q include=%v", flag, include)
	}
	// The sanctioned form is untouched: a measured partial spill still renders.
	part := servingProfile{Backend: "cuda", Include26B: true, MoE26B: "n_cpu_moe", NCPUMoE: 14}
	if flag, include := moePlacement(part, "high"); flag != "--n-cpu-moe 14 -ngl 999" || !include {
		t.Errorf("a measured partial spill must still render, got %q include=%v", flag, include)
	}
}

// G4. deriveRender refuses a tier whose 26B placement would park every expert in host RAM, BEFORE any flag
// is computed, and says what to fix. It does not depend on the RAM tier: a `min` box used to have the 26B
// dropped silently instead of the defect being named. A tier that does not include the 26B is not refused
// (the seed rule allows moe_26b cpu_moe beside include_26b false).
func TestDeriveRenderRefusesAPlacementThatWouldPutEveryExpertInRAM(t *testing.T) {
	derive := func(t *testing.T, ram string, edit func(e map[string]any)) error {
		t.Helper()
		doc := tableDoc(t)
		edit(tierEntry(doc, "blackwell-16"))
		req := renderReq("blackwell-16", "linux", &pinnedVLLM{})
		req.RAMTier = ram
		_, err := deriveRender(marshalTable(t, doc), req)
		return err
	}
	for _, ram := range []string{"min", "high"} {
		t.Run("cpu_moe on a cuda tier, ram "+ram, func(t *testing.T) {
			err := derive(t, ram, func(e map[string]any) { e["moe_26b"] = "cpu_moe" })
			if err == nil {
				t.Fatal("deriveRender accepted a cuda tier whose 26B puts every expert in host RAM")
			}
			for _, want := range []string{"blackwell-16", "cpu_moe", "INV-1"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal must carry %q, got: %v", want, err)
				}
			}
		})
		t.Run("n_cpu_moe naming no N, ram "+ram, func(t *testing.T) {
			err := derive(t, ram, func(e map[string]any) {
				e["moe_26b"], e["n_cpu_moe_max"] = "n_cpu_moe", 14
				delete(e, "n_cpu_moe")
			})
			if err == nil {
				t.Fatal("deriveRender accepted a partial placement that names no N")
			}
			for _, want := range []string{"blackwell-16", "no N", "INV-1"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal must carry %q, got: %v", want, err)
				}
			}
		})
	}
	t.Run("a tier that does not include the 26B still derives", func(t *testing.T) {
		if err := derive(t, "high", func(e map[string]any) { e["include_26b"], e["moe_26b"] = false, "cpu_moe" }); err != nil {
			t.Fatalf("include_26b false names no placement to refuse: %v", err)
		}
	})
	t.Run("a measured partial spill still derives", func(t *testing.T) {
		if err := derive(t, "high", func(e map[string]any) {
			e["moe_26b"], e["n_cpu_moe"], e["n_cpu_moe_max"] = "n_cpu_moe", 14, 14
		}); err != nil {
			t.Fatalf("a spill with a named N within its ceiling is the sanctioned form: %v", err)
		}
	})
}

// G5 (cpuMoeExempt). Only the cpu backend may carry the every-expert flag past the write gate. The old
// second half of the exemption (any profile with an off-matrix literal) was a concession to the installer
// writing that literal, and the installer no longer does.
func TestCPUMoeExemptIsTheCPUBackendOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    servingProfile
		want bool
	}{
		{"cpu backend", servingProfile{Backend: "cpu"}, true},
		{"cpu backend with the inert literal", servingProfile{Backend: "cpu", moeLiteral: "--cpu-moe"}, true},
		{"cuda", servingProfile{Backend: "cuda"}, false},
		{"cuda with an off-matrix literal", servingProfile{Backend: "cuda", moeLiteral: "-ngl 999"}, false},
		{"vulkan with its off-matrix literal", servingProfile{Backend: "vulkan", moeLiteral: "-ngl 999"}, false},
	} {
		if got := cpuMoeExempt(tc.p); got != tc.want {
			t.Errorf("%s: cpuMoeExempt = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// nonCommentLines drops every line whose first non-space character is `#`. A comment that trails code on
// the same line stays: it is on a code line, and the pin below treats a flag in one as written.
func nonCommentLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// G8. Source pin over everything setup/ runs or renders: no non-comment line spells the every-expert
// placement (`--cpu-moe`, its short form `-cmoe`, or the LLAMA_ARG_CPU_MOE environment twin). The dashed flag
// is not the `cpu_moe` mode name, and `--n-cpu-moe` is a different string that does not match. The selftest
// used to INSERT the flag into the live llama-swap.yaml; install.ps1 computed it for the unknown-profile
// fallback; both were the installer writing what the audit refuses.
func TestInstallerSourcesWriteNoEveryExpertFlag(t *testing.T) {
	files := []string{"setup/install.ps1", "setup/selftest.ps1", "setup/install.sh", "setup/detect.ps1"}
	tmpls, err := filepath.Glob("setup/templates/llama-swap.*.yaml")
	if err != nil || len(tmpls) < 8 {
		t.Fatalf("found %d serving templates (err %v): the pin went blind", len(tmpls), err)
	}
	files = append(files, tmpls...)
	flag := regexp.MustCompile(`(?i)--cpu-moe|(^|[\s'"])-cmoe([\s'"]|$)|LLAMA_ARG_CPU_MOE`)
	for _, f := range files {
		text := repoFile(t, filepath.ToSlash(f))
		if len(text) < 200 {
			t.Fatalf("%s is nearly empty: the pin went blind", f)
		}
		for i, l := range nonCommentLines(text) {
			if flag.MatchString(l) {
				t.Errorf("%s writes the every-expert placement (non-comment line %d): %s", f, i+1, strings.TrimSpace(l))
			}
		}
	}
}

// G10. selftest.ps1 neither carries a `-CpuMoe` switch nor edits the live yaml, and its tier loop tells the
// operator what to do through Get-MoEFailureAdvice. The `-CpuMoe:$residentIsMoe` arguments of the ctx probe
// are the live part: G8 matches the dashed flag, not the PowerShell switch name, so they need their own pin.
func TestSelftestHasNoCpuMoeSwitchAndNoRemediator(t *testing.T) {
	text := repoFile(t, "setup/selftest.ps1")
	for i, l := range nonCommentLines(text) {
		if strings.Contains(strings.ToLower(l), "cpumoe") {
			t.Errorf("selftest.ps1 still has a CpuMoe switch or variable (non-comment line %d): %s", i+1, strings.TrimSpace(l))
		}
	}
	if strings.Contains(text, "Invoke-Remediate26B") {
		t.Error("selftest.ps1 still names Invoke-Remediate26B: the function that edits the live llama-swap.yaml is retired")
	}
	const seam = "if ($env:OFFLOAD_SELFTEST_DOT_SOURCE -eq '1') { return }"
	at := strings.Index(text, seam)
	if at < 0 {
		t.Fatalf("selftest.ps1 lost its dot-source seam line %q", seam)
	}
	if !strings.Contains(text[at:], "Get-MoEFailureAdvice") {
		t.Error("the tier loop (MAIN, below the dot-source seam) must call Get-MoEFailureAdvice for a 26B tier that fails")
	}
}
