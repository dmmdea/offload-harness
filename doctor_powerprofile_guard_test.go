package main

import (
	"io/fs"
	"strings"
	"testing"
	"time"
)

// These tests pin behaviours of the power row that the first tests left unguarded: each one fails against a named
// mutation of the row (the mutation is in the comment above the test), so the row's promises are held by a test and
// not by the code reading well.

// The row prints the outcome word only when it is the applier's own token. Anything else (free text with a path, a
// control character, a newline, an empty or oversized value) prints as a fixed phrase on every path that names an
// outcome, so a record cannot put its text into doctor's output. Mutation: powerProfileOutcomeWord returns the
// outcome unchanged.
func TestPowerRowPrintsNoFreeTextFromARecordsOutcome(t *testing.T) {
	bootedAt := powerNow.Add(-5 * time.Hour)
	hostile := map[string]string{
		"free text with a path and an escape": "failed C:/x\x1b[31mCRIMSON",
		"a newline":                           "failed\nsecond line",
		"empty":                               "",
		"too long":                            strings.Repeat("a", 33),
		"upper case":                          "Failed",
	}
	for name, outcome := range hostile {
		afterBoot := goodPowerState(powerNow.Add(-3 * time.Hour))
		afterBoot["outcome"] = outcome
		beforeBoot := goodPowerState(powerNow.Add(-49 * time.Hour))
		beforeBoot["outcome"] = outcome
		dryRun := goodPowerState(powerNow.Add(-3 * time.Hour))
		dryRun["outcome"] = outcome
		dryRun["dry_run"] = true
		for path, st := range map[string]map[string]any{"after the boot": afterBoot, "before the boot": beforeBoot, "a dry run": dryRun} {
			got := powerRow(t, writePowerState(t, st), powerNow, bootedAt)
			mustHave(t, got, "power:      WARN  ", "an unrecognised outcome")
			for _, leak := range []string{"C:/x", "CRIMSON", "second line", "\x1b"} {
				if strings.Contains(got, leak) {
					t.Errorf("%s, %s: the row prints %q from the record's outcome:\n%q", name, path, leak, got)
				}
			}
		}
	}
}

// The record's skip list reads in the shapes a writer can produce: an array, a bare string (a one-item list piped
// through PowerShell's JSON encoder), and null (no list). Mutation: skipList decodes as a plain []string.
func TestPowerRowReadsTheSkipListInEveryShapeAWriterProduces(t *testing.T) {
	bootedAt := powerNow.Add(-5 * time.Hour)
	ts := powerNow.Add(-3 * time.Hour)

	bare := goodPowerState(ts)
	bare["named_skips"] = powerFixtureCard + ": something else was skipped"
	mustHave(t, powerRow(t, writePowerState(t, bare), powerNow, bootedAt), "power:      WARN  ", "only part is in force", "1 lever skip")

	bareByDesign := goodPowerState(ts)
	bareByDesign["named_skips"] = powerFixtureCard + ": power cap skipped by design"
	mustHave(t, powerRow(t, writePowerState(t, bareByDesign), powerNow, bootedAt), "power:      OK    ", "1 lever skipped by design")

	none := goodPowerState(ts)
	none["named_skips"] = nil
	got := powerRow(t, writePowerState(t, none), powerNow, bootedAt)
	mustHave(t, got, "power:      OK    ", "9 of 9 steps ok")
	if strings.Contains(got, "skip") {
		t.Errorf("a null skip list is no skips:\n%s", got)
	}

	two := goodPowerState(ts)
	two["named_skips"] = []any{powerFixtureCard + ": a", powerFixtureCard + ": b"}
	mustHave(t, powerRow(t, writePowerState(t, two), powerNow, bootedAt), "2 lever skips")
}

// A record's time may sit a little ahead of now (two clocks that differ by a minute) without being a finding, and
// reads as zero age; past the slack it is a clock problem. Mutations: the slack set to zero (a record two minutes
// ahead turns UNKNOWN), the slack widened (a record six minutes ahead is believed), and the bound made exclusive
// (a record exactly at the slack turns UNKNOWN).
func TestPowerRowToleratesASmallClockSkewAndNoMore(t *testing.T) {
	bootedAt := powerNow.Add(-5 * time.Hour)
	for _, ahead := range []time.Duration{2 * time.Minute, 5 * time.Minute} {
		got := powerRow(t, writePowerState(t, goodPowerState(powerNow.Add(ahead))), powerNow, bootedAt)
		mustHave(t, got, "power:      OK    ", "under a minute ago")
		if strings.Contains(got, "UNKNOWN") {
			t.Errorf("a record %v ahead of now is inside the clock slack:\n%s", ahead, got)
		}
	}
	got := powerRow(t, writePowerState(t, goodPowerState(powerNow.Add(6*time.Minute))), powerNow, bootedAt)
	mustHave(t, got, "power:      UNKNOWN ", "in the future")
}

// A record with no schema_version is not a record of the wrong schema: the row says it carries none. Mutation: the
// zero case folded into the wrong-schema message.
func TestPowerRowSaysWhenTheRecordCarriesNoSchemaVersion(t *testing.T) {
	st := goodPowerState(powerNow)
	delete(st, "schema_version")
	got := powerRow(t, writePowerState(t, st), powerNow, powerNow.Add(-5*time.Hour))
	mustHave(t, got, "power:      UNKNOWN ", "carries no schema_version")
	if strings.Contains(got, "schema_version 0") {
		t.Errorf("a missing schema_version is not schema 0:\n%s", got)
	}
}

// Whitespace around the timestamp (a hand-edited or re-wrapped file) does not make the record unreadable.
// Mutation: the TrimSpace on the timestamp removed.
func TestPowerRowReadsATimestampWithStrayWhitespace(t *testing.T) {
	st := goodPowerState(powerNow.Add(-3 * time.Hour))
	st["ts"] = " \t" + powerNow.Add(-3*time.Hour).Format(powerApplierTS) + " \n"
	got := powerRow(t, writePowerState(t, st), powerNow, powerNow.Add(-5*time.Hour))
	mustHave(t, got, "power:      OK    ", "3h ago")
}

// The boot apply is due ten minutes after the start and not before: one second short of ten minutes the missing
// record is not yet a finding, and at ten minutes exactly it is. Mutation: the comparison made inclusive.
func TestPowerRowBootApplyGraceEndsAtTenMinutes(t *testing.T) {
	path := writePowerState(t, goodPowerState(powerNow.Add(-49*time.Hour)))
	mustHave(t, powerRow(t, path, powerNow, powerNow.Add(-powerProfileBootApplyGrace+time.Second)), "power:      SKIP  ", "not yet a finding")
	got := powerRow(t, path, powerNow, powerNow.Add(-powerProfileBootApplyGrace))
	mustHave(t, got, "power:      WARN  ", "no power profile was applied this boot", "10 min")
}

// The reason a read failed carries no path: an error's own text names the file. Mutation: the permission case
// dropped, so a refused read reads as an unreadable file.
func TestPowerProfileReadFailureNamesTheCauseAndNoPath(t *testing.T) {
	denied := &fs.PathError{Op: "open", Path: "never-printed/state.json", Err: fs.ErrPermission}
	if got := powerProfileReadFailure(denied); got != "permission denied" {
		t.Errorf("a refused read must say so, got %q", got)
	}
	other := &fs.PathError{Op: "read", Path: "never-printed/state.json", Err: fs.ErrInvalid}
	got := powerProfileReadFailure(other)
	if got != "it is not a readable file" || strings.Contains(got, "never-printed") {
		t.Errorf("any other failure is a generic, path-free reason, got %q", got)
	}
}
