package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Decision is the broker's verdict on a mutating action.
type Decision string

const (
	Allow Decision = "allow" // perform it
	Ask   Decision = "ask"   // needs human approval (unattended => deny-and-queue)
	Deny  Decision = "deny"  // never perform
)

// ActionKind is the class of mutating action the broker gates. P2 covers file
// mutations; later phases extend this (shell, network) behind the same broker.
type ActionKind string

const (
	ActWrite  ActionKind = "write"
	ActDelete ActionKind = "delete"
	ActFetch  ActionKind = "fetch" // P3: outbound HTTP(S) GET to a host (egress-allowlist gated)
	ActShell  ActionKind = "shell" // P4.6: run a command inside the OS sandbox (opt-in, audited)
	// ActBrowse (ADR 0060): drive the operator's own browser; Path holds the start
	// URL's lowercased host. Opt-in (WithBrowse), audited, rule-tightenable by host.
	ActBrowse ActionKind = "browse"
	// ActPark records a self-flagged high-risk call parked on an unattended run
	// (Loop.WithParkRecorder → ask queue). Queue/audit vocabulary only — the
	// broker never classifies it and the rule table rejects it implicitly
	// (validate allows write/delete/fetch only).
	ActPark ActionKind = "park"
)

// Action is a proposed mutating operation. Path is worktree-relative (the
// worktree FS boundary itself is enforced separately by os.Root); Exists tells
// the broker whether the target already exists (new vs overwrite).
type Action struct {
	Kind   ActionKind
	Path   string
	Exists bool
}

// Policy is the deterministic deny→ask→allow broker — the cage that must exist
// BEFORE any mutating tool is granted. It is the single chokepoint for every
// effectful action. For an UNATTENDED run, "ask" resolves to deny-and-queue
// (never proceed without a human), making approval-before-destructive a
// mechanism rather than a hope.
type Policy struct {
	unattended  bool
	audit       *AuditLog
	allow       Allowlist // P3 egress allowlist; the zero value permits nothing (default-deny)
	allowShell  bool      // P4.6 shell capability; off by default (default-deny)
	allowBrowse bool      // ADR 0060 browse capability; off by default (default-deny)
	askQueue    *AuditLog // P5b: optional reviewable queue of asks deferred on an unattended run

	// auditAdvisory (register SF-02, audit_all_doors=warn): a failed audit write is
	// reported once (auditWarn) and never changes the decision.
	auditAdvisory bool
	auditWarn     sync.Once
	// readGate is the read floor (SF-07), shared with every tool that reads file
	// content, the write and GitHub tools included.
	readGate *readGate
	// auditByKey: the enforcing trail came from audit_all_doors=enforce (SF-02).
	auditByKey bool

	allowOverwrite bool // open-write: Allow overwrite of an existing file within the worktree
	allowDelete    bool // open-write: Allow delete of a file within the worktree

	// rules is the structural risk table (rules.go): tighten-only, evaluated
	// after the unconditional built-ins and before capability/posture defaults.
	// Seeded with the secret-material floor; extended via WithRules/LoadRules.
	rules []Rule
}

// NewPolicy builds a broker. unattended=true makes every "ask" deny-and-queue.
// audit may be nil (decisions then aren't logged). Egress is deny-all (no
// allowlist) — use NewPolicyWithEgress to grant outbound hosts. Every broker
// carries the built-in secret-material rule floor (rules.go defaultRules).
func NewPolicy(unattended bool, audit *AuditLog) *Policy {
	return &Policy{unattended: unattended, audit: audit, rules: defaultRules()}
}

// NewPolicyWithEgress is NewPolicy plus a P3 egress allowlist (the set of hosts
// web_fetch may reach). An empty allowlist is default-deny. The allowlist is set
// once and never mutated afterward, so classify stays pure and deterministic.
func NewPolicyWithEgress(unattended bool, audit *AuditLog, allow Allowlist) *Policy {
	return &Policy{unattended: unattended, audit: audit, allow: allow, rules: defaultRules()}
}

// WithShell enables (or disables) the ActShell capability. Off by default; the
// CLI turns it on only when --allow-shell is set AND the OS sandbox is available.
// Set once at startup before any Decide, so classify stays deterministic.
func (p *Policy) WithShell(allowed bool) *Policy {
	p.allowShell = allowed
	return p
}

// WithBrowse enables (or disables) the ActBrowse capability (ADR 0060). Off by
// default; Build turns it on only when the browse tool is actually granted. Set
// once at startup before any Decide, so classify stays deterministic.
func (p *Policy) WithBrowse(allowed bool) *Policy {
	p.allowBrowse = allowed
	return p
}

// WithWritePosture sets the "open-write" posture. When overwrite is true the
// broker ALLOWS overwriting an existing file within the worktree instead of
// asking; when del is true it ALLOWS delete. This is the "fully open in the
// worktree" mode — the worktree boundary (os.Root) and the unconditional .git
// deny remain the safety rail. Off by default; set once at startup before any
// Decide so classify stays deterministic.
func (p *Policy) WithWritePosture(overwrite, del bool) *Policy {
	p.allowOverwrite = overwrite
	p.allowDelete = del
	return p
}

// WithAskQueue attaches a reviewable queue that records every ask DEFERRED on an
// unattended run (so a human can review/approve them later). Off by default; the
// standalone runner sets it. The action is still denied in the moment (unattended
// ask → deny-and-queue) — this just parks the pending request for review.
func (p *Policy) WithAskQueue(q *AuditLog) *Policy {
	p.askQueue = q
	return p
}

// classify is the pure, deterministic policy. deny → ask → allow, first match
// wins, deny unconditional.
func (p *Policy) classify(a Action) (Decision, string) {
	// DENY repo metadata robustly — but ONLY for file mutations, where a.Path is a
	// worktree path. For ActFetch a.Path is a host, and for ActShell it is a command
	// line, so this path-segment scan does not apply (and would spuriously reject
	// e.g. `find . -path */.git/*`); the shell's .git protection is enforced in the
	// OS cage by masking <worktree>/.git, not by string-matching the command line.
	if a.Kind == ActWrite || a.Kind == ActDelete {
		// The broker is handed the MODEL's path string, but os.Root resolves it using
		// FS semantics: Windows is case-insensitive and strips trailing dots/spaces, so
		// ".GIT", ".git.", "sub/../.GIT" all land in the real .git. Normalize EACH
		// segment (lowercase + strip trailing " .") and reject any .git component, so
		// the broker's notion of the path matches what the filesystem will actually
		// touch. (Regression: a fresh-context review wrote files into .git/hooks/.)
		clean := filepath.ToSlash(filepath.Clean(a.Path))
		for _, seg := range strings.Split(clean, "/") {
			if strings.ToLower(strings.TrimRight(seg, " .")) == ".git" {
				return Deny, "writes/deletes under .git are not allowed"
			}
		}
	}
	switch a.Kind {
	case ActDelete:
		if p.allowDelete {
			return Allow, "delete within worktree (open-write posture)"
		}
		return Ask, "deleting a file requires approval"
	case ActWrite:
		if a.Exists {
			if p.allowOverwrite {
				return Allow, "overwrite within worktree (open-write posture)"
			}
			return Ask, "overwriting an existing file requires approval"
		}
		return Allow, "create a new file within the worktree"
	case ActFetch:
		// Binary allow/deny: the egress allowlist IS the human's pre-authorization,
		// so fetch never enters the Ask tier. Default-deny when not allowlisted.
		// a.Path holds the host (already parsed from the URL by the fetch tool).
		if p.allow.permits(a.Path) {
			return Allow, "host on egress allowlist"
		}
		return Deny, "host " + a.Path + " not on egress allowlist"
	case ActShell:
		// The OS sandbox (no network, FS-confined, syscall-limited) is the
		// containment; the broker's role here is the opt-in capability gate plus the
		// audit trail of every command (a.Path holds the command line). Not an Ask
		// tier — the sandbox is what makes autonomous contained execution safe.
		if p.allowShell {
			return Allow, "shell command in the OS sandbox"
		}
		return Deny, "shell capability not enabled"
	case ActBrowse:
		// The grant is the pre-authorization (Build refuses it on an unattended run
		// without a host allowlist, and the sidecar's deny-list refuses publish/send-
		// class controls); rules may still deny or queue hosts. a.Path is the host.
		if p.allowBrowse {
			return Allow, "browse capability enabled for host " + a.Path
		}
		return Deny, "browse capability not enabled"
	}
	return Deny, "unknown action kind"
}

// Decide returns the EFFECTIVE decision (resolving ask→deny for unattended runs)
// plus a reason, and records it to the audit log. This is what tools call. If an
// ALLOW cannot be audited, it is downgraded to DENY — an unauditable mutation is
// not performed (a mutating agent must leave a trail).
func (p *Policy) Decide(a Action) (Decision, string) {
	v := p.decide(a)
	return v.eff, v.reason
}

// Explain is Decide with the structured refusal body (register SF-25): the same decision
// and reason, and the same single audit record, plus the rule that produced it and what
// the model can do next. Tools return Refusal.Text() for anything but Allow.
func (p *Policy) Explain(a Action) (Decision, Refusal) {
	v := p.decide(a)
	ref := Refusal{Decision: v.eff, Rule: ruleLabel(v.fired), Reason: v.reason, Asked: v.asked}
	switch {
	case v.auditFailed:
		ref.Rule = "audit trail: the decision could not be recorded"
		ref.NextSteps = nextSteps(a.Kind, causeAudit)
	case v.asked:
		ref.NextSteps = nextSteps(a.Kind, causeAsked)
	case v.fired.Glob != "":
		ref.NextSteps = nextSteps(a.Kind, causeRule)
	default:
		ref.NextSteps = nextSteps(a.Kind, causeBuiltIn)
	}
	return v.eff, ref
}

// verdict is decide's result: the effective decision and its reason, the table rule that
// fired (zero when the built-in floor decided), whether the decision was an ask before an
// unattended run resolved it to deny, and whether an allow was refused because the audit
// trail could not be written.
type verdict struct {
	eff         Decision
	reason      string
	fired       Rule
	asked       bool
	auditFailed bool
}

// decide is the body of Decide and Explain.
func (p *Policy) decide(a Action) verdict {
	// Structural risk table (rules.go): tighten-only, first match wins. Sits
	// between classify's unconditional built-ins (a rule cannot resurrect what
	// .git-deny killed — classify runs first and its Deny wins below) and the
	// capability/posture defaults (a rule CAN veto what posture would allow).
	// Evaluated here rather than inside classify so the fired rule's severity
	// and glob reach the audit trail as STRUCTURED fields — a morning review
	// sorts a night's queue by severity, which must never mean parsing prose.
	d, reason := p.classify(a)
	var fired Rule
	if r, ok := p.ruleFor(a); ok && (stricter(r.Decision, d) || (r.Decision == d && d == Ask)) {
		// Tighten-only in BOTH directions of composition: a rule upgrades
		// classify's Allow to Ask/Deny and its Ask to Deny (the secret floor
		// must hard-deny even where posture-less classify would merely ask),
		// but can never soften a classify Deny. The equal-strictness Ask arm
		// (review finding 2026-08-14): when classify already asks (e.g. delete
		// without the open-delete posture) a matching Ask rule is not stricter,
		// but its severity/glob/reason are exactly what the morning review
		// sorts the queue by — record it as fired rather than dropping it. A
		// classify Deny keeps its own reason even when a rule also matches:
		// the built-in denial (.git, egress) is the more precise record.
		d = r.Decision
		fired = r
		reason = "[" + string(r.Severity) + "] " + r.Reason
	}
	eff := d
	if d == Ask && p.unattended {
		// Park the pending ask for later human review BEFORE rewriting the reason, so
		// the queue carries the original "why it needs approval". Best-effort: a queue
		// write failure must not change the (already safe) deny outcome — but the
		// reason must tell the truth about whether anything was queued (review
		// finding 2026-08-14: asserting "queued" with no queue attached sends the
		// morning review to an empty file believing nothing was attempted).
		queued := false
		if p.askQueue != nil && p.askQueue.record(a, Ask, reason, fired) == nil {
			queued = true
		}
		eff = Deny
		if queued {
			reason = "requires approval; unattended run → denied & queued (" + reason + ")"
		} else {
			reason = "requires approval; unattended run → denied; NOT queued (no ask-queue attached or queue write failed) (" + reason + ")"
		}
	}
	auditFailed := false
	if p.audit != nil {
		if err := p.audit.record(a, eff, reason, fired); err != nil {
			switch {
			case p.auditAdvisory:
				// Warn mode reports a broken trail and keeps going: this door never had
				// the audit-write failure mode, and adding it is the enforce step.
				p.auditWarn.Do(func() {
					log.Printf("agent broker: audit write failed, continuing without the trail (audit_all_doors=warn): %v", err)
				})
			case eff == Allow:
				eff = Deny
				auditFailed = true
				reason = "refusing to proceed: audit write failed (" + err.Error() + ")"
				if p.auditByKey {
					reason += "; " + auditEnforcedHint
				}
				_ = p.audit.record(a, eff, reason, fired) // best-effort: try to log the downgrade itself
			}
		}
	}
	return verdict{eff: eff, reason: reason, fired: fired, asked: d == Ask, auditFailed: auditFailed}
}

// Refusal is the structured body of a deny or ask (register SF-25): the decision, the rule
// that produced it, the reason, and what the model can do next. It never offers an approval
// verb: an unattended ask is an operator decision outside the run, not something the model
// can request, and a refusal that invites "ask for approval" teaches the model to loop on it.
type Refusal struct {
	Decision  Decision
	Rule      string
	Reason    string
	NextSteps string
	// Asked: the decision was ask before an unattended run resolved it to deny.
	Asked bool
}

// Text is the tool result the model sees. The first line is the one-line refusal tools
// returned before SF-25; the rule and the next step follow on their own lines. Each field is
// flattened to one line, so an operator rule's reason cannot add lines of its own.
func (r Refusal) Text() string {
	return fmt.Sprintf("NOT performed (%s): %s", r.Decision, oneLine(r.Reason)) + r.tail()
}

// tail is the rule and next-step lines that follow a refusal's first line. The read floor,
// whose first line is "read refused: ...", appends the same tail.
func (r Refusal) tail() string {
	return "\nrule: " + oneLine(r.Rule) + "\nnext: " + oneLine(r.NextSteps)
}

// oneLine replaces line breaks with spaces.
func oneLine(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}

// refusalError carries a broker refusal through a check that returns an error (validateURL
// runs on every redirect hop too, where only the reason reaches the transport error), so
// the tool that called the check can still return the structured body.
type refusalError struct{ ref Refusal }

func (e refusalError) Error() string { return e.ref.Reason }

// notPerformedFrom is a URL check's error as a tool result: the structured refusal when the
// broker refused, else the one-line deny it always was.
func notPerformedFrom(err error) error {
	var re refusalError
	if errors.As(err, &re) {
		return NotPerformed(re.ref.Text())
	}
	return NotPerformed(fmt.Sprintf("NOT performed (deny): %s", err))
}

// ruleLabel names the table rule that fired, or the built-in floor when none did.
func ruleLabel(fired Rule) string {
	if fired.Glob == "" {
		return "built-in"
	}
	return fmt.Sprintf("%s %s [%s]", fired.Kind, fired.Glob, fired.Severity)
}

// refusalCause is WHY an action was refused. It decides the next step before the action's
// kind does (SF-25 review): while the audit trail is broken every change fails the same way,
// so "write elsewhere" only sends the model through new paths that each fail, and a
// rule-protected path is not something to reach another way.
type refusalCause int

const (
	causeBuiltIn refusalCause = iota // classify's own floor, or a capability this run lacks
	causeRule                        // a protection rule fired: the secret floor or the operator's table
	causeAsked                       // an ask that an unattended run resolved to deny
	causeAudit                       // an allow refused because the audit trail could not be written
)

// nextSteps is the concrete thing the model can do instead, by cause and then by action
// kind. It names an operator decision for an ask and never an approval verb (approve,
// approval, confirm, permission, authorize): the approval verb is a separate decision (S-23).
func nextSteps(k ActionKind, c refusalCause) string {
	switch c {
	case causeAudit:
		return "no action can proceed in this run: the audit trail cannot be written, and every action is refused the same way; stop retrying and say in your final answer that the audit trail failed"
	case causeAsked:
		return "this action needs an operator decision outside this run; continue without it and say in your final answer what you could not do and why"
	case causeRule:
		switch k {
		case ActFetch, ActBrowse:
			return "the rule above refuses this host; do not reach the same content another way, continue without it, and say in your final answer what you could not do"
		case ActRead:
			return "the rule above keeps this file unread; do not reach its content another way (a copy, another path, a search), continue without it, and say in your final answer what you could not do"
		}
		return "the rule above protects this path; leave it as it is, do not put the same content under another name or path, continue without it, and say in your final answer what you could not do"
	}
	switch k {
	case ActWrite:
		return "the file was not written; write elsewhere inside the workspace, or finish without it and say why in your final answer"
	case ActDelete:
		return "nothing was deleted; leave the file in place and continue, or say in your final answer why it should go"
	case ActFetch:
		return "the request was not sent; only hosts on this run's egress allowlist are reachable, so continue without it"
	case ActShell:
		return "the command was not run; use an allowlisted executable with plain arguments, or continue without it"
	case ActBrowse:
		return "the page was not opened; browsing is not available to this run, so continue without it"
	}
	return "the action was not performed; continue without it and say why in your final answer"
}

// stricter reports whether decision x is strictly more restrictive than y
// (Deny > Ask > Allow). Used to compose the rule table with classify: rules
// tighten, never loosen.
func stricter(x, y Decision) bool {
	rank := func(d Decision) int {
		switch d {
		case Deny:
			return 2
		case Ask:
			return 1
		}
		return 0
	}
	return rank(x) > rank(y)
}

// AuditLog is an append-only JSONL record of every brokered decision — kept
// separate from the savings ledger, never co-mingled.
type AuditLog struct {
	mu   sync.Mutex
	path string
	// Hash chain (register SF-08, config audit_chain): when runID is set every row
	// carries run_id, seq and prev_sha256 (the SHA-256 of the previous row of the
	// same run, exactly as written), and EndRun closes the run with a run_end row.
	// One AuditLog per Build, so one chain per run; runs interleave in the file.
	runID  string
	seq    int
	prev   string
	closed bool // EndRun wrote the run_end; a second EndRun writes nothing
}

// NewAuditLog returns a logger writing to path (created on first record).
func NewAuditLog(path string) *AuditLog { return &AuditLog{path: path} }

// maxAuditPath bounds the Path recorded per audit entry. ActShell records the full
// model-supplied command line as Path; a pathological command must not bloat the
// (mutex-held) JSONL audit trail. Short write/delete/fetch paths are unaffected.
const maxAuditPath = 4096

func clampAudit(s string) string {
	if len(s) <= maxAuditPath {
		return s
	}
	cut := s[:maxAuditPath]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…(truncated)"
}

type auditEntry struct {
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	// Severity + Rule identify a risk-table hit as STRUCTURED fields (which
	// rule fired, at what grade) so a review can sort/filter a night's queue
	// without parsing prose. Empty when no rule fired.
	Severity string `json:"severity,omitempty"`
	Rule     string `json:"rule,omitempty"`
	// The chain fields (SF-08); absent on an unchained trail, so its rows keep
	// today's shape byte for byte.
	RunID string `json:"run_id,omitempty"`
	Seq   int    `json:"seq,omitempty"`
	Prev  string `json:"prev_sha256,omitempty"`
	// On the run_end row only: the run's decision rows and the hash of the last.
	Count int    `json:"count,omitempty"`
	Head  string `json:"head,omitempty"`
}

// Record appends one decision as a JSON line and RETURNS any error so the broker
// can refuse to perform an unauditable allow. A nil receiver is a no-op (nil err).
func (l *AuditLog) Record(a Action, d Decision, reason string) error {
	return l.record(a, d, reason, Rule{})
}

// record is Record plus the fired risk rule (zero value = none).
func (l *AuditLog) record(a Action, d Decision, reason string, fired Rule) error {
	if l == nil {
		return nil
	}
	e := auditEntry{TS: time.Now().Unix(), Kind: string(a.Kind), Path: clampAudit(a.Path), Exists: a.Exists, Decision: string(d), Reason: reason,
		Severity: string(fired.Severity), Rule: fired.Glob}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(e)
}

// appendLocked writes one row, chaining it when the trail is chained. Called with
// l.mu held, so a run's seq and prev_sha256 follow the order its rows reach the file.
func (l *AuditLog) appendLocked(e auditEntry) error {
	if l.runID != "" {
		e.RunID, e.Seq, e.Prev = l.runID, l.seq+1, l.prev
	}
	row, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := l.writeLineLocked(row); err != nil {
		return err // the row is not there, so the chain does not advance past it
	}
	if l.runID != "" {
		l.seq++
		l.prev = rowHash(row)
	}
	return nil
}

// writeLineLocked appends one JSON line to the trail (l.mu held).
func (l *AuditLog) writeLineLocked(row []byte) error {
	b := append(append([]byte{}, row...), '\n')
	if dir := filepath.Dir(l.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}
