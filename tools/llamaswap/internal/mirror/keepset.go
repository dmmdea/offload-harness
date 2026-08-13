// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package mirror

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// KeepSet is the set of models that must not be unloaded.
//
// BINDING SOURCE RULE: the keep-set is derived from the llama-swap YAML
// (ttl:-1 / ttl:0 seats) unioned with the CLI's own config. It is NEVER derived
// from the server's ttl field — GET /running reports ttl:0 for a model
// configured ttl:-1 (verified live on v249), so a keep-set read off the API
// would be built on a value the API is known to misreport.
//
// Refusal matches ALIASES as well as canonical ids. On this deployment the mem0
// stack is reachable as text-embedding / local-embed / reranker-v2-m3 /
// v0.12-reranker; an id-only check would let `unload local-embed` through and
// take the memory stack down.
type KeepSet struct {
	Members  []KeepSetMember
	Sources  []string
	Warnings []string

	byName map[string]int // lowercased id or alias -> index into Members
}

// KeepSetMember is one protected model with every name it answers to.
type KeepSetMember struct {
	ID      string   `json:"id"`
	Aliases []string `json:"aliases,omitempty"`
	Origin  string   `json:"origin"`
}

// KeepSetOptions selects where the keep-set is read from. Empty fields fall
// back to environment variables and then to on-disk defaults.
type KeepSetOptions struct {
	// YAMLPath is the llama-swap config. Parsed READ-ONLY; this CLI never
	// writes the live YAML (its comments are the operator's decision record).
	YAMLPath string
	// ConfigPath is the CLI's own JSON config; an optional "keep_set" array
	// there unions with the YAML-derived set.
	ConfigPath string
	// Extra members supplied by a flag.
	Extra []string
}

// Env vars used to point the keep-set loader at non-default locations. Tests
// and non-Windows hosts rely on these; there is no hardcoded fallback beyond
// the documented Windows install path.
const (
	EnvYAMLPath = "LLAMASWAP_YAML"
	EnvKeepSet  = "LLAMASWAP_KEEP_SET"
	EnvConfig   = "LLAMASWAP_CONFIG"
)

// DefaultYAMLPath is this deployment's llama-swap config location.
const DefaultYAMLPath = `C:\llama-swap\llama-swap.yaml`

// LoadKeepSet resolves the keep-set. It never fails on a missing source: a
// missing YAML is a recorded warning, because the honest answer to "is this
// model protected?" when the config cannot be read is "unknown, and here is
// why", not a silent empty set.
func LoadKeepSet(opts KeepSetOptions) *KeepSet {
	ks := &KeepSet{byName: map[string]int{}}

	yamlPath := opts.YAMLPath
	if yamlPath == "" {
		yamlPath = os.Getenv(EnvYAMLPath)
	}
	if yamlPath == "" {
		if _, err := os.Stat(DefaultYAMLPath); err == nil {
			yamlPath = DefaultYAMLPath
		}
	}
	if yamlPath != "" {
		seats, err := ParseYAMLSeats(yamlPath)
		if err != nil {
			ks.Warnings = append(ks.Warnings, fmt.Sprintf("llama-swap YAML %s not readable (%v); keep-set falls back to CLI config only", yamlPath, err))
		} else {
			ks.Sources = append(ks.Sources, "yaml:"+yamlPath)
			for _, seat := range seats {
				if !seat.Resident() {
					continue
				}
				ks.add(KeepSetMember{
					ID:      seat.ID,
					Aliases: seat.Aliases,
					Origin:  fmt.Sprintf("yaml ttl=%s", seat.TTLText),
				})
			}
		}
	} else {
		ks.Warnings = append(ks.Warnings, "no llama-swap YAML found; set "+EnvYAMLPath+" or keep_set in the CLI config")
	}

	// CLI config: {"keep_set": ["embeddinggemma", "local-embed"]}
	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		cfgPath = os.Getenv(EnvConfig)
	}
	if cfgPath != "" {
		names, err := keepSetFromConfigFile(cfgPath)
		if err != nil && !os.IsNotExist(err) {
			ks.Warnings = append(ks.Warnings, fmt.Sprintf("CLI config %s: %v", cfgPath, err))
		} else if len(names) > 0 {
			ks.Sources = append(ks.Sources, "config:"+cfgPath)
			for _, n := range names {
				ks.addName(n, "cli-config")
			}
		}
	}

	if env := strings.TrimSpace(os.Getenv(EnvKeepSet)); env != "" {
		ks.Sources = append(ks.Sources, "env:"+EnvKeepSet)
		for _, n := range strings.Split(env, ",") {
			ks.addName(strings.TrimSpace(n), "env")
		}
	}
	for _, n := range opts.Extra {
		ks.addName(strings.TrimSpace(n), "flag")
	}
	return ks
}

func keepSetFromConfigFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		KeepSet []string `json:"keep_set"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse keep_set: %w", err)
	}
	return cfg.KeepSet, nil
}

func (k *KeepSet) add(m KeepSetMember) {
	if m.ID == "" {
		return
	}
	if idx, ok := k.byName[strings.ToLower(m.ID)]; ok {
		// Merge aliases into the existing member rather than duplicating it.
		existing := &k.Members[idx]
		for _, a := range m.Aliases {
			if _, seen := k.byName[strings.ToLower(a)]; !seen {
				existing.Aliases = append(existing.Aliases, a)
				k.byName[strings.ToLower(a)] = idx
			}
		}
		return
	}
	idx := len(k.Members)
	k.Members = append(k.Members, m)
	k.byName[strings.ToLower(m.ID)] = idx
	for _, a := range m.Aliases {
		k.byName[strings.ToLower(a)] = idx
	}
}

// addName adds a bare name from config/env/flag. If it already resolves to a
// known member (by id or alias) nothing is duplicated.
func (k *KeepSet) addName(name, origin string) {
	if name == "" {
		return
	}
	if _, ok := k.byName[strings.ToLower(name)]; ok {
		return
	}
	k.add(KeepSetMember{ID: name, Origin: origin})
}

// Match reports whether a name (canonical id OR alias) is protected.
func (k *KeepSet) Match(name string) (KeepSetMember, bool) {
	if k == nil || name == "" {
		return KeepSetMember{}, false
	}
	idx, ok := k.byName[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return KeepSetMember{}, false
	}
	return k.Members[idx], true
}

// MatchAny checks a whole name set (a roster entry's id plus its aliases) so a
// keep-set entry recorded under any one spelling protects all of them.
func (k *KeepSet) MatchAny(names ...string) (KeepSetMember, bool) {
	for _, n := range names {
		if m, ok := k.Match(n); ok {
			return m, true
		}
	}
	return KeepSetMember{}, false
}

// Names returns every protected name (ids and aliases), sorted.
func (k *KeepSet) Names() []string {
	if k == nil {
		return nil
	}
	out := make([]string, 0, len(k.byName))
	for n := range k.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Empty reports whether nothing is protected — worth surfacing loudly, since an
// empty keep-set means the structural protection is not actually in place.
func (k *KeepSet) Empty() bool { return k == nil || len(k.Members) == 0 }

// YAMLSeat is one model stanza read from the llama-swap YAML.
type YAMLSeat struct {
	ID      string
	Aliases []string
	TTL     int
	TTLSet  bool
	TTLText string
	Cmd     string
}

// Resident reports whether the seat is configured to stay loaded. llama-swap
// treats ttl:-1 as "never unload"; ttl:0 means "no TTL configured for this
// seat", which on a deployment with no globalTTL is also effectively resident.
// Both are protected: over-protecting costs a --force-keepset flag, while
// under-protecting costs a memory-stack outage.
func (s YAMLSeat) Resident() bool { return s.TTLSet && (s.TTL == -1 || s.TTL == 0) }

// ParseYAMLSeats reads model ids, aliases, ttl, and cmd from a llama-swap
// config. READ-ONLY, and deliberately a narrow line scanner rather than a full
// YAML load: this CLI must never rewrite the live config (its comments are the
// operator's decision record), so it has no need of a round-trippable parse,
// and the module has no YAML dependency to add without touching go.mod.
//
// Handles: quoted and bare model keys, flow-sequence aliases (["a","b"]) and
// block-sequence aliases, trailing comments, and block scalars (cmd: | / >),
// whose bodies are skipped by indentation.
func ParseYAMLSeats(path string) ([]YAMLSeat, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

	var seats []YAMLSeat
	inModels := false
	modelsIndent := 0
	var cur *YAMLSeat
	collectingAliases := false
	aliasIndent := -1
	skipBlockIndent := -1

	flush := func() {
		if cur != nil {
			seats = append(seats, *cur)
			cur = nil
		}
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if skipBlockIndent >= 0 {
			if indent > skipBlockIndent {
				continue
			}
			skipBlockIndent = -1
		}

		if !inModels {
			if indent == 0 && strings.HasPrefix(trimmed, "models:") {
				inModels = true
				modelsIndent = 0
			}
			continue
		}
		// A new top-level key ends the models block.
		if indent <= modelsIndent && !strings.HasPrefix(trimmed, "-") {
			flush()
			collectingAliases = false
			if !strings.HasPrefix(trimmed, "models:") {
				break
			}
			continue
		}

		if collectingAliases {
			if strings.HasPrefix(trimmed, "-") && (aliasIndent < 0 || indent >= aliasIndent) {
				aliasIndent = indent
				if cur != nil {
					cur.Aliases = append(cur.Aliases, unquoteYAML(stripComment(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")))))
				}
				continue
			}
			collectingAliases = false
			aliasIndent = -1
		}

		// Model key: exactly one level under `models:`.
		if indent == modelsIndent+2 && strings.HasSuffix(trimmed, ":") {
			flush()
			id := unquoteYAML(strings.TrimSuffix(trimmed, ":"))
			cur = &YAMLSeat{ID: id}
			continue
		}
		if cur == nil {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if value == "|" || value == ">" || value == "|-" || value == ">-" {
			skipBlockIndent = indent
			continue
		}
		value = stripComment(value)
		switch key {
		case "ttl":
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				cur.TTL = n
				cur.TTLSet = true
				cur.TTLText = strings.TrimSpace(value)
			}
		case "aliases":
			if value == "" {
				collectingAliases = true
				aliasIndent = -1
				continue
			}
			cur.Aliases = append(cur.Aliases, parseFlowSequence(value)...)
		case "cmd":
			cur.Cmd = unquoteYAML(value)
		}
	}
	flush()
	return seats, nil
}

// stripComment removes a trailing "# ..." comment, respecting quotes so a '#'
// inside a quoted command string is not treated as a comment marker.
func stripComment(v string) string {
	inSingle, inDouble := false, false
	for i, r := range v {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble && (i == 0 || v[i-1] == ' ' || v[i-1] == '\t') {
				return strings.TrimSpace(v[:i])
			}
		}
	}
	return strings.TrimSpace(v)
}

func parseFlowSequence(v string) []string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "[") {
		return nil
	}
	v = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	var out []string
	for _, part := range strings.Split(v, ",") {
		p := unquoteYAML(strings.TrimSpace(part))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func unquoteYAML(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
