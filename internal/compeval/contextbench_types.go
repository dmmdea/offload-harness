// contextbench_types.go - the shared shapes of the contextbench adapter: the
// streamed operations, the gold each family grades against, the generator knobs,
// and the canonical byte form of an instance.
//
// contextbench measures what an agent keeps, updates and offloads when its input
// does not fit the context window. Four task families stream more content than the
// budget holds and are graded from the agent's own context. The adapter names every
// identifier `contextbench`; it is unrelated to the kv-cache reuse bench (kvbench).
package compeval

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// The four task families.
const (
	CBFamilyKV     = "kv_store"
	CBFamilyLog    = "log_triage"
	CBFamilyNeedle = "needle_retention"
	CBFamilySudoku = "sudoku_sketchpad"
)

// CBFamilies is the fixed order every report and run walks the families in.
var CBFamilies = []string{CBFamilyKV, CBFamilyLog, CBFamilyNeedle, CBFamilySudoku}

// CBOp is one streamed operation, delivered to the agent as one turn.
type CBOp struct {
	Index  int    `json:"index"`
	Header string `json:"header"`
	Body   string `json:"body"`
}

// CBKVQuery is one GET: the key asked about and the value that was set for it.
type CBKVQuery struct {
	QID   int    `json:"qid"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

// CBLogQuery is one log question. Type is "lookup" or "count".
type CBLogQuery struct {
	QID    int    `json:"qid"`
	Type   string `json:"type"`
	Prompt string `json:"prompt"`
	Answer string `json:"answer"`
}

// CBSudokuDelta is the gold for one sketchpad version: the changed cells, or the
// full board when Reset is set (version 0 and every new board).
type CBSudokuDelta struct {
	V     int               `json:"v"`
	Reset bool              `json:"reset,omitempty"`
	Cells map[string]string `json:"cells"`
}

// CBGold is what a family's grader scores against. Only the fields of the
// instance's own family are set.
type CBGold struct {
	KV          []CBKVQuery     `json:"kv,omitempty"`
	Log         []CBLogQuery    `json:"log,omitempty"`
	NeedleLines []string        `json:"needle_lines,omitempty"`
	Side        int             `json:"side,omitempty"`
	InitialGrid string          `json:"initial_grid,omitempty"`
	Deltas      []CBSudokuDelta `json:"deltas,omitempty"`
}

// CBInstance is one generated task: the operation stream, the gold, and the
// numbers that describe it. It is a pure function of (family, seed, knobs).
type CBInstance struct {
	ID     string         `json:"id"`
	Family string         `json:"family"`
	Level  string         `json:"level"`
	Seed   int64          `json:"seed"`
	Ops    []CBOp         `json:"ops"`
	Gold   CBGold         `json:"gold"`
	Meta   map[string]any `json:"meta"`
}

// CBKnobs are the generator parameters. A family reads only its own.
type CBKnobs struct {
	// kv_store
	Records, ValueWords, Queries, RecordsPerChunk int
	// log_triage (Queries is shared)
	Lines, LinesPerBatch int
	// needle_retention
	Chunks, NeedlesPerChunk, FillerLines, WordsPerLine int
	// sudoku_sketchpad
	Box, Givens, Boards, Turns int
}

// pyHashTag is the 8-hex-digit tag the generators derive ids and uniqueness tags
// from: the first eight hex digits of sha1 over the colon-joined parts.
func pyHashTag(parts ...any) string {
	strs := make([]string, len(parts))
	for i, p := range parts {
		strs[i] = fmt.Sprint(p)
	}
	return sha1Hex8(strings.Join(strs, ":"))
}

// sha1Hex8 is the first eight hex digits of sha1(s). sha1 is a tag generator here, not
// a security primitive: the tags only have to match the original generator's ids.
func sha1Hex8(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// approxTokens4 is the generators' meta estimate: about four characters per token.
func approxTokens4(lines []string) int {
	n := 0
	for _, s := range lines {
		n += utf8.RuneCountInString(s) + 1
	}
	return n / 4
}

// pyJSONString quotes s the way Python's json.dumps(ensure_ascii=False) does.
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// OpsJSONL is the stream in the upstream wire form: one line per operation,
// `{"index": N, "header": "...", "body": "..."}` with Python's separators. Two
// instances from the same (family, seed, knobs) have identical bytes.
func (in *CBInstance) OpsJSONL() []byte {
	var b strings.Builder
	for _, op := range in.Ops {
		fmt.Fprintf(&b, `{"index": %d, "header": %s, "body": %s}`+"\n", op.Index, pyJSONString(op.Header), pyJSONString(op.Body))
	}
	return []byte(b.String())
}

// sortedKeys returns a map's keys in order (deterministic iteration).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
