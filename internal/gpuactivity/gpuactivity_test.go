package gpuactivity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelperExitNow is a helper process that exits at once — a real, dead pid
// for the staleness rule.
func TestHelperExitNow(t *testing.T) {
	if os.Getenv("GPUACTIVITY_HELPER") == "1" {
		os.Exit(0)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperExitNow")
	cmd.Env = append(os.Environ(), "GPUACTIVITY_HELPER=1")
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	return cmd.Process.Pid
}

// A registered run is listed with its progress, and disappears at End.
func TestBeginListsTheRunAndEndRemovesIt(t *testing.T) {
	reg := OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	h, err := reg.Begin(Run{Seat: "agent-pool", Kind: "agent_run", Origin: "node-a", Goal: "Summarize   the\nledger", MaxSteps: 12, Phase: "admission"})
	if err != nil {
		t.Fatal(err)
	}
	runs := reg.OnSeat(time.Now(), "AGENT-POOL")
	if len(runs) != 1 || runs[0].PID != os.Getpid() || runs[0].Goal != "Summarize the ledger" || runs[0].Phase != "admission" {
		t.Fatalf("listed: %+v", runs)
	}
	h.OnStep(3, 2310)
	got := reg.List(time.Now())[0]
	if got.Step != 3 || got.TokensOut != 2310 || got.Phase != "running" {
		t.Fatalf("step not recorded: %+v", got)
	}
	if s := got.Summary(time.Now()); !strings.Contains(s, "step 3/12") || !strings.Contains(s, "2310 tokens") || !strings.Contains(s, "from node-a") {
		t.Fatalf("summary: %q", s)
	}
	h.End()
	h.End() // idempotent
	if runs := reg.List(time.Now()); len(runs) != 0 {
		t.Fatalf("run still listed after End: %+v", runs)
	}
	if left, _ := filepath.Glob(filepath.Join(reg.Dir(), "*")); len(left) != 0 {
		t.Fatalf("files left behind: %v", left)
	}
}

// A record whose process is gone is debris: not listed, and swept.
func TestARunOfADeadProcessIsSwept(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gpu", "activity")
	reg := OpenAt(dir)
	h, err := reg.Begin(Run{Seat: "seat", Kind: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.End()
	// Rewrite the record with a dead pid and no start time, as a crashed
	// process would have left it.
	h.Update(func(r *Run) { r.PID = deadPID(t); r.StartTimeMs = 0 })
	if runs := reg.List(time.Now()); len(runs) != 0 {
		t.Fatalf("dead process still listed: %+v", runs)
	}
	if _, err := os.Stat(h.path); !os.IsNotExist(err) {
		t.Fatalf("stale record not swept: %v", err)
	}
}

// A live process that stopped beating is not a run anyone waits for.
func TestASilentRunIsStaleAfterTheHeartbeatTTL(t *testing.T) {
	reg := OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	h, err := reg.Begin(Run{Seat: "seat", Kind: "agent_run"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.End()
	later := time.Now().Add(HeartbeatTTL + time.Second)
	if runs := reg.List(later); len(runs) != 0 {
		t.Fatalf("silent run listed past the TTL: %+v", runs)
	}
}

// A nil handle is a no-op everywhere: a launcher that could not register keeps running.
func TestNilHandleIsSafe(t *testing.T) {
	var h *Handle
	h.OnStep(1, 1)
	h.Phase("x")
	h.End()
	if r := h.Run(); r.PID != 0 {
		t.Fatal("nil handle returned a run")
	}
	if h := Start("", filepath.Join(t.TempDir(), "root"), Run{}); h != nil { // no seat => refused, nil handle
		t.Fatal("a run without a seat must not register")
	}
}

func TestParseGPUsAndProcesses(t *testing.T) {
	gpus := ParseGPUs("0, GPU-1111aaaa-2222-3333-4444-555566667777, NVIDIA GeForce RTX 5060 Ti, 87, 14450, 16311\r\n" +
		"1, GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000, NVIDIA GeForce RTX 5070 Ti, [N/A], 1915, 16303\n" +
		"garbage line\n")
	if len(gpus) != 2 {
		t.Fatalf("gpus: %+v", gpus)
	}
	if !gpus[0].UtilKnown || gpus[0].UtilPct != 87 || gpus[0].MemUsedMiB != 14450 || gpus[0].Name != "NVIDIA GeForce RTX 5060 Ti" {
		t.Fatalf("gpu 0: %+v", gpus[0])
	}
	if gpus[1].UtilKnown {
		t.Fatalf("[N/A] utilization must read as unknown, not idle: %+v", gpus[1])
	}
	procs := ParseProcesses("58596, [N/A], GPU-0c3843d3-5721-d9f7-47fe-89fdb8373e24, C:\\Program Files\\Python314\\python.exe\n" +
		"4242, 1536, GPU-1111aaaa-2222-3333-4444-555566667777, /usr/bin/vllm, with comma\n")
	if len(procs) != 2 || procs[0].UsedKnown || procs[0].PID != 58596 || !strings.HasSuffix(procs[0].Name, "python.exe") {
		t.Fatalf("procs: %+v", procs)
	}
	if !procs[1].UsedKnown || procs[1].UsedMiB != 1536 || procs[1].Name != "/usr/bin/vllm, with comma" {
		t.Fatalf("proc 1: %+v", procs[1])
	}
}

// The verdict vocabulary, one case each, from the readings alone.
func TestAssessVocabulary(t *testing.T) {
	now := time.Now()
	seat := SeatState{Name: "agent-pool", Loaded: true, Inflight: 1, Source: "metrics"}
	run := Run{Kind: "agent_run", PID: 77, Seat: "agent-pool", StartedAtMs: now.Add(-90 * time.Second).UnixMilli(), Step: 2, MaxSteps: 12}
	busy := []GPU{{Index: 1, Name: "RTX", UtilPct: 88, UtilKnown: true, MemTotalMiB: 16000}}
	quiet := []GPU{{Index: 1, Name: "RTX", UtilPct: 2, UtilKnown: true, MemTotalMiB: 16000}}
	holder := &Holder{PID: 60480, Alive: true, Class: "text", AgeSec: 562, Command: "bash rerender.sh", Draining: true}
	cases := []struct {
		name    string
		view    View
		verdict string
		note    []string
	}{
		{"free", View{At: now, GPUs: quiet}, VerdictFree, []string{"cards quiet"}},
		{"loaded idle", View{At: now, Seat: SeatState{Name: "agent-pool", Loaded: true}, GPUs: quiet}, VerdictLoadedIdle, []string{"resident", "ttl"}},
		{"working unreserved", View{At: now, Seat: seat, Runs: []Run{run}}, VerdictWorking, []string{"1 request(s) in flight", "1 registered run(s)", "agent_run pid 77", "step 2/12"}},
		{"held working on seat", View{At: now, Held: true, Holder: holder, Seat: seat}, VerdictWorking, []string{"lease is held AND work is in flight", "holder pid 60480", "draining", "running: bash rerender.sh"}},
		{"held working on cards", View{At: now, Held: true, Holder: holder, GPUs: busy, Seat: SeatState{Name: "agent-pool"}}, VerdictHeldWorking, []string{"88% on card 1", "holder's own job"}},
		{"held idle", View{At: now, Held: true, Holder: holder, GPUs: quiet, Seat: SeatState{Name: "agent-pool"}}, VerdictHeldIdle, []string{"NOTHING is running", "at most 2%", "waiting (a drain, a queue), loading, or stalled"}},
		{"held idle unknown util", View{At: now, Held: true, Holder: holder, GPUErr: "nvidia-smi: not found", Seat: SeatState{Name: "agent-pool"}}, VerdictHeldIdle, []string{"utilization unknown"}},
		{"busy outside", View{At: now, GPUs: busy, Seat: SeatState{Name: "agent-pool"}, Processes: []GPUProcess{{PID: 9, Name: `C:\x\game.exe`}}}, VerdictBusyOutside, []string{"does not own", "game.exe (pid 9)"}},
		{"stale", View{At: now, Stale: true, Holder: &Holder{PID: 5, Reason: "bench"}}, VerdictStaleHolder, []string{"holder that is gone", "pid 5", "reclaims it"}},
		{"stale record beside a loading seat", View{At: now, Stale: true, Holder: &Holder{PID: 5, Reason: "bench"}, Seat: SeatState{Name: "agent-pool", Loaded: true, Starting: true}}, VerdictWorking, []string{"agent-pool is loading", "pid 5", "reclaims it"}},
		{"stale record beside a registered run", View{At: now, Stale: true, Holder: &Holder{PID: 5}, Runs: []Run{run}}, VerdictWorking, []string{"1 registered run(s)", "left over"}},
	}
	for _, c := range cases {
		verdict, note := Assess(c.view)
		if verdict != c.verdict {
			t.Errorf("%s: verdict %q, want %q (%s)", c.name, verdict, c.verdict, note)
		}
		for _, want := range c.note {
			if !strings.Contains(note, want) {
				t.Errorf("%s: note lacks %q: %s", c.name, want, note)
			}
		}
	}
}

// Snapshot composes a real lease dir and registry without a seat or a driver.
func TestSnapshotReadsTheLeaseRootAndTheRegistry(t *testing.T) {
	root := t.TempDir()
	reg, err := Open("", root)
	if err != nil {
		t.Fatal(err)
	}
	h, err := reg.Begin(Run{Seat: "agent-pool", Kind: "contract", Origin: "job-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.End()
	v := Snapshot(context.Background(), Options{StateDir: root})
	if v.Verdict != VerdictWorking || len(v.Runs) != 1 || v.Runs[0].Origin != "job-1" {
		t.Fatalf("snapshot: %s %s runs=%+v", v.Verdict, v.Note, v.Runs)
	}
	m := v.Map()
	if m["verdict"] != VerdictWorking || m["runs"] == nil {
		t.Fatalf("map: %v", m)
	}
	if lines := v.Lines(); len(lines) == 0 || !strings.HasPrefix(lines[0], "activity: WORKING") {
		t.Fatalf("lines: %v", lines)
	}
}

// TestTheCoherenceProbePhaseHealsOnTheFirstStep (reviewer finding, D-118): the
// MCP agent_run door sets the probe phase and the loop is what runs next, so a
// run that never healed it advertised "[coherence-probe]" beside step 7 of a
// multi-minute run in `gpu status` and offload_status. Every pre-run phase
// heals the moment the seat produces a step.
func TestTheCoherenceProbePhaseHealsOnTheFirstStep(t *testing.T) {
	for _, phase := range []string{"admission", "cold-load", "coherence-probe"} {
		t.Run(phase, func(t *testing.T) {
			reg := OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
			h, err := reg.Begin(Run{Seat: "seat", Kind: "agent_run"})
			if err != nil {
				t.Fatal(err)
			}
			defer h.End()
			h.Phase(phase)
			h.OnStep(1, 100)
			if got := h.Run().Phase; got != "running" {
				t.Fatalf("phase = %q after the first step, want %q", got, "running")
			}
			if sum := h.Run().Summary(time.Now()); strings.Contains(sum, phase) {
				t.Fatalf("summary still advertises the pre-run phase: %q", sum)
			}
		})
	}
}

// TestALaterPhaseSurvivesAStep: the heal list is the PRE-RUN phases only. A
// phase the loop itself sets (the forced final step) must not be overwritten by
// the step that follows it, or the status view loses the one phase that says
// the run is finishing.
func TestALaterPhaseSurvivesAStep(t *testing.T) {
	reg := OpenAt(filepath.Join(t.TempDir(), "gpu", "activity"))
	h, err := reg.Begin(Run{Seat: "seat", Kind: "agent_run"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.End()
	h.Phase("final")
	h.OnStep(9, 900)
	if got := h.Run().Phase; got != "final" {
		t.Fatalf("phase = %q, want the loop's own %q kept", got, "final")
	}
}

// TestHeldWorkingNeedsACardTheHarnessCanRunOn pins the 2026-09-20 defect: on the
// Qube, while the operator played a game, `gpu status` read
// `held-working — 33% on card 1 (RTX 5070 Ti)` although the lease holder had
// spent 4 SECONDS of CPU in 141 minutes and the two cards it fenced were at 0%.
// Card 1 is the display card and the 33% was the game. held-working outranks
// held-idle, so the verdict that exists for a stalled holder could never fire on
// a box anyone was using, and three jobs queued behind a holder doing nothing.
func TestHeldWorkingNeedsACardTheHarnessCanRunOn(t *testing.T) {
	const (
		card0 = "GPU-3ee161b5-c188-495b-eaeb-291e6e6e1d97" // 5060 Ti, harness
		card1 = "GPU-2a44210f-6739-2d89-0e21-44cd5143faf7" // 5070 Ti, the display card
		card2 = "GPU-0c3843d3-5721-d9f7-47fe-89fdb8373e24" // 5060 Ti, harness
	)
	// The Qube's real sample: every process nvidia-smi lists is a WDDM GRAPHICS
	// process on the display card with `[N/A]` memory, and the harness's own
	// resident seat on card 0 produces no row at all.
	desktop := []GPUProcess{
		{PID: 18396, Name: `C:\Windows\explorer.exe`, GPUUUID: card1},
		{PID: 65980, Name: `V:\Battle.net\World of Warcraft\_classic_beta_\WowB.exe`, GPUUUID: card1},
		{PID: 11988, Name: `C:\Program Files\Google\Chrome\Application\chrome.exe`, GPUUUID: card1},
	}
	qube := func(u0, u1, u2 int) []GPU {
		return []GPU{
			{Index: 0, UUID: card0, Name: "NVIDIA GeForce RTX 5060 Ti", UtilPct: u0, UtilKnown: true, MemTotalMiB: 16311},
			{Index: 1, UUID: card1, Name: "NVIDIA GeForce RTX 5070 Ti", UtilPct: u1, UtilKnown: true, MemTotalMiB: 16303},
			{Index: 2, UUID: card2, Name: "NVIDIA GeForce RTX 5060 Ti", UtilPct: u2, UtilKnown: true, MemTotalMiB: 16311},
		}
	}
	idleSeat := SeatState{Name: "agent-pool"}
	holder := &Holder{PID: 26464, Alive: true, Class: "text", AgeSec: 8456, Reason: "seat-gate-a107-armA-tuned"}
	now := time.Now()

	for _, c := range []struct {
		name    string
		view    View
		verdict string
		want    []string
		absent  []string
	}{
		{
			name:    "the game on the display card is NOT the holder working",
			view:    View{At: now, Held: true, Holder: holder, Seat: idleSeat, GPUs: qube(0, 33, 0), Processes: desktop},
			verdict: VerdictHeldIdle,
			want:    []string{"NOTHING is running", "display card", "not the holder's work", "33% on card 1", "held 8456s"},
			absent:  []string{"this is the holder's own job"},
		},
		{
			name:    "a busy card the harness CAN use is the holder working",
			view:    View{At: now, Held: true, Holder: holder, Seat: idleSeat, GPUs: qube(90, 33, 0), Processes: desktop},
			verdict: VerdictHeldWorking,
			want:    []string{"90% on card 0", "the holder's own job"},
			absent:  []string{"card 1"},
		},
		{
			// The guard: a single-GPU box runs its seats on its display card by
			// necessity. Excluding it would make held-working unreachable there.
			name: "a single-GPU box still reports held-working on its only card",
			view: View{At: now, Held: true, Holder: holder, Seat: idleSeat,
				GPUs:      []GPU{{Index: 0, UUID: card1, Name: "RTX 3070 Laptop", UtilPct: 90, UtilKnown: true, MemTotalMiB: 8192}},
				Processes: desktop},
			verdict: VerdictHeldWorking,
			want:    []string{"90% on card 0", "the holder's own job"},
		},
		{
			// Linux: --query-compute-apps lists only CUDA processes, with real
			// memory. Nothing is flagged, so every card stays eligible.
			name: "a CUDA process with known memory never marks its card as a display",
			view: View{At: now, Held: true, Holder: holder, Seat: idleSeat, GPUs: qube(0, 88, 0),
				Processes: []GPUProcess{{PID: 900, Name: "/opt/llama/llama-server", UsedMiB: 9000, UsedKnown: true, GPUUUID: card1}}},
			verdict: VerdictHeldWorking,
			want:    []string{"88% on card 1", "the holder's own job"},
		},
		{
			// Unleased, the game IS the answer to "is anything using this box".
			name:    "with no lease the display card still reads busy-outside",
			view:    View{At: now, Seat: idleSeat, GPUs: qube(0, 33, 0), Processes: desktop},
			verdict: VerdictBusyOutside,
			want:    []string{"33% on card 1", "work the harness does not own"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			verdict, note := Assess(c.view)
			if verdict != c.verdict {
				t.Fatalf("verdict = %q, want %q\nnote: %s", verdict, c.verdict, note)
			}
			for _, w := range c.want {
				if !strings.Contains(note, w) {
					t.Errorf("note missing %q\ngot: %s", w, note)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(note, a) {
					t.Errorf("note must not contain %q\ngot: %s", a, note)
				}
			}
		})
	}
}
