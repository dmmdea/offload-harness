// seatparallel.go - how many requests a seat serves at once, read from a serving config.
//
// The fleet node publishes a worker count (fleet_max_concurrent_jobs, default 4) and the delegator
// deals a node as many jobs as that count leaves free. On a single-slot seat (--parallel 1) the
// engine runs one request at a time, so a node that publishes four workers over it holds three jobs
// that are only waiting inside the engine, and the delegator counts them as parallel work. Two readers
// need the seat's real slot count: the tier-table test that keeps a seeded cap from drifting from the
// template it is served by, and `doctor`'s capacity row that compares the cap a node publishes with
// the live serving config.
package servingtmpl

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// autoSlots is what llama-server serves when --parallel is negative or "auto": llama.cpp's server.cpp
// sets n_parallel to 4 when the value is negative. A config that STATES -1 or auto therefore states a
// slot count; only a config that states nothing leaves the count to the pinned build's default, which
// this file cannot read.
const autoSlots = 4

var (
	// parallelFlagRe matches llama.cpp's slot count in every spelling of the flag: `--parallel N`,
	// `--parallel=N`, and the short `-np N` / `-np=N`. N is a number, which may be negative (-1), or the
	// word auto.
	parallelFlagRe = regexp.MustCompile(`(?:^|\s)(?:--parallel|-np)(?:\s+|=)(-?\d+|(?i:auto))\b`)
	// parallelEnvRe is the flag's environment twin, which llama-server reads the same way. A flag
	// on the command line overrides it, exactly as llama-server resolves the two.
	parallelEnvRe = regexp.MustCompile(`(?:^|[\s,\[])LLAMA_ARG_N_PARALLEL=(-?\d+|(?i:auto))\b`)
)

// SeatSlots is the request concurrency a serving config states for one seat, and where it states it.
type SeatSlots struct {
	// N is how many requests the seat handles at once; always at least 1.
	N int
	// Auto: a llama.cpp entry that states --parallel -1 or auto. N is autoSlots, which is what
	// llama-server resolves it to.
	Auto bool
	// Limit: the entry is not a llama.cpp one (a vLLM seat runs through a wrapper script) and N is its
	// llama-swap concurrencyLimit, which the vLLM seat's installer sets to the engine's max_num_seqs.
	Limit bool
}

// SeatParallel reports how many requests the llama.cpp entry that serves alias (a model key or one
// of its aliases) handles at the same time: its `--parallel N`, `--parallel=N`, `-np N` or
// LLAMA_ARG_N_PARALLEL=N, read from the entry as llama-swap runs it (a `${name}` macro replaced by
// its text first, the way Audit reads it). A flag on the command line beats the environment twin,
// and the last of several flags wins, as llama-server parses them. A stated -1 or auto reads as
// autoSlots.
//
// ok=false is "unknown", never a guess; SeatParallelWhy says which unknown it is.
func SeatParallel(text, alias string) (slots int, ok bool) {
	slots, why := SeatParallelWhy(text, alias)
	return slots, why == ""
}

// SeatParallelWhy is SeatParallel with the reason an unknown is unknown, as one operator-facing
// clause (empty when slots is known): the config does not parse, no entry answers to the alias (or two
// claim it), the entry is not a llama.cpp one (no llama-server in its command), or it states no slot
// count. An entry with no flag is unknown on purpose, because what the pinned llama.cpp build then
// defaults to is not something this file can read.
func SeatParallelWhy(text, alias string) (slots int, why string) {
	s, why := readSeatSlots(text, alias, false)
	return s.N, why
}

// SeatSlotsWhy is SeatParallelWhy for a reader that must also say what a seat that is not a llama.cpp
// one serves: an entry whose command does not run llama-server and that states a llama-swap
// concurrencyLimit (how a vLLM seat is rendered, limit = max_num_seqs) reads as that limit, and the
// result says so (SeatSlots.Limit). A llama.cpp entry is read exactly as SeatParallelWhy reads it, and
// its concurrencyLimit is never a slot count (llama-swap defaults it to 10 whatever the engine runs).
// The reason is empty when the slots are known.
func SeatSlotsWhy(text, alias string) (SeatSlots, string) {
	return readSeatSlots(text, alias, true)
}

func readSeatSlots(text, alias string, limitOK bool) (SeatSlots, string) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return SeatSlots{}, "no seat is named"
	}
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
		Macros any                       `yaml:"macros"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return SeatSlots{}, "the serving config is not parseable YAML"
	}
	name, entry, why := seatEntry(doc.Models, alias)
	if why != "" {
		return SeatSlots{}, why
	}
	macros := macroTable(doc.Macros)
	cmd, _ := entry["cmd"].(string)
	cmd = expandMacros(cmd, macros)
	if !strings.Contains(strings.ToLower(cmd), "llama-server") {
		if !limitOK {
			return SeatSlots{}, fmt.Sprintf("%s is not a llama.cpp entry (its command does not run llama-server)", name)
		}
		if n, ok := entryLimit(entry); ok {
			return SeatSlots{N: n, Limit: true}, ""
		}
		return SeatSlots{}, fmt.Sprintf("%s is not a llama.cpp entry (its command does not run llama-server) and states no concurrencyLimit", name)
	}
	if n, auto, found := lastStated(parallelFlagRe, cmd); found {
		return slotsOrWhy(name, n, auto)
	}
	if n, auto, found := lastStated(parallelEnvRe, expandMacros(flatEnv(entry["env"]), macros)); found {
		return slotsOrWhy(name, n, auto)
	}
	return SeatSlots{}, fmt.Sprintf("%s states no --parallel, -np or LLAMA_ARG_N_PARALLEL, so its slot count is not in the file", name)
}

// slotsOrWhy accepts a slot count of one or more; zero is not a count llama-server runs with.
func slotsOrWhy(name string, n int, auto bool) (SeatSlots, string) {
	if n < 1 {
		return SeatSlots{}, fmt.Sprintf("%s states a slot count of %d", name, n)
	}
	return SeatSlots{N: n, Auto: auto}, ""
}

// entryLimit reads the llama-swap concurrencyLimit of an entry: a whole number of one or more.
func entryLimit(entry map[string]any) (int, bool) {
	var n int64
	switch v := entry["concurrencyLimit"].(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case uint64:
		if v > math.MaxInt32 {
			return 0, false
		}
		n = int64(v)
	case float64:
		if v != math.Trunc(v) || v > math.MaxInt32 {
			return 0, false
		}
		n = int64(v)
	default:
		return 0, false
	}
	if n < 1 || n > math.MaxInt32 {
		return 0, false
	}
	return int(n), true
}

// seatEntry finds the model entry that answers to alias: the model key itself, else the one entry
// whose `aliases` list names it. Two entries claiming one alias is a config llama-swap refuses, so
// it has no answer here rather than a first-listed one. why is empty when an entry was found.
func seatEntry(models map[string]map[string]any, alias string) (name string, entry map[string]any, why string) {
	if m, ok := models[alias]; ok {
		return alias, m, ""
	}
	var names []string
	for n, m := range models {
		list, _ := m["aliases"].([]any)
		for _, a := range list {
			if s, _ := a.(string); strings.TrimSpace(s) == alias {
				names = append(names, n)
				break
			}
		}
	}
	switch len(names) {
	case 0:
		return "", nil, fmt.Sprintf("no entry of the serving config answers to %q", alias)
	case 1:
		return names[0], models[names[0]], ""
	}
	sort.Strings(names)
	return "", nil, fmt.Sprintf("%d entries (%s) all claim the alias %q", len(names), strings.Join(names, ", "), alias)
}

// lastStated returns the slot count in the last match of re: the number it states, or autoSlots (auto=true)
// for the word auto and for any negative number. found=false when there is no match or the digits do not
// fit an int.
func lastStated(re *regexp.Regexp, from string) (n int, auto, found bool) {
	all := re.FindAllStringSubmatch(from, -1)
	if len(all) == 0 {
		return 0, false, false
	}
	stated := all[len(all)-1][1]
	if strings.EqualFold(stated, "auto") {
		return autoSlots, true, true
	}
	n, err := strconv.Atoi(stated)
	if err != nil {
		return 0, false, false
	}
	if n < 0 {
		return autoSlots, true, true
	}
	return n, false, true
}
