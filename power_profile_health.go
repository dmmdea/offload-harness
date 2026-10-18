package main

import (
	"encoding/json"
	"math"
	"regexp"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// powerProfileHealthTTL is how long fleet-serve keeps its reading of the power profile state before it reads the
// file again. Health is polled often and never blocks, and the record changes once per boot, so a read every half
// minute is current enough; it also bounds what a state file on a slow path can cost the health handler.
const powerProfileHealthTTL = 30 * time.Second

// powerProfileClockLockStep is the description of an applier step that locks the clocks ("<card>: lock 200..2500 MHz").
// The record has no structured field for the lock, and the description is free text that names a card, so it is
// matched and never echoed: all that leaves this file is whether one such step returned success.
var powerProfileClockLockStep = regexp.MustCompile(`\block \d{1,5}\.\.\d{1,5} MHz\b`)

// powerProfileHealthOf is the reading in the form /fleet/health publishes (fleetnode.PowerProfileHealth). It maps the
// doctor's verdicts one to one, so the row and the field cannot disagree: OK is "ok"; WARN (no apply this boot, a dry
// run, a run that ended without applying, a partial apply) is "skipped"; SKIP and UNKNOWN (no file, a file that
// cannot be trusted, a boot apply not yet due) are "unknown". One case is stricter than the row: a whole apply on a
// box that cannot say when it booted is "unknown", because the field claims the profile was applied at boot, and the
// row can only say it was applied.
func powerProfileHealthOf(r powerProfileReading) *fleetnode.PowerProfileHealth {
	h := &fleetnode.PowerProfileHealth{State: fleetnode.PowerProfileUnknown}
	switch r.kind {
	case powerAbsent:
		h.Reason = "no_state"
	case powerUnreadable:
		h.Reason = "unreadable"
	case powerNotJSON, powerBadTime, powerFuture:
		h.Reason = "bad_state"
	case powerNoSchema, powerOtherSchema:
		h.Reason = "bad_schema"
	default:
		h.AppliedAt = r.ts.UTC().Format(time.RFC3339)
		switch r.kind {
		case powerNotDue:
			h.Reason = "not_due"
		case powerStaleBoot:
			h.State, h.Reason = fleetnode.PowerProfileSkipped, "stale_boot"
		case powerDryRun:
			h.State, h.Reason = fleetnode.PowerProfileSkipped, "dry_run"
		case powerNotWhole:
			h.State, h.Reason = fleetnode.PowerProfileSkipped, powerProfileOutcomeToken(r.st.Outcome)
		case powerPartial:
			h.State, h.Reason = fleetnode.PowerProfileSkipped, "partial"
		case powerApplied:
			if !r.bootKnown {
				h.Reason = "boot_unknown"
				break
			}
			h.State = fleetnode.PowerProfileOK
			h.LimitW = powerProfileLimits(r.st.Boards)
			h.ClockLock = powerProfileClockLocked(r.st.Steps)
		}
	}
	return h
}

// powerProfileOutcomeToken is the record's outcome word when it is the applier's own token, and "unrecognised" when it
// is anything else, so a record cannot put free text into the field.
func powerProfileOutcomeToken(outcome string) string {
	if powerProfileOutcomePattern.MatchString(outcome) {
		return outcome
	}
	return "unrecognised"
}

// powerProfileMaxCards bounds limit_w: no node has more cards than this, and a record that lists more is not trusted
// for the rest.
const powerProfileMaxCards = 16

// powerProfileLimits is the power limit of each board in the record, in the record's order, rounded to whole watts. The
// applier writes the driver's own reading at the end of the run, so after a whole apply it is the limit in force then.
// A board with no usable limit is skipped; a shape this reader does not know gives none.
func powerProfileLimits(raw json.RawMessage) []int {
	type board struct {
		PowerLimitW *float64 `json:"power_limit_w"`
	}
	var boards []board
	if len(raw) == 0 || json.Unmarshal(raw, &boards) != nil {
		var one board
		if len(raw) == 0 || json.Unmarshal(raw, &one) != nil {
			return nil
		}
		boards = []board{one}
	}
	var out []int
	for _, b := range boards {
		if b.PowerLimitW == nil || math.IsNaN(*b.PowerLimitW) || *b.PowerLimitW <= 0 || *b.PowerLimitW > 10000 {
			continue
		}
		out = append(out, int(math.Round(*b.PowerLimitW)))
		if len(out) == powerProfileMaxCards {
			break
		}
	}
	return out
}

// powerProfileClockLocked is whether a step of the record that returned success locked the clocks.
func powerProfileClockLocked(raw json.RawMessage) bool {
	type step struct {
		Desc string `json:"desc"`
		OK   bool   `json:"ok"`
	}
	var steps []step
	if len(raw) == 0 || json.Unmarshal(raw, &steps) != nil {
		var one step
		if len(raw) == 0 || json.Unmarshal(raw, &one) != nil {
			return false
		}
		steps = []step{one}
	}
	for _, s := range steps {
		if s.OK && powerProfileClockLockStep.MatchString(s.Desc) {
			return true
		}
	}
	return false
}

// powerProfileHealthReporter is what fleet-serve hands fleetnode.Options.PowerProfile: the state file the setting
// names, read at most once per powerProfileHealthTTL and judged by the same reading as the doctor's power row. It
// never fails: a node with no setting, no file or a file that cannot be trusted answers "unknown" with the reason,
// which is a published answer and not an error. It returns a copy, so a caller cannot change the memo.
func powerProfileHealthReporter() func() *fleetnode.PowerProfileHealth {
	var (
		mu  sync.Mutex
		at  time.Time
		val *fleetnode.PowerProfileHealth
	)
	return func() *fleetnode.PowerProfileHealth {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if val == nil || now.Sub(at) >= powerProfileHealthTTL {
			val = powerProfileHealthOf(readPowerProfile(powerProfileStatePath(), now, powerProfileBootTime(now)))
			at = now
		}
		c := *val
		return &c
	}
}
