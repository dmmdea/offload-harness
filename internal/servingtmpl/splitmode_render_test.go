package servingtmpl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/mediaseat"
)

// A-25, render side. The tier table can declare split_mode and the rendered llama-swap
// entry can still not carry `-sm tensor`: the seat block is built by hand in
// servingtmpl, and a field nothing renders is a field that measured nothing. Until now
// the only gate on this flag read the JSON (`TestTripleBlackwellVisionSeatIsTheMeasuredWinner`),
// never the rendered command.
//
// The flag is worth +74% generation at byte-identical output (19.6 -> 34.1 tok/s on a
// fixed image+prompt, 5/5 identical, CUDA-X 2026-09-05), so both halves are asserted
// here: it is PRESENT with its measured proportions on the pair-spanning vision seat,
// and ABSENT from the single-card seats of the same tier, where `-sm` means nothing.
func TestTripleBlackwellRendersTheMeasuredSplitMode(t *testing.T) {
	seats := tierSeats(t, "blackwell-3x16")

	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "llama-swap.win-triple-blackwell.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p := params()
	p.GOOS = "windows"
	p.Ctx = 131072
	p.KVType = "q8_0"
	p.IncludeQ38 = true
	p.Home = "C:/offload"
	p.Seats = seats
	out, err := Render(string(b), p)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	cfg := parseSwapConfig(t, out)

	vision, ok := cfg.Models["qwen3-vl-32b"]
	if !ok {
		t.Fatalf("blackwell-3x16 renders no qwen3-vl-32b entry — the tier's measured vision winner is missing "+
			"from its own template. models: %v", renderedModelIDs(cfg))
	}
	for _, want := range []string{"-sm tensor", "--tensor-split 25,25"} {
		if !strings.Contains(vision.Cmd, want) {
			t.Errorf("rendered qwen3-vl-32b command is missing %q. The pair measurement is the reason this seat "+
				"is usable at all: -sm tensor at the same split gave byte-identical output 5/5 and 19.6 -> 34.1 "+
				"tok/s. cmd:\n%s", want, vision.Cmd)
		}
	}
	// A 32B does not fit one 16 GB card, and the flow-sequence form must survive as ONE
	// env entry — `env: [CUDA_VISIBLE_DEVICES=0,2]` reads as two items in YAML and hands
	// the seat a single card.
	if !hasEnvValue(vision.Env, "CUDA_VISIBLE_DEVICES=0,2") {
		t.Errorf("rendered qwen3-vl-32b env is %v, want a single entry CUDA_VISIBLE_DEVICES=0,2 — a 32B does not "+
			"fit one 16 GB card, and device 1 is the display card", vision.Env)
	}

	// Absence. Every other seat on this tier is single-card; `-sm` there is noise at
	// best, and copied noise is what the 3-card media block was made of.
	for id, m := range cfg.Models {
		if id == "qwen3-vl-32b" {
			continue
		}
		if len(devicesOf(m.Env)) > 1 {
			continue // a pair-spanning text seat legitimately carries -sm layer
		}
		if strings.Contains(m.Cmd, "-sm ") {
			t.Errorf("single-card seat %q renders a split-mode flag (%q) — `-sm` divides a model across cards and "+
				"means nothing on one", id, strings.TrimSpace(m.Cmd))
		}
	}
}

// tierSeats reads one tier's committed media seats. A fixture would pass while the
// shipped declaration was broken, which is the failure class this file exists for.
func tierSeats(t *testing.T, tier string) []mediaseat.Seat {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			MediaSeats []mediaseat.Seat `json:"media_seats"`
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
	return p.MediaSeats
}

func devicesOf(env []string) []string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(strings.TrimSpace(e), "CUDA_VISIBLE_DEVICES="); ok {
			return strings.Split(v, ",")
		}
	}
	return nil
}

func renderedModelIDs(cfg swapConfig) []string {
	out := make([]string, 0, len(cfg.Models))
	for id := range cfg.Models {
		out = append(out, id)
	}
	return out
}
