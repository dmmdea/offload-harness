// Package floors reads setup/floors.json, the one file that may name a build number.
//
// A floor is the minimum build a seat or the harness needs. A ceiling or an exact pin is not
// representable here, and the update verb never selects a candidate under a floor (ADR 0099).
// The root package embeds the file (go:embed cannot reach setup/ from below it) and hands the bytes to Parse.
package floors

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Entry is one upstream's floors: the harness floor and the per-seat floors.
type Entry struct {
	Harness string
	Seats   map[string]string
}

// Set is the parsed floors file keyed by upstream name (llama.cpp, whisper.cpp, ...).
type Set map[string]Entry

// Parse reads the floors file. Every value may be a JSON number or string.
func Parse(raw []byte) (Set, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("floors.json: %w", err)
	}
	out := Set{}
	for name, body := range top {
		if strings.HasPrefix(name, "_") {
			continue
		}
		var e struct {
			Harness json.RawMessage            `json:"harness"`
			Seats   map[string]json.RawMessage `json:"seats"`
		}
		if err := json.Unmarshal(body, &e); err != nil {
			return nil, fmt.Errorf("floors.json %s: %w", name, err)
		}
		ent := Entry{Harness: scalar(e.Harness), Seats: map[string]string{}}
		for s, v := range e.Seats {
			ent.Seats[s] = scalar(v)
		}
		out[name] = ent
	}
	return out, nil
}

func scalar(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	return strings.Trim(s, `"`)
}

var numRun = regexp.MustCompile(`\d+(?:\.\d+)*`)

// Key extracts the comparable part of an upstream id: the first dotted number run
// ("b11669" -> [11669], "master-968-abc1234" -> [968], "v268" -> [268], "1.8.3" -> [1 8 3]).
func Key(id string) []int {
	m := numRun.FindString(id)
	if m == "" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(m, ".") {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}

// Compare orders two ids of the same upstream by Key: -1, 0, +1. An id with no number is the lowest.
func Compare(a, b string) int {
	ka, kb := Key(a), Key(b)
	for i := 0; i < len(ka) || i < len(kb); i++ {
		var x, y int
		if i < len(ka) {
			x = ka[i]
		}
		if i < len(kb) {
			y = kb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Comparable reports whether an id and a floor are the same shape (both dotted versions, or both single
// build numbers); a whisper.cpp b-tag against a dotted version floor is not.
func Comparable(id, floor string) bool {
	return (len(Key(id)) > 1) == (len(Key(floor)) > 1) && len(Key(floor)) > 0 && len(Key(id)) > 0
}

// Floor is the highest floor that applies to an upstream (the harness floor and every seat floor), "" when none.
func (s Set) Floor(upstream string) string {
	e, ok := s[upstream]
	if !ok {
		return ""
	}
	best := e.Harness
	for _, v := range e.Seats {
		if best == "" || Compare(v, best) > 0 {
			best = v
		}
	}
	return best
}

// Below reports whether id is under the upstream's floor. An id that cannot be compared with the floor
// (different shape) is not below it; the second return value says whether a comparison was made.
func (s Set) Below(upstream, id string) (below, compared bool) {
	f := s.Floor(upstream)
	if f == "" || !Comparable(id, f) {
		return false, false
	}
	return Compare(id, f) < 0, true
}
