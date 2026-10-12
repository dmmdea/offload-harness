package fleetnode

// F11, fix round: a card the thermal query read with nothing limiting its clocks publishes throttle_reasons as an empty
// array; a card whose row read nothing publishes no key. The two used to look the same on /fleet/health.

import (
	"reflect"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func TestHealthGPUDevicesTellAClearReadFromNoRead(t *testing.T) {
	th := &fakeThermal{rows: []gpuprobe.Thermal{
		{Index: 0, UUID: thermalUUID0, TemperatureC: tempOf(41), ThrottleReasons: []string{}}, // read: nothing limiting
		{Index: 1, UUID: thermalUUID1, TemperatureC: tempOf(52)},                              // the throttle column was not read
	}}
	raw, devs := healthDevices(t, settledProbe(smiRun(smiTwoCards), th.read, newThermalClock().now, (&logLines{}).logf))
	if len(devs) != 2 {
		t.Fatalf("gpu_devices = %s, want 2", raw)
	}
	got, present := devs[0]["throttle_reasons"]
	if !present || !reflect.DeepEqual(got, []any{}) {
		t.Errorf("gpu_devices[0].throttle_reasons = %v (present %v), want [] for a card read with nothing active: %s", got, present, raw)
	}
	if devs[0]["temperature_c"] != float64(41) {
		t.Errorf("gpu_devices[0].temperature_c = %v, want 41", devs[0]["temperature_c"])
	}
	if _, present := devs[1]["throttle_reasons"]; present {
		t.Errorf("gpu_devices[1] carries throttle_reasons = %v: its throttle column was not read, the key must be absent", devs[1]["throttle_reasons"])
	}
	if devs[1]["temperature_c"] != float64(52) {
		t.Errorf("gpu_devices[1].temperature_c = %v, want 52", devs[1]["temperature_c"])
	}
}
