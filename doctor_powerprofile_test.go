package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The power row reads the state file a separate tool writes after it applies a GPU power limit and clock
// lock at boot, and says whether a profile was applied this boot, how old that run is, and whether the run
// was whole. It is informational (never red, never an exit code), prints no path, no card id and no host
// name, and treats an unreadable file as UNKNOWN, never as absent.
//
// The fixtures are synthetic: this repository is public, so the host is "node-a" and the card id is the
// all-zero shape.

const (
	powerFixtureHost = "node-a"
	powerFixtureCard = "GPU-00000000-0000-0000-0000-000000000000"
	// powerApplierTS is the timestamp layout the applier writes (PowerShell's Get-Date -Format o): seven
	// fractional digits and a numeric offset.
	powerApplierTS = "2006-01-02T15:04:05.0000000-07:00"
)

var powerNow = time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)

// goodPowerState is the record of a whole, successful boot apply at ts.
func goodPowerState(ts time.Time) map[string]any {
	return map[string]any{
		"schema_version":       1,
		"ts":                   ts.Format(powerApplierTS),
		"host":                 powerFixtureHost,
		"outcome":              "applied_in_full",
		"detail":               "9 step(s), 0 named skip(s)",
		"exit_code":            0,
		"dry_run":              false,
		"driver":               "100.00",
		"profile_driver_stamp": "100.00",
		"driver_matches_stamp": true,
		"steps_planned":        9,
		"steps_ok":             9,
		"steps": []any{
			map[string]any{"desc": powerFixtureCard + ": power cap 150 W", "exit_code": 0, "ok": true},
		},
		"named_skips": []any{},
		"boards":      []any{map[string]any{"uuid": powerFixtureCard, "name": "Example Card", "power_limit_w": 150}},
	}
}

func writePowerState(t *testing.T, st map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// powerRow runs the row and holds it to what every verdict shares: one line, the row's label, and nothing
// that identifies the box (the path it read, the host, the card).
func powerRow(t *testing.T, path string, now, bootedAt time.Time) string {
	t.Helper()
	var b strings.Builder
	writePowerProfileSection(&b, path, now, bootedAt)
	got := b.String()
	if !strings.HasPrefix(got, "power:      ") {
		t.Fatalf("the row must start with its 12-column label:\n%q", got)
	}
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Errorf("the power verdict is exactly one row, got:\n%q", got)
	}
	for _, leak := range []string{powerFixtureHost, powerFixtureCard, filepath.Dir(path), "state.json"} {
		if leak != "" && leak != "." && strings.Contains(got, leak) {
			t.Errorf("the row prints %q, which names this box:\n%s", leak, got)
		}
	}
	return got
}

func mustHave(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("the row lacks %q:\n%s", want, got)
		}
	}
}

// No file is not an error: a box that applies no power profile at boot records nothing. One plain line
// says so, names the setting that points the row at a file, and never reads WARN or UNKNOWN. An empty path
// (the setting is unset, and there is no platform default) says the same.
func TestPowerRowSaysNothingIsRecordedWhenThereIsNoStateFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "state.json")
	for name, path := range map[string]string{"absent file": missing, "no path": ""} {
		got := powerRow(t, path, powerNow, powerNow.Add(-5*time.Hour))
		mustHave(t, got, "power:      SKIP  no power profile state is recorded", "set LOCAL_OFFLOAD_POWER_PROFILE_STATE to the path of that file")
		if strings.Contains(got, "WARN") || strings.Contains(got, "UNKNOWN") {
			t.Errorf("%s: an absent state file is not a finding:\n%s", name, got)
		}
	}
}

// A whole apply that ran after the box started is the good verdict. It carries the age, the step count, and
// the limit of what it knows: the row reads the record, not the card.
func TestPowerRowIsOKForAWholeApplyThisBoot(t *testing.T) {
	bootedAt := powerNow.Add(-5 * time.Hour)
	path := writePowerState(t, goodPowerState(powerNow.Add(-3*time.Hour)))
	got := powerRow(t, path, powerNow, bootedAt)
	mustHave(t, got,
		"power:      OK    ",
		"power profile applied this boot",
		"3h ago",
		"9 of 9 steps ok",
		"recorded state only",
		"live power limit and clock lock are not compared",
	)
	if strings.Contains(got, "WARN") {
		t.Errorf("a whole apply this boot is not a warning:\n%s", got)
	}
}

// The incident this row exists for: the boot apply never ran (or never recorded), so the newest record is
// from an earlier boot and the card is at stock limits. The row says no profile was applied this boot and
// how old the last record is.
func TestPowerRowWarnsWhenTheLastRecordPredatesThisBoot(t *testing.T) {
	bootedAt := powerNow.Add(-5 * time.Hour)
	path := writePowerState(t, goodPowerState(powerNow.Add(-49*time.Hour)))
	got := powerRow(t, path, powerNow, bootedAt)
	mustHave(t, got,
		"power:      WARN  ",
		"no power profile was applied this boot",
		"the last recorded run was 2 days ago",
		"(applied_in_full)",
		"stock",
	)
}

// Just after boot the applier has not had its turn: the row waits instead of crying wolf at every start.
func TestPowerRowDoesNotWarnBeforeTheBootApplyIsDue(t *testing.T) {
	bootedAt := powerNow.Add(-3 * time.Minute)
	path := writePowerState(t, goodPowerState(powerNow.Add(-49*time.Hour)))
	got := powerRow(t, path, powerNow, bootedAt)
	mustHave(t, got, "power:      SKIP  ", "3 min ago", "not yet a finding")
	if strings.Contains(got, "WARN") {
		t.Errorf("a box three minutes into its boot must not be warned about its boot apply:\n%s", got)
	}
}

// The run's outcome names what did not happen. Every outcome other than a whole apply is a WARN that says
// which one it was and what it means for the card, and none prints the applier's free-form detail (it can
// carry a path).
func TestPowerRowWarnsOnEveryOutcomeThatLeavesTheProfileOut(t *testing.T) {
	bootedAt := powerNow.Add(-5 * time.Hour)
	for outcome, meaning := range map[string]string{
		"cookie_blocked": "the crash guard skipped this run",
		"plan_empty":     "planned no steps",
		"failed":         "a step failed",
		"refused":        "refused the profile",
		"dry_run":        "dry run",
	} {
		st := goodPowerState(powerNow.Add(-3 * time.Hour))
		st["outcome"] = outcome
		st["detail"] = "clear-cookie.ps1 missing from " + filepath.Join("secret", "dir")
		got := powerRow(t, writePowerState(t, st), powerNow, bootedAt)
		mustHave(t, got, "power:      WARN  ", outcome, meaning, "3h ago")
		if strings.Contains(got, "secret") {
			t.Errorf("%s: the row prints the applier's detail text:\n%s", outcome, got)
		}
	}
	// A dry run is also stated by the flag, whatever the outcome word says.
	st := goodPowerState(powerNow.Add(-3 * time.Hour))
	st["dry_run"] = true
	mustHave(t, powerRow(t, writePowerState(t, st), powerNow, bootedAt), "WARN", "dry run")
}

// The crash guard also skips a run when a cookie from an earlier apply is still on disk, and a resume from sleep a few
// minutes after the boot apply does exactly that: the earlier apply is still in force and the cookie clears itself
// within ten minutes. The record cannot tell that run from one that met a cookie stranded by an earlier boot, so
// the row says what the record supports (this run applied nothing, and how each kind of cookie ends) and does not
// claim that the card is at stock limits or that a person must act.
func TestPowerRowDoesNotClaimStockLimitsForACookieBlockedRunMinutesAfterAnApply(t *testing.T) {
	bootedAt := powerNow.Add(-5 * time.Hour)
	st := goodPowerState(powerNow.Add(-3 * time.Minute))
	st["outcome"] = "cookie_blocked"
	st["steps_planned"] = 0
	st["steps_ok"] = 0
	got := powerRow(t, writePowerState(t, st), powerNow, bootedAt)
	mustHave(t, got,
		"power:      WARN  ",
		"3 min ago",
		"ended cookie_blocked",
		"the crash guard skipped this run because a cookie from an earlier apply is on disk",
		"this run applied nothing",
		"a cookie from this boot clears itself within ten minutes",
		"one from an earlier boot stays until a person clears it",
	)
	for _, claim := range []string{"stock", "stays at", "nothing was applied", "marked unfinished"} {
		if strings.Contains(got, claim) {
			t.Errorf("the row claims %q, which a cookie_blocked record cannot support:\n%s", claim, got)
		}
	}
}

// A run that returned success can still be partial: a step short of the plan, a lever skipped, or a driver
// that differs from the profile's stamp (the tuned levers then wait for a re-evaluation). Each is a WARN that
// says what is missing. A lever skipped by the applier's own design (a card whose power is firmware-owned has
// no cap to apply) is not a finding, or that box would warn on every boot.
func TestPowerRowWarnsOnAPartialApplyButNotOnASkipByDesign(t *testing.T) {
	bootedAt := powerNow.Add(-5 * time.Hour)
	ts := powerNow.Add(-3 * time.Hour)

	short := goodPowerState(ts)
	short["steps_ok"] = 6
	mustHave(t, powerRow(t, writePowerState(t, short), powerNow, bootedAt), "WARN", "only part is in force", "6 of 9 steps ok")

	skipped := goodPowerState(ts)
	skipped["named_skips"] = []any{powerFixtureCard + ": something else was skipped"}
	mustHave(t, powerRow(t, writePowerState(t, skipped), powerNow, bootedAt), "WARN", "only part is in force", "1 lever skip")

	drift := goodPowerState(ts)
	drift["driver_matches_stamp"] = false
	mustHave(t, powerRow(t, writePowerState(t, drift), powerNow, bootedAt), "WARN", "only part is in force", "driver differs from the profile's stamp")

	// null means the driver or the stamp was unknown: not a mismatch.
	unknown := goodPowerState(ts)
	unknown["driver_matches_stamp"] = nil
	mustHave(t, powerRow(t, writePowerState(t, unknown), powerNow, bootedAt), "power:      OK    ")

	byDesign := goodPowerState(ts)
	byDesign["named_skips"] = []any{powerFixtureCard + ": no power_w in profile (lever absent on this machine) - power cap skipped by design"}
	got := powerRow(t, writePowerState(t, byDesign), powerNow, bootedAt)
	mustHave(t, got, "power:      OK    ", "1 lever skipped by design")
}

// A boot time the platform cannot give is not a boot time of zero: the row still reports the last run and its
// age, and says it cannot tell whether that was this boot. It does not claim "this boot", and does not warn.
func TestPowerRowSaysSoWhenTheBootTimeIsUnknown(t *testing.T) {
	path := writePowerState(t, goodPowerState(powerNow.Add(-3*time.Hour)))
	got := powerRow(t, path, powerNow, time.Time{})
	mustHave(t, got, "power:      OK    ", "3h ago", "boot time is unreadable", "whether it ran this boot is unknown")
	if strings.Contains(got, "applied this boot") {
		t.Errorf("the row claims this boot without knowing when it began:\n%s", got)
	}
}

// Unknown is its own answer, never "absent" and never "fine": a file that cannot be read, is not JSON, is
// from a schema this doctor does not read, or carries no usable time says UNKNOWN and why, without the path.
func TestPowerRowIsUnknownWhenTheFileCannotBeTrusted(t *testing.T) {
	bootedAt := powerNow.Add(-5 * time.Hour)
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	noTS := goodPowerState(powerNow)
	delete(noTS, "ts")
	badTS := goodPowerState(powerNow)
	badTS["ts"] = "yesterday"
	newer := goodPowerState(powerNow)
	newer["schema_version"] = 2
	future := goodPowerState(powerNow.Add(3 * time.Hour))
	cases := map[string]struct {
		path string
		why  string
	}{
		"not json":       {write("{ this is not json"), "is not valid JSON"},
		"empty file":     {write(""), "is not valid JSON"},
		"no timestamp":   {writePowerState(t, noTS), "no usable timestamp"},
		"bad timestamp":  {writePowerState(t, badTS), "no usable timestamp"},
		"newer schema":   {writePowerState(t, newer), "schema_version 2"},
		"future":         {writePowerState(t, future), "in the future"},
		"a directory":    {t.TempDir(), "cannot be read"},
		"json not state": {write(`["a","list"]`), "is not valid JSON"},
	}
	for name, c := range cases {
		got := powerRow(t, c.path, powerNow, bootedAt)
		mustHave(t, got, "power:      UNKNOWN ", c.why)
		if strings.Contains(got, "no power profile state is recorded") {
			t.Errorf("%s: an unreadable file was reported as an absent one:\n%s", name, got)
		}
	}
}

// The shape the applier really writes (every value synthetic)
// parses: the timestamp with seven fractional digits and an offset, the steps and boards arrays, the nulls.
func TestPowerRowReadsTheShapeTheApplierWrites(t *testing.T) {
	const real = `{
    "schema_version":  1,
    "ts":  "2026-01-02T09:00:00.1234567-05:00",
    "host":  "node-a",
    "outcome":  "applied_in_full",
    "detail":  "3 step(s), 0 named skip(s)",
    "exit_code":  0,
    "dry_run":  false,
    "driver":  "100.00",
    "profile_driver_stamp":  "100.00",
    "driver_matches_stamp":  true,
    "os_build":  "10000.1000",
    "profile_sha256":  "AB",
    "exe_sha256":  "CD",
    "exe_path":  "tool",
    "steps_planned":  3,
    "steps_ok":  3,
    "steps":  [
        { "desc":  "GPU-00000000-0000-0000-0000-000000000000: power cap 250 W", "exit_code":  0, "ok":  true },
        { "desc":  "GPU-00000000-0000-0000-0000-000000000000: graphics offset +0 MHz", "exit_code":  0, "ok":  true },
        { "desc":  "GPU-00000000-0000-0000-0000-000000000000: lock 200..2500 MHz", "exit_code":  0, "ok":  true }
    ],
    "named_skips":  [

    ],
    "cookie_guard":  true,
    "timer_launched":  true,
    "timer_method":  "start_process_system",
    "timer_pid":  4242,
    "cookie_token":  "0000",
    "cookie_on_disk":  true,
    "cookie_moved_aside":  null,
    "boards":  [
        { "uuid":  "GPU-00000000-0000-0000-0000-000000000000", "name":  "Example Card", "vbios":  "98.00", "power_limit_w":  150 }
    ]
}`
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(real), 0o644); err != nil {
		t.Fatal(err)
	}
	// 09:00 at -05:00 is 14:00 UTC; now is 17:00 UTC, so the record is 3h old; the box booted at 13:00 UTC.
	now := time.Date(2026, 1, 2, 17, 0, 0, 0, time.UTC)
	got := powerRow(t, path, now, time.Date(2026, 1, 2, 13, 0, 0, 0, time.UTC))
	mustHave(t, got, "power:      OK    ", "applied this boot", "3h ago", "3 of 3 steps ok")
}

// The state file is found by the setting that names it and by nothing else. The harness knows no tool's install
// folder, so with the setting unset the row has no path on any platform, whatever sits under %ProgramData%.
func TestPowerProfileStatePathIsTheSettingAndNothingElse(t *testing.T) {
	override := filepath.Join(t.TempDir(), "elsewhere.json")
	t.Setenv("LOCAL_OFFLOAD_POWER_PROFILE_STATE", override)
	if got := powerProfileStatePath(); got != override {
		t.Errorf("the setting must win: got %q, want %q", got, override)
	}
	t.Setenv("LOCAL_OFFLOAD_POWER_PROFILE_STATE", "  "+override+" \t")
	if got := powerProfileStatePath(); got != override {
		t.Errorf("stray whitespace around the setting must not reach the path: got %q, want %q", got, override)
	}

	t.Setenv("LOCAL_OFFLOAD_POWER_PROFILE_STATE", "")
	programData := t.TempDir()
	if err := os.MkdirAll(filepath.Join(programData, "applier"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(programData, "applier", "state.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ProgramData", programData)
	if got := powerProfileStatePath(); got != "" {
		t.Errorf("with the setting unset there is no path on any platform, got %q", got)
	}
}

// The platform gives a boot time on the platforms the harness runs on, and it is a moment before now.
func TestPowerProfileBootTimeIsPlausibleWhereThePlatformCanSayIt(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("no boot time reader on " + runtime.GOOS)
	}
	now := time.Now()
	booted := powerProfileBootTime(now)
	if booted.IsZero() {
		t.Fatal("this platform can say when it booted, the reader said nothing")
	}
	if !booted.Before(now) || now.Sub(booted) > 5*365*24*time.Hour {
		t.Errorf("a boot time of %v is not a plausible moment before %v", booted, now)
	}
}

// The row is in doctor's output, between the pure-config rows, and never changes the exit code: a WARN row
// and an UNKNOWN row both leave doctor green.
func TestDoctorRunPrintsThePowerRowAndStaysGreen(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	old := goodPowerState(time.Now().Add(-1000 * time.Hour))
	for name, path := range map[string]string{
		"warn":    writePowerState(t, old),
		"unknown": filepath.Join(t.TempDir()), // a directory
		"absent":  filepath.Join(t.TempDir(), "state.json"),
	} {
		t.Setenv("LOCAL_OFFLOAD_POWER_PROFILE_STATE", path)
		var out strings.Builder
		if err := doctorRun(cfg, nil, &out); err != nil {
			t.Fatalf("%s: a power row must never fail doctor: %v\n%s", name, err, out.String())
		}
		if !strings.Contains(out.String(), "\npower:      ") {
			t.Errorf("%s: doctor output lacks the power row:\n%s", name, out.String())
		}
	}
}

// An age reads the way an operator says it, rounded to the minute, so a record a hair under three hours old reads
// 3h and an hour boundary never prints "60 min".
func TestPowerProfileAgeReadsLikeAnOperatorSaysIt(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                  "under a minute",
		59 * time.Second:                   "under a minute",
		time.Minute:                        "1 min",
		12*time.Minute + 20*time.Second:    "12 min",
		59*time.Minute + 40*time.Second:    "1h",
		3 * time.Hour:                      "3h",
		3*time.Hour - 200*time.Millisecond: "3h",
		5*time.Hour + 5*time.Minute:        "5h05m",
		47*time.Hour + 59*time.Minute:      "47h59m",
		48 * time.Hour:                     "2 days",
		100 * time.Hour:                    "4 days",
	} {
		if got := powerProfileAge(d); got != want {
			t.Errorf("powerProfileAge(%v) = %q, want %q", d, got, want)
		}
	}
}
