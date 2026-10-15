// Package recipe is the recipe store of ADR 0036's 2026-10-15 amendment: a recipe is a typed
// record of closed-vocabulary agent_env_rules data, tagged with the seat capability it was
// distilled for, the acceptance-passing runs it came from and its status.
//
// A recipe is DATA and nothing else. The record has six declared keys and refuses any other,
// the rule set is core.AgentEnvRules decoded the same strict way (so rules_code, the envharness
// key the ADR refuses by name, is an unknown key), and every string in it has a shape:
//
//	id, seat_tag     a slug; the id is also the file name in the store, so it cannot climb out of it
//	tool and argument names   a lower-case identifier
//	provenance entry a run id, no spaces
//	rewrite_error.text   one line of plain ASCII prose, at most 160 bytes, none of the characters
//	                     that carry code (backtick, $, braces, brackets, quotes, pipes, parentheses)
//	pattern          printable ASCII, at most 200 bytes, a Go (RE2) expression that does not match
//	                 the empty string (a pattern that fires on every result is a wipe, not a rule)
//
// The shapes are not a safety argument by themselves. What keeps a recipe harmless is that an env
// rule never grants or denies an effect (ADR 0036, decision 3), that a recipe is only ever
// proposed or published by the audit, and that an operator applies it. The shapes keep the store
// from holding anything that is not rule data, so the next reader can review it as data.
//
// Publishing is the audit's result and nothing else: a published record must carry the audit
// that earned it (verdict ADOPT, steps no worse than the incumbent's), so a record cannot be
// published by editing one word.
package recipe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/eval"
)

// Statuses.
const (
	StatusProposed  = "proposed"
	StatusPublished = "published"
	StatusDemoted   = "demoted"
)

// Bounds on the free-form parts of a record.
const (
	MaxTextBytes    = 160 // rewrite_error.text: the bound the trace note uses for what the model was told
	MaxPatternBytes = 200
	MaxProvenance   = 64
)

var (
	idRe         = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	seatTagRe    = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*$`)
	seatRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,95}$`)
	nameRe       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	runIDRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	textRe       = regexp.MustCompile(`^[A-Za-z0-9 .,:'_/-]{1,160}$`)
	lowerHex64Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Recipe is one record.
type Recipe struct {
	// ID names the record and its file in the store.
	ID string `json:"id"`
	// SeatTag is the seat capability class the recipe was distilled for and audited on. A recipe
	// is evidence about one class of seat; it is never audited on another.
	SeatTag string `json:"seat_tag"`
	// Rules is the closed-vocabulary rule set: what would replace the seat's agent_env_rules.
	Rules core.AgentEnvRules `json:"rules"`
	// Provenance lists the acceptance-passing runs the rules were distilled from.
	Provenance []string `json:"provenance"`
	// Status is proposed, published or demoted.
	Status string `json:"status"`
	// Audit is what the last recipe-audit found. Nil on a proposed recipe.
	Audit *Audit `json:"audit,omitempty"`
}

// Audit is the record of a recipe-audit run: which seat, which pre-registration, which era, the
// gate's verdict and the mean steps of the two arms over the tasks both finished.
type Audit struct {
	Seat           string  `json:"seat"`
	BaselineHash   string  `json:"baseline_hash"`
	EraMarker      string  `json:"era_marker"`
	Verdict        string  `json:"verdict"`
	StepsIncumbent float64 `json:"steps_incumbent"`
	StepsCandidate float64 `json:"steps_candidate"`
}

// StepsNoWorse reports whether the candidate used no more steps than the incumbent.
func (a Audit) StepsNoWorse() bool { return a.StepsCandidate <= a.StepsIncumbent }

// CheckSeat reports whether s is shaped like a seat alias.
func CheckSeat(s string) error {
	if !seatRe.MatchString(s) {
		return fmt.Errorf("seat %q: want a seat alias (letters, digits and . _ : / @ + -, at most 96 characters)", s)
	}
	return nil
}

// Decode reads one recipe: exactly one JSON document, no unknown key anywhere in it (the rules
// and the audit included), no trailing data, and a record that validates.
func Decode(raw []byte) (Recipe, error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")) // Windows editors write a byte-order mark
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r Recipe
	if err := dec.Decode(&r); err != nil {
		return Recipe{}, fmt.Errorf("recipe: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Recipe{}, errors.New("recipe: trailing data after the record")
	}
	if err := r.Validate(); err != nil {
		return Recipe{}, err
	}
	return r, nil
}

// Validate refuses a record that is not closed-vocabulary rule data with its provenance and an
// audit consistent with its status. It reports every problem it finds, not the first.
func (r Recipe) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if len(r.ID) > 64 || !idRe.MatchString(r.ID) {
		bad("id %q: want a slug of lower-case letters, digits and single dashes, at most 64 characters", r.ID)
	}
	if len(r.SeatTag) > 48 || !seatTagRe.MatchString(r.SeatTag) {
		bad("seat_tag %q: want a slug of lower-case letters, digits and single . _ - separators, at most 48 characters", r.SeatTag)
	}
	errs = append(errs, validateRules(&r.Rules)...)

	if n := len(r.Provenance); n == 0 || n > MaxProvenance {
		bad("provenance: %d run ids, want 1 to %d (a recipe is distilled from acceptance-passing runs, so it names them)", n, MaxProvenance)
	}
	seen := map[string]bool{}
	for i, id := range r.Provenance {
		switch {
		case !runIDRe.MatchString(id):
			bad("provenance[%d] %q: want a run id (letters, digits and . _ -, no spaces, at most 64 characters)", i, id)
		case seen[id]:
			bad("provenance[%d] %q: listed twice", i, id)
		}
		seen[id] = true
	}

	switch r.Status {
	case StatusProposed:
		if r.Audit != nil {
			bad("status %s with an audit: nothing has audited a proposed recipe", r.Status)
		}
	case StatusPublished:
		switch {
		case r.Audit == nil:
			bad("status %s with no audit: a recipe is published only by the audit that earned it", r.Status)
		case r.Audit.Verdict != eval.VerdictAdopt:
			bad("status %s but the audit verdict is %q, not %s", r.Status, r.Audit.Verdict, eval.VerdictAdopt)
		case !r.Audit.StepsNoWorse():
			bad("status %s but the audit's steps got worse (%g against %g)", r.Status, r.Audit.StepsCandidate, r.Audit.StepsIncumbent)
		}
	case StatusDemoted:
	default:
		bad("status %q: want %s, %s or %s", r.Status, StatusProposed, StatusPublished, StatusDemoted)
	}
	if a := r.Audit; a != nil {
		if err := CheckSeat(a.Seat); err != nil {
			bad("audit: %v", err)
		}
		if !lowerHex64Re.MatchString(a.BaselineHash) {
			bad("audit.baseline_hash: want 64 lower-case hex characters")
		}
		if !lowerHex64Re.MatchString(a.EraMarker) {
			bad("audit.era_marker: want 64 lower-case hex characters")
		}
		switch a.Verdict {
		case eval.VerdictAdopt, eval.VerdictBlock, eval.VerdictInconclusive:
		default:
			bad("audit.verdict %q: want %s, %s or %s", a.Verdict, eval.VerdictAdopt, eval.VerdictBlock, eval.VerdictInconclusive)
		}
		for name, v := range map[string]float64{"steps_incumbent": a.StepsIncumbent, "steps_candidate": a.StepsCandidate} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				bad("audit.%s: %v, want a finite number that is not negative", name, v)
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("recipe: %w", errors.Join(errs...))
}

// validateRules holds the rule set to the engine's own Validate and then to the shapes above.
func validateRules(r *core.AgentEnvRules) []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if r.IsZero() {
		bad("rules: the rule set changes nothing")
		return errs
	}
	if err := r.Validate(); err != nil {
		errs = append(errs, err)
	}
	name := func(where, n string) {
		if !nameRe.MatchString(n) {
			bad("%s: name %q is not a lower-case identifier (letters, digits and underscores, starting with a letter, at most 48 characters)", where, n)
		}
	}
	for i, t := range r.DenyTools {
		name(fmt.Sprintf("deny_tools[%d]", i), t)
	}
	for i, t := range r.AllowTools {
		name(fmt.Sprintf("allow_tools[%d]", i), t)
	}
	for t := range r.MaxCallsPerTool {
		name("max_calls_per_tool", t)
	}
	for t, args := range r.ArgLimits {
		name("arg_limits", t)
		for a := range args {
			name(fmt.Sprintf("arg_limits[%s]", t), a)
		}
	}
	for i, p := range r.ObservationStrip {
		if err := checkPattern(fmt.Sprintf("observation_strip[%d]", i), p); err != nil {
			errs = append(errs, err)
		}
	}
	for i, rw := range r.RewriteError {
		if err := checkPattern(fmt.Sprintf("rewrite_error[%d].match", i), rw.Match); err != nil {
			errs = append(errs, err)
		}
		if !textRe.MatchString(rw.Text) {
			bad("rewrite_error[%d].text: want one line of plain prose (letters, digits, spaces and . , : ' _ / -), 1 to %d bytes: free text is read by the seat, so it carries no code characters", i, MaxTextBytes)
		}
	}
	return errs
}

// checkPattern holds a regexp to the shape bound. A pattern that does not compile is the engine's
// Validate to report, so it is passed over here.
func checkPattern(where, p string) error {
	if len(p) > MaxPatternBytes {
		return fmt.Errorf("%s: pattern is %d bytes, the bound is %d", where, len(p), MaxPatternBytes)
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] > 0x7e {
			return fmt.Errorf("%s: pattern holds a character outside printable ASCII at byte %d", where, i)
		}
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil
	}
	if re.MatchString("") {
		return fmt.Errorf("%s: pattern %q matches the empty string, so it would fire on every result", where, p)
	}
	return nil
}

// Encode writes the canonical bytes of a record: two-space indent, LF, one trailing newline, no
// HTML escaping. It validates first, so a record that would not decode is never written.
func (r Recipe) Encode() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("recipe: %w", err)
	}
	return buf.Bytes(), nil
}
