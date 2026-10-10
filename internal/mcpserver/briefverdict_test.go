package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// The brief line is what a session reads first. A lease that is orphaned, overdue or
// stalled leads with that word in capitals, so it cannot be skimmed past as "held".
func TestBriefVerdictLineLeadsWithOrphanedOverdueStalled(t *testing.T) {
	for _, tc := range []struct{ verdict, lead string }{
		{"held-orphaned", "ORPHANED"},
		{"held-overdue", "OVERDUE"},
		{"held-stalled", "STALLED"},
		{"tree-orphan", "ORPHANED"},
	} {
		view := map[string]any{
			"verdict": tc.verdict, "held": true, "pid": 77, "class": "media", "age_s": 72000, "reason": "film",
			"activity": map[string]any{"note": "the lease's owner (session s) has been gone for 40m0s [holder pid 77]"},
		}
		line := gpuLeaseVerdictLine(view)
		if !strings.HasPrefix(line, tc.lead+" (") || !strings.Contains(line, tc.verdict) {
			t.Errorf("%s: the line must lead with %s and keep the verdict word: %s", tc.verdict, tc.lead, line)
		}
	}
	// A healthy or merely working lease keeps the verdict word first, as before.
	line := gpuLeaseVerdictLine(map[string]any{"verdict": "held-working", "held": true, "pid": 7, "class": "media", "age_s": 10})
	if !strings.HasPrefix(line, "held-working") {
		t.Fatalf("an unescalated verdict leads with its own word: %s", line)
	}
}

func orphanedFixture(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "gpu", "lease")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rec := map[string]any{
		"epoch": 41, "class": "media", "reason": "film clips (synthetic)", "command": "python film.py",
		"holder":         map[string]any{"pid": os.Getpid(), "start_time_ms": 0},
		"acquired_at_ms": now.Add(-20 * time.Hour).UnixMilli(), "expires_at_ms": now.Add(-2 * time.Hour).UnixMilli(), "renewed_at_ms": now.UnixMilli(),
		"owner": map[string]any{"session": "sess-gone", "pid": 2000000000, "tracked": true},
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o666); err != nil {
		t.Fatal(err)
	}
	mark, _ := json.Marshal(map[string]any{"since_ms": now.Add(-40 * time.Minute).UnixMilli(), "by_pid": 1})
	if err := os.WriteFile(filepath.Join(dir, "orphan.41"), mark, 0o666); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StateDir = root
	return cfg
}

func TestOffloadStatusBriefLeadsWithTheOrphanedLease(t *testing.T) {
	statusSamplesGPU = false
	t.Cleanup(func() { statusSamplesGPU = true })
	view := localLeaseView(context.Background(), orphanedFixture(t))
	if view["verdict"] != "held-orphaned" {
		t.Fatalf("verdict %v (%v)", view["verdict"], view["activity"])
	}
	line := gpuLeaseVerdictLine(view)
	for _, want := range []string{"ORPHANED (held-orphaned)", "sess-gone", "gpu takeover --epoch 41", "queue with:"} {
		if !strings.Contains(line, want) {
			t.Errorf("brief line lacks %q:\n%s", want, line)
		}
	}
}

// The ownership block of the verdict JSON is a contract other sessions branch on: its keys
// are pinned by a golden file. A key added or renamed is a change to every reader.
func TestGPULeaseVerdictJSONSchemaIsStable(t *testing.T) {
	statusSamplesGPU = false
	t.Cleanup(func() { statusSamplesGPU = true })
	view := localLeaseView(context.Background(), orphanedFixture(t))
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatal(err)
	}
	holder, _ := generic["activity"].(map[string]any)["holder"].(map[string]any)
	schema := map[string][]string{"view": schemaKeys(generic), "activity.holder": schemaKeys(holder)}
	got, _ := json.MarshalIndent(schema, "", "  ")
	want, err := os.ReadFile(filepath.Join("testdata", "gpu_lease_verdict_schema.golden.json"))
	if err != nil {
		t.Fatalf("golden missing: %v\n%s", err, got)
	}
	norm := func(b []byte) string { return strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n")) }
	if norm(got) != norm(want) {
		t.Fatalf("the verdict JSON schema changed.\ngot:\n%s\nwant:\n%s", got, want)
	}
	var ks map[string][]string
	_ = json.Unmarshal(want, &ks)
	if !reflect.DeepEqual(ks["view"], schemaKeys(generic)) {
		t.Fatal("unreachable: golden compared above")
	}
}

func schemaKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// `working` outranks the standing verdicts (work in flight is the truth about the seat), but
// the brief is the line a session reads first and it must not read as an ordinary busy box:
// a stalled, orphaned or overdue lease leads with that word in capitals whatever the verdict
// word is, names what is wrong ahead of the work description, and ends with the takeover
// command of THAT lease (its own epoch, not the lowest live one).
func TestBriefLeadsWithTheStandingEvenWhenWorkIsInFlight(t *testing.T) {
	h := &gpuactivity.Holder{PID: 77, Alive: true, Class: "media", Epoch: 43, Stalled: true, Orphaned: true,
		OwnerState: "gone", OwnerSession: "sess-gone", OrphanedForS: 2400, OrphanGraceS: 900,
		Progress: &gpuactivity.ProgressState{File: "/w/log.jsonl", State: "stalled", AgeSec: 3 * 3600, StallSec: 7200}}
	long := strings.Repeat("refactor the ledger reader and rewrite every caller ", 8)
	runs := []gpuactivity.Run{
		{Kind: "agent_run", PID: 11, Seat: "agent-pool", Phase: "running", Step: 3, MaxSteps: 12, Goal: long, StartedAtMs: time.Now().Add(-10 * time.Minute).UnixMilli()},
		{Kind: "contract", PID: 12, Seat: "agent-pool", Phase: "running", Goal: long, StartedAtMs: time.Now().Add(-10 * time.Minute).UnixMilli()},
	}
	verdict, note := gpuactivity.Assess(gpuactivity.View{At: time.Now(), Held: true, Holder: h, Runs: runs,
		Seat: gpuactivity.SeatState{Name: "agent-pool", Inflight: 2, Loaded: true}})
	if verdict != "working" {
		t.Fatalf("setup: work in flight reads `working`, got %s", verdict)
	}
	view := map[string]any{
		"verdict": verdict, "held": true, "pid": 77, "class": "media", "age_s": 72000, "reason": "film",
		"epoch":    uint64(41), // the LOWEST live lease: not the one the verdict is about
		"activity": map[string]any{"note": note, "holder": h},
	}
	line := gpuLeaseVerdictLine(view)
	if !strings.HasPrefix(line, "STALLED (working)") {
		t.Errorf("the line must lead with STALLED and keep the verdict word:\n%s", line)
	}
	for _, want := range []string{"no activity for 3h0m0s", "gone for 40m0s", "gpu takeover --epoch 43", "queue with:"} {
		if !strings.Contains(line, want) {
			t.Errorf("brief line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "--epoch 41") {
		t.Errorf("the takeover command names the lowest epoch, not the stalled lease:\n%s", line)
	}
	// Orphaned alone leads with ORPHANED, overdue alone with OVERDUE.
	for _, tc := range []struct {
		h    *gpuactivity.Holder
		lead string
	}{
		{&gpuactivity.Holder{Epoch: 5, Orphaned: true, OwnerState: "gone", OrphanedForS: 2400, OrphanGraceS: 900}, "ORPHANED (working)"},
		{&gpuactivity.Holder{Epoch: 5, Overdue: true, OverdueBySec: 7200}, "OVERDUE (working)"},
	} {
		view["activity"] = map[string]any{"note": "work", "holder": tc.h}
		if got := gpuLeaseVerdictLine(view); !strings.HasPrefix(got, tc.lead) || !strings.Contains(got, "--epoch 5") {
			t.Errorf("want a line leading %q naming epoch 5:\n%s", tc.lead, got)
		}
	}
	// A healthy holder keeps the plain verdict word and no takeover advice.
	view["activity"] = map[string]any{"note": "work", "holder": &gpuactivity.Holder{Epoch: 5}}
	if got := gpuLeaseVerdictLine(view); !strings.HasPrefix(got, "working") || strings.Contains(got, "takeover") {
		t.Errorf("a healthy lease is not escalated:\n%s", got)
	}
}

// Two live leases, the stalled one the HIGHER epoch: the brief view, its takeover command and
// its owner and progress all describe the stalled lease, never the lowest epoch's.
func TestBriefDescribesTheEscalatedLeaseNotTheLowestEpoch(t *testing.T) {
	statusSamplesGPU = false
	t.Cleanup(func() { statusSamplesGPU = true })
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	healthy, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "healthy render", Devices: []string{"GPU-TEST-0"}, TTL: time.Hour,
		Owner: gpulease.Owner{Session: "sess-healthy", PID: os.Getpid()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = healthy.Release() }()
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(prog, []byte("{\"detail\":\"clip 4 of 17\"}\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	_ = os.Chtimes(prog, old, old)
	stalled, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", Devices: []string{"GPU-TEST-1"}, TTL: time.Hour,
		Unattended: true, ProgressFile: prog, Stall: 10 * time.Millisecond, Owner: gpulease.Owner{Session: "sess-film", PID: os.Getpid()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stalled.Release() }()
	time.Sleep(60 * time.Millisecond)
	if stalled.Epoch() <= healthy.Epoch() {
		t.Fatalf("the stalled lease must be the higher epoch (healthy %d, stalled %d)", healthy.Epoch(), stalled.Epoch())
	}

	cfg := config.Default()
	cfg.StateDir = root
	view := localLeaseView(context.Background(), cfg)
	if view["verdict"] != "held-stalled" {
		t.Fatalf("verdict %v", view["verdict"])
	}
	if view["epoch"] != stalled.Epoch() {
		t.Errorf("view.epoch = %v, want the stalled lease's %d (the lowest live lease is %d)", view["epoch"], stalled.Epoch(), healthy.Epoch())
	}
	if o, _ := view["owner"].(*gpulease.Owner); o == nil || o.Session != "sess-film" {
		t.Errorf("view.owner must be the stalled lease's owner: %+v", view["owner"])
	}
	if p, _ := view["progress"].(*gpulease.Progress); p == nil || p.File != prog {
		t.Errorf("view.progress must be the stalled lease's contract: %+v", view["progress"])
	}
	line := gpuLeaseVerdictLine(view)
	if want := "gpu takeover --epoch " + strconv.FormatUint(stalled.Epoch(), 10); !strings.Contains(line, want) {
		t.Errorf("the takeover command must name the stalled lease (%s):\n%s", want, line)
	}
	if bad := "--epoch " + strconv.FormatUint(healthy.Epoch(), 10) + " "; strings.Contains(line, bad) {
		t.Errorf("the takeover command names the healthy lease:\n%s", line)
	}
}

// An escalated lease's standing is the one thing the brief must not cut: every standing clause
// at once, with a real (long) session id, still reaches the line whole.
func TestBriefDoesNotClipTheStandingOfAnEscalatedLease(t *testing.T) {
	h := &gpuactivity.Holder{PID: 77, Alive: true, Class: "media", Epoch: 43, Stalled: true, Orphaned: true, Overdue: true, TreeOrphan: true,
		OwnerState: "gone", OwnerSession: "00000000-0000-4000-8000-000000000001", OrphanedForS: 2400, OrphanGraceS: 900, OverdueBySec: 7200,
		Progress: &gpuactivity.ProgressState{File: "/w/log.jsonl", State: "stalled", AgeSec: 3 * 3600, StallSec: 7200}}
	verdict, note := gpuactivity.Assess(gpuactivity.View{At: time.Now(), Held: true, Holder: h,
		Seat: gpuactivity.SeatState{Name: "agent-pool", Inflight: 1, Loaded: true}})
	if verdict != "working" {
		t.Fatalf("setup: %s", verdict)
	}
	view := map[string]any{"verdict": verdict, "held": true, "pid": 77, "class": "media", "age_s": 1, "reason": "film",
		"activity": map[string]any{"note": note, "holder": h}}
	line := gpuLeaseVerdictLine(view)
	for _, want := range []string{"no activity for 3h0m0s", "gone for 40m0s", "past its declared window by 2h0m0s", "gpu takeover --epoch 43"} {
		if !strings.Contains(line, want) {
			t.Errorf("the escalated line was clipped before %q:\n%s", want, line)
		}
	}
}

// The brief line names the holder as a LEASE of a class, the phrase `gpu status` leads with: the
// bare class ("held by pid 7 (media, ...") read as a model, and a bench's reservation was taken
// for a text seat (F9, 2026-10-07). Exclusive and draining stay on the line.
func TestBriefVerdictLineNamesALeaseOfAClass(t *testing.T) {
	line := gpuLeaseVerdictLine(map[string]any{
		"verdict": "held-idle", "held": true, "pid": 7, "class": "media", "exclusive": true, "draining": true, "age_s": 10, "reason": "film",
	})
	if want := "; held by a media-class lease (pid 7, exclusive, draining, 10s): film"; !strings.Contains(line, want) {
		t.Errorf("the brief line must say %q:\n%s", want, line)
	}
	if strings.Contains(line, "held by pid") {
		t.Errorf("the brief line names the pid before the lease, which reads as a process, not a reservation:\n%s", line)
	}
	text := gpuLeaseVerdictLine(map[string]any{"verdict": "held-working", "held": true, "pid": 9, "class": "text", "age_s": 3})
	if want := "; held by a text-class lease (pid 9, 3s)"; !strings.Contains(text, want) {
		t.Errorf("a text lease reads %q:\n%s", want, text)
	}
}
