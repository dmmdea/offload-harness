# Fleet Overview (PAIR-inspired operator surface) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the offload harness the operator surface the operator liked in NVIDIA PAIR — a live per-node overview page, a cluster-wide jobs and errors feed, a `fleet-smoke` traffic test, a `top` terminal view — plus two placement signals (served-model gate, GPU-utilization tie-breaker), without adopting PAIR.

**Architecture:** Nodes (`fleet-serve`) grow four additive health fields and one new read-only route (`GET /fleet/jobs`); all sampling stays in the existing background samplers. The delegator grows a new `internal/fleetview` package: a poller that folds every node's health + jobs + the local delegation-log into one in-memory `Overview`, served as JSON on `/api/overview` and as one embedded vanilla-JS HTML page on `/` by a new `fleet-ui` verb (loopback by default, tailnet with `--listen-trusted-network`). `fleet-smoke` and `top` are thin consumers of `delegate.Run` and `/api/overview` respectively. The placement gate in `internal/delegate/gate.go` reads two new `NodeView` fields.

**Tech Stack:** Go 1.26 (`net/http` `ServeMux` patterns, `embed`, `httptest`), `nvidia-smi` CSV, `/proc` on Linux, `kernel32.GlobalMemoryStatusEx` + `GetSystemTimes` on Windows, vanilla HTML/JS/SVG, `github.com/charmbracelet/bubbletea` for `top` only.

**Spec:** `docs/superpowers/specs/2026-09-03-fleet-overview-spec.md`

## Global Constraints

- Never-cloud, tailnet-only: UI listener defaults to `127.0.0.1:18813`; non-loopback binds need `--listen-trusted-network`; every outbound HTTP client uses `netguard.SafeTransport(nil)`.
- Additive wire only: new `/fleet/health` fields are `omitempty`; absence decodes to zero and is read as UNKNOWN (the house rule `AgentCtxTokens == 0` already follows).
- Health never blocks: no sampling, no llama-swap call, no `nvidia-smi` inside `handleHealth`.
- No JS frameworks, no CDN, no external requests from the page.
- Version bump to `0.113.0` in `VERSION`, `internal/buildinfo/buildinfo.go`, `.printing-press.json` — together, in Task 10.
- Docs in the same PR: `docs/systems/fleet-overview.md`, `docs/systems/fleet-node.md` (health fields), `docs/README.md` index, `AGENTS.md` pointer, ADR 0034, CHANGELOG, `P:\Port Directory\qube-ports.md` row for 18813.
- Placement law unchanged: `Place` returns local when `!localBusy`; the three existing `betterRemote` keys keep their order; new logic only appends.
- Tests: table-driven, in the package's existing `_test.go` style; every new guard is broken once at its call site during Task verification (clean-ship Consequential rule 3 applies to Task 7, the placement change).
- Repo root for all paths below: `D:\Dev\dmmdea\local-offload-public` (work in the worktree `D:\Dev\dmmdea\trees\local-offload-public-fleet-overview`, branch `feat/fleet-overview` — create it from `main` at execution time; the plan branch is `docs/fleet-overview-plan`).

---

## File map

| Path | Responsibility |
|---|---|
| `internal/fleetnode/vram.go` | `GPUDevice.UtilPct` + parsing of the 6-field nvidia-smi line |
| `main.go:2055` | the nvidia-smi device query gains `utilization.gpu` |
| `internal/hostsample/hostsample.go`, `hostsample_linux.go`, `hostsample_windows.go`, `hostsample_test.go` | CPU % and RAM used/total sampler (new package) |
| `internal/fleetnode/server.go` | health fields `gpu_util_pct`, `host_cpu_pct`, `host_ram_used_gb`, `host_ram_total_gb`, `served_models`; route `GET /fleet/jobs` |
| `internal/fleetnode/jobs.go` | job metadata (task, model, timestamps) + `Recent(n)` |
| `internal/delegate/nodeview.go`, `gate.go`, `gate_test.go` | `ServedModels`, `GpuUtilPct`, `GpuUtilKnown`; gate + tie-breaker |
| `internal/fleetview/overview.go`, `poller.go`, `errors.go`, `server.go`, `ui.html`, `*_test.go` | delegator-side aggregation, HTTP API, page (new package) |
| `fleet_ui_cmd.go` (repo root, beside `gpu_cmd.go`) | `fleet-ui` verb |
| `fleet_smoke_cmd.go`, `fleet_smoke_test.go` | `fleet-smoke` verb |
| `top_cmd.go`, `internal/fleetview/topmodel.go` | `top` terminal view |
| `docs/systems/fleet-overview.md`, `docs/architecture/decisions/0034-fleet-overview-is-a-read-only-page-on-the-delegator.md`, `docs/systems/fleet-node.md`, `docs/README.md`, `AGENTS.md`, `CHANGELOG.md` | docs |

---

### Task 1: GPU utilization on the node

**Files:**
- Modify: `internal/fleetnode/vram.go:57-110` (`GPUDevice`, `ParseSmiMemoryDevices`)
- Modify: `main.go:2055` (the device query command)
- Modify: `internal/fleetnode/server.go:372-456` (`healthPayload`) and `:462-540` (`handleHealth`)
- Test: `internal/fleetnode/vram_test.go`, `internal/fleetnode/server_test.go`

**Interfaces:**
- Consumes: `Snapshot.Devices []GPUDevice` (already published by `StartDeviceProbeSampler`).
- Produces: `GPUDevice.UtilPct int` (`json:"util_pct"`), health `gpu_util_pct int` (`json:"gpu_util_pct,omitempty"`) = the BUSIEST device's utilization (PAIR's rule, documented as such), and `GpuUtilKnown` semantics: the field is emitted only when the source is nvidia-smi and the line carried 6 fields.

- [ ] **Step 1: Write the failing parser test**

Append to `internal/fleetnode/vram_test.go`:

```go
func TestParseSmiMemoryDevices_SixFieldsCarriesUtilization(t *testing.T) {
	out := "0, GPU-aaaa, NVIDIA GeForce RTX 5070 Ti, 16303, 2456, 37\r\n" +
		"1, GPU-bbbb, NVIDIA GeForce RTX 5060 Ti, 16311, 14100, 0\r\n"
	devs, err := ParseSmiMemoryDevices(out)
	if err != nil {
		t.Fatal(err)
	}
	if devs[0].UtilPct != 37 || devs[1].UtilPct != 0 {
		t.Fatalf("util: got %d,%d want 37,0", devs[0].UtilPct, devs[1].UtilPct)
	}
	if !devs[0].UtilKnown || !devs[1].UtilKnown {
		t.Fatal("6-field line must mark UtilKnown")
	}
}

func TestParseSmiMemoryDevices_FiveFieldsStillParses(t *testing.T) {
	out := "0, GPU-aaaa, NVIDIA GeForce RTX 3050, 6144, 1024\n"
	devs, err := ParseSmiMemoryDevices(out)
	if err != nil {
		t.Fatal(err)
	}
	if devs[0].UtilKnown || devs[0].UtilPct != 0 {
		t.Fatal("5-field line must leave util UNKNOWN")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/fleetnode -run 'TestParseSmiMemoryDevices_' -v`
Expected: FAIL — `devs[0].UtilPct undefined`.

- [ ] **Step 3: Implement**

In `internal/fleetnode/vram.go` extend the struct and the parser:

```go
type GPUDevice struct {
	Index    int     `json:"index"`
	UUID     string  `json:"uuid"`
	Name     string  `json:"name"`
	TotalGiB float64 `json:"vram_total_gb"`
	FreeGiB  float64 `json:"vram_free_gb"`
	// UtilPct is nvidia-smi utilization.gpu (0-100) when the query carried it.
	// UtilKnown distinguishes "0 %" from "not queried" (a 5-field line from an
	// older launcher); consumers must never treat an unknown as idle.
	UtilPct   int  `json:"util_pct"`
	UtilKnown bool `json:"util_known"`
}
```

In `ParseSmiMemoryDevices`, replace the `len(fields) != 5` refusal with:

```go
		if len(fields) != 5 && len(fields) != 6 {
			return nil, fmt.Errorf("nvidia-smi device query: want 5 or 6 CSV fields, got %d in %q", len(fields), line)
		}
		// ... existing index/uuid/name/total/used parsing unchanged ...
		if len(fields) == 6 {
			u, err := strconv.Atoi(strings.TrimSpace(fields[5]))
			if err != nil || u < 0 || u > 100 {
				return nil, fmt.Errorf("nvidia-smi utilization.gpu %q: not a percentage", strings.TrimSpace(fields[5]))
			}
			d.UtilPct, d.UtilKnown = u, true
		}
```

In `main.go:2055` change the query to:

```go
out, err := exec.Command("nvidia-smi", "--query-gpu=index,uuid,name,memory.total,memory.used,utilization.gpu", "--format=csv,noheader,nounits").Output()
```

In `internal/fleetnode/server.go` add to `healthPayload` (after `GpuDevices`):

```go
	// GpuUtilPct is the BUSIEST device's utilization (PAIR's multi-GPU rule,
	// adopted deliberately: the shared card is the one that matters). Omitted
	// when no device published a known utilization — absent ≠ idle.
	GpuUtilPct   int  `json:"gpu_util_pct,omitempty"`
	GpuUtilKnown bool `json:"gpu_util_known,omitempty"`
```

and in `handleHealth` after `payload := healthPayload{...}`:

```go
	for _, d := range snap.Devices {
		if !d.UtilKnown {
			continue
		}
		payload.GpuUtilKnown = true
		if d.UtilPct > payload.GpuUtilPct {
			payload.GpuUtilPct = d.UtilPct
		}
	}
```

- [ ] **Step 4: Write the health test and run all**

Append to `internal/fleetnode/server_test.go` (use the existing `newTestServer` and an `Options.Snapshot` returning two devices, one `UtilKnown` at 37 and one at 12):

```go
func TestHealth_GpuUtilIsBusiestDevice(t *testing.T) {
	opts := &Options{Snapshot: func() (Snapshot, bool) {
		return Snapshot{TotalGiB: 32, FreeGiB: 20, At: time.Now(), Devices: []GPUDevice{
			{Index: 0, UUID: "a", UtilPct: 12, UtilKnown: true},
			{Index: 1, UUID: "b", UtilPct: 37, UtilKnown: true},
		}}, true
	}}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest("GET", "/fleet/health", nil))
	var got struct {
		Util  int  `json:"gpu_util_pct"`
		Known bool `json:"gpu_util_known"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Util != 37 || !got.Known {
		t.Fatalf("got %+v want util=37 known=true", got)
	}
}
```

Run: `go test ./internal/fleetnode -v -run 'Util|ParseSmiMemoryDevices'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/fleetnode/vram.go internal/fleetnode/vram_test.go internal/fleetnode/server.go internal/fleetnode/server_test.go main.go
git commit -m "fleet-node: advertise gpu_util_pct (busiest device) from nvidia-smi utilization.gpu"
```

---

### Task 2: Host CPU and RAM sampler

**Files:**
- Create: `internal/hostsample/hostsample.go`, `internal/hostsample/hostsample_linux.go`, `internal/hostsample/hostsample_windows.go`, `internal/hostsample/hostsample_other.go`, `internal/hostsample/hostsample_test.go`
- Modify: `internal/fleetnode/server.go` (`Options.Host`, health fields), `main.go` fleet-serve wiring near `:2290-2320`

**Interfaces:**
- Produces: `package hostsample` — `type Sample struct { CPUPct int; RAMUsedGiB, RAMTotalGiB float64; At time.Time; Known bool }`, `type Sampler struct{...}`, `func Start(ctx context.Context, interval time.Duration) *Sampler`, `func (s *Sampler) Load() (Sample, bool)`.
- Health fields: `host_cpu_pct int`, `host_ram_used_gb float64`, `host_ram_total_gb float64`, all `omitempty`, emitted only when `Known`.

- [ ] **Step 1: Write the failing tests**

`internal/hostsample/hostsample_test.go`:

```go
package hostsample

import (
	"context"
	"testing"
	"time"
)

func TestCPUPctFromTwoReadings(t *testing.T) {
	// idle 100→180 (+80), total 200→300 (+100) ⇒ busy 20 %
	got := cpuPct(cpuTimes{idle: 100, total: 200}, cpuTimes{idle: 180, total: 300})
	if got != 20 {
		t.Fatalf("got %d want 20", got)
	}
}

func TestCPUPctNoElapsedIsUnknown(t *testing.T) {
	if got := cpuPct(cpuTimes{idle: 5, total: 9}, cpuTimes{idle: 5, total: 9}); got != -1 {
		t.Fatalf("got %d want -1 (unknown)", got)
	}
}

func TestStartPublishesWithinTwoIntervals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := Start(ctx, 50*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if smp, ok := s.Load(); ok {
			if smp.RAMTotalGiB <= 0 {
				t.Fatalf("RAM total must be positive, got %v", smp.RAMTotalGiB)
			}
			if smp.CPUPct < 0 || smp.CPUPct > 100 {
				t.Fatalf("cpu %% out of range: %d", smp.CPUPct)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no sample published")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/hostsample -v`
Expected: FAIL — package does not exist / `cpuPct` undefined.

- [ ] **Step 3: Implement the portable half**

`internal/hostsample/hostsample.go`:

```go
// Package hostsample publishes a coarse host CPU % and RAM used/total for
// /fleet/health. It is a background sampler like fleetnode's VRAM Sampler:
// the health handler only ever Loads the last value.
package hostsample

import (
	"context"
	"sync/atomic"
	"time"
)

type Sample struct {
	CPUPct      int     // 0-100
	RAMUsedGiB  float64
	RAMTotalGiB float64
	At          time.Time
}

// cpuTimes is one cumulative CPU-time reading (any unit; ratios only).
type cpuTimes struct{ idle, total float64 }

// cpuPct is busy % between two readings; -1 = no elapsed time (unknown).
func cpuPct(prev, cur cpuTimes) int {
	dt := cur.total - prev.total
	if dt <= 0 {
		return -1
	}
	busy := dt - (cur.idle - prev.idle)
	p := int(busy/dt*100 + 0.5)
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p
}

type Sampler struct{ v atomic.Value }

func (s *Sampler) Load() (Sample, bool) {
	x := s.v.Load()
	if x == nil {
		return Sample{}, false
	}
	return x.(Sample), true
}

// Start samples every interval. The first CPU % needs two readings, so the
// first publish happens at the second tick; RAM is read on every tick.
func Start(ctx context.Context, interval time.Duration) *Sampler {
	s := &Sampler{}
	go func() {
		prev, ok := readCPU()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			cur, cok := readCPU()
			used, total, rok := readRAM()
			if !cok || !rok || !ok {
				prev, ok = cur, cok
				continue
			}
			if p := cpuPct(prev, cur); p >= 0 {
				s.v.Store(Sample{CPUPct: p, RAMUsedGiB: used, RAMTotalGiB: total, At: time.Now()})
			}
			prev = cur
		}
	}()
	return s
}
```

`internal/hostsample/hostsample_linux.go` (`//go:build linux`):

```go
package hostsample

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

func readCPU() (cpuTimes, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return cpuTimes{}, false
	}
	fields := strings.Fields(sc.Text()) // "cpu user nice system idle iowait irq softirq steal ..."
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, false
	}
	var total, idle float64
	for i, fld := range fields[1:] {
		v, err := strconv.ParseFloat(fld, 64)
		if err != nil {
			return cpuTimes{}, false
		}
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idle += v
		}
	}
	return cpuTimes{idle: idle, total: total}, true
}

func readRAM() (usedGiB, totalGiB float64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	var total, avail float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseFloat(fields[1], 64)
		switch fields[0] {
		case "MemTotal:":
			total = kb
		case "MemAvailable:":
			avail = kb
		}
	}
	if total <= 0 {
		return 0, 0, false
	}
	return (total - avail) / (1 << 20), total / (1 << 20), true
}
```

`internal/hostsample/hostsample_windows.go` (`//go:build windows`), using `kernel32` directly so no PowerShell spawn happens every tick:

```go
package hostsample

import (
	"syscall"
	"unsafe"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes    = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatx = kernel32.NewProc("GlobalMemoryStatusEx")
)

type filetime struct{ lo, hi uint32 }

func (f filetime) f64() float64 { return float64(uint64(f.hi)<<32 | uint64(f.lo)) }

func readCPU() (cpuTimes, bool) {
	var idle, kernel, user filetime
	r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return cpuTimes{}, false
	}
	// kernel time INCLUDES idle on Windows.
	return cpuTimes{idle: idle.f64(), total: kernel.f64() + user.f64()}, true
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func readRAM() (usedGiB, totalGiB float64, ok bool) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 || m.TotalPhys == 0 {
		return 0, 0, false
	}
	g := float64(1 << 30)
	return float64(m.TotalPhys-m.AvailPhys) / g, float64(m.TotalPhys) / g, true
}
```

`internal/hostsample/hostsample_other.go` (`//go:build !linux && !windows`): both readers return `false`.

- [ ] **Step 4: Wire it into health**

`internal/fleetnode/server.go` — add to `Options`:

```go
	// Host reports the last host CPU/RAM sample (hostsample.Sampler.Load).
	// nil omits the host_* fields; the handler never samples itself.
	Host func() (hostsample.Sample, bool)
```

Add to `healthPayload`:

```go
	HostCPUPct     int     `json:"host_cpu_pct,omitempty"`
	HostRAMUsedGb  float64 `json:"host_ram_used_gb,omitempty"`
	HostRAMTotalGb float64 `json:"host_ram_total_gb,omitempty"`
```

In `handleHealth`, after the util loop:

```go
	if s.opts.Host != nil {
		if h, ok := s.opts.Host(); ok {
			payload.HostCPUPct = h.CPUPct
			payload.HostRAMUsedGb = h.RAMUsedGiB
			payload.HostRAMTotalGb = h.RAMTotalGiB
		}
	}
```

In `main.go` fleet-serve, beside `reclaim := startReclaimTracking(...)`:

```go
	host := hostsample.Start(ctx, 5*time.Second)
```

and pass `Host: host.Load,` into `fleetnode.Options{...}`.

- [ ] **Step 5: Run tests, then verify live**

Run: `go test ./internal/hostsample ./internal/fleetnode`
Expected: PASS.

Run (from the worktree, a scratch port so the live node is untouched):
`go run . fleet-serve --listen 127.0.0.1:18899 --node-id scratch` in the background, then
`curl -s http://127.0.0.1:18899/fleet/health | python -c "import sys,json; d=json.load(sys.stdin); print({k:d.get(k) for k in ('gpu_util_pct','gpu_util_known','host_cpu_pct','host_ram_used_gb','host_ram_total_gb')})"`
Expected: after ~10 s all five keys present and plausible (RAM total ≈ 128 on Qube). Kill the scratch server.

- [ ] **Step 6: Commit**

```bash
git add internal/hostsample main.go internal/fleetnode/server.go
git commit -m "fleet-node: host_cpu_pct + host_ram_* from a background hostsample sampler"
```

---

### Task 3: Served-model advertisement

**Files:**
- Modify: `internal/fleetnode/server.go:125-260` (`agentResidency`, `refreshAgentResidency`, `handleHealth`)
- Test: `internal/fleetnode/server_test.go` (extend the existing residency tests that inject `rosterServes`)

**Interfaces:**
- Consumes: `swapclient.FetchRoster(ctx, endpoint, timeout)` → `Roster.IDs()`.
- Produces: health `served_models []string` (`json:"served_models,omitempty"`), refreshed on the same TTL and single-flight latch as `agent_seat_resident`. Empty/absent = UNKNOWN.

- [ ] **Step 1: Write the failing test**

Append to `internal/fleetnode/server_test.go`:

```go
func TestHealth_ServedModelsRideResidencyRefresh(t *testing.T) {
	s, _ := newTestServer(t, agentCfg(), &fakeRunner{}, nil)
	s.rosterIDs = func(ctx context.Context, endpoint string) ([]string, error) {
		return []string{"qwen3.5-9b-agent", "gemma-4-e4b"}, nil
	}
	s.rosterServes = func(ctx context.Context, endpoint, seat string) (bool, error) { return true, nil }
	// first call schedules the refresh; poll until published
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest("GET", "/fleet/health", nil))
		var got struct {
			Served []string `json:"served_models"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		if len(got.Served) == 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("served_models never published")
}
```

(`agentCfg()` = a `config.Config` with `FleetAgentEnabled: true` and `AgentCtxTokens: 8192`; reuse whatever helper the residency tests already use — grep `agent_seat_resident` in `server_test.go` and mirror its config.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/fleetnode -run ServedModels -v`
Expected: FAIL — `s.rosterIDs undefined`.

- [ ] **Step 3: Implement**

In `server.go`: add `rosterIDs func(ctx context.Context, endpoint string) ([]string, error)` to `Server` (default in `New`: a closure over `swapclient.FetchRoster(ctx, endpoint, 5*time.Second)` returning `r.IDs()`), add `served []string` to `agentResidency`, and in `refreshAgentResidency` after the residency probe:

```go
	ids, ierr := s.rosterIDs(ctx, endpoint)
	if ierr != nil {
		ids = nil // unknown on failure — never keep a stale list
	}
	a.mu.Lock()
	a.served = ids
	// ... existing resident/at/inflight writes ...
	a.mu.Unlock()
```

Add `func (s *Server) servedModels() []string` returning a copy under `a.mu` (it does not trigger a refresh; `agentResident()` already does, and health calls both). In `handleHealth` inside `if s.agentLane {`:

```go
		payload.ServedModels = s.servedModels()
```

with `ServedModels []string \`json:"served_models,omitempty"\`` on `healthPayload`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/fleetnode`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/fleetnode/server.go internal/fleetnode/server_test.go
git commit -m "fleet-node: advertise served_models from the roster probe (same TTL as residency)"
```

---

### Task 4: Job metadata and `GET /fleet/jobs`

**Files:**
- Modify: `internal/fleetnode/jobs.go` (`job`, `AcceptSpec`, `JobView`, `Admit`, `finish`, new `Recent`)
- Modify: `internal/fleetnode/server.go:306-314` (routes), `handleDispatch` (pass metadata), new `handleJobs`
- Test: `internal/fleetnode/jobs_test.go`, `internal/fleetnode/server_test.go`

**Interfaces:**
- Produces: `AcceptSpec.Task string`, `AcceptSpec.Model string`; `JobView` gains `Task, Model string; AcceptedAt, StartedAt, FinishedAt time.Time`; `func (j *Jobs) Recent(n int) []JobView` (newest first, terminal and live, **Data always nil**); route `GET /fleet/jobs?limit=50` returning `{"jobs":[{"id","task","model","state","agent","accepted_at","started_at","finished_at","wall_ms","error"}]}`. `error` is truncated to 200 runes. No auth (metadata only; the payload is never listed).

- [ ] **Step 1: Write the failing store test**

Append to `internal/fleetnode/jobs_test.go`:

```go
func TestRecentNewestFirstWithMetadataAndNoData(t *testing.T) {
	j := NewJobs(time.Hour, 4)
	defer j.DrainAndStop(time.Second)
	run := func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{"secret":1}`), nil }
	j.Admit("a", AcceptSpec{Task: "agent-run", Model: "qwen3.5-9b-agent", Agent: true}, run)
	time.Sleep(10 * time.Millisecond)
	j.Admit("b", AcceptSpec{Task: "image-gen", Model: "sdxl"}, run)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v, ok := j.Get("b"); ok && v.State == JobDone {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := j.Recent(10)
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "a" {
		t.Fatalf("order: %+v", got)
	}
	if got[0].Task != "image-gen" || got[0].Model != "sdxl" || got[0].Data != nil {
		t.Fatalf("metadata/data: %+v", got[0])
	}
	if got[0].FinishedAt.IsZero() || got[0].AcceptedAt.IsZero() {
		t.Fatal("timestamps must be set")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/fleetnode -run TestRecent -v`
Expected: FAIL — unknown fields `Task`/`Model`, `Recent` undefined.

- [ ] **Step 3: Implement**

`jobs.go`: add to `job`: `task, model string; acceptedAt, startedAt, finishedAt time.Time`; to `AcceptSpec`: `Task, Model string`; to `JobView`: `Task, Model string; AcceptedAt, StartedAt, FinishedAt time.Time`. In `Admit` set `task/model/acceptedAt: j.now()`; where `claimLocked` flips state to running set `startedAt`; in `finish` set `finishedAt`. Then:

```go
// Recent returns up to n jobs, newest by acceptedAt first, WITHOUT payloads:
// this is the cluster jobs feed, and a listing must never leak an agent
// job's result past the per-job bearer gate on /fleet/jobs/{id}.
func (j *Jobs) Recent(n int) []JobView {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := make([]JobView, 0, len(j.m))
	for id, jb := range j.m {
		out = append(out, JobView{ID: id, State: jb.state, Error: jb.err, Agent: jb.agent,
			Task: jb.task, Model: jb.model, AcceptedAt: jb.acceptedAt, StartedAt: jb.startedAt, FinishedAt: jb.finishedAt})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].AcceptedAt.After(out[b].AcceptedAt) })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}
```

`server.go`: in `handleDispatch`, where `AcceptSpec` is built, set `Task: env.TaskType` and `Model:` = `s.agentSeat` when `env.TaskType == string(core.TaskAgentRun)`, else `env.ModelFamily`. Register `mux.HandleFunc("GET /fleet/jobs", s.handleJobs)` and:

```go
type jobFeedRow struct {
	ID         string `json:"id"`
	Task       string `json:"task"`
	Model      string `json:"model,omitempty"`
	State      string `json:"state"`
	Agent      bool   `json:"agent,omitempty"`
	AcceptedAt int64  `json:"accepted_at"`
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	WallMs     int64  `json:"wall_ms,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	rows := make([]jobFeedRow, 0, limit)
	for _, v := range s.jobs.Recent(limit) {
		row := jobFeedRow{ID: v.ID, Task: v.Task, Model: v.Model, State: string(v.State), Agent: v.Agent,
			AcceptedAt: v.AcceptedAt.Unix(), Error: truncateRunes(v.Error, 200)}
		if !v.StartedAt.IsZero() {
			row.StartedAt = v.StartedAt.Unix()
		}
		if !v.FinishedAt.IsZero() {
			row.FinishedAt = v.FinishedAt.Unix()
			if !v.StartedAt.IsZero() {
				row.WallMs = v.FinishedAt.Sub(v.StartedAt).Milliseconds()
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": rows})
}
```

(`truncateRunes`: cut on a rune boundary — copy the loop from `intentLedger.dispatched`.)

- [ ] **Step 4: Route test and run**

Append to `server_test.go`:

```go
func TestJobsFeed_ListsMetadataOnly(t *testing.T) {
	s, jobs := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	jobs.Admit("j1", AcceptSpec{Task: "image-gen", Model: "sdxl"}, func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"png":"..."}`), nil
	})
	time.Sleep(50 * time.Millisecond)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest("GET", "/fleet/jobs?limit=5", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"task":"image-gen"`) || strings.Contains(rr.Body.String(), "png") {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}
```

Run: `go test ./internal/fleetnode`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/fleetnode/jobs.go internal/fleetnode/jobs_test.go internal/fleetnode/server.go internal/fleetnode/server_test.go
git commit -m "fleet-node: job metadata + GET /fleet/jobs feed (payload-free)"
```

---

### Task 5: `internal/fleetview` — poller and overview model

**Files:**
- Create: `internal/fleetview/overview.go`, `internal/fleetview/poller.go`, `internal/fleetview/errors.go`, `internal/fleetview/poller_test.go`

**Interfaces:**
- Consumes: `delegate.FetchNodeView` is NOT reused (it discards the graph fields); this package decodes health itself with a loose struct. Consumes `GET /fleet/jobs`, and the delegation-log corpus at `cfg.BaseDir()/delegation-log/YYYY-MM-DD.jsonl` (rows are `delegationLogLine` in `internal/delegate/run.go:2360`; decode loosely: `ts, job_id, node, seat, placement_reason, deferred, defer_class, acceptance_pass, wall_ms, error`, plus `contract.goal`).
- Produces:

```go
type Point struct { At int64 `json:"at"`; GpuUtil int `json:"gpu_util"`; CPU int `json:"cpu"`; VramFree float64 `json:"vram_free_gb"`; RamUsed float64 `json:"ram_used_gb"` }
type Node struct {
	Base string `json:"base"`; NodeID string `json:"node_id"`; Reachable bool `json:"reachable"`; ProbeError string `json:"probe_error,omitempty"`
	Version string `json:"harness_version,omitempty"`; GpuVendor, GpuArch string
	VramTotal, VramFree float64; Devices []map[string]any `json:"gpu_devices,omitempty"`
	GpuUtil int `json:"gpu_util_pct"`; GpuUtilKnown bool `json:"gpu_util_known"`
	HostCPU int `json:"host_cpu_pct"`; RamUsed, RamTotal float64
	AgentEnabled, AgentResident bool; AgentSeat string; AgentCtx int; ServedModels []string
	QueueDepth, JobsRunning, JobsQueued, MaxConcurrent, MaxQueue int
	History []Point `json:"history"`; Jobs []map[string]any `json:"jobs"`
	LastSeen int64 `json:"last_seen"`
}
type Error struct { At int64 `json:"at"`; Severity string `json:"severity"`; Node string `json:"node"`; Source string `json:"source"`; Message string `json:"message"` }
type Overview struct { At int64; DelegationEnabled bool; Nodes []Node; Errors []Error; Delegations []map[string]any }
type Poller struct{...}
func NewPoller(cfg config.Config, bases []string, interval time.Duration, history int) *Poller
func (p *Poller) Run(ctx context.Context)
func (p *Poller) Snapshot() Overview
```

(Use proper `json` tags on every exported field — the tags above are abbreviated where obvious: `vram_total_gb`, `vram_free_gb`, `host_ram_used_gb`, `host_ram_total_gb`, `agent_enabled`, `agent_seat_resident`, `agent_seat`, `agent_ctx_tokens`, `served_models`, `queue_depth`, `jobs_running`, `jobs_queued`, `max_concurrent_jobs`, `max_queue_depth`, `gpu_vendor`, `gpu_arch`, `delegation_enabled`, `at`, `nodes`, `errors`, `delegations`.)

- [ ] **Step 1: Write the failing poller test**

`internal/fleetview/poller_test.go`:

```go
package fleetview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

func fakeNode(t *testing.T, util int) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "n1", "vram_total_gb": 16.0, "vram_free_gb": 9.5,
			"gpu_util_pct": util, "gpu_util_known": true, "host_cpu_pct": 12,
			"host_ram_used_gb": 20.0, "host_ram_total_gb": 64.0,
			"agent_enabled": true, "agent_seat": "qwen3.5-9b-agent", "agent_seat_resident": true,
			"served_models": []string{"qwen3.5-9b-agent"}, "queue_depth": 1, "jobs_running": 1,
		})
	})
	mux.HandleFunc("GET /fleet/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{
			{"id": "agd-1", "task": "agent-run", "state": "error", "error": "boom", "accepted_at": time.Now().Unix()},
		}})
	})
	return httptest.NewServer(mux)
}

func TestPollerFoldsHealthJobsAndErrors(t *testing.T) {
	srv := fakeNode(t, 42)
	defer srv.Close()
	p := NewPoller(config.Config{AgentDelegationEnabled: true}, []string{srv.URL}, 30*time.Millisecond, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go p.Run(ctx)
	for {
		ov := p.Snapshot()
		if len(ov.Nodes) == 1 && ov.Nodes[0].Reachable && len(ov.Nodes[0].History) >= 2 {
			n := ov.Nodes[0]
			if n.GpuUtil != 42 || n.HostCPU != 12 || n.ServedModels[0] != "qwen3.5-9b-agent" || len(n.Jobs) != 1 {
				t.Fatalf("node: %+v", n)
			}
			if len(ov.Errors) == 0 || ov.Errors[0].Source != "job" || ov.Errors[0].Node != "n1" {
				t.Fatalf("errors: %+v", ov.Errors)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("never folded: %+v", p.Snapshot())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestPollerUnreachableIsAnError(t *testing.T) {
	p := NewPoller(config.Config{}, []string{"http://127.0.0.1:1"}, 20*time.Millisecond, 3)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go p.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	ov := p.Snapshot()
	if ov.Nodes[0].Reachable || len(ov.Errors) == 0 || ov.Errors[0].Source != "probe" {
		t.Fatalf("%+v", ov)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	srv := fakeNode(t, 1)
	defer srv.Close()
	p := NewPoller(config.Config{}, []string{srv.URL}, 5*time.Millisecond, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go p.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	if h := len(p.Snapshot().Nodes[0].History); h != 3 {
		t.Fatalf("history len %d want 3", h)
	}
}
```

Note: `netguard.SafeTransport` refuses non-loopback, non-tailnet hosts; `httptest` servers are `127.0.0.1`, so they pass.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/fleetview -v`
Expected: FAIL — package missing.

- [ ] **Step 3: Implement**

`overview.go` holds the types above. `poller.go`:

```go
package fleetview

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

const (
	probeTimeout = 5 * time.Second
	maxBody      = 1 << 20
	jobsPerNode  = 50
	maxErrors    = 200
)

type Poller struct {
	cfg      config.Config
	bases    []string
	interval time.Duration
	history  int
	client   *http.Client
	mu       sync.RWMutex
	nodes    map[string]*Node // by base
	errors   []Error
	seenErr  map[string]bool // "node|job-id" so a job error is reported once
}

func NewPoller(cfg config.Config, bases []string, interval time.Duration, history int) *Poller {
	p := &Poller{cfg: cfg, bases: bases, interval: interval, history: history,
		client: &http.Client{Transport: netguard.SafeTransport(nil), Timeout: probeTimeout},
		nodes: map[string]*Node{}, seenErr: map[string]bool{}}
	for _, b := range bases {
		p.nodes[b] = &Node{Base: b}
	}
	return p
}

func (p *Poller) Run(ctx context.Context) {
	p.tick(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

// tick probes every base in parallel (the mcpserver status probe's pattern:
// per-node budgets, never one shared deadline).
func (p *Poller) tick(ctx context.Context) {
	var wg sync.WaitGroup
	for _, base := range p.bases {
		wg.Add(1)
		go func(base string) {
			defer wg.Done()
			h, herr := p.getJSON(ctx, base+"/fleet/health")
			j, _ := p.getJSON(ctx, fmt.Sprintf("%s/fleet/jobs?limit=%d", base, jobsPerNode)) // a pre-0.113.0 node 404s: jobs stay empty
			p.fold(base, h, herr, j)
		}(strings.TrimRight(base, "/"))
	}
	wg.Wait()
	p.foldDelegationLog()
}

func (p *Poller) getJSON(ctx context.Context, u string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if p.cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.FleetAuthToken)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %.200s", resp.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	return m, nil
}
```

`fold` (same file) locks `p.mu`, and for a probe error sets `Reachable=false, ProbeError=err.Error()` and appends `Error{Severity:"error", Node: base-or-node_id, Source:"probe", Message: err}` (deduped per base per consecutive failure — record once, clear the dedupe key when the node comes back). On success it copies every health field with helper `num(m, key) float64` / `str` / `boolv` / `strs`, appends `Point{At: now, GpuUtil, CPU, VramFree, RamUsed}` and trims `History` to `p.history`, sets `Jobs` from `j["jobs"]`, and for every job whose `state == "error"` not yet in `seenErr` appends `Error{Severity:"error", Source:"job", Node: node_id, Message: task + ": " + error}`. `errors` is trimmed to `maxErrors`, newest last.

`errors.go` — `foldDelegationLog` reads today's and yesterday's `filepath.Join(p.cfg.BaseDir(), "delegation-log", day+".jsonl")`, keeps the last 100 rows as `Delegations` (`ts, job_id, node, seat, placement_reason, deferred, defer_class, acceptance_pass, wall_ms, error, goal` — goal truncated to 120 runes), and appends an `Error{Severity:"warn", Source:"delegation", Node: row.node, Message: defer_class + ": " + error-or-placement_reason}` for each `deferred` or `!acceptance_pass` row not already in `seenErr` (key `"deleg|"+job_id`). A missing file is not an error.

`Snapshot()` returns a deep copy under `RLock` with `Nodes` in `bases` order, `At: time.Now().Unix()`, `DelegationEnabled: p.cfg.AgentDelegationEnabled`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/fleetview -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add internal/fleetview
git commit -m "fleetview: poller folding node health, jobs feed and delegation-log into one Overview"
```

---

### Task 6: `/api/overview`, the page, and the `fleet-ui` verb

**Files:**
- Create: `internal/fleetview/server.go`, `internal/fleetview/ui.html`, `internal/fleetview/server_test.go`, `fleet_ui_cmd.go`
- Modify: `main.go` verb switch (`:125` area) and help text (`:259` area)

**Interfaces:**
- Produces: `func NewHandler(p *Poller) http.Handler` serving `GET /` (the embedded page), `GET /api/overview` (`Poller.Snapshot()` JSON, `Cache-Control: no-store`), `GET /healthz` (`{"ok":true}`); verb `local-offload fleet-ui [--listen 127.0.0.1:18813] [--listen-trusted-network] [--interval 5s] [--history 120] [--remote URL]...` (remotes default to `cfg.DelegateRemotes` plus `http://<cfg.FleetListen>` when `fleet_listen` is non-loopback, so the delegator's own node is a card too).

- [ ] **Step 1: Write the failing handler test**

`internal/fleetview/server_test.go`:

```go
package fleetview

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

func TestHandlerServesPageAndOverview(t *testing.T) {
	p := NewPoller(config.Config{}, nil, time.Second, 5)
	h := NewHandler(p)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	body := rr.Body.String()
	for _, id := range []string{`id="cluster"`, `id="cards"`, `id="jobs"`, `id="errors"`, `fetch('/api/overview')`} {
		if !strings.Contains(body, id) {
			t.Fatalf("page missing %s", id)
		}
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Fatal("page must not reference external resources")
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/overview", nil))
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rr.Body.String(), `"nodes"`) {
		t.Fatalf("overview: %d %s", rr.Code, rr.Body.String())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/fleetview -run Handler -v`
Expected: FAIL — `NewHandler` undefined.

- [ ] **Step 3: Implement the handler**

`internal/fleetview/server.go`:

```go
package fleetview

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

//go:embed ui.html
var uiHTML []byte

func NewHandler(p *Poller) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(uiHTML)
	})
	mux.HandleFunc("GET /api/overview", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(p.Snapshot())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return mux
}
```

- [ ] **Step 4: Write the page**

`internal/fleetview/ui.html` — one file, no external references. Structure (write it fully; the ids are what the test and the `top` view key on):

```html
<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Fleet overview</title>
<style>
:root{--bg:#0f1115;--card:#171a21;--fg:#e6e6e6;--mut:#8a919e;--ok:#5fd38d;--warn:#f0c052;--err:#ff6b6b;--gpu:#7aa2ff;--vram:#4ecdc4;--cpu:#c792ea;--ram:#f78c6c}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.4 system-ui,sans-serif}
header{display:flex;gap:16px;align-items:center;padding:12px 20px;border-bottom:1px solid #262a33}
header h1{font-size:16px;margin:0}
#cluster{color:var(--mut)}
main{display:grid;grid-template-columns:1fr;gap:16px;padding:16px 20px}
#cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(420px,1fr));gap:16px}
.card{background:var(--card);border:1px solid #262a33;border-radius:10px;padding:14px}
.card h2{font-size:15px;margin:0 0 6px;display:flex;gap:8px;align-items:center}
.badge{font-size:11px;padding:2px 6px;border-radius:6px;background:#262a33;color:var(--mut)}
.badge.ok{background:#173d2a;color:var(--ok)}.badge.err{background:#4a1f1f;color:var(--err)}
.kv{display:grid;grid-template-columns:auto 1fr;gap:2px 10px;color:var(--mut);font-size:12px;margin:6px 0}
.kv b{color:var(--fg);font-weight:500}
svg.spark{width:100%;height:110px;background:#0b0d11;border-radius:6px}
.legend{display:flex;gap:12px;font-size:11px;color:var(--mut);margin-top:4px}
.legend i{display:inline-block;width:10px;height:10px;border-radius:2px;margin-right:4px;vertical-align:middle}
.bar{height:8px;background:#262a33;border-radius:4px;overflow:hidden;margin:2px 0}.bar i{display:block;height:100%;background:var(--vram)}
table{width:100%;border-collapse:collapse;font-size:12px}th,td{text-align:left;padding:6px 8px;border-bottom:1px solid #262a33}th{color:var(--mut);font-weight:500}
.sev-error{color:var(--err)}.sev-warn{color:var(--warn)}
.models{font-size:11px;color:var(--mut);word-break:break-all}
</style></head><body>
<header><h1>Fleet overview</h1><span id="cluster">connecting…</span></header>
<main>
<section id="cards"></section>
<section class="card"><h2>Jobs</h2><table><thead><tr><th>when</th><th>node</th><th>task</th><th>model</th><th>state</th><th>wall</th><th>error / reason</th></tr></thead><tbody id="jobs"></tbody></table></section>
<section class="card"><h2>Errors</h2><table><thead><tr><th>age</th><th>severity</th><th>node</th><th>source</th><th>message</th></tr></thead><tbody id="errors"></tbody></table></section>
</main>
<script>
const $=s=>document.querySelector(s);
const ago=t=>{const s=Math.max(0,Math.floor(Date.now()/1000-t));return s<60?s+'s':s<3600?Math.floor(s/60)+'m':Math.floor(s/3600)+'h'};
const esc=x=>String(x??'').replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
function spark(hist,n){
  const W=400,H=110,pad=4;if(!hist||!hist.length)return `<svg class="spark" viewBox="0 0 ${W} ${H}"></svg>`;
  const x=i=>pad+(W-2*pad)*i/Math.max(1,hist.length-1);
  const line=(key,max,color)=>{const pts=hist.map((p,i)=>`${x(i).toFixed(1)},${(H-pad-(H-2*pad)*Math.min(1,(p[key]||0)/max)).toFixed(1)}`).join(' ');return `<polyline fill="none" stroke="${color}" stroke-width="1.5" points="${pts}"/>`};
  return `<svg class="spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">${line('gpu_util',100,'var(--gpu)')}${line('cpu',100,'var(--cpu)')}${line('vram_free_gb',n.vram_total_gb||1,'var(--vram)')}${line('ram_used_gb',n.host_ram_total_gb||1,'var(--ram)')}</svg>`;
}
function card(n){
  const st=n.reachable?'<span class="badge ok">online</span>':'<span class="badge err">unreachable</span>';
  const seat=n.agent_enabled?`<span class="badge ${n.agent_seat_resident?'ok':''}">${esc(n.agent_seat)} ${n.agent_seat_resident?'resident':'cold'}</span>`:'<span class="badge">no agent lane</span>';
  const devs=(n.gpu_devices||[]).map(d=>`<div class="kv"><span>${esc(d.name)}</span><b>${(d.vram_total_gb-d.vram_free_gb).toFixed(1)} / ${d.vram_total_gb.toFixed(1)} GB${d.util_known?` · ${d.util_pct}%`:''}</b></div><div class="bar"><i style="width:${(100*(1-d.vram_free_gb/d.vram_total_gb)).toFixed(0)}%"></i></div>`).join('');
  return `<div class="card"><h2>${esc(n.node_id||n.base)} ${st} ${seat} <span class="badge">${esc(n.harness_version||'')}</span></h2>
  ${n.reachable?'':`<div class="sev-error">${esc(n.probe_error)}</div>`}
  ${spark(n.history,n)}<div class="legend"><span><i style="background:var(--gpu)"></i>GPU ${n.gpu_util_known?n.gpu_util_pct+'%':'n/a'}</span><span><i style="background:var(--cpu)"></i>CPU ${n.host_cpu_pct}%</span><span><i style="background:var(--vram)"></i>VRAM free ${(n.vram_free_gb||0).toFixed(1)} GB</span><span><i style="background:var(--ram)"></i>RAM ${(n.host_ram_used_gb||0).toFixed(0)} / ${(n.host_ram_total_gb||0).toFixed(0)} GB</span></div>
  ${devs}
  <div class="kv"><span>queue</span><b>${n.jobs_running} running · ${n.jobs_queued} queued · max ${n.max_concurrent_jobs||'?'} / ${n.max_queue_depth||'?'}</b><span>ctx</span><b>${n.agent_ctx_tokens||'?'} tok</b><span>base</span><b>${esc(n.base)}</b></div>
  <div class="models">${(n.served_models||[]).map(esc).join(' · ')||'served models unknown'}</div></div>`;
}
function render(o){
  const up=o.nodes.filter(n=>n.reachable).length;
  $('#cluster').textContent=`${up}/${o.nodes.length} nodes reachable · delegation ${o.delegation_enabled?'on':'off'} · ${new Date(o.at*1000).toLocaleTimeString()}`;
  $('#cards').innerHTML=o.nodes.map(card).join('');
  const rows=[];
  for(const n of o.nodes)for(const j of (n.jobs||[]))rows.push({t:j.accepted_at,node:n.node_id||n.base,task:j.task,model:j.model,state:j.state,wall:j.wall_ms,msg:j.error});
  for(const d of (o.delegations||[]))rows.push({t:d.ts,node:d.node,task:'delegate',model:d.seat,state:d.deferred?'deferred':(d.acceptance_pass?'passed':'failed'),wall:d.wall_ms,msg:d.error||d.defer_class||d.placement_reason});
  rows.sort((a,b)=>b.t-a.t);
  $('#jobs').innerHTML=rows.slice(0,100).map(r=>`<tr><td>${ago(r.t)}</td><td>${esc(r.node)}</td><td>${esc(r.task)}</td><td>${esc(r.model)}</td><td>${esc(r.state)}</td><td>${r.wall?(r.wall/1000).toFixed(1)+'s':''}</td><td>${esc(r.msg)}</td></tr>`).join('');
  $('#errors').innerHTML=(o.errors||[]).slice().reverse().map(e=>`<tr><td>${ago(e.at)}</td><td class="sev-${esc(e.severity)}">${esc(e.severity)}</td><td>${esc(e.node)}</td><td>${esc(e.source)}</td><td>${esc(e.message)}</td></tr>`).join('');
}
async function tick(){try{const r=await fetch('/api/overview');render(await r.json())}catch(e){$('#cluster').textContent='overview unavailable: '+e}}
tick();setInterval(tick,5000);
</script></body></html>
```

- [ ] **Step 5: Add the verb**

`fleet_ui_cmd.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetview"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// fleetUIRemotes is the roster the overview polls: the configured delegate
// remotes plus this box's own fleet-serve when it is bound beyond loopback
// (a delegator that is also a node deserves a card).
func fleetUIRemotes(cfg config.Config, explicit []string) []string {
	if len(explicit) > 0 {
		return explicit
	}
	out := append([]string(nil), cfg.DelegateRemotes...)
	if cfg.FleetListen != "" && !netguard.LoopbackAddr(cfg.FleetListen) {
		out = append(out, "http://"+cfg.FleetListen)
	}
	return out
}

func runFleetUI(args []string) error {
	fs := flag.NewFlagSet("fleet-ui", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:18813", "listen address (loopback unless --listen-trusted-network)")
	trusted := fs.Bool("listen-trusted-network", false, "allow --listen beyond loopback (the Tailscale address). The page is read-only but unauthenticated — tailnet only, NEVER 0.0.0.0.")
	interval := fs.Duration("interval", 5*time.Second, "poll interval")
	history := fs.Int("history", 120, "sparkline points kept per node")
	var remotes multiFlag
	fs.Var(&remotes, "remote", "node base URL (repeatable; default: delegate_remotes + this box's fleet_listen)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !*trusted && !netguard.LoopbackAddr(*listen) {
		return fmt.Errorf("fleet-ui: %s is not loopback; pass --listen-trusted-network to bind a tailnet address (never 0.0.0.0)", *listen)
	}
	if strings.HasPrefix(*listen, "0.0.0.0") || strings.HasPrefix(*listen, "[::]") || strings.HasPrefix(*listen, ":") {
		return fmt.Errorf("fleet-ui: refusing to bind all interfaces (%s)", *listen)
	}
	bases := fleetUIRemotes(cfg, remotes)
	if len(bases) == 0 {
		return fmt.Errorf("fleet-ui: no nodes to poll — set delegate_remotes in the config or pass --remote")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	p := fleetview.NewPoller(cfg, bases, *interval, *history)
	go p.Run(ctx)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("fleet-ui: listen %s: %w", *listen, err)
	}
	fmt.Fprintf(os.Stderr, "[fleet-ui] http://%s — polling %d node(s) every %s\n", *listen, len(bases), *interval)
	srv := &http.Server{Handler: fleetview.NewHandler(p), ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
```

(Check how `config.Load()` is actually spelled in this repo: `grep -n 'config.Load' main.go` and mirror the call the other verbs make — several take a path flag.) Add `case "fleet-ui": err = runFleetUI(args)` beside `fleet-measure` in `main.go` and this help line after the `fleet-measure` one:

```
  local-offload fleet-ui [--listen 127.0.0.1:18813] [--listen-trusted-network] [--interval 5s] [--remote URL]...   live overview page: node cards (GPU/VRAM/CPU/RAM graphs, seat, served models), cluster jobs + errors feed (docs/systems/fleet-overview.md)
```

- [ ] **Step 6: Run tests, then render it once for real**

Run: `go test ./internal/fleetview ./ && go build -o offload-harness.exe .`
Expected: PASS, build ok.

Run: `./offload-harness.exe fleet-ui` (defaults: 127.0.0.1:18813, remotes from config), then in another shell `curl -s http://127.0.0.1:18813/api/overview | python -c "import sys,json; o=json.load(sys.stdin); print([(n['node_id'],n['reachable'],len(n['history']),len(n['jobs'])) for n in o['nodes']], len(o['errors']))"`
Expected: one tuple per configured node, `reachable=True` for lenovo-ampere6 and aorus-ampere8, history growing on a second call. Open `http://127.0.0.1:18813/` in a browser and confirm cards render with moving sparklines and no console errors (the constitution's "inspect your own output" rule — the page is the deliverable). Stop the server with Ctrl-C.

- [ ] **Step 7: Commit**

```bash
git add internal/fleetview fleet_ui_cmd.go main.go
git commit -m "fleet-ui: read-only overview page + /api/overview on the delegator (loopback by default)"
```

---

### Task 7: Placement — served-model gate and utilization tie-breaker

**Files:**
- Modify: `internal/delegate/nodeview.go:23-140` (`NodeView`, `healthWire`, `FetchNodeView`)
- Modify: `internal/delegate/gate.go:98-200` (`betterRemote`, `remoteEligible`)
- Modify: `internal/mcpserver/mcpserver.go:660-672` (publish the two new facts in `offload_status.fleet.nodes[]`)
- Test: `internal/delegate/gate_test.go`

**Interfaces:**
- Consumes: health `served_models`, `gpu_util_pct`, `gpu_util_known` (Tasks 1 and 3).
- Produces: `NodeView.ServedModels []string`, `NodeView.GpuUtilPct int`, `NodeView.GpuUtilKnown bool`; `func seatServed(v NodeView) bool`; fourth `betterRemote` key.

- [ ] **Step 1: Write the failing gate tests**

Append to `internal/delegate/gate_test.go`:

```go
func TestRemoteEligible_ServedModelsWithoutSeatIsIneligible(t *testing.T) {
	r := eligibleRemote()
	r.ServedModels = []string{"gemma-4-e4b"} // published, seat absent
	if remoteEligible(schemaSubtask(), r) {
		t.Fatal("a node whose served_models omits its agent seat must not be placed on")
	}
	r.ServedModels = []string{"gemma-4-e4b", r.AgentSeat}
	if !remoteEligible(schemaSubtask(), r) {
		t.Fatal("seat present in served_models must be eligible")
	}
	r.ServedModels = nil // pre-0.113.0 node: unknown, not a refusal
	if !remoteEligible(schemaSubtask(), r) {
		t.Fatal("absent served_models is UNKNOWN and must not gate")
	}
}

func TestBetterRemote_UtilizationBreaksQueueTies(t *testing.T) {
	a, b := eligibleRemote(), eligibleRemote()
	a.NodeID, b.NodeID = "a", "b"
	a.GpuUtilPct, a.GpuUtilKnown = 80, true
	b.GpuUtilPct, b.GpuUtilKnown = 10, true
	if !betterRemote(b, a) || betterRemote(a, b) {
		t.Fatal("lower known utilization must win an otherwise equal pair")
	}
	// queue depth still outranks utilization
	b.QueueDepth = a.QueueDepth + 1
	if betterRemote(b, a) {
		t.Fatal("utilization must never override QueueDepth")
	}
}

func TestBetterRemote_UnknownUtilizationNeverLoses(t *testing.T) {
	known, unknown := eligibleRemote(), eligibleRemote()
	known.GpuUtilPct, known.GpuUtilKnown = 5, true
	if betterRemote(known, unknown) || betterRemote(unknown, known) {
		t.Fatal("an unknown utilization is neither credited nor blamed — roster order keeps the tie")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/delegate -run 'ServedModels|Utilization' -v`
Expected: FAIL — fields undefined.

- [ ] **Step 3: Implement**

`nodeview.go`: add to `NodeView`:

```go
	// ServedModels is the node's advertised roster (health served_models).
	// nil/empty = UNKNOWN (a pre-0.113.0 node): never a refusal. When
	// published, the agent seat must be in it — a stronger check than the
	// cached residency flag, and the capability gate PAIR calls "the exact
	// requested model is present".
	ServedModels []string
	// GpuUtilPct is the busiest device's utilization; GpuUtilKnown is false
	// when the node did not publish it. Read by betterRemote as the LAST key —
	// a tie-breaker, never a primary signal (operator decision 2026-09-03).
	GpuUtilPct   int
	GpuUtilKnown bool
```

to `healthWire`: `ServedModels []string \`json:"served_models"\``, `GpuUtilPct int \`json:"gpu_util_pct"\``, `GpuUtilKnown bool \`json:"gpu_util_known"\``; copy them in `FetchNodeView`.

`gate.go`: in `remoteEligible`, add a condition beside `AgentResident`:

```go
	if !seatServed(r) {
		return false
	}
```

```go
// seatServed: true when the node publishes no roster (unknown) or when the
// roster names the agent seat (case-insensitive, like swapclient.Roster.Serves).
func seatServed(v NodeView) bool {
	if len(v.ServedModels) == 0 {
		return true
	}
	for _, m := range v.ServedModels {
		if strings.EqualFold(m, v.AgentSeat) {
			return true
		}
	}
	return false
}
```

In `betterRemote`, replace the final line with:

```go
	if candidate.QueueDepth != incumbent.QueueDepth {
		return candidate.QueueDepth < incumbent.QueueDepth
	}
	//  4. Lower GPU utilization — ONLY when both publish it. An unknown is
	//     neither credited nor blamed (the AgentCtxTokens == 0 rule), so a
	//     pre-0.113.0 node keeps its roster-order tie.
	if candidate.GpuUtilKnown && incumbent.GpuUtilKnown {
		return candidate.GpuUtilPct < incumbent.GpuUtilPct
	}
	return false
```

Update the doc comment above `betterRemote` ("Three ordered keys" → "Four ordered keys", adding key 4 in the same voice). `mcpserver.go`: add `n["served_models"] = r.view.ServedModels` and, when `r.view.GpuUtilKnown`, `n["gpu_util_pct"] = r.view.GpuUtilPct`.

- [ ] **Step 4: Run tests; break the guard once (clean-ship Consequential rule 3)**

Run: `go test ./internal/delegate ./internal/mcpserver`
Expected: PASS.

Temporarily change `seatServed` to `return true` on its first line, run `go test ./internal/delegate -run ServedModels` — Expected: FAIL (proves the gate is load-bearing). Restore with Edit, re-run — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/delegate/nodeview.go internal/delegate/gate.go internal/delegate/gate_test.go internal/mcpserver/mcpserver.go
git commit -m "delegate: served_models capability gate + GPU utilization as the fourth (tie-break) ranking key"
```

---

### Task 8: `fleet-smoke` verb

**Files:**
- Create: `fleet_smoke_cmd.go`, `fleet_smoke_test.go`
- Modify: `main.go` verb switch + help text

**Interfaces:**
- Consumes: `delegate.PrepareContract(delegate.SubtaskSpec{...}, "")`, `delegate.Run(ctx, cfg, p.RunAgentContract, contracts, "remote", []string{base})`, `[]delegate.PlacedResult` fields `Node, Seat, Placement, JobID, Deferred, Reason, WallMs, AcceptanceFailures` (read `internal/delegate/run.go:65` for exact names before coding), `Summary.Succeeded`.
- Produces: `func smokeContract(nodeHint string) delegate.SubtaskSpec`, `func renderSmokeTable(rows []smokeRow) string`, verb `local-offload fleet-smoke [--remote URL]... [--timeout 120] [--json]`, exit non-zero when any row is not `PASS`.

- [ ] **Step 1: Write the failing tests**

`fleet_smoke_test.go`:

```go
package main

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/delegate"
)

func TestSmokeContractIsGroundedAndCheap(t *testing.T) {
	spec := smokeContract("lenovo-ampere6")
	c, err := delegate.PrepareContract(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxSteps != 1 || c.TimeoutSec != 60 {
		t.Fatalf("smoke must be one step / 60 s, got %d/%d", c.MaxSteps, c.TimeoutSec)
	}
	if !strings.Contains(strings.Join(c.Acceptance, " "), "contains:PONG-lenovo-ampere6") {
		t.Fatalf("acceptance must anchor on a token that only the doc carries: %v", c.Acceptance)
	}
	if len(c.Context) != 1 || !strings.Contains(c.Context[0].Text, "PONG-lenovo-ampere6") {
		t.Fatal("the token must be IN the context doc, not only in the goal (parrot-passable otherwise)")
	}
	if strings.Contains(c.Goal, "PONG-lenovo-ampere6") {
		t.Fatal("goal must not carry the token — an echo of the goal would pass")
	}
}

func TestRenderSmokeTable(t *testing.T) {
	out := renderSmokeTable([]smokeRow{{Base: "http://a:18811", Node: "a", Seat: "s", Placement: "remote", WallMs: 1234, Verdict: "PASS"}, {Base: "http://b:18811", Verdict: "FAIL", Detail: "probe: dial refused"}})
	if !strings.Contains(out, "PASS") || !strings.Contains(out, "dial refused") || !strings.Contains(out, "1.2s") {
		t.Fatalf("table:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test . -run 'Smoke' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

`fleet_smoke_cmd.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

type smokeRow struct {
	Base      string `json:"base"`
	Node      string `json:"node,omitempty"`
	Seat      string `json:"seat,omitempty"`
	Placement string `json:"placement,omitempty"`
	JobID     string `json:"job_id,omitempty"`
	WallMs    int64  `json:"wall_ms,omitempty"`
	Verdict   string `json:"verdict"` // PASS | FAIL | DEFER
	Detail    string `json:"detail,omitempty"`
}

// smokeContract is the harness's "Test traffic" button: one step, 60 s, and
// an acceptance anchored on a token that lives ONLY in the context doc, so a
// seat that echoes the goal cannot pass (the delegation skill's parrot rule).
func smokeContract(nodeHint string) delegate.SubtaskSpec {
	token := "PONG-" + nodeHint
	return delegate.SubtaskSpec{AgentContract: core.AgentContract{
		Goal:         "Read the provided document and reply with the exact token it contains in the `reply` field. Nothing else.",
		Context:      []core.ContextDoc{{Name: "smoke.txt", Text: "The token is: " + token}},
		OutputSchema: json.RawMessage(`{"properties":{"reply":{"type":"string"}}}`),
		Acceptance:   []string{"contains:" + token},
		MaxSteps:     1,
		TimeoutSec:   60,
	}}
}

func renderSmokeTable(rows []smokeRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-32s %-18s %-20s %-10s %8s  %s\n", "node", "seat", "placement", "verdict", "wall", "detail")
	for _, r := range rows {
		node := r.Node
		if node == "" {
			node = r.Base
		}
		wall := ""
		if r.WallMs > 0 {
			wall = fmt.Sprintf("%.1fs", float64(r.WallMs)/1000)
		}
		fmt.Fprintf(&b, "%-32s %-18s %-20s %-10s %8s  %s\n", node, r.Seat, r.Placement, r.Verdict, wall, r.Detail)
	}
	return b.String()
}

func runFleetSmoke(args []string) error {
	fs := flag.NewFlagSet("fleet-smoke", flag.ExitOnError)
	var remotes multiFlag
	fs.Var(&remotes, "remote", "node base URL (repeatable; default delegate_remotes)")
	timeout := fs.Int("timeout", 120, "overall seconds per node")
	asJSON := fs.Bool("json", false, "machine-readable rows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, p, err := loadConfigAndPipeline() // mirror runDelegate's own config+pipeline construction (main.go:1904-1945)
	if err != nil {
		return err
	}
	bases := remotes
	if len(bases) == 0 {
		bases = cfg.DelegateRemotes
	}
	if len(bases) == 0 {
		return fmt.Errorf("fleet-smoke: no nodes — set delegate_remotes or pass --remote")
	}
	rows := make([]smokeRow, 0, len(bases))
	failed := false
	for _, base := range bases {
		hint := strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
		hint = strings.Split(hint, ":")[0]
		c, perr := delegate.PrepareContract(smokeContract(hint), "")
		if perr != nil {
			return perr
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second)
		results, _, rerr := delegate.Run(ctx, cfg, p.RunAgentContract, []core.AgentContract{c}, "remote", []string{base})
		cancel()
		row := smokeRow{Base: base}
		switch {
		case rerr != nil:
			row.Verdict, row.Detail = "FAIL", rerr.Error()
		case len(results) == 0:
			row.Verdict, row.Detail = "FAIL", "no result row"
		default:
			r := results[0]
			row.Node, row.Seat, row.Placement, row.JobID, row.WallMs = r.Node, r.Seat, r.Placement, r.JobID, r.WallMs
			switch {
			case r.Deferred:
				row.Verdict, row.Detail = "DEFER", r.Reason
			case len(r.AcceptanceFailures) > 0:
				row.Verdict, row.Detail = "FAIL", strings.Join(r.AcceptanceFailures, "; ")
			case !strings.HasPrefix(r.Placement, "route=remote"):
				row.Verdict, row.Detail = "FAIL", "did not land on the node: "+r.Placement
			default:
				row.Verdict = "PASS"
			}
		}
		if row.Verdict != "PASS" {
			failed = true
		}
		rows = append(rows, row)
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	fmt.Print(renderSmokeTable(rows))
	if failed {
		return fmt.Errorf("fleet-smoke: %d node(s) did not PASS", countNot(rows, "PASS"))
	}
	return nil
}

func countNot(rows []smokeRow, verdict string) int {
	n := 0
	for _, r := range rows {
		if r.Verdict != verdict {
			n++
		}
	}
	return n
}
```

Replace `loadConfigAndPipeline()` with whatever `runDelegate` (main.go:1904-1945) does to obtain `cfg` and `p` — copy those exact lines; do not invent a helper if none exists. Confirm the `PlacedResult` field names at `internal/delegate/run.go:65` and adjust `r.Node/Seat/Placement/JobID/WallMs/Deferred/Reason/AcceptanceFailures` to the real names. Add `case "fleet-smoke": err = runFleetSmoke(args)` and the help line:

```
  local-offload fleet-smoke [--remote URL]... [--timeout 120] [--json]   send one grounded one-step contract to EVERY node and print where each landed (node, seat, placement, wall, verdict); non-zero unless all PASS
```

- [ ] **Step 4: Run tests, then run it for real**

Run: `go test . -run Smoke && go build -o offload-harness.exe . && ./offload-harness.exe fleet-smoke`
Expected: a two-row table, both `PASS`, wall under ~30 s each (Lenovo 4B and Aorus 9B seats are resident per `offload_status`). If a row is `DEFER` with a budget reason, that is a real finding, not a test bug — report it.

- [ ] **Step 5: Commit**

```bash
git add fleet_smoke_cmd.go fleet_smoke_test.go main.go
git commit -m "fleet-smoke: one grounded contract per node, table of where each landed"
```

---

### Task 9: `top` terminal view

**Files:**
- Create: `internal/fleetview/topmodel.go`, `internal/fleetview/topmodel_test.go`, `top_cmd.go`
- Modify: `go.mod` (add `github.com/charmbracelet/bubbletea` — `go get github.com/charmbracelet/bubbletea@latest`, no pin beyond what `go.mod` records), `main.go` verb switch + help

**Interfaces:**
- Consumes: `GET <ui-base>/api/overview` (Task 6). The view is a CLIENT of a running `fleet-ui`; on a headless box it points at the delegator's tailnet URL.
- Produces: `func RenderTop(o Overview, width int) string` (pure; tested), `local-offload top [--ui http://127.0.0.1:18813] [--interval 5s]`.

- [ ] **Step 1: Write the failing render test**

`internal/fleetview/topmodel_test.go`:

```go
package fleetview

import (
	"strings"
	"testing"
)

func TestRenderTopShowsNodesJobsErrors(t *testing.T) {
	o := Overview{At: 1, DelegationEnabled: true, Nodes: []Node{
		{Base: "http://node-a:18811", NodeID: "lenovo-ampere6", Reachable: true, AgentSeat: "qwen3.5-4b-agent", AgentResident: true, GpuUtil: 7, GpuUtilKnown: true, VramFree: 4.1, VramTotal: 6, HostCPU: 3, RamUsed: 10, RamTotal: 64, JobsRunning: 0, JobsQueued: 0,
			Jobs: []map[string]any{{"id": "agd-1", "task": "agent-run", "state": "done", "wall_ms": float64(900)}}},
		{Base: "http://node-b:18811", Reachable: false, ProbeError: "dial refused"},
	}, Errors: []Error{{At: 1, Severity: "error", Node: "node-b", Source: "probe", Message: "dial refused"}}}
	out := RenderTop(o, 120)
	for _, want := range []string{"lenovo-ampere6", "7%", "4.1/6.0", "qwen3.5-4b-agent", "agd-1", "dial refused", "1/2 reachable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/fleetview -run RenderTop -v`
Expected: FAIL — `RenderTop` undefined.

- [ ] **Step 3: Implement**

`topmodel.go` — `RenderTop` builds three sections with `fmt.Fprintf` and a `strings.Builder`: a header `Fleet overview  %d/%d reachable  delegation %s`, a node table (`NODE  SEAT  RES  GPU%  VRAM free/total  CPU%  RAM  RUN/QUEUED`) with `%-20s`-style columns, then `JOBS` (last 15 across nodes, newest first: `age node task model state wall`) and `ERRORS` (last 10). It also holds the Bubble Tea model:

```go
type topModel struct {
	ui       string
	interval time.Duration
	client   *http.Client
	ov       Overview
	err      error
	width    int
}
type tickMsg time.Time
type overviewMsg struct{ ov Overview; err error }

func NewTop(ui string, interval time.Duration) tea.Model {
	return topModel{ui: strings.TrimRight(ui, "/"), interval: interval, width: 100,
		client: &http.Client{Transport: netguard.SafeTransport(nil), Timeout: 5 * time.Second}}
}
func (m topModel) Init() tea.Cmd { return tea.Batch(m.fetch(), tea.Tick(m.interval, func(t time.Time) tea.Msg { return tickMsg(t) })) }
func (m topModel) fetch() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.Get(m.ui + "/api/overview")
		if err != nil { return overviewMsg{err: err} }
		defer resp.Body.Close()
		var ov Overview
		if err := json.NewDecoder(resp.Body).Decode(&ov); err != nil { return overviewMsg{err: err} }
		return overviewMsg{ov: ov}
	}
}
func (m topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.KeyMsg:
		if v.String() == "q" || v.String() == "ctrl+c" { return m, tea.Quit }
	case tea.WindowSizeMsg:
		m.width = v.Width
	case tickMsg:
		return m, tea.Batch(m.fetch(), tea.Tick(m.interval, func(t time.Time) tea.Msg { return tickMsg(t) }))
	case overviewMsg:
		m.ov, m.err = v.ov, v.err
	}
	return m, nil
}
func (m topModel) View() string {
	if m.err != nil { return "fleet-ui unreachable at " + m.ui + ": " + m.err.Error() + "\n(q to quit)\n" }
	return RenderTop(m.ov, m.width) + "\nq quit\n"
}
```

`top_cmd.go` — `runTop(args)`: flags `--ui` (default `http://127.0.0.1:18813`) and `--interval` (5 s); `tea.NewProgram(fleetview.NewTop(ui, interval), tea.WithAltScreen()).Run()`. Wire `case "top":` and the help line:

```
  local-offload top [--ui http://127.0.0.1:18813] [--interval 5s]   terminal view of the fleet overview (for headless boxes; reads a running fleet-ui)
```

- [ ] **Step 4: Run tests and try it on the Lenovo**

Run: `go test ./internal/fleetview && go build .`
Expected: PASS.

With `fleet-ui --listen <delegator-tailnet-ip>:18813 --listen-trusted-network` running on Qube, on the Lenovo run `local-offload top --ui http://<delegator>:18813` (after the binary is dropped there per `docs/systems/fleet-node.md`'s Linux path) and confirm the three sections refresh; `q` exits and restores the terminal.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/fleetview/topmodel.go internal/fleetview/topmodel_test.go top_cmd.go main.go
git commit -m "top: terminal view of the fleet overview (Bubble Tea client of fleet-ui)"
```

---

### Task 10: Docs, ADR, changelog, version, port file

**Files:**
- Create: `docs/systems/fleet-overview.md` (from `docs/templates/system.md`), `docs/architecture/decisions/0034-fleet-overview-is-a-read-only-page-on-the-delegator.md` (from `docs/templates/adr.md`)
- Modify: `docs/systems/fleet-node.md` (health table: `gpu_util_pct`, `gpu_util_known`, `host_cpu_pct`, `host_ram_used_gb`, `host_ram_total_gb`, `served_models`; new route `GET /fleet/jobs`), `docs/README.md` (index row), `AGENTS.md` (pointer beside the fleet-node line), `CHANGELOG.md` (`## [0.113.0] — <date> — fleet overview: the PAIR-inspired operator surface` with Added/Changed), `VERSION`, `internal/buildinfo/buildinfo.go`, `.printing-press.json` → `0.113.0`, `P:\Port Directory\qube-ports.md` (row `18813 | local-offload fleet-ui | 127.0.0.1 (tailnet with --listen-trusted-network) | ...`)

- [ ] **Step 1: Write the system doc**

`docs/systems/fleet-overview.md` — fill every template heading. Must state: what the page shows and where each number comes from (health field → card element), the poll cadence and history bound, why the page is read-only and unauthenticated (metadata only; `/fleet/jobs` never carries payloads; tailnet-only bind), the errors taxonomy (`probe` / `job` / `delegation`, severities `error` / `warn`), the `fleet-smoke` contract and why its token lives in the context doc, the `top` client model, and the two placement changes with a pointer to `gate.go`'s key list. Questions the doc answers: "why is a node's utilization n/a?" (pre-0.113.0 launcher or non-nvidia-smi source), "why does the page show a job the node no longer lists?" (TTL eviction, one hour), "why did a node stop being placed on after upgrading?" (served_models omits the seat: the roster alias changed).

- [ ] **Step 2: Write the ADR**

ADR 0034 status `Accepted`, date today. Context: the PAIR gap analysis (2026-09-03) — PAIR's operator surface was the part worth porting; its discovery/pairing/mTLS and port takeover were not (tailnet + static roster already stronger). Decision: a read-only overview served by the DELEGATOR from existing node data, additive health fields only, GPU utilization as a tie-breaker only. Consequences: no new trust surface, no node-side UI, a fifth process on the delegator (`fleet-ui`) the operator starts by hand (no scheduler — house rule). Alternatives: adopting PAIR with a llama-swap shim (rejected: coarse scheduler, engine lock-in); a node-side page on every box (rejected: N pages, and the delegator already holds the fleet view); utilization as a primary key (rejected: PAIR's own README lists it as the reason its routing suits only similar machines).

- [ ] **Step 3: Update fleet-node.md, README index, AGENTS.md, CHANGELOG, version, port file**

`CHANGELOG.md` entry (under a new `## [0.113.0]` heading, keep `## [Unreleased]` empty above it):

```
### Added
- **`fleet-ui`** verb + `internal/fleetview`: a read-only overview page on the delegator (`127.0.0.1:18813`; tailnet with `--listen-trusted-network`) — one card per node with GPU-utilization/CPU/VRAM/RAM sparklines, seat + residency, served models, queue; a cluster jobs feed (node job stores + the delegation-log corpus) and an errors feed (probe / job / delegation). `/api/overview` is the JSON behind it.
- **`top`** verb: the same overview in a terminal (Bubble Tea client of a running `fleet-ui`) for headless boxes.
- **`fleet-smoke`** verb: one grounded one-step contract per node, table of node/seat/placement/wall/verdict, non-zero unless every node PASSes.
- **`/fleet/health`** additive fields: `gpu_util_pct` + `gpu_util_known` (busiest device), `host_cpu_pct`, `host_ram_used_gb`, `host_ram_total_gb`, `served_models`. **`GET /fleet/jobs`**: payload-free job metadata feed.
### Changed
- **Placement**: a node whose published `served_models` omits its agent seat is ineligible (unknown roster still eligible); GPU utilization is the FOURTH ranking key, a tie-breaker after queue depth, only when both nodes publish it (ADR 0034).
```

Bump: `VERSION` → `0.113.0`; `internal/buildinfo/buildinfo.go` `const Version = "0.113.0"`; `.printing-press.json` `"version": "0.113.0"`. Append the 18813 row to `P:\Port Directory\qube-ports.md` in the Listeners table.

- [ ] **Step 4: Gate**

Run: `go vet ./... && go test ./...`
Expected: green, including `docs_lint_test.go` (new system doc linked from `docs/README.md`; ADR numbered and templated) and `docs_tiers_test.go`.

- [ ] **Step 5: Commit**

```bash
git add docs AGENTS.md CHANGELOG.md VERSION internal/buildinfo/buildinfo.go .printing-press.json
git commit -m "docs+0.113.0: fleet overview system doc, ADR 0034, health fields, changelog"
```

---

### Task 11: Ship (clean-ship, Consequential tier because Task 7 touches placement)

- [ ] **Step 1:** `pr-review-toolkit:code-reviewer` once on the whole diff (`model: "sonnet"`); fix findings once.
- [ ] **Step 2:** `offload_review_diff` on the diff via a free seat that has not seen this session; triage findings (a `defer` does not block).
- [ ] **Step 3:** Open the PR with `gh --repo dmmdea/local-offload-public pr create` (account `dmmdea`, switched and verified in the same command per the account-separation skill). Body: Intent (PAIR-inspired operator surface, spec link) · what changed (the 10 commits) · how tested (`go test ./...`, the live `fleet-ui` render, `fleet-smoke` table, Lenovo `top`) · risk **medium** (placement gate change; mitigated by the unknown-is-eligible rule and the three pinned tests).
- [ ] **Step 4:** Merge only on the operator's explicit authorization in the conversation. Then deploy: build on Qube, drop the binary to the Aorus (parity rule — same day) and the Lenovo (per `lenovo-offload-harness-drop-target` memory: binary drop, not a git pull), restart the two `fleet-serve` nodes, confirm `curl <node>/fleet/health` shows `gpu_util_known`, `host_ram_total_gb`, `served_models` on both, then start `fleet-ui` on Qube and screenshot the page with all three cards live.

---

## Self-review

**Spec coverage:** Overview UI → Tasks 1, 2, 3, 5, 6. Jobs feed → Tasks 4, 5, 6. Errors feed → Task 5 (`errors.go`), Task 6 (table). Terminal view → Task 9. Capability gate → Tasks 3, 7. `fleet-smoke` → Task 8. Utilization tie-breaker → Tasks 1, 7. Docs/version/port → Task 10. Constraints (loopback default, SafeTransport, additive fields, no CDN) are each pinned by a test or a refusal in code.

**Placeholders:** none — the two "mirror the existing call" notes (config loading in Tasks 6 and 8; `PlacedResult` field names in Task 8) name the exact file:line to copy from rather than inventing a helper that may not exist.

**Type consistency:** `hostsample.Sample` / `Sampler.Load` used identically in Tasks 2 and 6; `AcceptSpec.Task/Model` and `JobView` fields in Task 4 match the `/fleet/jobs` rows Task 5 decodes (`accepted_at`, `wall_ms`, `error`, `task`, `model`, `state`); `Overview`/`Node`/`Error` JSON keys in Task 5 match the page's property reads in Task 6 (`gpu_util_pct`, `gpu_util_known`, `host_cpu_pct`, `host_ram_used_gb`, `host_ram_total_gb`, `vram_free_gb`, `vram_total_gb`, `gpu_devices[].util_pct/util_known`, `served_models`, `jobs_running`, `jobs_queued`, `max_concurrent_jobs`, `max_queue_depth`, `agent_ctx_tokens`, `history[].gpu_util/cpu/vram_free_gb/ram_used_gb`, `delegations[].ts/node/seat/deferred/acceptance_pass/wall_ms/error/defer_class/placement_reason`) and `RenderTop` in Task 9; `NodeView.ServedModels/GpuUtilPct/GpuUtilKnown` in Task 7 match the health keys from Tasks 1 and 3.
