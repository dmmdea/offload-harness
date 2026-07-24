// harvest.go — trace→corpus harvest with REDACTION-AT-HARVEST. Real replay
// corpora are built from the standalone agent's trace files (cmd/local-agent
// traceRecord JSON, full []agent.Msg transcript). Real transcripts routinely
// carry identity-shaped content — git output alone embeds author emails — and
// VetPII refuses a corpus rather than scrubbing it in place (scrubbing would
// change the bytes the hash pins). So redaction happens HERE, before the corpus
// file exists: deterministic placeholder substitution over the exact refusal
// classes, then the VetPII gate re-runs on the result as defense-in-depth — a
// harvest whose output would still refuse is an error, never a written file.
//
// Fidelity contract: the whole transcript is kept (including the system turn),
// ProtectedPrefix is set to the preamble length (turns before the first
// assistant turn), and KeepRecent to the live loop's DefaultKeepRecent —
// mirroring how production calls compact(), so the replay exerts the same
// pressure production does. Transcripts the ladder already compacted mid-run
// (elision markers / skeletons in tool bodies) are REFUSED with a note: their
// raw content is unrecoverable, and replaying compaction-of-compacted text
// would measure the ladder against its own output.
package compeval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// TraceRecord mirrors cmd/local-agent's on-disk trace shape: record fields are
// snake_case-tagged there; the embedded agent.Msg fields are UNTAGGED, so the
// transcript decodes with Go's default capitalized keys — decoding straight
// into []agent.Msg matches the writer byte-for-byte.
type TraceRecord struct {
	ID         string      `json:"id"`
	Goal       string      `json:"goal"`
	StopReason string      `json:"stop_reason"`
	Steps      int         `json:"steps"`
	Output     string      `json:"output"`
	Error      string      `json:"error,omitempty"`
	Transcript []agent.Msg `json:"transcript"`
}

// MsgsToTurns is ToMsgs' inverse: ladder messages → the corpus wire format.
// agent.Msg's IsError has no wire slot on purpose — the production ladder never
// reads it, so carrying it would imply a replay distinction that doesn't exist.
func MsgsToTurns(msgs []agent.Msg) []Turn {
	out := make([]Turn, len(msgs))
	for i, m := range msgs {
		t := Turn{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			t.ToolCalls = append(t.ToolCalls, TurnToolCall{ID: c.ID, Name: c.Name, Args: c.Args})
		}
		out[i] = t
	}
	return out
}

// redactOverrides widens a vet class's substitution regex where redacting only
// the vet match would leave sensitive residue behind. private-key-block: the
// vet pattern recognizes just the BEGIN header, so the redactor takes the
// WHOLE block — through the END marker, or to end-of-string when the block is
// truncated (over-redaction is the safe direction).
var redactOverrides = map[string]*regexp.Regexp{
	"private-key-block": regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|\z)`),
}

// redactPatterns are DERIVED from piiPatterns (the vet's own table) so parity
// holds by construction: a vet class added tomorrow is automatically redacted
// with its own regex unless an override widens it — the residual-PII gate can
// only trip on a genuine widening bug, never on a forgotten mirror entry.
var redactPatterns = func() []struct {
	name string
	re   *regexp.Regexp
} {
	out := make([]struct {
		name string
		re   *regexp.Regexp
	}, 0, len(piiPatterns))
	for _, p := range piiPatterns {
		re := p.re
		if o, ok := redactOverrides[p.name]; ok {
			re = o
		}
		out = append(out, struct {
			name string
			re   *regexp.Regexp
		}{p.name, re})
	}
	return out
}()

// redactor substitutes stable placeholders per entry: the same matched text
// always yields the same placeholder (numbered by first appearance, per class),
// so distinctness survives redaction — two different emails stay two different
// entities for retention measurement, and re-running the harvest over the same
// traces produces byte-identical output.
type redactor struct {
	assigned map[string]string // class \x00 matched-text → placeholder
	counts   map[string]int    // class → next placeholder index
	Hits     map[string]int    // class → total substitutions (stats)
}

func newRedactor() *redactor {
	return &redactor{assigned: map[string]string{}, counts: map[string]int{}, Hits: map[string]int{}}
}

func (r *redactor) redact(s string) string {
	for _, p := range redactPatterns {
		s = p.re.ReplaceAllStringFunc(s, func(m string) string {
			r.Hits[p.name]++
			key := p.name + "\x00" + m
			if ph, ok := r.assigned[key]; ok {
				return ph
			}
			r.counts[p.name]++
			ph := fmt.Sprintf("[redacted-%s-%d]", p.name, r.counts[p.name])
			r.assigned[key] = ph
			return ph
		})
	}
	return s
}

// ClassifyKind buckets an entry by what its TOOL payloads are made of — the
// compressible content the ladder actually works on. Byte-weighted majority
// (≥60%) picks the bucket; no majority is honestly "mixed"; no tool payloads at
// all means the transcript is user/assistant text → "prose". Deterministic:
// same turns, same kind, no model involved.
func ClassifyKind(turns []Turn) string {
	var jsonB, logB, codeB, textB int
	for _, t := range turns {
		if t.Role != "tool" {
			continue
		}
		c := strings.TrimSpace(t.Content)
		if c == "" {
			continue
		}
		n := len(c)
		switch {
		case looksJSON(c):
			jsonB += n
		case looksLog(c):
			logB += n
		case looksCode(c):
			codeB += n
		default:
			textB += n
		}
	}
	total := jsonB + logB + codeB + textB
	if total == 0 {
		return "prose"
	}
	ranked := []struct {
		kind string
		b    int
	}{
		{"tool-json", jsonB}, {"logs", logB}, {"code", codeB}, {"tool-text", textB},
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].b > ranked[j].b })
	if ranked[0].b*10 >= total*6 {
		return ranked[0].kind
	}
	return "mixed"
}

func looksJSON(s string) bool {
	return (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) && json.Valid([]byte(s))
}

// logLineRe recognizes one log-shaped line: leading timestamp, a level tag, a
// test outcome token, an exit status, or a panic header.
var logLineRe = regexp.MustCompile(`(?i)^\s*(\d{4}-\d{2}-\d{2}[T ]|\d{2}:\d{2}:\d{2}\b|\[?(error|warn|warning|info|debug|fatal|trace)\]?[: \]]|(---\s+)?(PASS|FAIL|SKIP|ok)\b|exit (code|status)\b|panic:)`)

func looksLog(s string) bool { return lineShare(s, logLineRe) >= 0.4 }

// codeLineRe recognizes one code-shaped line: declaration keywords, comment
// leaders, or brace/semicolon line endings.
var codeLineRe = regexp.MustCompile(`^\s*(func |package |import\b|type \w+|var |const |def |class |return\b|if\b.*\{$|for\b.*\{$|//|/\*)|[{};]\s*$`)

func looksCode(s string) bool { return lineShare(s, codeLineRe) >= 0.3 }

// lineShare is the fraction of non-blank lines matching re.
func lineShare(s string, re *regexp.Regexp) float64 {
	lines := strings.Split(s, "\n")
	total, hits := 0, 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		total++
		if re.MatchString(ln) {
			hits++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

// preambleLen counts the turns before the first assistant turn — the prefix the
// live loop protects (system + exemplars + recall + objective). Minimum 1.
func preambleLen(turns []Turn) int {
	for i, t := range turns {
		if t.Role == "assistant" {
			if i < 1 {
				return 1
			}
			return i
		}
	}
	return 1
}

// HarvestOpts tunes a harvest pass.
type HarvestOpts struct {
	// MinTurns skips traces whose transcript is shorter (default 3 — a
	// transcript with no tool exchange exerts no compaction pressure).
	MinTurns int
	// MaxEntries caps the harvest (0 = all traces).
	MaxEntries int
	// IDPrefix namespaces harvested entry ids (default "hv-").
	IDPrefix string
}

func (o HarvestOpts) withDefaults() HarvestOpts {
	if o.MinTurns <= 0 {
		o.MinTurns = 3
	}
	if o.IDPrefix == "" {
		o.IDPrefix = "hv-"
	}
	return o
}

// HarvestNote records one skipped trace file and why.
type HarvestNote struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// HarvestStats is the harvest's honest accounting: every input file is either
// harvested or named in Skipped — silent drops would read as coverage.
type HarvestStats struct {
	Files      int            `json:"files"`
	Harvested  int            `json:"harvested"`
	Skipped    []HarvestNote  `json:"skipped,omitempty"`
	// PreCompacted counts traces refused because their transcript already
	// carried ladder artifacts (see the fidelity gate in HarvestTraces).
	PreCompacted int            `json:"pre_compacted,omitempty"`
	Redactions   map[string]int `json:"redactions,omitempty"` // class → substitutions
	Kinds        map[string]int `json:"kinds"`                // kind → entries
}

// HarvestTraces reads every *.json trace under dir (sorted by name — the
// harvest is deterministic), converts, redacts, classifies, and gates. A trace
// that cannot be parsed or is too short is skipped WITH a note; residual PII
// after redaction fails the whole harvest.
func HarvestTraces(dir string, opts HarvestOpts) ([]Entry, HarvestStats, error) {
	opts = opts.withDefaults()
	stats := HarvestStats{Redactions: map[string]int{}, Kinds: map[string]int{}}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, stats, err
	}
	sort.Strings(paths)
	var entries []Entry
	for _, p := range paths {
		stats.Files++
		if opts.MaxEntries > 0 && len(entries) >= opts.MaxEntries {
			stats.Skipped = append(stats.Skipped, HarvestNote{File: filepath.Base(p), Reason: "max-entries cap reached"})
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			stats.Skipped = append(stats.Skipped, HarvestNote{File: filepath.Base(p), Reason: "read: " + err.Error()})
			continue
		}
		var rec TraceRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			stats.Skipped = append(stats.Skipped, HarvestNote{File: filepath.Base(p), Reason: "parse: " + err.Error()})
			continue
		}
		if len(rec.Transcript) < opts.MinTurns {
			stats.Skipped = append(stats.Skipped, HarvestNote{File: filepath.Base(p), Reason: fmt.Sprintf("transcript has %d turn(s) < min %d", len(rec.Transcript), opts.MinTurns)})
			continue
		}
		// Fidelity gate 1: refuse transcripts the ladder ALREADY compacted
		// mid-run (elision markers / skeletons in tool bodies). The raw content
		// is unrecoverable, and replaying compaction-of-compacted text would
		// measure the ladder against its own output — ratios near 1 on exactly
		// the high-pressure traces a flip decision cares most about.
		if n := countCompactionArtifacts(rec.Transcript); n > 0 {
			stats.PreCompacted++
			stats.Skipped = append(stats.Skipped, HarvestNote{File: filepath.Base(p), Reason: fmt.Sprintf("pre-compacted transcript (%d ladder-artifact tool turn(s)) — raw content unrecoverable", n)})
			continue
		}
		// Fidelity gate 2: structural validation the strict loader will apply,
		// run HERE so one malformed trace becomes a skip note instead of
		// aborting the whole harvest at write time.
		if reason := structuralProblem(rec.Transcript); reason != "" {
			stats.Skipped = append(stats.Skipped, HarvestNote{File: filepath.Base(p), Reason: reason})
			continue
		}
		turns := MsgsToTurns(rec.Transcript)
		red := newRedactor()
		for i := range turns {
			turns[i].Content = red.redact(turns[i].Content)
			for j := range turns[i].ToolCalls {
				turns[i].ToolCalls[j].Name = red.redact(turns[i].ToolCalls[j].Name)
				turns[i].ToolCalls[j].Args = red.redact(turns[i].ToolCalls[j].Args)
			}
		}
		for class, n := range red.Hits {
			stats.Redactions[class] += n
		}
		kind := ClassifyKind(turns)
		// Entry id from the FILE name (unique on disk; already sanitized by the
		// trace writer) — record ids can collide across goal queues.
		base := strings.TrimSuffix(filepath.Base(p), ".json")
		entries = append(entries, Entry{
			ID:              opts.IDPrefix + base,
			Kind:            kind,
			Turns:           turns,
			// Mirror PRODUCTION replay pressure: the live loop compacts with
			// keepRecent=agent.DefaultKeepRecent; leaving this 0 would replay
			// at the harness default (1) — systematically harsher than
			// production on every entry.
			KeepRecent:      agent.DefaultKeepRecent,
			ProtectedPrefix: preambleLen(turns),
		})
		stats.Kinds[kind]++
		stats.Harvested++
	}
	if err := refuseResidualPII(entries); err != nil {
		return nil, stats, err
	}
	return entries, stats, nil
}

// countCompactionArtifacts counts tool turns whose body is a PRODUCT of the
// production ladder (elision marker or skeleton) rather than raw content.
func countCompactionArtifacts(msgs []agent.Msg) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "tool" && agent.IsCompactionArtifact(m.Content) {
			n++
		}
	}
	return n
}

// structuralProblem pre-applies the strict loader's per-turn checks to a
// transcript and returns a skip reason, or "" when clean — so a malformed
// trace (e.g. a backend that returned empty tool-call ids) degrades to a
// per-file note instead of failing the whole harvest at write time.
func structuralProblem(msgs []agent.Msg) string {
	for i, m := range msgs {
		if m.Role == "" {
			return fmt.Sprintf("turn %d missing role", i)
		}
		if m.Role == "tool" && m.ToolCallID == "" {
			return fmt.Sprintf("tool turn %d missing tool_call_id (the strict loader would refuse it)", i)
		}
	}
	return ""
}

// refuseResidualPII is the harvest's exit gate: redaction targets exactly the
// vet classes, so a residual finding means the redactor and the vet have
// drifted — refuse to produce a file rather than write one Load would refuse
// (or worse, one a weakened vet would let through).
func refuseResidualPII(entries []Entry) error {
	findings := VetPII(entries)
	if len(findings) == 0 {
		return nil
	}
	var parts []string
	for _, f := range findings {
		parts = append(parts, f.EntryID+":"+f.Class)
	}
	return fmt.Errorf("harvest refused: %d residual PII finding(s) after redaction (%s) — redactor/vet drift, fix before harvesting", len(findings), strings.Join(parts, ", "))
}

// WriteCorpus writes entries as corpus JSONL and returns the corpus hash —
// obtained by RE-LOADING the bytes through Load, the strict production reader,
// so a written corpus is round-trip-proven loadable and the returned hash is
// exactly the one every eval artifact will be stamped with. The write is
// atomic-by-rename: the re-load runs against a temp file, so a refused corpus
// never exists at the destination path.
func WriteCorpus(path string, entries []Entry) (string, error) {
	if len(entries) == 0 {
		return "", fmt.Errorf("write corpus: no entries")
	}
	var buf strings.Builder
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return "", err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o644); err != nil {
		return "", err
	}
	loaded, hash, err := Load(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("corpus failed the strict re-load (nothing written to %s): %w", path, err)
	}
	if len(loaded) != len(entries) {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("corpus re-loaded %d entries, expected %d (nothing written to %s)", len(loaded), len(entries), path)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return hash, nil
}
