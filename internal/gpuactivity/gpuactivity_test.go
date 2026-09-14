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
