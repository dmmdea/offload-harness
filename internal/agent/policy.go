package agent

import (
	"encoding/json"
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
	unattended bool
	audit      *AuditLog
	allow      Allowlist // P3 egress allowlist; the zero value permits nothing (default-deny)
	allowShell bool      // P4.6 shell capability; off by default (default-deny)
	askQueue   *AuditLog // P5b: optional reviewable queue of asks deferred on an unattended run
}

// NewPolicy builds a broker. unattended=true makes every "ask" deny-and-queue.
// audit may be nil (decisions then aren't logged). Egress is deny-all (no
// allowlist) — use NewPolicyWithEgress to grant outbound hosts.
func NewPolicy(unattended bool, audit *AuditLog) *Policy {
	return &Policy{unattended: unattended, audit: audit}
}

// NewPolicyWithEgress is NewPolicy plus a P3 egress allowlist (the set of hosts
// web_fetch may reach). An empty allowlist is default-deny. The allowlist is set
// once and never mutated afterward, so classify stays pure and deterministic.
func NewPolicyWithEgress(unattended bool, audit *AuditLog, allow Allowlist) *Policy {
	return &Policy{unattended: unattended, audit: audit, allow: allow}
}

// WithShell enables (or disables) the ActShell capability. Off by default; the
// CLI turns it on only when --allow-shell is set AND the OS sandbox is available.
// Set once at startup before any Decide, so classify stays deterministic.
func (p *Policy) WithShell(allowed bool) *Policy {
	p.allowShell = allowed
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
		return Ask, "deleting a file requires approval"
	case ActWrite:
		if a.Exists {
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
	}
	return Deny, "unknown action kind"
}

// Decide returns the EFFECTIVE decision (resolving ask→deny for unattended runs)
// plus a reason, and records it to the audit log. This is what tools call. If an
// ALLOW cannot be audited, it is downgraded to DENY — an unauditable mutation is
// not performed (a mutating agent must leave a trail).
func (p *Policy) Decide(a Action) (Decision, string) {
	d, reason := p.classify(a)
	eff := d
	if d == Ask && p.unattended {
		// Park the pending ask for later human review BEFORE rewriting the reason, so
		// the queue carries the original "why it needs approval". Best-effort: a queue
		// write failure must not change the (already safe) deny outcome.
		if p.askQueue != nil {
			_ = p.askQueue.Record(a, Ask, reason)
		}
		eff = Deny
		reason = "requires approval; unattended run → denied & queued (" + reason + ")"
	}
	if p.audit != nil {
		if err := p.audit.Record(a, eff, reason); err != nil && eff == Allow {
			eff = Deny
			reason = "refusing to proceed: audit write failed (" + err.Error() + ")"
			_ = p.audit.Record(a, eff, reason) // best-effort: try to log the downgrade itself
		}
	}
	return eff, reason
}

// AuditLog is an append-only JSONL record of every brokered decision — kept
// separate from the savings ledger, never co-mingled.
type AuditLog struct {
	mu   sync.Mutex
	path string
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
}

// Record appends one decision as a JSON line and RETURNS any error so the broker
// can refuse to perform an unauditable allow. A nil receiver is a no-op (nil err).
func (l *AuditLog) Record(a Action, d Decision, reason string) error {
	if l == nil {
		return nil
	}
	e := auditEntry{TS: time.Now().Unix(), Kind: string(a.Kind), Path: clampAudit(a.Path), Exists: a.Exists, Decision: string(d), Reason: reason}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
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
