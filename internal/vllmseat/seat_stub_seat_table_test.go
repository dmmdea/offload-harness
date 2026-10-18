package vllmseat

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// Register B-57. Every Windows seat's llama-swap entry runs the SAME seat-cmd.ps1 and seat-cmdstop.ps1 from one seat
// directory, passing its own seat id. The render baked ONE seat's id, port and load budget into them, so a box with two
// Windows seats on one port could not use the rendered stubs: the second seat waited for the first seat's id for its whole
// load budget and failed, and the only way out was a hand-made copy of the stub with none of the template's later fixes
// (crash cleanup, stop marker, stop-result read). The stubs now carry a table from the seat argument to the seat's id,
// port and load budget, rendered from every Windows seat the tier declares, and refuse a seat argument that is not in it.

// stubAlpha and stubBeta are two generic Windows seats on two ports (the reference box shares one port between its two
// seats; two ports make "its own port" observable). Beta has a shorter load budget than the default 480 s.
func stubAlpha() Spec {
	s := pinned()
	s.ID, s.Aliases, s.Unit = "alpha-vllm", []string{"alpha-pool"}, "vllm-alpha-seat"
	s.Port, s.MPPort = 18797, 18796
	return s
}

func stubBeta() Spec {
	s := pinned()
	s.ID, s.Aliases, s.Unit = "beta-vllm", []string{"beta-pool"}, "vllm-beta-seat"
	s.Port, s.MPPort = 18807, 18806
	s.HealthCheckTimeout = 100
	return s
}

const (
	alphaRow = `'alpha-vllm' = @{ Id = 'alpha-vllm'; Port = 18797; HealthTimeout = 480 }`
	betaRow  = `'beta-vllm' = @{ Id = 'beta-vllm'; Port = 18807; HealthTimeout = 100 }`
)

// stubRuntime is the box: a scratch seat directory and a documentation address for the engine.
func stubRuntime(dir string) Runtime {
	rt := wslRT()
	rt.SeatDir = filepath.ToSlash(dir)
	rt.ProxyHost = "192.0.2.10"
	return rt
}

func mustRenderStubs(t *testing.T, dir string, self Spec, others ...Spec) map[string]string {
	t.Helper()
	files, err := self.ArtifactsWith(wslTemplatesDir(), stubRuntime(dir), others)
	if err != nil {
		t.Fatalf("rendering %s with %d other seat(s): %v", self.ID, len(others), err)
	}
	return files
}

// TestSeatStubsCarryEveryWindowsSeatOfTheRender is the Go-only half: it needs no PowerShell. Both stubs hold a row for
// each seat (id, port, load budget), the baked single-seat forms are gone, and the text does not depend on which seat the
// render was run for or on the order the others were listed in. Rendering the seats one after the other therefore writes
// the same two files each time, and the last render cannot drop a seat.
func TestSeatStubsCarryEveryWindowsSeatOfTheRender(t *testing.T) {
	dir := t.TempDir()
	a, b := stubAlpha(), stubBeta()
	fromAlpha := mustRenderStubs(t, dir, a, b)
	fromBeta := mustRenderStubs(t, dir, b, a)
	for _, f := range []string{"seat-cmd.ps1", "seat-cmdstop.ps1"} {
		for _, row := range []string{alphaRow, betaRow} {
			if !strings.Contains(fromAlpha[f], row) {
				t.Errorf("%s rendered for %s holds no row %q:\n%s", f, a.ID, row, fromAlpha[f])
			}
		}
		if fromAlpha[f] != fromBeta[f] {
			t.Errorf("%s differs between the render for %s and the render for %s: the last render would drop or rewrite the other seat", f, a.ID, b.ID)
		}
		if again := mustRenderStubs(t, dir, a, b, a); again[f] != fromAlpha[f] {
			t.Errorf("%s differs when the others are listed in another order, or with the rendered seat repeated", f)
		}
		// Sorted by id, so the text is stable: alpha before beta whichever seat is first.
		if strings.Index(fromBeta[f], alphaRow) > strings.Index(fromBeta[f], betaRow) {
			t.Errorf("%s lists the seats out of id order", f)
		}
	}
	// The forms that baked one seat in must be gone: a stub that still compares against a literal id, or probes a literal
	// port, serves one seat whatever the table says.
	for _, f := range []string{"seat-cmd.ps1", "seat-cmdstop.ps1"} {
		for _, baked := range []string{"-contains 'alpha-vllm'", "-contains 'beta-vllm'", ":18797/", ":18807/", "Max(60, 480"} {
			if strings.Contains(fromAlpha[f], baked) {
				t.Errorf("%s still bakes %q in", f, baked)
			}
		}
	}
	// A render for one seat alone is a table of one: nothing about the other seat leaks in.
	solo := mustRenderStubs(t, dir, a)
	if !strings.Contains(solo["seat-cmd.ps1"], alphaRow) || strings.Contains(solo["seat-cmd.ps1"], "beta-vllm") {
		t.Errorf("Artifacts for one seat must hold that seat only:\n%s", solo["seat-cmd.ps1"])
	}
	if viaArtifacts, err := a.Artifacts(wslTemplatesDir(), stubRuntime(dir)); err != nil || viaArtifacts["seat-cmd.ps1"] != solo["seat-cmd.ps1"] {
		t.Errorf("Artifacts must equal ArtifactsWith without others (err %v)", err)
	}
}

// The seats the table takes are checked as the seat argument they become: a refusal at render time, naming the seat, beats a
// stub whose table the PowerShell parser rejects (or reads differently) on the box.
func TestSeatStubsRefuseASeatTheyCannotServe(t *testing.T) {
	dir := t.TempDir()
	a := stubAlpha()
	linux := ref()
	badID := stubBeta()
	badID.ID = "beta'; Remove-Item x; '"
	noPort := stubBeta()
	noPort.Port = 0
	clash := stubBeta()
	clash.ID, clash.Port = a.ID, a.Port+1
	for name, tc := range map[string]struct {
		self   Spec
		others []Spec
		want   string
	}{
		"a seat that starts through systemd":    {a, []Spec{linux}, "starts through"},
		"an id that is not a seat argument":     {a, []Spec{badID}, "must match"},
		"a port out of range":                   {a, []Spec{noPort}, "out of range"},
		"one id on two ports":                   {a, []Spec{clash}, "given twice"},
		"others for a seat that has no stub":    {linux, []Spec{stubBeta()}, "renders no shared PowerShell stub"},
		"others for a seat that has no stub, 2": {linux, []Spec{a, stubBeta()}, "read by nothing"},
	} {
		t.Run(name, func(t *testing.T) {
			files, err := tc.self.ArtifactsWith(wslTemplatesDir(), stubRuntime(dir), tc.others)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal naming %q, got files=%d err=%v", tc.want, len(files), err)
			}
			if len(files) != 0 {
				t.Errorf("a refused render still returned %d file(s)", len(files))
			}
		})
	}
	// The same seat given again with the same values is not a conflict (a caller may pass the tier's whole set).
	if _, err := a.ArtifactsWith(wslTemplatesDir(), stubRuntime(dir), []Spec{a, stubBeta()}); err != nil {
		t.Errorf("the rendered seat repeated with the same values must be accepted: %v", err)
	}
}

// seatTableDriver stands in for the cmdlets the two stubs call, like seatCmdDriver does, but for what the seat table
// decides: the port each probe goes to, the id a port answers /v1/models with, the load budget and what the unknown-seat
// refusal does. A simulated clock (Start-Sleep advances it) makes a 460 s load budget instant.
//
// Models is "port=id,port=id", or "none" for a world where nothing answers. In mode "start" a served port answers /health
// twice, then stops; the seat's start task is Running throughout, so a stub that exits after the silence skips the crash
// cleanup (a live launcher owns the seat) and the stop task is never started. In mode "stop" a served port keeps answering
// /health, so a stop stub that watches the wrong port never sees the seat go quiet. Every call is written to the call log.
const seatTableDriver = `param([string]$Stub, [string]$Seat, [string]$Calls, [string]$Mode, [string]$Models)
$ErrorActionPreference = 'Continue'
$global:CallLog = $Calls
$global:Mode = $Mode
$global:Probes = 0
$global:Served = @{}
if ($Models -and $Models -ne 'none') { foreach ($p in $Models.Split(',')) { $kv = $p.Split('='); $global:Served[$kv[0]] = $kv[1] } }
$global:T0 = [datetime]'2026-01-01T00:00:00'
$global:Clock = $global:T0
function Secs { [int][math]::Round(($global:Clock - $global:T0).TotalSeconds) }
function Get-Date { param([string]$Format) if ($Format) { return $global:Clock.ToString($Format) }; return $global:Clock }
function Note([string]$m) { Add-Content -Path $global:CallLog -Value $m }
function Start-Sleep { param($Seconds) $global:Clock = $global:Clock.AddSeconds([double]$Seconds) }
function Start-ScheduledTask { [CmdletBinding()] param([string]$TaskName) Note "start-task $TaskName" }
function Get-ScheduledTask { [CmdletBinding()] param([string]$TaskName) return [pscustomobject]@{ State = 'Running' } }
function Invoke-RestMethod {
  [CmdletBinding()] param([string]$Uri, [int]$TimeoutSec)
  Note "get $Uri"
  if ($Uri -notmatch '^http://[^:/]+:([0-9]+)(/.*)$') { throw "unexpected uri $Uri" }
  $port = $Matches[1]; $path = $Matches[2]
  $id = $global:Served[$port]
  if ($null -eq $id) { throw 'Unable to connect to the remote server' }
  if ($path -eq '/v1/models') { return [pscustomobject]@{ data = @([pscustomobject]@{ id = $id }) } }
  if ($global:Mode -eq 'stop') { return $null }
  $global:Probes++
  if ($global:Probes -le 2) { return $null }
  throw 'Unable to connect to the remote server'
}
& $Stub -Seat $Seat
Note "clock $(Secs)"
Note "exit=$LASTEXITCODE"
`

// tableRun is what one run of a rendered stub left behind: the call log, every log file the stub wrote (the per-seat
// log, and the fixed one the unknown-seat refusal uses), and the files in the scratch seat directory.
type tableRun struct {
	Calls string
	Log   string
	Files []string
}

func (r tableRun) exit() string {
	for _, l := range strings.Split(r.Calls, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "exit="); ok {
			return v
		}
	}
	return ""
}

func (r tableRun) clock() int {
	for _, l := range strings.Split(r.Calls, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "clock "); ok {
			n, _ := strconv.Atoi(v)
			return n
		}
	}
	return -1
}

// gets lists the URIs the stub asked for, in order.
func (r tableRun) gets() []string {
	var out []string
	for _, l := range strings.Split(r.Calls, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "get "); ok {
			out = append(out, v)
		}
	}
	return out
}

func (r tableRun) has(file string) bool {
	for _, f := range r.Files {
		if f == file {
			return true
		}
	}
	return false
}

// runTableStub writes the rendered artifact into a fresh scratch seat directory (the one the files were rendered for) and
// runs it under the driver for the seat argument.
func runTableStub(t *testing.T, host string, render func(dir string) map[string]string, artifact, seat, mode, models string) tableRun {
	t.Helper()
	dir := t.TempDir()
	files := render(dir)
	stub := filepath.Join(dir, artifact)
	driver := filepath.Join(dir, "driver.ps1")
	callLog := filepath.Join(dir, "calls.txt")
	if err := os.WriteFile(stub, []byte(files[artifact]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driver, []byte(seatTableDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := winexec.CommandContext(ctx, host, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", driver, "-Stub", stub, "-Seat", seat, "-Calls", callLog, "-Mode", mode, "-Models", models).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: the driver failed (%v):\n%s", artifact, seat, err, out)
	}
	raw, _ := os.ReadFile(callLog)
	var log strings.Builder
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if strings.HasPrefix(e.Name(), "seat-cmd") && strings.HasSuffix(e.Name(), ".log") {
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			// Out-File writes UTF-16 in Windows PowerShell 5.1: drop the NULs so the log reads as text.
			log.WriteString(e.Name() + ": " + strings.ReplaceAll(strings.ReplaceAll(string(b), "\x00", ""), "\r", ""))
		}
	}
	r := tableRun{Calls: strings.ReplaceAll(string(raw), "\r", ""), Log: log.String(), Files: names}
	t.Logf("%s for %s via %s (%s, models %s)\ncalls:\n%s\nlogs:\n%s", artifact, seat, filepath.Base(host), mode, models, r.Calls, r.Log)
	return r
}

func forEachPowerShell(t *testing.T, run func(t *testing.T, host string)) {
	t.Helper()
	hosts := powerShellHosts(t)
	if len(hosts) == 0 {
		t.Skip("no PowerShell on this box: the seat stubs run under it")
	}
	for _, host := range hosts {
		host := host
		t.Run(strings.TrimSuffix(filepath.Base(host), ".exe"), func(t *testing.T) { run(t, host) })
	}
}

// renderBoth is the render every PowerShell test runs: the stubs for the two seats, as the tier's render writes them.
func renderBoth(t *testing.T) func(dir string) map[string]string {
	return func(dir string) map[string]string { return mustRenderStubs(t, dir, stubAlpha(), stubBeta()) }
}

// TestSeatCmdWaitsForItsOwnSeatOnItsOwnPort: with two seats in the table, each seat argument probes ITS port, waits for ITS
// id, comes up, and starts ITS task. The other seat's port is never asked: a stub that did would be watching the wrong seat.
func TestSeatCmdWaitsForItsOwnSeatOnItsOwnPort(t *testing.T) {
	forEachPowerShell(t, func(t *testing.T, host string) {
		for _, tc := range []struct {
			seat, port, otherPort string
		}{
			{"alpha-vllm", "18797", "18807"},
			{"beta-vllm", "18807", "18797"},
		} {
			t.Run(tc.seat, func(t *testing.T) {
				r := runTableStub(t, host, renderBoth(t), "seat-cmd.ps1", tc.seat, "start", "18797=alpha-vllm,18807=beta-vllm")
				if r.exit() != "0" {
					t.Fatalf("%s: exit=%q, want 0 (up, then the silence exit)", tc.seat, r.exit())
				}
				if !strings.Contains(r.Calls, "start-task vllm-seat-"+tc.seat+"\n") {
					t.Errorf("%s must start its own task vllm-seat-%s:\n%s", tc.seat, tc.seat, r.Calls)
				}
				if !strings.Contains(r.Log, "seat up") {
					t.Errorf("%s never came up although its id answers on its port:\n%s", tc.seat, r.Log)
				}
				gets := r.gets()
				if len(gets) == 0 || gets[0] != "http://192.0.2.10:"+tc.port+"/v1/models" {
					t.Errorf("%s must ask %s/v1/models first, asked %v", tc.seat, tc.port, gets)
				}
				for _, g := range gets {
					if strings.Contains(g, ":"+tc.otherPort+"/") {
						t.Errorf("%s asked the other seat's port: %s", tc.seat, g)
					}
				}
			})
		}
	})
}

// TestSeatCmdWaitsForItsOwnIdNotAnotherSeats: the id on the seat's port must be the seat's own. Beta's port answering with
// alpha's id is not beta (a second seat on a port the first seat still holds), so beta must not come up on it; and alpha's
// stub must not come up on beta's id.
func TestSeatCmdWaitsForItsOwnIdNotAnotherSeats(t *testing.T) {
	forEachPowerShell(t, func(t *testing.T, host string) {
		for _, tc := range []struct{ seat, models string }{
			{"beta-vllm", "18807=alpha-vllm,18797=alpha-vllm"},
			{"alpha-vllm", "18797=beta-vllm,18807=beta-vllm"},
		} {
			t.Run(tc.seat, func(t *testing.T) {
				r := runTableStub(t, host, renderBoth(t), "seat-cmd.ps1", tc.seat, "start", tc.models)
				if r.exit() != "4" || !strings.Contains(r.Log, "never answered") || strings.Contains(r.Log, "seat up") {
					t.Errorf("%s came up on another seat's id (exit=%q); it must wait out its budget and exit 4:\n%s", tc.seat, r.exit(), r.Log)
				}
			})
		}
	})
}

// TestSeatCmdUsesTheLoadBudgetOfItsOwnSeat: the table carries each seat's load budget (llama-swap's health timeout less 20 s,
// floored at 60 s), so a seat with a short budget is not held to the render's seat's 480 s.
func TestSeatCmdUsesTheLoadBudgetOfItsOwnSeat(t *testing.T) {
	forEachPowerShell(t, func(t *testing.T, host string) {
		for _, tc := range []struct {
			seat     string
			min, max int
		}{
			{"alpha-vllm", 460, 475}, // 480 - 20
			{"beta-vllm", 80, 95},    // 100 - 20
		} {
			t.Run(tc.seat, func(t *testing.T) {
				r := runTableStub(t, host, renderBoth(t), "seat-cmd.ps1", tc.seat, "start", "none")
				if c := r.clock(); r.exit() != "4" || c < tc.min || c > tc.max {
					t.Errorf("%s gave up at %d s with exit=%q, want exit 4 between %d and %d s (its own load budget)", tc.seat, c, r.exit(), tc.min, tc.max)
				}
			})
		}
	})
}

// TestSeatCmdRefusesASeatItWasNotRenderedFor: an unknown seat argument logs and exits 5 before it starts a task or asks a
// port, and it writes no file named after the argument (the argument is not trusted into a path).
func TestSeatCmdRefusesASeatItWasNotRenderedFor(t *testing.T) {
	forEachPowerShell(t, func(t *testing.T, host string) {
		// A near miss is as unknown as a stranger: only the exact ids of the table are served.
		for _, seat := range []string{"gamma-vllm", "alpha-vllm-2", "alpha"} {
			t.Run(seat, func(t *testing.T) {
				r := runTableStub(t, host, renderBoth(t), "seat-cmd.ps1", seat, "start", "18797=alpha-vllm,18807=beta-vllm")
				if r.exit() != "5" {
					t.Errorf("an unknown seat must exit 5, got %q", r.exit())
				}
				if strings.Contains(r.Calls, "start-task") || len(r.gets()) != 0 {
					t.Errorf("an unknown seat must start nothing and ask nothing:\n%s", r.Calls)
				}
				if !strings.Contains(r.Log, "unknown seat '"+seat+"'") || !strings.Contains(r.Log, "alpha-vllm, beta-vllm") {
					t.Errorf("the refusal must name the seat and the seats the stub serves:\n%s", r.Log)
				}
				for _, f := range r.Files {
					if strings.Contains(f, seat) {
						t.Errorf("the refusal wrote a file named after the unknown seat: %s", f)
					}
				}
			})
		}
	})
}

// TestSeatCmdstopStopsItsOwnSeatOnItsOwnPort: the stop stub watches the port of the seat it was asked to stop. The other
// seat keeps answering on its port the whole time, so a stub watching it would wait out its 55 s and exit 1. It leaves the
// marker for this seat only and starts this seat's stop task.
func TestSeatCmdstopStopsItsOwnSeatOnItsOwnPort(t *testing.T) {
	forEachPowerShell(t, func(t *testing.T, host string) {
		for _, tc := range []struct{ seat, port, otherPort, models string }{
			{"alpha-vllm", "18797", "18807", "18807=beta-vllm"},
			{"beta-vllm", "18807", "18797", "18797=alpha-vllm"},
		} {
			t.Run(tc.seat, func(t *testing.T) {
				r := runTableStub(t, host, renderBoth(t), "seat-cmdstop.ps1", tc.seat, "stop", tc.models)
				if r.exit() != "0" || !strings.Contains(r.Log, "seat stopped") {
					t.Errorf("%s: the stop must see its own port quiet and exit 0, got exit=%q:\n%s", tc.seat, r.exit(), r.Log)
				}
				if !strings.Contains(r.Calls, "start-task vllm-seat-stop-"+tc.seat+"\n") {
					t.Errorf("%s: its own stop task was not started:\n%s", tc.seat, r.Calls)
				}
				if !r.has("seat-stop-requested-" + tc.seat) {
					t.Errorf("%s: no stop marker for it in %v", tc.seat, r.Files)
				}
				for _, g := range r.gets() {
					if !strings.HasPrefix(g, "http://192.0.2.10:"+tc.port+"/") {
						t.Errorf("%s: the stop asked a port that is not its own: %s", tc.seat, g)
					}
				}
			})
		}
	})
}

// TestSeatCmdstopRefusesASeatItWasNotRenderedFor: an unknown seat is refused before the marker is written or a task is
// started, so a misnamed entry cannot leave a marker that makes a later crash look like an unload.
func TestSeatCmdstopRefusesASeatItWasNotRenderedFor(t *testing.T) {
	forEachPowerShell(t, func(t *testing.T, host string) {
		r := runTableStub(t, host, renderBoth(t), "seat-cmdstop.ps1", "gamma-vllm", "stop", "18797=alpha-vllm,18807=beta-vllm")
		if r.exit() != "5" {
			t.Errorf("an unknown seat must exit 5, got %q", r.exit())
		}
		if strings.Contains(r.Calls, "start-task") || len(r.gets()) != 0 {
			t.Errorf("an unknown seat must stop nothing and ask nothing:\n%s", r.Calls)
		}
		if !regexp.MustCompile(`unknown seat 'gamma-vllm'`).MatchString(r.Log) {
			t.Errorf("the refusal must name the seat:\n%s", r.Log)
		}
		for _, f := range r.Files {
			if strings.Contains(f, "gamma") || strings.HasPrefix(f, "seat-stop-requested") {
				t.Errorf("the refusal left %s behind", f)
			}
		}
	})
}
