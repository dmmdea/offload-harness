package agent

// Structural risk rules for the policy broker — the RESHAPED remnant of the
// turnstone two-tier judge (roast council verdict 2026-08-10, all five personas
// converging): deterministic rules live INSIDE the broker's classify chokepoint
// as its missing severity column, not as a parallel "judge subsystem", and they
// are the ONLY layer that may block. Everything probabilistic is annotation.
//
// Two hard design lines, both council findings:
//
//   - Rules are STRUCTURAL — action kind + a glob over the worktree-relative
//     path (write/delete) or host (fetch). There are deliberately NO rules over
//     shell command lines: classify's own .git note already litigated this
//     (string-matching command lines is a WAF — bypassable by quoting/expansion
//     tricks while false-positiving on legitimate work); shell semantics are
//     enforced in the OS cage, not by pattern-matching text.
//
//   - Rules may only TIGHTEN (deny or ask), never grant. A rule table that can
//     allow would create a second source of authority at the chokepoint and an
//     incentive to under-maintain the built-in policy. Grants stay where they
//     are: capability flags and posture, set once at startup.
//
// The table is versioned and diffable on purpose: an operator can read, test,
// and review it like code (LoadRules), and the audit trail records which rule
// fired at what severity.

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Severity grades a rule hit for the audit trail. It does not change the
// decision — deny is deny at any severity — but it lets the morning review
// sort a night's queue by what actually matters.
type Severity string

const (
	SevCritical Severity = "critical"
	SevHigh     Severity = "high"
	SevMedium   Severity = "medium"
	SevLow      Severity = "low"
)

// Rule is one structural policy rule. First match wins; evaluated after the
// unconditional built-in denies (.git) and BEFORE capability/posture defaults,
// so a rule can veto what posture would have allowed but can never resurrect
// what the built-ins deny.
type Rule struct {
	// Kind is the action class the rule applies to. ActShell is REJECTED at
	// validation: there is nothing structural to match in a command line, and
	// the cage owns shell containment (see the package comment).
	Kind ActionKind `json:"kind"`
	// Glob matches the worktree-relative path (write/delete) or the host
	// (fetch), using path.Match semantics against the slash-cleaned value; a
	// glob without a slash also matches the path's basename, so "*.pem"
	// catches "sub/dir/server.pem".
	Glob string `json:"glob"`
	// Decision must be Deny or Ask — tighten-only, never Allow.
	Decision Decision `json:"decision"`
	Severity Severity `json:"severity"`
	// Reason is what the model (and the audit trail) sees when the rule fires.
	Reason string `json:"reason"`
}

// validate rejects a rule that could loosen policy or that can never match.
func (r Rule) validate() error {
	switch r.Kind {
	case ActWrite, ActDelete, ActFetch:
	case ActShell:
		return fmt.Errorf("rule %q: shell rules are not supported — command lines are not structurally matchable; the OS cage owns shell containment", r.Glob)
	default:
		return fmt.Errorf("rule %q: unknown kind %q", r.Glob, r.Kind)
	}
	if r.Decision != Deny && r.Decision != Ask {
		return fmt.Errorf("rule %q: decision must be deny or ask (tighten-only), got %q", r.Glob, r.Decision)
	}
	switch r.Severity {
	case SevCritical, SevHigh, SevMedium, SevLow:
	default:
		return fmt.Errorf("rule %q: unknown severity %q", r.Glob, r.Severity)
	}
	// `**` is syntactically valid to path.Match but each `*` stops at `/`, so a
	// recursive-looking glob would load clean and silently cover one level — the
	// worst failure mode for a table whose premise is "review it like code".
	// Reject it loudly; slashless globs already cover nested files via the
	// basename fallback (see matches).
	if strings.Contains(r.Glob, "**") {
		return fmt.Errorf("rule %q: '**' is not supported (path.Match globs do not recurse) — use a slashless glob like %q to match a basename at any depth", r.Glob, path.Base(r.Glob))
	}
	if _, err := path.Match(r.Glob, "probe"); err != nil {
		return fmt.Errorf("rule %q: bad glob: %v", r.Glob, err)
	}
	return nil
}

// normalizeSubject folds an action path/host the way the FILESYSTEM (or DNS)
// will actually resolve it, mirroring classify's .git normalization: Windows is
// case-insensitive and strips trailing dots/spaces, and hostnames are
// case-insensitive everywhere — so ".ENV", "server.pem." and
// "INTERNAL.example.com" must hit the same rules as their canonical forms.
// Unconditional (also on Linux): folding errs toward DENIAL, the fail-safe
// direction for a tighten-only table. (Review finding CRITICAL-1/2, 2026-08-10:
// the floor was case-bypassable on Windows and the fetch veto was defeatable by
// casing a redirect Location header.)
func normalizeSubject(kind ActionKind, p string) string {
	if kind == ActFetch {
		return strings.ToLower(p) // a.Path is a host
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	segs := strings.Split(clean, "/")
	for i, s := range segs {
		segs[i] = strings.ToLower(strings.TrimRight(s, " ."))
	}
	return strings.Join(segs, "/")
}

// matches reports whether the rule fires for the action. Subjects are
// normalized (normalizeSubject) and globs compared lowercase, so matching is
// case-insensitive by design; a slashless glob also tries the basename so
// extension rules cover nested files.
func (r Rule) matches(a Action) bool {
	if r.Kind != a.Kind {
		return false
	}
	glob := strings.ToLower(r.Glob)
	subject := normalizeSubject(a.Kind, a.Path)
	if ok, _ := path.Match(glob, subject); ok {
		return true
	}
	if !strings.Contains(glob, "/") {
		if ok, _ := path.Match(glob, path.Base(subject)); ok {
			return true
		}
	}
	return false
}

// defaultRules is the small built-in floor: secret-material patterns that no
// agent run has any business mutating, whatever the posture flags say. Kept
// deliberately short — operator-specific protections (config files, data
// dirs) belong in a loaded table where they are visible and diffable, not
// buried in code.
func defaultRules() []Rule {
	mk := func(kind ActionKind, glob string) Rule {
		return Rule{Kind: kind, Glob: glob, Decision: Deny, Severity: SevCritical,
			Reason: "secret-material path (" + glob + ") — never mutated by an agent run"}
	}
	var rs []Rule
	// ".env*" (not ".env" + ".env.*") also covers .envrc (direnv — routinely
	// holds live secrets), .env-prod, .env_bak — review sub-threshold finding.
	for _, g := range []string{".env*", "*.pem", "*.key", "id_rsa*", "id_ed25519*"} {
		rs = append(rs, mk(ActWrite, g), mk(ActDelete, g))
	}
	return rs
}

// WithRules appends validated rules to the broker's table (after the built-in
// floor). Set once at startup before any Decide, like every other Policy
// option, so classify stays pure and deterministic. Returns an error rather
// than silently dropping an invalid rule — a policy table that half-loads is
// worse than one that refuses to.
func (p *Policy) WithRules(rules []Rule) (*Policy, error) {
	for _, r := range rules {
		if err := r.validate(); err != nil {
			return nil, err
		}
	}
	p.rules = append(p.rules, rules...)
	return p, nil
}

// LoadRules reads a JSON rule table (an array of Rule) — the versioned,
// diffable operator surface. A missing file is an error: pointing the flag at
// a path that does not exist means the operator THINKS a table is active.
func LoadRules(pathname string) ([]Rule, error) {
	b, err := os.ReadFile(pathname)
	if err != nil {
		return nil, err
	}
	var rs []Rule
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("rule table %s: %v", pathname, err)
	}
	for _, r := range rs {
		if err := r.validate(); err != nil {
			return nil, fmt.Errorf("rule table %s: %v", pathname, err)
		}
	}
	return rs, nil
}

// ruleFor returns the first matching rule, if any.
func (p *Policy) ruleFor(a Action) (Rule, bool) {
	for _, r := range p.rules {
		if r.matches(a) {
			return r, true
		}
	}
	return Rule{}, false
}
