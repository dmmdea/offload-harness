package servingtmpl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A-70: the llama.cpp 27B ships --parallel 1 and is FLAT under fan-out (34.6-37.4
// tok/s aggregate at c1..c32, and it 429s from c16: 6 of 16 and 22 of 32 requests
// failed in the baseline arm). On the SAME 5060 Ti pair, at the SAME -c, --parallel 8
// holds single-stream (W1 43.4 vs 43.6) and roughly TRIPLES aggregate: 78.7 / 110.8 /
// 100.7 / 106.8 at c4/8/16/32, 0 failures (arm par8-pair 2026-09-01,
// llamacpp-par8-pair.json, ledger "FAIRNESS ARM par8-pair").
//
// It ships as its own entry, and the reason is the thing this gate protects. llama.cpp
// DIVIDES -c among its slots: eight slots at --ctx-size 131072 serve 16,384 tokens
// each. The same night's Lenovo arm measured the mechanism in-house ("--parallel 8
// -c 32768" -> "16k/32k prompts exceed the 4k/slot window"). Both pair tiers declare
// qwen3.8-27b as their fallback agent lane at fallback_agent_ctx_tokens 131072, so
// moving the flag onto that seat would serve an eighth of the advertised window --
// the same advertise-more-than-you-serve defect blackwell-8 shipped once.
//
// So the assertions come in pairs: the flag is PRESENT on the twin and ABSENT from
// every seat whose window is load-bearing, on both templates and on the single-card
// templates the measurement never covered.
func TestPairSpanningTemplatesShipTheMeasuredFanoutTwin(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		pinned     bool // the 3-card template pins its pair-spanning seats
	}{
		{"dual-blackwell", "llama-swap.win-dual-blackwell.yaml", false},
		{"triple-blackwell", "llama-swap.win-triple-blackwell.yaml", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderPairTemplate(t, tc.file, true)
			cfg := parseSwapConfig(t, out)

			twin, ok := cfg.Models["qwen3.8-27b-par8"]
			if !ok {
				t.Fatalf("%s renders no qwen3.8-27b-par8 entry -- the measured 3x-aggregate operating point "+
					"(--parallel 8 on the pair) is not in the template at all", tc.file)
			}
			if !strings.Contains(twin.Cmd, "--parallel 8") {
				t.Errorf("%s twin does not carry --parallel 8; that flag IS the measurement (aggregate "+
					"78.7/110.8/100.7/106.8 at c4/8/16/32 vs the seat's flat 34.6-37.4). cmd:\n%s", tc.file, twin.Cmd)
			}
			// The -c the arm used: the tier macro, unchanged. par16 needed -c 49152 and
			// a 27,23 split to start at all; par8 ran at the seat's own context.
			if !strings.Contains(twin.Cmd, "--ctx-size 131072") {
				t.Errorf("%s twin does not render the measured --ctx-size 131072 (the -c arm par8-pair ran at); "+
					"cmd:\n%s", tc.file, twin.Cmd)
			}
			if twin.TTL == nil || *twin.TTL != 300 {
				t.Errorf("%s twin ttl = %v, want 300 -- every seat unloads after five idle minutes", tc.file, twin.TTL)
			}
			if tc.pinned && !hasEnvValue(twin.Env, "CUDA_VISIBLE_DEVICES=0,2") {
				t.Errorf("%s twin must be pinned to the 5060 Ti pair (0,2): the measurement is the PAIR arm, and "+
					"an unpinned -sm layer seat spreads onto the display card. env: %v", tc.file, twin.Env)
			}

			// The seat itself keeps its undivided window. This is the assertion that
			// makes the twin a twin instead of a rewrite.
			seat, ok := cfg.Models["qwen3.8-27b"]
			if !ok {
				t.Fatalf("%s renders no qwen3.8-27b entry", tc.file)
			}
			if !strings.Contains(seat.Cmd, "--parallel 1") {
				t.Errorf("%s moved --parallel off 1 on qwen3.8-27b. That seat is the tier's fallback agent lane at "+
					"fallback_agent_ctx_tokens 131072, and llama.cpp divides -c among slots -- N slots would serve "+
					"131072/N per request against an advertised 131072. cmd:\n%s", tc.file, seat.Cmd)
			}
			// The long twin, where it exists, is single-slot for the same reason.
			if long, ok := cfg.Models["qwen3.8-27b-262k"]; ok && !strings.Contains(long.Cmd, "--parallel 1") {
				t.Errorf("%s long-context twin is not --parallel 1 -- 262,144 divided by slots is not a long lane. "+
					"cmd:\n%s", tc.file, long.Cmd)
			}

			// Matrix membership: an entry no set names can never be scheduled.
			if !strings.Contains(cfg.Matrix.Sets["text"], "q38p") {
				t.Errorf("%s twin is not a member of the text set (%q) -- llama-swap would never schedule it",
					tc.file, cfg.Matrix.Sets["text"])
			}
			if cfg.Matrix.Vars["q38p"] != "qwen3.8-27b-par8" {
				t.Errorf("%s matrix var q38p = %q, want qwen3.8-27b-par8", tc.file, cfg.Matrix.Vars["q38p"])
			}
		})
	}
}

// The twin rides its parent's gate. A tier with include_qwen38 off must render neither,
// and must leave no dangling matrix reference behind -- the failure dropModel's own
// post-check exists to catch.
func TestDroppingQ38TakesTheFanoutTwinWithIt(t *testing.T) {
	for _, file := range []string{"llama-swap.win-dual-blackwell.yaml", "llama-swap.win-triple-blackwell.yaml"} {
		out := renderPairTemplate(t, file, false)
		for _, id := range []string{"qwen3.8-27b-par8", "q38p"} {
			if strings.Contains(out, id) {
				t.Errorf("%s with include_qwen38 off still mentions %q -- llama-swap rejects a set naming a var "+
					"that no model defines", file, id)
			}
		}
	}
}

// Absence where it must not appear. The measurement is a TWO-CARD result on a 5060 Ti
// pair; nothing licenses eight slots on a single-card tier, where the same eight
// recurrent-state buffers (~350 MB each on this GDN hybrid) would sit beside the whole
// 16.7 GB of weights on one card. win-cuda-resident is the 27B's single-card template
// (blackwell-32 / -48 / -72).
func TestSingleCardTemplatesKeepTheirSingleSlot27B(t *testing.T) {
	for _, file := range []string{
		"llama-swap.win-cuda-resident.yaml",
		"llama-swap.win-cuda.yaml",
		"llama-swap.linux-cuda.yaml",
		"llama-swap.win-cpu.yaml",
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", file))
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if strings.Contains(src, "qwen3.8-27b-par8") {
			t.Errorf("%s declares the fan-out twin; the arm that measured it ran on a TWO-CARD 5060 Ti pair and "+
				"says nothing about this tier", file)
		}
		for _, l := range strings.Split(src, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			if strings.Contains(l, "--parallel 8") {
				t.Errorf("%s carries --parallel 8 (%q) -- unmeasured on this tier", file, strings.TrimSpace(l))
			}
		}
	}
}

// renderPairTemplate renders one of the two pair-spanning Blackwell templates at the
// tier values both of them ship (ctx 131072, q8_0 KV) so the assertions read the real
// numbers rather than the test defaults.
func renderPairTemplate(t *testing.T, file string, includeQ38 bool) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", file))
	if err != nil {
		t.Fatal(err)
	}
	p := params()
	p.GOOS = "windows"
	p.Ctx = 131072
	p.KVType = "q8_0"
	p.IncludeQ38 = includeQ38
	out, err := Render(string(b), p)
	if err != nil {
		t.Fatalf("render %s: %v", file, err)
	}
	return out
}

type swapConfig struct {
	Models map[string]struct {
		Cmd     string   `yaml:"cmd"`
		Env     []string `yaml:"env"`
		Aliases []string `yaml:"aliases"`
		TTL     *int     `yaml:"ttl"`
	} `yaml:"models"`
	Matrix struct {
		Vars map[string]string `yaml:"vars"`
		Sets map[string]string `yaml:"sets"`
	} `yaml:"matrix"`
}

// parseSwapConfig reads the rendered DOCUMENT rather than regexing it: a regex over
// config text cannot tell a mapping from a comment, which is how four gates in this
// repo went blind at once.
func parseSwapConfig(t *testing.T, out string) swapConfig {
	t.Helper()
	var cfg swapConfig
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("rendered config is not parseable YAML: %v", err)
	}
	if len(cfg.Models) == 0 {
		t.Fatal("rendered config declares no models -- this gate went blind")
	}
	return cfg
}

func hasEnvValue(env []string, want string) bool {
	for _, e := range env {
		if strings.TrimSpace(e) == want {
			return true
		}
	}
	return false
}
