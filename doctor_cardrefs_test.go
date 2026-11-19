package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// TestDoctorNamesAConfiguredCardUUIDNoLiveCardHas is the incident of 2026-11-19: one of three cards was replaced and
// the config kept the removed card's UUID in gpu_comfy_order and in the llama-swap seats' CUDA_VISIBLE_DEVICES. The
// ComfyUI order was dropped with one note line and the seats (a seat pinned only to a missing UUID sees no CUDA
// device and loads on the CPU) with none, and doctor said nothing. Doctor must now fail, name each stale UUID with
// the setting that holds it, and name the live card the stale settings do not name; and when the card table cannot be read it must
// say UNKNOWN and fail nothing.
func TestDoctorNamesAConfiguredCardUUIDNoLiveCardHas(t *testing.T) {
	const (
		liveA = "GPU-aaaa1111-0000-4000-8000-00000000000a"
		liveB = "GPU-bbbb2222-0000-4000-8000-00000000000b"
		liveC = "GPU-cccc3333-0000-4000-8000-00000000000c" // the replacement: the stale settings do not name it
		gone  = "GPU-dddd4444-0000-4000-8000-00000000000d" // the removed card
	)
	yaml := "models:\n" +
		"  embed-seat:\n    env: [\"CUDA_VISIBLE_DEVICES=" + gone + "\"]\n    cmd: llama-server -ngl 99 -m e.gguf\n" +
		"  chat-seat:\n    env: [\"CUDA_VISIBLE_DEVICES=" + liveB + "\"]\n    cmd: llama-server -ngl 99 -m c.gguf\n"
	servingPath := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(servingPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.GPUComfyOrder = "GPU-aaaa1111," + gone[:12] + ",GPU-bbbb2222"
	cfg.ServingConfigPath = servingPath

	prev := doctorGPURead
	t.Cleanup(func() { doctorGPURead = prev })

	doctorGPURead = func(context.Context) ([]gpuprobe.Device, error) {
		return []gpuprobe.Device{
			{Index: 0, UUID: liveA, Name: "Card X", TotalGiB: 16, FreeGiB: 16},
			{Index: 1, UUID: liveB, Name: "Card Y", TotalGiB: 16, FreeGiB: 16},
			{Index: 2, UUID: liveC, Name: "Card X", TotalGiB: 16, FreeGiB: 16},
		}, nil
	}
	var out strings.Builder
	err := doctorRun(cfg, nil, &out)
	got := out.String()
	if err == nil || !strings.Contains(err.Error(), "match no live card") {
		t.Fatalf("doctor must exit non-zero on a card UUID no live card has, got %v\n%s", err, got)
	}
	for _, want := range []string{
		"card refs:  FAIL  gpu_comfy_order names " + gone[:12],
		"embed-seat", gone, "loads it on the CPU",
		"replacement candidates): idx 2 Card X " + liveC,
		"fix: ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor output must contain %q:\n%s", want, got)
		}
	}
	for _, ln := range strings.Split(got, "\n") {
		if strings.HasPrefix(ln, "card refs:") && strings.Contains(ln, "chat-seat") {
			t.Errorf("a seat pinned to a live card is not stale: %s", ln)
		}
	}

	// Unknown is not absent: with no card table nothing is judged, and nothing fails.
	doctorGPURead = func(context.Context) ([]gpuprobe.Device, error) { return nil, errors.New("nvidia-smi: not on PATH") }
	out.Reset()
	if err := doctorRun(cfg, nil, &out); err != nil {
		t.Fatalf("an unreadable card table must not fail doctor: %v\n%s", err, out.String())
	}
	if got := out.String(); !strings.Contains(got, "card refs:  UNKNOWN") || strings.Contains(got, "card refs:  FAIL") {
		t.Errorf("an unreadable card table is UNKNOWN, never FAIL:\n%s", got)
	}
}
