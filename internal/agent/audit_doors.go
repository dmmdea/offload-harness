package agent

import (
	"fmt"
	"strings"
)

// DoorAuditPlan is what an agent door hands Build for the broker audit trail
// (register SF-02, config key audit_all_doors).
//
//   - Path: the trail to attach ("" = none, today's behaviour for a run without browse).
//   - Advisory: warn mode; a failed write is reported once and never turns an allow
//     into a deny (see Policy.WithAuditAdvisory).
//   - Refuse: the door must not run; the reason names the key and the fix.
//   - Note: a run that proceeds without the trail the mode asked for says why.
type DoorAuditPlan struct {
	Path     string
	Advisory bool
	Refuse   string
	Note     string
	// Enforced: the trail is enforcing because audit_all_doors=enforce asked for it
	// (not a browse grant), so a denial it causes names the key and the way out.
	Enforced bool
}

// doorAuditPath resolves the trail's home; a variable so a test can make it
// unresolvable without touching the real home directory.
var doorAuditPath = DefaultAuditPath

// DoorAudit maps an audit_all_doors mode onto the trail one agent door attaches; base is
// the harness install root (config `home`), under which the trail lives ("" = the user home).
// "" and "off" keep today's behaviour exactly: no trail unless browse asks for one.
// "warn" attaches an advisory trail; "enforce" an enforcing one (an audit write
// failure denies the action, as a browse run's trail always has). A browse run keeps
// its enforcing trail in every mode, because its grant requires one (ADR 0060). An
// unknown mode refuses the run rather than guessing which record the operator meant.
func DoorAudit(mode string, allowBrowse bool, base string) DoorAuditPlan {
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case "", "off", "warn", "enforce":
	default:
		return DoorAuditPlan{Refuse: fmt.Sprintf("audit_all_doors: unknown mode %q (valid: off, warn, enforce); fix the config, or set it to warn or off", mode)}
	}
	if allowBrowse {
		// An unresolvable path here makes Build refuse the browse grant itself,
		// with its own note: unchanged from before this key existed.
		return DoorAuditPlan{Path: doorAuditPath(base)}
	}
	switch m {
	case "warn":
		p := doorAuditPath(base)
		if p == "" {
			return DoorAuditPlan{Note: "audit_all_doors=warn: no audit path resolves (the home directory is unknown), so this run leaves no broker trail; set HOME (USERPROFILE on Windows) for this process"}
		}
		return DoorAuditPlan{Path: p, Advisory: true}
	case "enforce":
		p := doorAuditPath(base)
		if p == "" {
			return DoorAuditPlan{Refuse: "audit_all_doors=enforce: no audit path resolves (the home directory is unknown), and an enforced agent door never runs without its trail; set HOME (USERPROFILE on Windows) for this process, or set audit_all_doors to warn or off"}
		}
		return DoorAuditPlan{Path: p, Enforced: true}
	}
	return DoorAuditPlan{}
}

// WithAuditAdvisory makes the attached audit trail advisory (audit_all_doors=warn):
// every decision is still written, but a failed write is reported once on the log
// and never turns an allow into a deny. Set once at build time, before any Decide.
func (p *Policy) WithAuditAdvisory(on bool) *Policy {
	p.auditAdvisory = on
	return p
}

// auditEnforcedHint is appended to an enforcing trail's write-failure denial when the
// trail came from audit_all_doors=enforce, so the model and the transcript say which
// key turned a broken trail into a refusal and how to clear it.
const auditEnforcedHint = "audit_all_doors=enforce refuses an action it cannot record: make the trail writable (agent-audit.jsonl under the home directory), or set audit_all_doors to warn or off"

// WithAuditEnforcedByKey marks the attached trail as enforcing because of
// audit_all_doors=enforce (SF-02); set once at build time, before any Decide.
func (p *Policy) WithAuditEnforcedByKey(on bool) *Policy {
	p.auditByKey = on
	return p
}
