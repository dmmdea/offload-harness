package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"
)

// powerProfileStateEnv names the state file the power row reads. It is the one setting of the row and the only way
// the row finds a file: the harness knows no tool's install folder, so a box that applies a profile at boot points
// the row at that tool's state file, and the row names this variable instead of printing a path.
const powerProfileStateEnv = "LOCAL_OFFLOAD_POWER_PROFILE_STATE"

const (
	// powerProfileSchema is the state record layout this doctor reads. A record that says another is UNKNOWN,
	// not guessed at.
	powerProfileSchema = 1
	// powerProfileBootApplyGrace is how long after the box starts the row waits for the boot apply to record
	// before it calls a missing record a finding: the applier runs a few minutes after boot, not at once.
	powerProfileBootApplyGrace = 10 * time.Minute
	// powerProfileClockSlack is how far ahead of now a record's time may sit before the row calls it a clock
	// problem; inside it the age reads as zero.
	powerProfileClockSlack = 5 * time.Minute
)

// powerProfileState is the part of the applier's state record this row reads. The record is written on every
// exit path of the apply, refusals included, and always describes the most recent run. "Applied" in it means
// the writes returned success: it makes no claim that the limit or the lock is in force now.
type powerProfileState struct {
	SchemaVersion int    `json:"schema_version"`
	TS            string `json:"ts"`
	Outcome       string `json:"outcome"`
	DryRun        bool   `json:"dry_run"`
	// DriverMatchesStamp is null when the driver or the profile's stamp was unknown: neither a match nor a
	// mismatch, so a pointer.
	DriverMatchesStamp *bool    `json:"driver_matches_stamp"`
	StepsPlanned       int      `json:"steps_planned"`
	StepsOK            int      `json:"steps_ok"`
	NamedSkips         skipList `json:"named_skips"`
	// Boards and Steps stay raw and are read leniently (powerProfileLimits, powerProfileClockLocked): they feed only
	// the limit_w and clock_lock fields of /fleet/health, so a value in a shape this reader does not know must cost
	// those two fields and never the verdict of a record that decoded before they were read.
	Boards json.RawMessage `json:"boards"`
	Steps  json.RawMessage `json:"steps"`
}

// skipList is the record's named skips. The applier writes an array: it keeps the list in an ordered hashtable,
// where PowerShell's JSON encoder leaves a one-item array an array. A writer that pipes the list through the encoder
// instead gets a bare string for one item, and an unset list is null, so both read as well: tolerance for a writer
// built another way, not a shape the applier is known to write.
type skipList []string

func (s *skipList) UnmarshalJSON(b []byte) error {
	var many []string
	if err := json.Unmarshal(b, &many); err == nil {
		*s = many
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err != nil {
		return err
	}
	*s = []string{one}
	return nil
}

// writePowerProfileSection prints one row for the GPU power profile a box applies at boot (a power limit, a clock
// lock): was a profile applied since this boot, how old is that run, and was it whole. The tool that applies the
// profile writes a state record on every run, refusals included; this row reads that record.
//
//	power:      OK      power profile applied this boot, 3h ago (9 of 9 steps ok); recorded state only, ...
//	power:      WARN    no power profile was applied this boot (the box has been up 5h): the last recorded run was ...
//	power:      WARN    the last power profile run, 3h ago, ended cookie_blocked: ...
//	power:      WARN    power profile applied this boot, 3h ago, but only part is in force: 6 of 9 steps ok
//	power:      SKIP    no power profile state is recorded on this box ...
//	power:      UNKNOWN the power profile state file is not valid JSON
//
// The record is the tool's statement of what it did, not a reading of the card, so the row says "recorded state
// only" and does not claim the limit or the lock is in force now. Comparing the recorded limit and lock with what
// the driver reports takes a reader of the card's power limit and clock lock, which the harness does not have yet;
// that comparison is the follow-up, and until then a card whose limit was changed after the apply reads green here.
//
// "This boot" is the record's time against bootedAt, when the operating system session began (zero when the platform
// cannot say, and the row then says it cannot tell). Within powerProfileBootApplyGrace of the start a missing record
// is not yet a finding. A Fast Startup shutdown keeps the kernel session on Windows, so bootedAt does not move for a
// power cycle through it.
//
// A named skip the applier marks "by design" (a card whose power limit is owned by firmware has none to apply) is
// counted as that and is not a finding, or such a box would warn on every boot. Any other skip, a step short of the
// plan, or a driver that differs from the profile's stamp (the tuned levers wait for a re-evaluation) is a WARN.
//
// An absent file is not an error: a box that applies no profile at boot records nothing, and the row says so in one
// line. A file that cannot be read, is not JSON, is of another schema or carries no usable time is UNKNOWN, never
// absent and never fine.
//
// The row prints no path, no host name and no card id, and not the applier's free-form detail (which can carry a
// path): doctor output is pasted into issues and chats, and the operator finds the file by the setting. It is
// informational, like the capacity and token rows: it never changes doctor's exit code.
//
// The judgement is readPowerProfile's, and /fleet/health publishes the same reading as its power_profile block
// (powerProfileHealthOf, power_profile_health.go): OK is "ok", WARN is "skipped", SKIP and UNKNOWN are "unknown".
func writePowerProfileSection(w io.Writer, path string, now, bootedAt time.Time) {
	row := func(status, format string, args ...any) {
		fmt.Fprintf(w, "power:      %-5s %s\n", status, fmt.Sprintf(format, args...))
	}
	r := readPowerProfile(path, now, bootedAt)
	ago := powerProfileAge(r.age)
	switch r.kind {
	case powerAbsent:
		row("SKIP", "%s", powerProfileAbsent)
	case powerUnreadable:
		row("UNKNOWN", "the power profile state file cannot be read (%s), so whether a profile was applied is unknown", r.failure)
	case powerNotJSON:
		row("UNKNOWN", "the power profile state file is not valid JSON, so whether a profile was applied is unknown")
	case powerNoSchema:
		row("UNKNOWN", "the power profile state file carries no schema_version, so it is not a record this doctor reads")
	case powerOtherSchema:
		row("UNKNOWN", "the power profile state file is schema_version %d and this doctor reads %d, so it cannot say whether a profile was applied", r.schema, powerProfileSchema)
	case powerBadTime:
		row("UNKNOWN", "the power profile state file has no usable timestamp, so the age of the last run is unknown")
	case powerFuture:
		row("UNKNOWN", "the power profile state file's timestamp is in the future (the clock changed?), so the age of the last run is unknown")
	case powerNotDue:
		row("SKIP", "the box booted %s ago and has recorded no power profile run since; the boot apply runs a few minutes after boot, so this is not yet a finding (the last recorded run was %s ago)", powerProfileAge(r.up), ago)
	case powerStaleBoot:
		row("WARN", "no power profile was applied this boot (the box has been up %s): the last recorded run was %s ago (%s), so the card may be running at stock limits", powerProfileAge(r.up), ago, r.outcome)
	case powerDryRun:
		shown := r.outcome
		if r.st.Outcome != "dry_run" {
			shown += " as a dry run"
		}
		row("WARN", "the last power profile run, %s ago, ended %s: %s", ago, shown, powerProfileOutcomeMeaning("dry_run"))
	case powerNotWhole:
		row("WARN", "the last power profile run, %s ago, ended %s: %s", ago, r.outcome, powerProfileOutcomeMeaning(r.st.Outcome))
	case powerPartial, powerApplied:
		when := ago + " ago"
		boot := ""
		if r.bootKnown {
			when = "this boot, " + when
		} else {
			boot = "; this box's boot time is unreadable, so whether it ran this boot is unknown"
		}
		if r.kind == powerPartial {
			row("WARN", "power profile applied %s, but only part is in force: %s%s", when, strings.Join(r.gaps, "; "), boot)
			return
		}
		detail := r.steps
		if r.byDesign > 0 {
			detail += fmt.Sprintf(", %d %s skipped by design", r.byDesign, powerProfilePlural(r.byDesign, "lever", "levers"))
		}
		row("OK", "power profile applied %s (%s)%s; recorded state only, the live power limit and clock lock are not compared", when, detail, boot)
	}
}

// powerProfileKind is what the state record says about the boot apply, one value per row the doctor can print.
type powerProfileKind int

const (
	powerAbsent      powerProfileKind = iota // no path set, or no file there: a box that applies no profile records nothing
	powerUnreadable                          // the file is there and cannot be read
	powerNotJSON                             // the file is not a JSON record
	powerNoSchema                            // the record carries no schema_version
	powerOtherSchema                         // the record is another schema than this reader's
	powerBadTime                             // the record's timestamp does not parse
	powerFuture                              // the record's timestamp is past the clock slack
	powerNotDue                              // no record since the boot, and the boot apply is not yet due
	powerStaleBoot                           // no record since the boot, and the boot apply is overdue
	powerDryRun                              // the last run was a dry run
	powerNotWhole                            // the last run ended in any outcome but applied_in_full
	powerPartial                             // applied, but a step, a lever or the driver stamp leaves part out
	powerApplied                             // applied whole
)

// powerProfileReading is the state record judged against the clock and the boot time: the one reading the doctor's
// power row prints and /fleet/health's power_profile publishes, so the two cannot disagree. Which fields are set
// depends on kind; age is the record's age with a small clock skew read as zero.
type powerProfileReading struct {
	kind      powerProfileKind
	failure   string // powerUnreadable: why, in words that carry no path
	schema    int    // powerOtherSchema: the record's schema_version
	st        powerProfileState
	ts        time.Time
	age       time.Duration
	up        time.Duration // powerNotDue, powerStaleBoot: how long the box has been up
	bootKnown bool
	outcome   string   // the outcome as printed (powerProfileOutcomeWord)
	steps     string   // "9 of 9 steps ok"
	gaps      []string // powerPartial: what is short
	byDesign  int      // named skips the applier marks "by design"
}

// readPowerProfile reads the state file at path and judges it. An absent file is not an error; a file that cannot be
// trusted is one of the unknown kinds, never absent and never fine (the rules are on writePowerProfileSection).
func readPowerProfile(path string, now, bootedAt time.Time) powerProfileReading {
	if path == "" {
		return powerProfileReading{kind: powerAbsent}
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return powerProfileReading{kind: powerAbsent}
	}
	if err != nil {
		return powerProfileReading{kind: powerUnreadable, failure: powerProfileReadFailure(err)}
	}
	var st powerProfileState
	if err := json.Unmarshal(raw, &st); err != nil {
		return powerProfileReading{kind: powerNotJSON}
	}
	if st.SchemaVersion != powerProfileSchema {
		if st.SchemaVersion == 0 {
			return powerProfileReading{kind: powerNoSchema}
		}
		return powerProfileReading{kind: powerOtherSchema, schema: st.SchemaVersion}
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(st.TS))
	if err != nil {
		return powerProfileReading{kind: powerBadTime}
	}
	age := now.Sub(ts)
	if age < -powerProfileClockSlack {
		return powerProfileReading{kind: powerFuture}
	}
	if age < 0 {
		age = 0
	}
	r := powerProfileReading{st: st, ts: ts, age: age, outcome: powerProfileOutcomeWord(st.Outcome), bootKnown: !bootedAt.IsZero()}

	if r.bootKnown && ts.Before(bootedAt) {
		r.up = now.Sub(bootedAt)
		r.kind = powerStaleBoot
		if r.up < powerProfileBootApplyGrace {
			r.kind = powerNotDue
		}
		return r
	}
	if st.DryRun || st.Outcome == "dry_run" {
		r.kind = powerDryRun
		return r
	}
	if st.Outcome != "applied_in_full" {
		r.kind = powerNotWhole
		return r
	}

	r.steps = fmt.Sprintf("%d of %d steps ok", st.StepsOK, st.StepsPlanned)
	if st.StepsOK < st.StepsPlanned {
		r.gaps = append(r.gaps, r.steps)
	}
	other := 0
	for _, s := range st.NamedSkips {
		if strings.Contains(s, "by design") {
			r.byDesign++
		} else {
			other++
		}
	}
	if other > 0 {
		r.gaps = append(r.gaps, fmt.Sprintf("%d lever %s", other, powerProfilePlural(other, "skip", "skips")))
	}
	if st.DriverMatchesStamp != nil && !*st.DriverMatchesStamp {
		r.gaps = append(r.gaps, "the driver differs from the profile's stamp, so the tuned levers wait for a re-evaluation")
	}
	r.kind = powerApplied
	if len(r.gaps) > 0 {
		r.kind = powerPartial
	}
	return r
}

// powerProfileReadFailure is why the state file could not be read, in words that carry no path: the error text of a
// failed read names the file.
func powerProfileReadFailure(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return "permission denied"
	}
	return "it is not a readable file"
}

// writePowerProfileRow is doctor's call of the row: the state file the setting names, the clock, and the time this
// operating system session began.
func writePowerProfileRow(w io.Writer) {
	now := time.Now()
	writePowerProfileSection(w, powerProfileStatePath(), now, powerProfileBootTime(now))
}

// powerProfileAbsent is the SKIP line for a box with no state to read. It names the setting, not a path.
var powerProfileAbsent = "no power profile state is recorded on this box (a tool that applies a power limit or clock lock at boot writes a state file this row reads; set " +
	powerProfileStateEnv + " to the path of that file)"

func powerProfilePlural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// powerProfileAge is a duration the way an operator reads it: "under a minute", "12 min", "3h", "3h05m", "2 days".
func powerProfileAge(d time.Duration) string {
	if d >= time.Minute {
		d = d.Round(time.Minute) // 2h59m59.8s reads as 3h, not 2h59m
	}
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 48*time.Hour:
		h, m := int(d/time.Hour), int(d/time.Minute)%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}

var powerProfileOutcomePattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// powerProfileOutcomeWord is the record's outcome as printed: the tool's own token, or a fixed phrase when the value
// is anything else, so a record cannot put free text (or a control character) into doctor's output.
func powerProfileOutcomeWord(outcome string) string {
	if powerProfileOutcomePattern.MatchString(outcome) {
		return outcome
	}
	return "an unrecognised outcome"
}

// powerProfileOutcomeMeaning is what an outcome other than a whole apply means for the card.
func powerProfileOutcomeMeaning(outcome string) string {
	switch outcome {
	case "cookie_blocked":
		return "the crash guard skipped this run because a cookie from an earlier apply is on disk, so this run applied nothing; a cookie from this boot clears itself within ten minutes, one from an earlier boot stays until a person clears it"
	case "plan_empty":
		return "the profile planned no steps, so nothing was applied"
	case "failed":
		return "a step failed, so the profile is not fully in force"
	case "refused":
		return "the applier refused the profile, so nothing was applied"
	case "dry_run":
		return "a dry run applies nothing, and it replaced the record of the real run"
	}
	return "not a whole apply, so the profile may not be in force"
}

// powerProfileStatePath is where the row reads the state record: the path the setting names, and nothing else. There
// is no platform default, because a default would write one tool's install folder into this repository; with the
// setting unset the row reports that nothing is recorded.
func powerProfileStatePath() string {
	return strings.TrimSpace(os.Getenv(powerProfileStateEnv))
}
