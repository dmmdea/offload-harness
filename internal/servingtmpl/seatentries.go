// seatentries.go - what a serving config states about each seat, for a reader that has to tell the
// seats' engines apart.
//
// seatparallel.go reads one thing about one seat (its slot count). The active-backend report
// (internal/backendchain) needs three other things about EVERY seat: the command line llama-swap runs
// for it (the engine is named there, or by a wrapper script), the address it listens on, and the path
// llama-swap polls to decide it is ready (`checkEndpoint`). They are read here, from the same parse and
// the same macro rules as the slot count, so "what the entry says" is one reading.
package servingtmpl

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SeatEntry is one `models:` entry of a serving config as llama-swap would run it.
type SeatEntry struct {
	// ID is the entry's key: the canonical model id llama-swap lists it under.
	ID string
	// Aliases are the entry's other names (`aliases:`), in file order.
	Aliases []string
	// Cmd is the entry's command with every `${macro}` replaced by its text, the way the command
	// reaches the process spawner. Empty when the entry states none.
	Cmd string
	// Proxy is the entry's `proxy:` address, empty when it states none (llama-swap then derives one
	// from ${PORT} at load time, so the file cannot say where the seat listens).
	Proxy string
	// CheckEndpoint is the entry's `checkEndpoint:`: the path llama-swap polls on the seat until it
	// answers. Empty when the entry states none (llama-swap's own default is /health); the word
	// `none` switches the poll off and is kept as written.
	CheckEndpoint string
}

// Names lists the id first and then every alias.
func (e SeatEntry) Names() []string {
	return append([]string{e.ID}, e.Aliases...)
}

// Answers reports whether name is the entry's id or one of its aliases, ignoring case and padding,
// the way llama-swap resolves a model name.
func (e SeatEntry) Answers(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, n := range e.Names() {
		if strings.EqualFold(strings.TrimSpace(n), name) {
			return true
		}
	}
	return false
}

// SeatEntries parses a serving config and returns every entry under `models:`, sorted by id. A text
// that is not YAML is an error, never an empty list: "the file lists no seats" and "the file could
// not be read" are different facts.
func SeatEntries(text string) ([]SeatEntry, error) {
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
		Macros any                       `yaml:"macros"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("the serving config is not parseable YAML")
	}
	macros := macroTable(doc.Macros)
	ids := make([]string, 0, len(doc.Models))
	for id := range doc.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]SeatEntry, 0, len(ids))
	for _, id := range ids {
		m := doc.Models[id]
		e := SeatEntry{ID: id}
		if list, ok := m["aliases"].([]any); ok {
			for _, a := range list {
				if s, ok := a.(string); ok && strings.TrimSpace(s) != "" {
					e.Aliases = append(e.Aliases, strings.TrimSpace(s))
				}
			}
		}
		if cmd, ok := m["cmd"].(string); ok {
			e.Cmd = strings.TrimSpace(expandMacros(cmd, macros))
		}
		if p, ok := m["proxy"].(string); ok {
			e.Proxy = strings.TrimSpace(expandMacros(p, macros))
		}
		if c, ok := m["checkEndpoint"].(string); ok {
			e.CheckEndpoint = strings.TrimSpace(c)
		}
		out = append(out, e)
	}
	return out, nil
}
