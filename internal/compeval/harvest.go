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
	"github.com/dmmdea/offload-harness/internal/piishape"
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
	tails    map[string]bool   // every tailWindow-byte window of every credential value replaced so far
}

func newRedactor() *redactor {
	return &redactor{assigned: map[string]string{}, counts: map[string]int{}, Hits: map[string]int{}, tails: map[string]bool{}}
}

// tailWindow is the length of a credential value's piece that must not survive redaction.
const tailWindow = 12

// tailClasses are the classes whose matches are credential values. Email and private-key header
// text are left out: a piece of an address (its domain) or of a PEM header is ordinary text
// elsewhere, so a window of it would refuse harvests that hold no secret.
var tailClasses = map[string]bool{
	"bearer-token": true, "api-key-assignment": true, "aws-access-key": true,
	"github-token": true, "provider-key": true, "jwt": true,
	"basic-auth": true, "auth-field": true, "cookie-header": true,
}

// placeholderRe finds an earlier substitution inside a later class's match.
var placeholderRe = regexp.MustCompile(`\[redacted-[a-z-]+-[0-9]+\]`)

// credentialValues are the secret parts of a match: the text after = or : for an assignment (the key
// name is kept in the output and is not a secret), the text after the word for a bearer token, the
// whole match for the bare-key classes; for a Basic header and a registry auth field the encoded
// value, and for a cookie header every long cookie value on the line.
func credentialValues(class, m string) []string {
	switch class {
	case "api-key-assignment":
		// The value is what the class itself took as the value: the body of a quoted literal, which may hold
		// a space, a comma, a semicolon or an ampersand, or the unquoted value. Remembering the type
		// annotation or the call in front of a quoted literal would make ordinary code that shares it look
		// like a leftover.
		if v := piishape.AssignmentValue(m); v != "" {
			return []string{v}
		}
		// Not a whole class match (a caller passed a piece): the value follows the last character that
		// cannot be part of an unquoted one; NAME=value has none, and the value then follows the first =
		// or :.
		if i := strings.LastIndexAny(m, " \t\r\n\f'\",;&\\\x1b"); i >= 0 {
			return []string{m[i+1:]}
		}
		if i := strings.IndexAny(m, "=:"); i >= 0 {
			return []string{strings.TrimLeft(m[i+1:], " \t\n\f\r'\"\\")}
		}
	case "auth-field":
		if i := strings.IndexAny(m, "=:"); i >= 0 {
			return []string{strings.TrimLeft(m[i+1:], " \t\n\f\r'\"\\")}
		}
	case "basic-auth":
		if f := strings.Fields(m); len(f) > 0 {
			return []string{f[len(f)-1]}
		}
	case "cookie-header":
		return cookieValues(m)
	case "bearer-token":
		if f := strings.Fields(m); len(f) > 0 {
			return []string{f[len(f)-1]}
		}
	}
	return []string{m}
}

// cookieAttributes are the Set-Cookie attributes that carry a long value which is ordinary text
// (a domain, a path, an expiry): they are not remembered, or a harvest that names the same domain
// elsewhere would be refused. (The values of SameSite, Secure and HttpOnly are at most six characters.)
var cookieAttributes = map[string]bool{"domain": true, "path": true, "expires": true, "max-age": true}

// cookieNameBefore is the cookie name that stands in front of the equals sign at i: the run of characters
// back from it that no delimiter of a header, a list or a quoted string breaks.
func cookieNameBefore(m string, i int) string {
	j := i
	for j > 0 && !strings.ContainsRune(" \t\r\n'\",;&\\:[]{}()<>", rune(m[j-1])) {
		j--
	}
	return m[j:i]
}

// cookieQuoteCloses reports whether a quote that has k backslashes in front of it closes a quoted value that
// was opened by a quote with b backslashes. The closing quote carries b; a quote inside the value carries twice
// as many plus one, and an escaped backslash inside the value adds 2b+2 more, so only a run of b plus a
// multiple of 2b+2 closes it. (A run shorter than b leaves a remainder that is never zero.)
func cookieQuoteCloses(k, b int) bool { return (k-b)%(2*b+2) == 0 }

// cookieEscapeMax is the longest backslash run that is the escape of a bare value (a unicode escape in text that was
// escaped four times has sixteen); the class stops at a longer one.
const cookieEscapeMax = 16

// cookieBareEscapeAt is the length of the escape that starts at q, a backslash run and the character it escapes,
// when the class takes it into an unquoted value, and 0 when it does not: a quote closes the string the header
// sits in, and the escape of a line break (an n or an r) ends the line.
func cookieBareEscapeAt(m string, q int) int {
	r := 0
	for q+r < len(m) && m[q+r] == '\\' {
		r++
	}
	if r == 0 || r > cookieEscapeMax || q+r >= len(m) || strings.IndexByte("\"'\r\nnr", m[q+r]) >= 0 {
		return 0
	}
	return r + 1
}

// cookieValueAfter reads the value that starts at i (just after an equals sign) and returns it, as it stands in the
// text, and the index where it ends. A value that opens with a double quote and any number of backslashes is the
// text between that quote and the quote that closes it at that level, whatever stands inside (a space, an
// apostrophe, an ampersand, a comma, a semicolon, a tab, a shell escape, an HTML escape, a line break), or up to the
// end of the span when the quote is never closed; the quotes are not part of the value. A line break does not end a
// quoted value, because a value that is open at one is not finished (the class decides how far such a span goes, and
// this reads what it was given). A bare value runs to the first space, quote, comma, semicolon or ampersand, and to
// a backslash that does not begin an escape the class takes into a bare value (a shell escape, a unicode escape).
func cookieValueAfter(m string, i int) (string, int) {
	b := 0
	for i+b < len(m) && m[i+b] == '\\' {
		b++
	}
	if i+b < len(m) && m[i+b] == '"' {
		p := i + b + 1
		for q := p; q < len(m); q++ {
			if m[q] != '"' {
				continue
			}
			// The run of backslashes in front of the quote stops at the opening quote, which is not one.
			k := 0
			for m[q-1-k] == '\\' {
				k++
			}
			if cookieQuoteCloses(k, b) {
				// The b backslashes in front of the quote belong to the closing quote, not to the value.
				return m[p : q-b], q + 1
			}
		}
		return m[p:], len(m)
	}
	q := i
	for q < len(m) {
		if m[q] == '\\' {
			n := cookieBareEscapeAt(m, q)
			if n == 0 {
				break
			}
			q += n
			continue
		}
		if strings.IndexByte(" \t\r\n'\",;&", m[q]) >= 0 {
			break
		}
		q++
	}
	return m[i:q], q
}

// cookieValues are all the values of 12 or more characters in a cookie span, as they stand in the text: a bare
// value, or a quoted one taken whole with its quotes, escapes and every character between them. The span is
// replaced whole, and a value that survives the substitution (a line cut by a quote the walk took for a closing
// one) must be seen by the value-tail gate, every value and not only the first. The span's short pairs (a theme)
// and the Set-Cookie attributes are ordinary text.
func cookieValues(m string) []string {
	var out []string
	for i := 0; i < len(m); i++ {
		if m[i] != '=' {
			continue
		}
		name := cookieNameBefore(m, i)
		value, end := cookieValueAfter(m, i+1)
		if end > i+1 {
			i = end - 1
		}
		if cookieAttributes[strings.ToLower(name)] || len(value) < tailWindow {
			continue
		}
		out = append(out, value)
	}
	return out
}

// remember records every tailWindow-byte window of a replaced credential value.
func (r *redactor) remember(class, m string) {
	if !tailClasses[class] {
		return
	}
	for _, value := range credentialValues(class, m) {
		for _, seg := range placeholderRe.Split(value, -1) {
			for i := 0; i+tailWindow <= len(seg); i++ {
				r.tails[seg[i:i+tailWindow]] = true
			}
		}
	}
}

// survivingTail reports whether s still holds tailWindow or more consecutive bytes of a credential
// value this redactor replaced. A tail such as ".payload.signature" matches no vet class, so the
// vet cannot see it; the redactor knows which values it was handed.
func (r *redactor) survivingTail(s string) bool {
	if len(r.tails) == 0 {
		return false
	}
	for i := 0; i+tailWindow <= len(s); i++ {
		if r.tails[s[i:i+tailWindow]] {
			return true
		}
	}
	return false
}

// redactLastClass is applied after every other class. An address-shaped piece can sit inside a
// credential value (GITHUB_TOKEN: a@b.c:rest); replacing it first would split the value, leave a
// placeholder where the value must start, and so hide the rest of it from the credential classes.
// The vet table keeps its order; only the order of substitution differs.
//
// The cookie class is deliberately not first. A cookie span ends in front of a quote that opens no quoted
// cookie value, and an unquoted header followed on the same line by a credential with a quoted value (a key
// word, a colon or an equals sign, a quote) would, run first, take the credential's key word into its span
// and stop in front of the quote: the value is then left with no key word for any class and no memory in the
// value-tail gate, and the residual gate stays clean. Run in the table's order, the credential classes take
// their quoted values by their own key words first, and the cookie class takes what is left of the header
// (a cookie whose name holds a key word is then found by both, and which class counts it does not matter).
const redactLastClass = "email"

func (r *redactor) redact(s string) string {
	for _, last := range [2]bool{false, true} {
		for _, p := range redactPatterns {
			if (p.name == redactLastClass) == last {
				s = r.apply(p.name, p.re, s)
			}
		}
	}
	return s
}

// apply replaces every match of one class in s by its placeholder. A class may start right after a JSON
// escape (the newline before a line that begins with a key word) or a terminal control sequence (a color
// code), and the match then carries it. That lead is not part of the credential, so it is kept and the
// placeholder follows it; replacing it would break the JSON of the Args it sits in.
func (r *redactor) apply(name string, re *regexp.Regexp, s string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		lead := piishape.Lead(m)
		m = m[len(lead):]
		r.Hits[name]++
		r.remember(name, m)
		key := name + "\x00" + m
		if ph, ok := r.assigned[key]; ok {
			return lead + ph
		}
		r.counts[name]++
		ph := fmt.Sprintf("[redacted-%s-%d]", name, r.counts[name])
		r.assigned[key] = ph
		return lead + ph
	})
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
// that cannot be parsed or is too short, or that still holds a piece of a redacted
// credential value, is skipped WITH a note; residual vet-class PII after
// redaction fails the whole harvest.
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
		kind := ClassifyKind(turns)
		// Entry id from the FILE name (unique on disk; already sanitized by the
		// trace writer) — record ids can collide across goal queues.
		base := strings.TrimSuffix(filepath.Base(p), ".json")
		// Exit gate beside the vet: a piece of a redacted credential value that is
		// still in the text means the redactor stopped short of the value, or that
		// ordinary text (source code assigning to a name that holds "token") reuses a
		// piece of what was taken as a value. Either way this trace is left out with
		// a note; one such trace never costs the others.
		if redactorLeftATail(red, turns) {
			stats.Skipped = append(stats.Skipped, HarvestNote{File: filepath.Base(p), Reason: fmt.Sprintf("still holds %d or more characters of a redacted credential value after redaction (the text is not echoed)", tailWindow)})
			continue
		}
		for class, n := range red.Hits {
			stats.Redactions[class] += n
		}
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

// redactorLeftATail reports whether any turn still holds a surviving piece of a credential value
// the redactor replaced.
func redactorLeftATail(red *redactor, turns []Turn) bool {
	for _, t := range turns {
		if red.survivingTail(t.Content) {
			return true
		}
		for _, c := range t.ToolCalls {
			if red.survivingTail(c.Name) || red.survivingTail(c.Args) {
				return true
			}
		}
	}
	return false
}

// countCompactionArtifacts counts tool turns whose body is a PRODUCT of the
// production ladder (elision marker, skeleton, dedupe reference) or by the
// optional log-collapse rung, or a receipt the optional evidence reducer wrote in
// place of a command's output, rather than raw content.
func countCompactionArtifacts(msgs []agent.Msg) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "tool" && (agent.IsCompactionArtifact(m.Content) || agent.IsLogCollapseArtifact(m.Content) || agent.IsEvidenceReceipt(m.Content)) {
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
