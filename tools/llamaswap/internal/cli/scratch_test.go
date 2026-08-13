// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
)

// The allowlist is the feature. These cases are the contract: three flags may
// move, everything else is refused, and refusal must be loud.
func TestScratchAllowlistRefusesEverythingElse(t *testing.T) {
	allowed := []string{"--port 18797", "-c 4096", "--ctx-size 4096", "-ngl 40", "--n-gpu-layers 40", "--gpu-layers 0"}
	for _, spec := range allowed {
		key, value, err := scratchParseSet(spec)
		if err != nil {
			t.Fatalf("--set %q rejected at parse: %v", spec, err)
		}
		if _, ok := scratchCanonical(key); !ok {
			t.Errorf("--set %q should be allowed", spec)
		}
		if value == "" {
			t.Errorf("--set %q parsed an empty value", spec)
		}
	}
	// Every one of these has invalidated a real evaluation when silently
	// dropped or silently changed.
	refused := []string{"--pooling cls", "--reasoning on", "--flash-attn off", "--cache-type-k q8_0",
		"-m other.gguf", "--jinja false", "-sm layer", "--image-min-tokens 256"}
	for _, spec := range refused {
		key, _, err := scratchParseSet(spec)
		if err != nil {
			continue // rejected even earlier: fine
		}
		if _, ok := scratchCanonical(key); ok {
			t.Errorf("--set %q must be REFUSED (it would make the eval seat differ from production in a way the ranking cannot see)", spec)
		}
	}
}

func TestScratchParseSetRejectsMalformed(t *testing.T) {
	for _, spec := range []string{"", "ctx-size 4096", "-c", "--port"} {
		if _, _, err := scratchParseSet(spec); err == nil {
			t.Errorf("--set %q should be a usage error", spec)
		}
	}
	if key, value, err := scratchParseSet("--ctx-size=8192"); err != nil || key != "--ctx-size" || value != "8192" {
		t.Errorf("equals spelling parsed as %q/%q err=%v", key, value, err)
	}
}

func TestScratchApplyReplacesInPlaceAndKeepsSpelling(t *testing.T) {
	tokens := mcSplitCmd(measureRealSeatCmd)
	out, was, added := scratchApply(tokens, "--ctx-size", "4096")
	if was != "131072" || added {
		t.Fatalf("previous -c value = %q added=%v", was, added)
	}
	joined := strings.Join(out, " ")
	if !strings.Contains(joined, "-c 4096") {
		t.Errorf("override not applied: %s", joined)
	}
	// Everything else must survive byte-for-byte.
	for _, mustKeep := range []string{"--jinja", "--reasoning off", "--flash-attn on", "--cache-type-k f16", "-sm none", "-ngl 99"} {
		if !strings.Contains(joined, mustKeep) {
			t.Errorf("scratch cmd dropped %q - that is the dropped-flag class this command exists to prevent:\n%s", mustKeep, joined)
		}
	}
	if got := scratchAppliedSpelling(tokens, "--ctx-size"); got != "-c" {
		t.Errorf("spelling = %q, want the production spelling -c", got)
	}
	if got := scratchAppliedSpelling(mcSplitCmd(measureEmbedSeatCmd), "--ctx-size"); got != "--ctx-size" {
		t.Errorf("spelling = %q, want --ctx-size", got)
	}
}

func TestScratchApplyAddsAbsentFlag(t *testing.T) {
	tokens := mcSplitCmd(measureEmbedSeatCmd)
	out, was, added := scratchApply(tokens, "--n-gpu-layers", "0")
	if !added || was != "" {
		t.Fatalf("expected an added flag, got was=%q added=%v", was, added)
	}
	if !strings.HasSuffix(strings.Join(out, " "), "--n-gpu-layers 0") {
		t.Errorf("appended flag missing: %s", strings.Join(out, " "))
	}
}

func TestScratchPortBand(t *testing.T) {
	flags := &rootFlags{}
	for _, port := range []int{9200, 11436, 18795, 18800, 0} {
		_, err := scratchBuildPlan(context.Background(), nil, flags, nil, measureRealSeatCmd, port, nil)
		if err == nil {
			t.Errorf("port %d should be refused (outside %d-%d)", port, scratchPortMin, scratchPortMax)
			continue
		}
		var typed *cliError
		if !errors.As(err, &typed) || typed.code != ExitPortConflict {
			t.Errorf("port %d: want ExitPortConflict, got %v", port, err)
		}
	}
}

// A plan derived from --from-cmd must not touch the network at all.
func TestScratchPlanFromCmdIsOffline(t *testing.T) {
	flags := &rootFlags{}
	plan, err := scratchBuildPlan(context.Background(), nil, flags, nil, measureRealSeatCmd, 18798, []string{"-c 4096"})
	if err != nil {
		t.Fatalf("scratchBuildPlan: %v", err)
	}
	if plan.Source != "--from-cmd" {
		t.Errorf("source = %q", plan.Source)
	}
	if plan.Port != 18798 || !strings.Contains(plan.ScratchCmd, "--port 18798") {
		t.Errorf("port override missing: %s", plan.ScratchCmd)
	}
	if !strings.Contains(plan.ScratchCmd, "-c 4096") {
		t.Errorf("ctx override missing: %s", plan.ScratchCmd)
	}
	if len(plan.Diff) != 2 {
		t.Fatalf("expected exactly 2 diff lines (port + ctx), got %d: %+v", len(plan.Diff), plan.Diff)
	}
	if !strings.HasPrefix(plan.HealthURL, "http://127.0.0.1:") {
		t.Errorf("health URL must use the IPv4 loopback literal, got %q", plan.HealthURL)
	}
	if !plan.Hidden {
		t.Error("scratch seats must spawn hidden")
	}
}

func TestScratchRefusedOverrideIsAUsageError(t *testing.T) {
	flags := &rootFlags{}
	_, err := scratchBuildPlan(context.Background(), nil, flags, nil, measureRealSeatCmd, 18797, []string{"--pooling cls"})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "REFUSED") {
		t.Errorf("refusal must be loud, got: %v", err)
	}
	var typed *cliError
	if !errors.As(err, &typed) || typed.code != 2 {
		t.Errorf("want usage exit 2, got %v", err)
	}
}

func TestScratchPortFreeDetectsAListener(t *testing.T) {
	ln, err := net.Listen("tcp", net.JoinHostPort(mcLoopback, "0"))
	if err != nil {
		t.Skipf("cannot bind a probe listener: %v", err)
	}
	defer ln.Close()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	err = scratchPortFree(port)
	if err == nil {
		t.Fatal("a busy port must be refused: a scratch seat that attaches to somebody else's listener measures that listener")
	}
	var typed *cliError
	if !errors.As(err, &typed) || typed.code != ExitPortConflict {
		t.Errorf("want ExitPortConflict, got %v", err)
	}
}
