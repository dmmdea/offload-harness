package agent

// The DEFAULT unattended rule table — loaded automatically by Build for every
// unattended run that does not pass its own table (TO-1 rescoped step 2,
// 2026-08-14). Why a default exists at all: the 2026-08-11 measurement showed
// the model's self-declared `security_risk` is a literal constant — 83/83
// emitted declarations were "low" across two production seats and five arms,
// including 81/81 structurally destructive calls — so with `--rules` defaulting
// to empty, an unattended run's only per-call gate above the capability flags
// was a 0%-recall annotation. A warning note (0.48.0) told the operator; this
// closes the gap by shipping the gate.
//
// What the table gates, and what it deliberately does not:
//
//   - Every DELETE queues for review (`delete *` → ask), with hard denies ahead
//     of it for evidence (*.jsonl), weights (*.gguf) and CI workflows. Deletion
//     was the measured unparked destructive class, and "recursive" or "mass"
//     deletes are per-call deletes — each one queues.
//   - Config/settings and dependency-manifest WRITES queue (or are denied where
//     the file is generated, e.g. lockfiles). Ordinary source writes stay
//     governed by the posture flags: an unattended agent whose every source
//     edit queues cannot do the work it was granted, and overwrite already
//     requires the explicit --allow-overwrite posture.
//   - There are NO shell rules — rules.go rejects ActShell rules by design
//     (command lines are not structurally matchable; the OS cage owns shell
//     containment) — and NO fetch rule: the egress allowlist is itself the
//     operator's pre-authorization (policy.go classify).
//
// Escape hatches (explicit, never implicit): `--rules off` (RulesOff) runs
// ungated as before — the builder then emits the UNGATED note — and
// `--rules <path>` REPLACES this table with the operator's own, so an operator
// can loosen (e.g. drop the delete catch-all) only by saying so in a reviewed,
// diffable file. Rules remain tighten-only in composition either way.

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// RulesOff is the sentinel RulesPath value that disables the default unattended
// rule table (an explicit opt-out, documented on --rules). It is compared as a
// literal: a real rule file cannot plausibly be named "off" bare of any path.
const RulesOff = "off"

//go:embed unattended-rules.json
var unattendedRulesJSON []byte

// UnattendedRules parses and validates the embedded default table. The embed
// guarantees presence at runtime regardless of install layout; validation still
// runs so a bad edit to the JSON fails loudly at Build rather than silently
// weakening the gate (fail closed, same contract as LoadRules).
func UnattendedRules() ([]Rule, error) {
	var rs []Rule
	if err := json.Unmarshal(unattendedRulesJSON, &rs); err != nil {
		return nil, fmt.Errorf("embedded unattended rule table: %v", err)
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("embedded unattended rule table is empty")
	}
	for _, r := range rs {
		if err := r.validate(); err != nil {
			return nil, fmt.Errorf("embedded unattended rule table: %v", err)
		}
	}
	return rs, nil
}
