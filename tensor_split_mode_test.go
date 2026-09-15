package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A-25. `-sm tensor` on Qwen3-VL-32B is a FREE win, and the measurement is what makes
// it free rather than a trade: on the reference 5060 Ti pair (CUDA-X 2026-09-05),
// moving the seat from `-sm layer` to `-sm tensor` at the SAME `--tensor-split 25,25`
// produced BYTE-IDENTICAL output 5/5 on a fixed image+prompt while generation went
// 19.6 -> 34.1 tok/s (+74%) and prefill 995 -> 926 (-7%). It reached exactly one tier
// (blackwell-3x16, PR #266) and the register carried the rest of the row open:
// "`-sm tensor` on `qwen3-vl-32b` is in `blackwell-3x16` only; it is in no other tier
// template".
//
// The audit behind this gate: blackwell-3x16 is the ONLY tier that seats
// Qwen3-VL-32B at all. blackwell-2x16 was excluded on a recorded reason, not an
// oversight — the seat spans BOTH cards of its pair, and on a 2-card box the second
// card is the one paying the desktop/DWM tax, so that tier would be handing ~25 GB of
// its 32 GB to a swappable vision seat; it needs its own fit measurement first. Every
// other tier is single-card, where `-sm` is meaningless, or seats a model small enough
// for one card. So the row has nothing left to propagate — and the thing worth
// shipping is the INVARIANT, so that the next tier to seat a pair-spanning VLM cannot
// inherit the flagless form the measurement beat.
//
// Stated as a rule over the whole table rather than as one tier's assertion:
//
//  1. Any seat named for the measured model must carry split_mode "tensor".
//  2. Any seat pinned to MORE than one device must declare a split_mode — llama.cpp's
//     default is `-sm layer`, the arm this measurement beat by 74%.
//  3. A seat pinned to ONE device must declare NEITHER split_mode nor tensor_split:
//     the flags are meaningless there, and a meaningless flag copied between tiers is
//     how the 3-card media block became a byte-for-byte copy of the 2-card one.
//  4. tensor_split is positional over the seat's own visible devices, so its arity
//     must match the pin.
func TestEveryMultiCardMediaSeatCarriesTheMeasuredSplitMode(t *testing.T) {
	// The model the measurement is about, matched on the weights file so a rename of
	// the seat id cannot walk the gate.
	const measuredWeights = "Qwen3-VL-32B"

	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			MediaSeats []struct {
				Kind        string   `json:"kind"`
				Name        string   `json:"name"`
				Model       string   `json:"model"`
				SplitMode   string   `json:"split_mode"`
				TensorSplit string   `json:"tensor_split"`
				GPUEnv      []string `json:"gpu_env"`
			} `json:"media_seats"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	if len(doc.Profiles) == 0 {
		t.Fatal("no profiles read — this gate went blind")
	}

	sawMeasuredModel := false
	for tier, p := range doc.Profiles {
		for _, s := range p.MediaSeats {
			devs := devicePin(s.GPUEnv)

			if strings.Contains(s.Model, measuredWeights) {
				sawMeasuredModel = true
				if s.SplitMode != "tensor" {
					t.Errorf("%s seat %q runs %s with split_mode %q, want \"tensor\": at the SAME --tensor-split, "+
						"tensor mode produced byte-identical output 5/5 on a fixed image+prompt while raising "+
						"generation 19.6 -> 34.1 tok/s (CUDA-X 2026-09-05). Identical output is what makes it free",
						tier, s.Name, measuredWeights, s.SplitMode)
				}
			}

			switch {
			case len(devs) > 1 && s.SplitMode == "":
				t.Errorf("%s seat %q spans %d devices (%v) with no split_mode — llama.cpp then uses `-sm layer`, "+
					"the arm the 2026-09-05 measurement beat by 74%% on generation at identical output",
					tier, s.Name, len(devs), devs)
			case len(devs) == 1 && (s.SplitMode != "" || s.TensorSplit != ""):
				t.Errorf("%s seat %q is pinned to the single device %v but declares split_mode %q / tensor_split "+
					"%q — both are meaningless on one card, and a meaningless flag copied between tiers is the "+
					"shape that produced the unadapted 3-card media block", tier, s.Name, devs, s.SplitMode, s.TensorSplit)
			}

			if s.SplitMode != "" && s.TensorSplit == "" {
				t.Errorf("%s seat %q declares split_mode %q with no tensor_split — llama.cpp would divide it by its "+
					"own default rather than the measured proportions", tier, s.Name, s.SplitMode)
			}
			if s.TensorSplit != "" && len(devs) > 0 && len(strings.Split(s.TensorSplit, ",")) != len(devs) {
				t.Errorf("%s seat %q tensor_split %q does not match its %d visible device(s) %v — the list is "+
					"positional", tier, s.Name, s.TensorSplit, len(devs), devs)
			}
		}
	}
	if !sawMeasuredModel {
		t.Fatalf("no tier seats %s any more — either the seat was dropped (a measured 11-point MMMU regression, "+
			"see TestTripleBlackwellVisionSeatIsTheMeasuredWinner) or this gate went blind on a rename",
			measuredWeights)
	}
}

// devicePin returns the seat's CUDA_VISIBLE_DEVICES list, or nil when the seat
// inherits the tier's.
func devicePin(env []string) []string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(strings.TrimSpace(e), "CUDA_VISIBLE_DEVICES="); ok {
			var out []string
			for _, d := range strings.Split(v, ",") {
				if d = strings.TrimSpace(d); d != "" {
					out = append(out, d)
				}
			}
			return out
		}
	}
	return nil
}
