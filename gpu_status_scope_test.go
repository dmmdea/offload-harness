package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// `gpu status` shows what the seat gates read for a legacy whole-node lease (plan P4): the
// cards the evidence rule scoped it to, whether that scope has widened, and, when it found
// nothing, the exact reason the lease stayed whole-node.

func legacyLeaseWithSidecar(t *testing.T, sidecar string) (string, *gpulease.Manager) {
	t.Helper()
	return legacyLeaseWithSidecarInference(t, sidecar, true)
}

// legacyLeaseWithSidecarInference holds a lease the way an OLDER binary wrote it, on a host
// whose config does (or does not) turn the inference of legacy leases on.
func legacyLeaseWithSidecarInference(t *testing.T, sidecar string, inference bool) (string, *gpulease.Manager) {
	t.Helper()
	cfg, m := scopedLeaseFixture(t)
	body := `{"state_dir": ` + strconv.Quote(m.Root()) + `, "gpu_card_scoped_leases": true, "gpu_legacy_scope_inference": ` + strconv.FormatBool(inference) + `}`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { modelaffinity.SetLegacyInference(false) })
	m.EmulateLegacyWriter()
	useQuietStatus(t, statusCards(), nil)
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return statusCards(), "", nil
	}))
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", Command: "python run.py"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	if sidecar != "" {
		body := strings.ReplaceAll(sidecar, "EPOCH", strconv.FormatUint(l.Epoch(), 10))
		if err := os.WriteFile(filepath.Join(m.Dir(), "seen."+strconv.FormatUint(l.Epoch(), 10)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, m
}

func TestGPUStatusJSONShowsAnInferredLegacyScope(t *testing.T) {
	cfg, _ := legacyLeaseWithSidecar(t, `{"epoch":EPOCH,"devices":["gpu-cccc0000-x"],"source":"command line","widened":true}`)
	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("one JSON document: %v\n%s", err, out)
	}
	if doc["seat_scope"] != "inferred" || doc["scope_widened"] != true {
		t.Fatalf("seat_scope=%v scope_widened=%v, want inferred and widened\n%s", doc["seat_scope"], doc["scope_widened"], out)
	}
	devs, _ := doc["inferred_devices"].([]any)
	if len(devs) == 0 {
		t.Fatalf("inferred_devices missing: %s", out)
	}
	leases, _ := doc["leases"].([]any)
	if len(leases) != 1 {
		t.Fatalf("leases = %v", doc["leases"])
	}
	row, _ := leases[0].(map[string]any)
	if row["scope"] != "whole-node" || row["seat_scope"] != "inferred" {
		t.Fatalf("the record stays a whole-node claim while the seats read it as inferred: %v", row)
	}
}

func TestGPUStatusTextSaysWhyALegacyLeaseStayedWholeNode(t *testing.T) {
	cfg, _ := legacyLeaseWithSidecar(t, "")
	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "seats: the whole node") || !strings.Contains(out, "stays whole-node") {
		t.Fatalf("status must say what the text seats treat the lease as, and why:\n%s", out)
	}
}

func TestGPUStatusTextSaysAnInferredScopeAndItsWidening(t *testing.T) {
	cfg, _ := legacyLeaseWithSidecar(t, `{"epoch":EPOCH,"devices":["gpu-cccc0000-x"],"source":"command line","widened":true}`)
	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "seats: fenced on cards") || !strings.Contains(out, "scope-widened") {
		t.Fatalf("status must show the inferred cards and the widening:\n%s", out)
	}
}

// With the switch off (the default) a legacy lease is the whole node, and status says which key
// turns the inference on.
func TestGPUStatusSaysAnOlderBinarysLeaseIsNotInferredWhileTheSwitchIsOff(t *testing.T) {
	cfg, _ := legacyLeaseWithSidecarInference(t, `{"epoch":EPOCH,"devices":["gpu-cccc0000-x"],"source":"command line"}`, false)
	out := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "seats: the whole node") || !strings.Contains(out, "gpu_legacy_scope_inference") {
		t.Fatalf("status must say the inference is off and which key turns it on:\n%s", out)
	}
	if strings.Contains(out, "fenced on cards") {
		t.Fatalf("nothing is inferred while the switch is off:\n%s", out)
	}
}
