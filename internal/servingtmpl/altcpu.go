// altcpu.go — the CPU seat family a dual-route node serves BESIDE its GPU seats
// (operator direction 2026-09-20: binxarn, an AMD APU, runs both a Vulkan iGPU
// route and a CPU route on the same box, and the harness offers both).
//
// A tier is one backend; its template is one backend's flags. This file renders
// a second, CPU-backed seat family into the SAME llama-swap config: the two chat
// weights the tier already serves (offload-e4b, gemma4-e2b) as `<id>-cpu`
// entries run by a CPU llama-server build, joined to the interactive set so they
// stay mutually exclusive with everything else (one model resident at a time —
// on an APU every seat shares the same DDR pool). The flags are the cpu
// template's exact shape (no -ngl, no --flash-attn), so a `-cpu` seat is the
// cpu tier's seat, not an approximation of it.
//
// What this deliberately is NOT: a second tier, a second llama-swap, or a
// per-request backend switch inside llama-server. The caller picks the route by
// seat id (`gemma4-e2b` vs `gemma4-e2b-cpu`), exactly as it picks any other seat.
package servingtmpl

import (
	"fmt"
	"regexp"
	"strings"
)

// altCPUSeats are the primary chat entries a CPU family mirrors: (template model
// id, matrix var id, aliases). Only entries the target template defines are
// mirrored; a template without the workhorse is refused, never silently thinned.
var altCPUSeats = []struct {
	model, varID string
	aliases      string
}{
	{"offload-e4b", "e4bc", "[gemma4-e4b-cpu, offload-cpu]"},
	{"gemma4-e2b", "e2bc", "[e2b-cpu]"},
}

// altCPUCommon is the cpu template's `common` macro, token for token
// (setup/templates/llama-swap.linux-cpu.yaml / win-cpu.yaml): no -ngl, no
// --flash-attn. Rendered inline rather than via ${common} because the host
// template's ${common} carries the GPU flags.
const altCPUCommon = "--ctx-size __CTX__ --cache-type-k __KV_K__ --cache-type-v __KV_V__ --cache-ram __CACHE_RAM__ " +
	"--threads __NTHREADS__ --jinja --reasoning off --port ${PORT} --host 127.0.0.1"

// insertAltCPUSeats appends the CPU seat family to a rendered-in-progress
// template and returns the fragment to join to the swappable set (" | e4bc | e2bc").
// It runs after insertSeats and before token substitution, so the __CTX__ /
// __KV_K__ / __NTHREADS__ tokens resolve exactly as they do for every other entry.
func insertAltCPUSeats(tmpl string, p Params) (string, string, error) {
	if p.Backend == "cpu" {
		return "", "", fmt.Errorf("alt_llama_bin_cpu given for a cpu tier — the cpu backend is the primary route here, there is no second CPU route to render")
	}
	out := tmpl
	exe := ""
	if p.GOOS == "windows" {
		exe = ".exe"
	}
	// The loader-path macro, Linux only: a CPU build links its own libggml-cpu.so
	// beside the binary and the host template's ${ld} names the GPU build's dir.
	envLine := ""
	if p.GOOS != "windows" {
		const ldAnchor = "\n  ld: "
		i := strings.Index(out, ldAnchor)
		if i < 0 {
			return "", "", fmt.Errorf("the target template has no `ld:` loader macro to place `ldcpu` beside — a Linux template with a CPU family must carry one")
		}
		eol := strings.Index(out[i+1:], "\n")
		if eol < 0 {
			return "", "", fmt.Errorf("malformed macros block: `ld:` is the last line")
		}
		insertAt := i + 1 + eol + 1
		macro := fmt.Sprintf("  ldcpu: \"LD_LIBRARY_PATH=%s:${LD_LIBRARY_PATH:-}\"\n", strings.TrimRight(p.AltCPULlamaBin, "/"))
		if !strings.Contains(out, "\n  ldcpu: ") {
			out = out[:insertAt] + macro + out[insertAt:]
		}
		envLine = "    env: [\"${ldcpu}\"]\n"
	}
	taken := existingVars(out)
	frag := ""
	bin := strings.TrimRight(p.AltCPULlamaBin, "/")
	for i, s := range altCPUSeats {
		if !definesModel(out, s.model) {
			return "", "", fmt.Errorf("the CPU seat family mirrors %q, which the target template does not define — a tier that declares alt_backends [cpu] must render into a template that serves it", s.model)
		}
		id := s.model + "-cpu"
		if definesModel(out, id) {
			return "", "", fmt.Errorf("seat %q is already defined by the template — the CPU family is rendered, never hand-written into a template", id)
		}
		if taken[s.varID] {
			return "", "", fmt.Errorf("matrix var %q is already taken in this template", s.varID)
		}
		model := modelFileFor(out, s.model)
		if model == "" {
			return "", "", fmt.Errorf("could not read the --model path of %q from the template", s.model)
		}
		parallel := ""
		if i == 0 {
			parallel = "--parallel 1 "
		}
		header := ""
		if i == 0 {
			header = "  # ---- CPU route: the same weights served by a CPU llama-server build (alt_backends [cpu]).\n" +
				"  # Rendered by servingtmpl/altcpu.go from the tier; flags are the cpu template's shape\n" +
				"  # (no -ngl, no --flash-attn). Mutually exclusive with every other interactive seat.\n"
		}
		block := header +
			"  " + id + ":\n" +
			"    aliases: " + s.aliases + "\n" +
			envLine +
			"    cmd: >-\n" +
			"      " + bin + "/llama-server" + exe + " --model " + model + "\n" +
			"      " + parallel + altCPUCommon + "\n" +
			"    checkEndpoint: /health\n" +
			"    ttl: 300\n"
		var err error
		if out, err = appendModel(out, block); err != nil {
			return "", "", err
		}
		if out, err = addMatrixVar(out, s.varID, id); err != nil {
			return "", "", err
		}
		taken[s.varID] = true
		frag += " | " + s.varID
	}
	return out, frag, nil
}

// modelEntryStartRe matches the start of a model entry (`  <name>:` on its own
// line) or a top-level key, which ends the models map. Go's regexp has no
// lookahead, so entries are split by their start offsets instead of matched whole.
var modelEntryStartRe = regexp.MustCompile(`(?m)^(  [A-Za-z0-9._-]+:\s*$|[A-Za-z][A-Za-z0-9_-]*:)`)

// modelFlagRe reads the weights path from a `--model <path>` or `-m <path>`.
var modelFlagRe = regexp.MustCompile(`(?:--model|-m) (\S+)`)

// modelFileFor reads the `--model <path>` (or `-m <path>`) argument of a named
// model entry so the CPU twin serves the identical weights file.
func modelFileFor(tmpl, name string) string {
	starts := modelEntryStartRe.FindAllStringIndex(tmpl, -1)
	for i, s := range starts {
		key := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(tmpl[s[0]:s[1]]), ":"))
		if key != name || !strings.HasPrefix(tmpl[s[0]:], "  ") {
			continue
		}
		end := len(tmpl)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		if f := modelFlagRe.FindStringSubmatch(tmpl[s[1]:end]); f != nil {
			return f[1]
		}
		return ""
	}
	return ""
}
