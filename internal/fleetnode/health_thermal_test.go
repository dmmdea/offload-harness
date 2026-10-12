package fleetnode

// F11: /fleet/health gpu_devices[] carries each card's temperature and the clock event reasons active on it.
//
// The numbers come from a second, optional nvidia-smi query that SmiDeviceProbe merges into the devices the
// 2 s sampler already publishes. The cadence is the point of most of these tests: the sampler ticks every 2 s,
// the thermal query runs at most once per 30 s, a failed one is not retried sooner than 60 s and says so once,
// and an answer older than 90 s is dropped, never published.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

const (
	thermalUUID0 = "GPU-1111aaaa-2222-3333-4444-555566667777"
	thermalUUID1 = "GPU-2222bbbb-3333-4444-5555-666677778888"
)

// smiTwoCards is the per-device query's answer for two synthetic cards (index, uuid, name, total, used, util,
// display_active, display_attached).
const smiTwoCards = "0, " + thermalUUID0 + ", NVIDIA GeForce RTX 5060 Ti, 16311, 867, 3, Disabled, No\r\n" +
	"1, " + thermalUUID1 + ", NVIDIA GeForce RTX 5070 Ti, 16303, 1200, 0, Disabled, No\r\n"

func smiRun(out string) func() (string, error) {
	return func() (string, error) { return out, nil }
}

func tempOf(v int) *int { return &v }

// thermalClock is the clock the cache reads.
type thermalClock struct{ t time.Time }

func newThermalClock() *thermalClock {
	return &thermalClock{t: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
}
func (c *thermalClock) now() time.Time          { return c.t }
func (c *thermalClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// fakeThermal is a thermal reader that counts its calls and can be told to fail.
type fakeThermal struct {
	mu        sync.Mutex
	calls     int
	err       error
	rows      []gpuprobe.Thermal
	deadlines []time.Duration
}

func (f *fakeThermal) read(ctx context.Context) ([]gpuprobe.Thermal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if dl, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, time.Until(dl))
	} else {
		f.deadlines = append(f.deadlines, -1)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func (f *fakeThermal) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func hotCard0() []gpuprobe.Thermal {
	return []gpuprobe.Thermal{
		{Index: 0, UUID: thermalUUID0, TemperatureC: tempOf(83), ThrottleReasons: []string{"sw_thermal_slowdown", "hw_thermal_slowdown"}},
		{Index: 1, UUID: thermalUUID1}, // the query could not read this card
	}
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, format)
}

func (l *logLines) n() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines)
}

func probeOnce(t *testing.T, p DeviceProbe) []GPUDevice {
	t.Helper()
	devs, err := p()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(devs) != 2 {
		t.Fatalf("probe returned %d devices, want 2", len(devs))
	}
	return devs
}

func TestSmiDeviceProbeQueriesThermalAtMostOncePerWindow(t *testing.T) {
	clk := newThermalClock()
	th := &fakeThermal{rows: hotCard0()}
	p := smiDeviceProbeWith(smiRun(smiTwoCards), th.read, clk.now, (&logLines{}).logf)
	for i := 0; i < 15; i++ { // 15 ticks at 2 s: t = 0 .. 28 s
		devs := probeOnce(t, p)
		if devs[0].TemperatureC == nil || *devs[0].TemperatureC != 83 {
			t.Fatalf("tick %d: device 0 = %+v, want the cached 83 C on every tick", i, devs[0])
		}
		clk.advance(2 * time.Second)
	}
	if got := th.count(); got != 1 {
		t.Fatalf("15 probe calls at 2 s spacing made %d thermal queries, want 1", got)
	}
	probeOnce(t, p) // t = 30 s: the window is over
	if got := th.count(); got != 2 {
		t.Fatalf("at 30 s the thermal query ran %d times in all, want 2", got)
	}
}

func TestSmiDeviceProbeMergesThermalForTheCardsThatHaveIt(t *testing.T) {
	th := &fakeThermal{rows: hotCard0()}
	p := smiDeviceProbeWith(smiRun(smiTwoCards), th.read, newThermalClock().now, (&logLines{}).logf)
	devs := probeOnce(t, p)
	if devs[0].TemperatureC == nil || *devs[0].TemperatureC != 83 ||
		!reflect.DeepEqual(devs[0].ThrottleReasons, []string{"sw_thermal_slowdown", "hw_thermal_slowdown"}) {
		t.Errorf("device 0 = %+v, want 83 C and both thermal reasons", devs[0])
	}
	if devs[1].TemperatureC != nil || len(devs[1].ThrottleReasons) != 0 {
		t.Errorf("device 1 = %+v, want neither: its row had nothing", devs[1])
	}
}

func TestSmiDeviceProbeDropsAThermalAnswerOlderThanNinetySeconds(t *testing.T) {
	clk := newThermalClock()
	th := &fakeThermal{rows: hotCard0()}
	p := smiDeviceProbeWith(smiRun(smiTwoCards), th.read, clk.now, (&logLines{}).logf)

	carries := func(label string, want bool) {
		t.Helper()
		devs := probeOnce(t, p)
		if got := devs[0].TemperatureC != nil; got != want {
			t.Fatalf("%s: device 0 carries a temperature = %v, want %v", label, got, want)
		}
	}
	carries("t=0 (the first answer)", true)
	th.err = errors.New("nvidia-smi: exit status 9")
	clk.advance(30 * time.Second)
	carries("t=30 (the query failed, the answer is 30 s old)", true)
	if th.count() != 2 {
		t.Fatalf("thermal calls = %d at t=30, want 2", th.count())
	}
	clk.advance(30 * time.Second)
	carries("t=60 (inside the 60 s back-off, the answer is 60 s old)", true)
	if th.count() != 2 {
		t.Fatalf("thermal calls = %d at t=60, want 2: a failed query is not retried sooner than 60 s", th.count())
	}
	clk.advance(30 * time.Second)
	carries("t=90 (retried and failed again, the answer is exactly 90 s old)", true)
	if th.count() != 3 {
		t.Fatalf("thermal calls = %d at t=90, want 3", th.count())
	}
	clk.advance(1 * time.Second)
	carries("t=91 (the answer is 91 s old)", false)
	if th.count() != 3 {
		t.Fatalf("thermal calls = %d at t=91, want 3", th.count())
	}
	// Dropped, not zeroed: the memory reading is still whole.
	devs := probeOnce(t, p)
	if devs[0].Name != "NVIDIA GeForce RTX 5060 Ti" || devs[0].TotalGiB <= 0 {
		t.Fatalf("device 0 = %+v: dropping the thermal answer must leave the memory reading alone", devs[0])
	}
}

func TestSmiDeviceProbeBacksOffAfterAFailureAndLogsOncePerOutage(t *testing.T) {
	clk := newThermalClock()
	th := &fakeThermal{err: errors.New("nvidia-smi: not on PATH")}
	lg := &logLines{}
	p := smiDeviceProbeWith(smiRun(smiTwoCards), th.read, clk.now, lg.logf)
	for i := 0; i < 75; i++ { // 150 s of 2 s ticks
		devs := probeOnce(t, p)
		if devs[0].TemperatureC != nil || len(devs[0].ThrottleReasons) != 0 {
			t.Fatalf("tick %d: device 0 = %+v, want no thermal field: nothing was ever read", i, devs[0])
		}
		clk.advance(2 * time.Second)
	}
	// Tried at t = 0, 60 and 120 s.
	if got := th.count(); got != 3 {
		t.Fatalf("a failing thermal query ran %d times in 150 s, want 3 (no sooner than every 60 s)", got)
	}
	if got := lg.n(); got != 1 {
		t.Fatalf("a failing thermal query logged %d lines, want 1", got)
	}
	// It recovers, then fails again: a new outage is announced again.
	th.mu.Lock()
	th.err, th.rows = nil, hotCard0()
	th.mu.Unlock()
	clk.advance(60 * time.Second)
	probeOnce(t, p)
	th.mu.Lock()
	th.err = errors.New("nvidia-smi: exit status 9")
	th.mu.Unlock()
	clk.advance(30 * time.Second)
	probeOnce(t, p)
	if got := lg.n(); got != 2 {
		t.Fatalf("a second outage after a recovery left %d log lines in all, want 2", got)
	}
}

func TestSmiDeviceProbeBoundsTheThermalReadWithADeadline(t *testing.T) {
	th := &fakeThermal{rows: hotCard0()}
	p := smiDeviceProbeWith(smiRun(smiTwoCards), th.read, newThermalClock().now, (&logLines{}).logf)
	probeOnce(t, p)
	if len(th.deadlines) != 1 || th.deadlines[0] <= 0 || th.deadlines[0] > 15*time.Second {
		t.Fatalf("thermal read deadlines = %v, want one deadline within 15 s: a wedged driver must not hold the sampler", th.deadlines)
	}
}

// The guard: the memory reading is the same with or without the thermal merge, and a failing run still fails.
func TestSmiDeviceProbeLeavesTheMemoryReadingAlone(t *testing.T) {
	want, err := ParseSmiMemoryDevices(smiTwoCards)
	if err != nil {
		t.Fatal(err)
	}
	th := &fakeThermal{rows: hotCard0()}
	got := probeOnce(t, smiDeviceProbeWith(smiRun(smiTwoCards), th.read, newThermalClock().now, (&logLines{}).logf))
	for i := range got {
		g := got[i]
		g.TemperatureC, g.ThrottleReasons = nil, nil
		if !reflect.DeepEqual(g, want[i]) {
			t.Errorf("device %d changed beyond the thermal fields:\n got %+v\nwant %+v", i, g, want[i])
		}
	}
	failing := smiDeviceProbeWith(func() (string, error) { return "", errors.New("boom") }, th.read, newThermalClock().now, (&logLines{}).logf)
	if _, err := failing(); err == nil || !strings.Contains(err.Error(), "nvidia-smi") {
		t.Fatalf("a failing nvidia-smi run answered %v, want the nvidia-smi error", err)
	}
	if got := th.count(); got != 1 {
		t.Fatalf("thermal queries = %d, want 1 (the failing device query must not spawn one)", got)
	}
}

// healthDevices serves /fleet/health from a sampler whose probe is the given one and returns the raw gpu_devices array.
func healthDevices(t *testing.T, p DeviceProbe) (raw json.RawMessage, devs []map[string]any) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	smp := StartDeviceProbeSampler(ctx, time.Hour, p, "")
	opts := &Options{NodeID: "n", Snapshot: smp.Load}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		GpuDevices json.RawMessage `json:"gpu_devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("health not JSON: %v", err)
	}
	if err := json.Unmarshal(body.GpuDevices, &devs); err != nil {
		t.Fatalf("gpu_devices not an array of objects: %v (%s)", err, body.GpuDevices)
	}
	return body.GpuDevices, devs
}

func TestHealthGPUDevicesCarryTemperatureWhenKnown(t *testing.T) {
	th := &fakeThermal{rows: hotCard0()}
	_, devs := healthDevices(t, smiDeviceProbeWith(smiRun(smiTwoCards), th.read, newThermalClock().now, (&logLines{}).logf))
	if len(devs) != 2 {
		t.Fatalf("gpu_devices = %v, want 2", devs)
	}
	if devs[0]["temperature_c"] != float64(83) {
		t.Errorf("gpu_devices[0].temperature_c = %v, want 83", devs[0]["temperature_c"])
	}
	if got, _ := devs[0]["throttle_reasons"].([]any); !reflect.DeepEqual(got, []any{"sw_thermal_slowdown", "hw_thermal_slowdown"}) {
		t.Errorf("gpu_devices[0].throttle_reasons = %v, want both thermal reasons", devs[0]["throttle_reasons"])
	}
	for _, k := range []string{"temperature_c", "throttle_reasons"} {
		if _, present := devs[1][k]; present {
			t.Errorf("gpu_devices[1] carries %s = %v: its query row was empty, the key must be absent", k, devs[1][k])
		}
	}
}

// The guard: a node whose thermal query fails publishes exactly the payload a node without the query publishes.
func TestHealthWithoutAThermalProbeIsByteIdentical(t *testing.T) {
	failing := &fakeThermal{err: errors.New("nvidia-smi: exit status 9")}
	got, _ := healthDevices(t, smiDeviceProbeWith(smiRun(smiTwoCards), failing.read, newThermalClock().now, (&logLines{}).logf))
	baseline, _ := healthDevices(t, func() ([]GPUDevice, error) { return ParseSmiMemoryDevices(smiTwoCards) })
	if string(got) != string(baseline) {
		t.Fatalf("gpu_devices with a failing thermal query differs from the payload without one:\n got %s\nwant %s", got, baseline)
	}
	if strings.Contains(string(got), "temperature_c") || strings.Contains(string(got), "throttle_reasons") {
		t.Fatalf("a thermal key leaked into %s", got)
	}
}
