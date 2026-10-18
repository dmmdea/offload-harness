package fleetnode

// The values of PowerProfileHealth.State.
const (
	// PowerProfileOK: a profile was applied since this boot began, whole.
	PowerProfileOK = "ok"
	// PowerProfileSkipped: the boot apply did not take, or took in part. Reason says how.
	PowerProfileSkipped = "skipped"
	// PowerProfileUnknown: the node cannot say. A node with no state file is here, with the reason "no_state":
	// a node that applies no profile is not a node in trouble.
	PowerProfileUnknown = "unknown"
)

// PowerProfileHealth is /fleet/health's power_profile block: whether the GPU power profile this node applies at boot
// (a power limit, a clock lock) took, read from the state record the boot-apply tool writes. It is the doctor's
// `power:` row in a form a delegator can read; it reports the record, not the card, so a limit changed after the
// apply still reads ok.
//
// State is "ok", "skipped" or "unknown" (the PowerProfile constants). Reason is a short token: the reason a node is
// not ok ("no_state", "unreadable", "bad_state", "bad_schema", "not_due", "boot_unknown", "stale_boot", "partial",
// "dry_run", "unrecognised"), or the applier's own outcome word for a run that ended without applying ("failed",
// "refused", "cookie_blocked", "plan_empty": lower-case letters, digits and underscores, 32 at most). It is never free text
// and never a path. LimitW is the driver's power limit of each card as the record read it, in the record's card order,
// and ClockLock says a step that locked the clocks returned success; both are published only when State is ok, so a
// skipped run never advertises a limit it did not set. AppliedAt is the record's own time, RFC 3339 in UTC, of the
// most recent run (the one the state describes), omitted when the record could not be read.
type PowerProfileHealth struct {
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	LimitW    []int  `json:"limit_w,omitempty"`
	ClockLock bool   `json:"clock_lock,omitempty"`
	AppliedAt string `json:"applied_at,omitempty"`
}
