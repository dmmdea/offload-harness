package fleetview

import (
	"fmt"
	"sort"
	"strings"
)

// AccelTelemetryLine renders one accelerator's compact health telemetry as the single line the
// overview and `top` show, e.g. "axera-ax650 34°C · NPU 0% · CMM 18/2944 MiB · clocks max". A key
// the node did not publish is left out (absent is unknown, never zero); a reading that failed shows
// its error; a degraded PCIe link is flagged. The page's ui.html carries the same format in JS.
func AccelTelemetryLine(id string, t map[string]any) string {
	if msg := str(t, "error"); msg != "" {
		return id + " telemetry unavailable: " + msg
	}
	parts := []string{}
	has := func(k string) bool { _, ok := t[k]; return ok }
	if has("temp_c") {
		parts = append(parts, fmt.Sprintf("%.0f°C", num(t, "temp_c")))
	}
	if has("npu_load_pct") {
		parts = append(parts, fmt.Sprintf("NPU %.0f%%", num(t, "npu_load_pct")))
	}
	if has("cmm_used_mib") && has("cmm_total_mib") {
		parts = append(parts, fmt.Sprintf("CMM %.0f/%.0f MiB", num(t, "cmm_used_mib"), num(t, "cmm_total_mib")))
	}
	switch {
	case boolv(t, "clocks_at_max"):
		parts = append(parts, "clocks max")
	case has("npu_mhz") && has("npu_max_mhz"):
		parts = append(parts, fmt.Sprintf("clocks NPU %.0f/%.0f MHz", num(t, "npu_mhz"), num(t, "npu_max_mhz")))
	}
	if boolv(t, "link_degraded") {
		parts = append(parts, "LINK DEGRADED")
	}
	if len(parts) == 0 {
		return id
	}
	return id + " " + strings.Join(parts, " · ")
}

// accelTelemetryIDs is the device ids of a node's telemetry in a stable order.
func accelTelemetryIDs(m map[string]map[string]any) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// nestedMaps reads a JSON object of JSON objects (health accelerator_telemetry); a value that is
// not an object is skipped, and nil comes back when none is.
func nestedMaps(m map[string]any, key string) map[string]map[string]any {
	raw, _ := m[key].(map[string]any)
	var out map[string]map[string]any
	for id, v := range raw {
		if inner, ok := v.(map[string]any); ok {
			if out == nil {
				out = map[string]map[string]any{}
			}
			out[id] = inner
		}
	}
	return out
}
