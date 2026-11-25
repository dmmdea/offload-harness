// seatctx.go - the context window a serving config states for one seat, and a fingerprint of the entry
// that states it.
//
// A node advertises the window its seat serves and the delegator sizes contracts to it. The window the
// ENGINE serves is what counts; the serving config is the best thing to ask without loading a model
// (ADR 0106), so this file reads it the way llama-server does: the last -c / --ctx-size on the command
// line, else LLAMA_ARG_CTX_SIZE, shared between the seat's slots.
package servingtmpl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	// ctxFlagRe matches llama.cpp's context size in every spelling of the flag: `-c N`, `-c=N`,
	// `--ctx-size N` and `--ctx-size=N`. The value is taken as one word so a macro that did not
	// resolve (`${CTX}`) is told from a number.
	ctxFlagRe = regexp.MustCompile(`(?:^|\s)(?:--ctx-size|-c)(?:\s+|=)(\S+)`)
	// ctxEnvRe is the flag's environment twin; a flag on the command line beats it.
	ctxEnvRe = regexp.MustCompile(`(?:^|[\s,\[])LLAMA_ARG_CTX_SIZE=(\S+)`)
	// kvUnifiedRe matches --kv-unified and -kvu, which make llama-server share one KV buffer between the
	// slots, so a request may use the whole of -c.
	kvUnifiedRe = regexp.MustCompile(`(?:^|\s)(?:--kv-unified|-kvu)(?:\s|$)`)
)

// SeatCtx is the context window, in tokens per request, that the serving config's entry for alias
// states: for a llama-server entry the last -c / --ctx-size (the flag beats LLAMA_ARG_CTX_SIZE), divided
// by the stated --parallel / -np / LLAMA_ARG_N_PARALLEL because llama.cpp splits -c among its slots, or
// undivided when the entry passes --kv-unified. The entry is read as llama-swap runs it (a `${name}`
// macro replaced by its text first).
//
// n=0 is "unknown", never a guess, and why says which unknown: the config does not parse, no entry
// answers (or two claim the alias), the entry is not a llama.cpp one (a vLLM seat's window lives in its
// own wrapper script, never in this file), it states no or a zero -c, a value that does not resolve, or
// a slot count that is not stated (so the division cannot be done). why is empty when n is known.
func SeatCtx(text, alias string) (n int, why string) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return 0, "no seat is named"
	}
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
		Macros any                       `yaml:"macros"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return 0, "the serving config is not parseable YAML"
	}
	name, entry, why := seatEntry(doc.Models, alias)
	if why != "" {
		return 0, why
	}
	macros := macroTable(doc.Macros)
	cmd, _ := entry["cmd"].(string)
	cmd = expandMacros(cmd, macros)
	if !strings.Contains(strings.ToLower(cmd), "llama-server") {
		return 0, fmt.Sprintf("%s is not a llama.cpp entry (its command does not run llama-server), so its window is not in the file", name)
	}
	env := expandMacros(flatEnv(entry["env"]), macros)
	stated := ""
	if all := ctxFlagRe.FindAllStringSubmatch(cmd, -1); len(all) > 0 {
		stated = all[len(all)-1][1]
	} else if all := ctxEnvRe.FindAllStringSubmatch(env, -1); len(all) > 0 {
		stated = all[len(all)-1][1]
	} else {
		return 0, fmt.Sprintf("%s states no --ctx-size, -c or LLAMA_ARG_CTX_SIZE, so its window is not in the file", name)
	}
	total, err := strconv.Atoi(strings.Trim(stated, `"'`))
	if err != nil {
		return 0, fmt.Sprintf("%s states a context size (%s) that does not resolve to a number", name, stated)
	}
	if total <= 0 {
		return 0, fmt.Sprintf("%s states a context size of %d, which is the model's own", name, total)
	}
	if kvUnifiedRe.MatchString(cmd) {
		return total, ""
	}
	slots := 0
	if p, _, found := lastStated(parallelFlagRe, cmd); found {
		slots = p
	} else if p, _, found := lastStated(parallelEnvRe, env); found {
		slots = p
	}
	if slots < 1 {
		return 0, fmt.Sprintf("%s states --ctx-size %d but no --parallel, -np or --kv-unified, so what one request may use is not in the file", name, total)
	}
	n = total / slots
	if n < 1 {
		return 0, fmt.Sprintf("%s states --ctx-size %d over %d slots", name, total, slots)
	}
	return n, ""
}

// fingerprintFields are the parts of a serving-config entry that decide what the engine serves: how it
// is started, what it is started with, and where llama-swap sends requests. A rename of an alias or a
// change of the idle timeout leaves the fingerprint alone.
var fingerprintFields = []string{"cmd", "cmdStop", "env", "proxy", "checkEndpoint", "concurrencyLimit"}

// SeatEntryFingerprint is a sha256 (hex) over the serving-config entry that answers to alias, with its
// macros expanded, so an edit to the entry (or to a macro it uses) changes it and nothing else does. It
// is "" when the config is unreadable or no single entry answers to alias; two readers that both get ""
// agree that nothing is known.
func SeatEntryFingerprint(text, alias string) string {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return ""
	}
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
		Macros any                       `yaml:"macros"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return ""
	}
	name, entry, why := seatEntry(doc.Models, alias)
	if why != "" {
		return ""
	}
	macros := macroTable(doc.Macros)
	picked := map[string]any{"name": name}
	for _, f := range fingerprintFields {
		if v, ok := entry[f]; ok {
			picked[f] = expandValue(v, macros)
		}
	}
	data, err := json.Marshal(picked) // map keys are written sorted, so the bytes are stable
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// expandValue replaces the macros in every string of a decoded YAML value.
func expandValue(v any, macros map[string]string) any {
	switch t := v.(type) {
	case string:
		return expandMacros(t, macros)
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = expandValue(x, macros)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = expandValue(x, macros)
		}
		return out
	}
	return v
}
