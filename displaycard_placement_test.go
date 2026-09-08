package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// blackwell-3x16 is the only tier whose device map contains a card the harness must
// not schedule onto: 0 = RTX 5060 Ti, 1 = RTX 5070 Ti *driving the display*,
// 2 = RTX 5060 Ti.
//
// The tier shipped in 0.113.32 with its ENTIRE media block copied byte-for-byte from
// blackwell-2x16, where the map is 0 = utility, 1 = fast. The copy therefore pinned
// the vision seat (11,751 MiB measured, 2026-09-05 CUDA-X baseline) and both media
// pools onto the display card, and never mentioned card 2 at all — which is also why
// the three-card box measured no media difference from the two-card one.
//
// Leaving ~3.5 GiB after the DWM tax is below the >=4 GiB floor established the hard
// way on 2026-09-04: a three-card engine at util 0.87-0.90 starved this card, Windows
// dropped to a 720p-class mode, and the box needed a reboot (clean event log, no TDR).
//
// Operator decision 2026-09-07: the 5070 Ti is CONTEXT/KV ONLY. It may grow the KV
// pool for long-context seats (78,506 -> 137,898 tokens across three cards) and the
// named opt-in over-2-card seats may span it, but no tier seat and no media pool is
// ever pinned to it.
func TestTripleBlackwellNeverSchedulesOntoTheDisplayCard(t *testing.T) {
	const tier = "blackwell-3x16"
	const displayCard = "1"

	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			ConfigSeed map[string]json.RawMessage `json:"config_seed"`
			MediaSeats []struct {
				Kind      string   `json:"kind"`
				Name      string   `json:"name"`
				Residency string   `json:"residency"`
				GPUEnv    []string `json:"gpu_env"`
			} `json:"media_seats"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	p, ok := doc.Profiles[tier]
	if !ok {
		t.Fatalf("tier %q not found — this gate went blind", tier)
	}
	if len(p.MediaSeats) == 0 {
		t.Fatalf("tier %q declares no media seats — this gate went blind", tier)
	}

	for _, s := range p.MediaSeats {
		for _, e := range s.GPUEnv {
			if strings.TrimSpace(e) == "CUDA_VISIBLE_DEVICES="+displayCard {
				t.Errorf("%s seat %q (%s) is pinned to CUDA_VISIBLE_DEVICES=%s — that is the "+
					"display card; starving it dropped the desktop to 720p on 2026-09-04",
					tier, s.Name, s.Kind, displayCard)
			}
		}
		// The template's own contract: swappable seats join the primary-card
		// alternatives on device 0; resident seats join as a co-resident
		// conjunction on device 2, which is what buys STT its concurrency.
		want := map[string]string{"swappable": "0", "resident": "2"}[s.Residency]
		if want == "" {
			continue
		}
		got := ""
		for _, e := range s.GPUEnv {
			if v, ok := strings.CutPrefix(strings.TrimSpace(e), "CUDA_VISIBLE_DEVICES="); ok {
				got = v
			}
		}
		if got != want {
			t.Errorf("%s seat %q is residency=%s so the triple-blackwell template expects "+
				"device %s, got %q", tier, s.Name, s.Residency, want, got)
		}
	}

	// DisTorch pool bindings name cards as "cuda:N" rather than through gpu_env.
	for key, val := range p.ConfigSeed {
		if !strings.Contains(key, "pool_") {
			continue
		}
		if strings.Contains(string(val), `"cuda:`+displayCard+`"`) {
			t.Errorf("%s seeds %s = %s — media pooling must not donate from the display card",
				tier, key, string(val))
		}
	}
}
