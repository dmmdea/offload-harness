package servingtmpl

import (
	"strings"
	"testing"
)

// The delegator counts a node by the workers it publishes, and the node's workers are only real up to the
// slots its agent seat serves. SeatParallel is the slots side: what the llama.cpp entry that answers to an
// alias runs at once, read the way llama-swap runs the entry (macros expanded, the alias resolved).
func TestSeatParallelReadsTheSlotsOfTheEntryThatAnswersToTheAlias(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   string
		alias string
		want  int    // 0 = unknown
		why   string // a fragment of the reason when unknown
	}{
		{"--parallel N", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel 3 --port ${PORT}
`, "seat", 3, ""},
		{"--parallel=N", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel=2 --port ${PORT}
`, "seat", 2, ""},
		{"-np N", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf -np 4 --port ${PORT}
`, "seat", 4, ""},
		{"-np=N", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf -np=5 --port ${PORT}
`, "seat", 5, ""},
		{"a Windows binary", `
models:
  seat:
    cmd: C:\llama\llama-server.exe --model m.gguf --parallel 1
`, "seat", 1, ""},
		{"the flag arrives through a ${macro} the cmd references", `
macros:
  common: >-
    --ctx-size 4096 --parallel 1 --jinja
models:
  seat:
    cmd: /bin/llama-server --model m.gguf ${common}
`, "seat", 1, ""},
		{"the flag arrives through a macro that references another macro", `
macros:
  slots: -np 6
  common: ${slots} --jinja
models:
  seat:
    cmd: /bin/llama-server --model m.gguf ${common}
`, "seat", 6, ""},
		{"a macro no entry references is never run", `
macros:
  unused: --parallel 8
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel 1
`, "seat", 1, ""},
		{"the environment twin", `
models:
  seat:
    env: ["LD_LIBRARY_PATH=/x", "LLAMA_ARG_N_PARALLEL=2"]
    cmd: /bin/llama-server --model m.gguf
`, "seat", 2, ""},
		{"the environment twin through a macro", `
macros:
  slots: LLAMA_ARG_N_PARALLEL=7
models:
  seat:
    env: ["${slots}"]
    cmd: /bin/llama-server --model m.gguf
`, "seat", 7, ""},
		{"a flag on the command line beats the environment twin, as llama-server resolves them", `
models:
  seat:
    env: ["LLAMA_ARG_N_PARALLEL=4"]
    cmd: /bin/llama-server --model m.gguf --parallel 1
`, "seat", 1, ""},
		{"the last of two flags wins, and adjacent flags both count", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf -np 1 --parallel 2
`, "seat", 2, ""},
		{"the alias of an entry", `
models:
  mimo-9b-agent:
    aliases: [mimo-9b, agent-seat]
    cmd: /bin/llama-server --model m.gguf --parallel 1
  other:
    aliases: [other-alias]
    cmd: /bin/llama-server --model o.gguf --parallel 4
`, "agent-seat", 1, ""},
		{"the model key beats an alias spelled the same elsewhere", `
models:
  seat:
    cmd: /bin/llama-server --model a.gguf --parallel 2
  other:
    aliases: [seat]
    cmd: /bin/llama-server --model o.gguf --parallel 4
`, "seat", 2, ""},
		{"two entries claim the alias, so there is no answer", `
models:
  a:
    aliases: [agent-seat]
    cmd: /bin/llama-server --model a.gguf --parallel 1
  b:
    aliases: [agent-seat]
    cmd: /bin/llama-server --model b.gguf --parallel 1
`, "agent-seat", 0, "all claim the alias"},
		{"no entry answers to the alias", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel 1
`, "missing", 0, "no entry"},
		{"an entry with no slot flag is unknown, never a guessed default", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --ctx-size 4096
`, "seat", 0, "states no --parallel"},
		{"a flag that only starts with the same letters is not the flag", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel-tokens 8 --npl 3
`, "seat", 0, "states no --parallel"},
		{"a vLLM entry is not a llama.cpp one", `
models:
  seat:
    cmd: /usr/bin/vllm serve /models/m --max-num-seqs 32 --parallel 4
`, "seat", 0, "not a llama.cpp entry"},
		{"an entry with no command", `
models:
  seat:
    aliases: [x]
`, "seat", 0, "not a llama.cpp entry"},
		{"a slot count of zero is not a count", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel 0
`, "seat", 0, "slot count of 0"},
		{"a negative slot count is llama.cpp's auto, which llama-server serves as 4 slots", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel -1
`, "seat", 4, ""},
		{"the word auto", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel auto
`, "seat", 4, ""},
		{"the word auto in any case, after an equals sign", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel=AUTO
`, "seat", 4, ""},
		{"-np -1", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf -np -1
`, "seat", 4, ""},
		{"the environment twin says auto too", `
models:
  seat:
    env: ["LLAMA_ARG_N_PARALLEL=-1"]
    cmd: /bin/llama-server --model m.gguf
`, "seat", 4, ""},
		{"a later explicit count beats an earlier auto", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel -1 --parallel 2
`, "seat", 2, ""},
		{"a word that only starts with auto is not auto", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel autonomous
`, "seat", 0, "states no --parallel"},
		{"a config that does not parse", "models: [", "seat", 0, "not parseable"},
		{"an empty alias", `
models:
  seat:
    cmd: /bin/llama-server --parallel 1
`, "  ", 0, "no seat is named"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SeatParallel(tc.cfg, tc.alias)
			if ok != (tc.want > 0) || got != tc.want {
				t.Fatalf("SeatParallel = %d, %v; want %d, ok=%v", got, ok, tc.want, tc.want > 0)
			}
			slots, why := SeatParallelWhy(tc.cfg, tc.alias)
			if tc.want > 0 {
				if slots != tc.want || why != "" {
					t.Fatalf("SeatParallelWhy = %d, %q; want %d and no reason", slots, why, tc.want)
				}
				return
			}
			if slots != 0 || why == "" || !strings.Contains(why, tc.why) {
				t.Fatalf("SeatParallelWhy = %d, %q; want 0 and a reason containing %q", slots, why, tc.why)
			}
		})
	}
}

// SeatSlotsWhy is the reader for a caller that must also say what a seat that is not a llama.cpp one serves.
// A vLLM seat is rendered as a wrapper script with a llama-swap concurrencyLimit equal to its max_num_seqs, so
// its slots ARE in the file even though no --parallel is.
func TestSeatSlotsWhyReadsAVLLMSeatsConcurrencyLimitAndNamesAnAutoCount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   string
		alias string
		want  SeatSlots
		why   string // a fragment of the reason when unknown
	}{
		{"a llama.cpp entry is read as SeatParallelWhy reads it", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel 3
`, "seat", SeatSlots{N: 3}, ""},
		{"a stated auto is flagged, with the 4 slots llama-server serves", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel -1
`, "seat", SeatSlots{N: 4, Auto: true}, ""},
		{"a vLLM entry reads as its concurrencyLimit", `
models:
  qwen3.8-27b-vllm:
    aliases: [agent-pool]
    cmd: /opt/seat/vllm-seat-cmd.sh
    proxy: http://127.0.0.1:18797
    concurrencyLimit: 32
`, "qwen3.8-27b-vllm", SeatSlots{N: 32, Limit: true}, ""},
		{"a vLLM entry through its alias", `
models:
  qwen3.8-27b-vllm:
    aliases: [agent-pool]
    cmd: /opt/seat/vllm-seat-cmd.sh
    concurrencyLimit: 4
`, "agent-pool", SeatSlots{N: 4, Limit: true}, ""},
		{"a vLLM entry that states no limit is unknown", `
models:
  seat:
    cmd: /opt/seat/vllm-seat-cmd.sh
`, "seat", SeatSlots{}, "states no concurrencyLimit"},
		{"a limit of zero is not a count", `
models:
  seat:
    cmd: /opt/seat/vllm-seat-cmd.sh
    concurrencyLimit: 0
`, "seat", SeatSlots{}, "not a llama.cpp entry"},
		{"a limit that is not a number is not a count", `
models:
  seat:
    cmd: /opt/seat/vllm-seat-cmd.sh
    concurrencyLimit: lots
`, "seat", SeatSlots{}, "states no concurrencyLimit"},
		{"a llama.cpp entry's concurrencyLimit is not its slots, the flag is", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf --parallel 1
    concurrencyLimit: 10
`, "seat", SeatSlots{N: 1}, ""},
		{"a llama.cpp entry with no flag is unknown whatever its concurrencyLimit", `
models:
  seat:
    cmd: /bin/llama-server --model m.gguf
    concurrencyLimit: 10
`, "seat", SeatSlots{}, "states no --parallel"},
		{"no entry answers", `
models:
  seat:
    cmd: /opt/seat/vllm-seat-cmd.sh
    concurrencyLimit: 8
`, "other", SeatSlots{}, "no entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := SeatSlotsWhy(tc.cfg, tc.alias)
			if got != tc.want {
				t.Fatalf("SeatSlotsWhy = %+v (%q); want %+v", got, why, tc.want)
			}
			if tc.want.N > 0 && why != "" || tc.want.N == 0 && !strings.Contains(why, tc.why) {
				t.Fatalf("SeatSlotsWhy reason = %q; want %q", why, tc.why)
			}
		})
	}
	// The llama.cpp-only reader keeps its meaning: a concurrencyLimit is never read as llama.cpp slots, which the seed
	// rule depends on (a vLLM tier is out of its scope because its seat is not a llama.cpp entry).
	vllm := `
models:
  seat:
    cmd: /opt/seat/vllm-seat-cmd.sh
    concurrencyLimit: 32
`
	if n, why := SeatParallelWhy(vllm, "seat"); n != 0 || !strings.Contains(why, "not a llama.cpp entry") {
		t.Fatalf("SeatParallelWhy read a vLLM entry: %d, %q", n, why)
	}
}
