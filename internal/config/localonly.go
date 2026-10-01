package config

import (
	"fmt"
	"strings"
)

// localOnlyAccelerators are the accelerator ids that never leave the box that carries them
// (register E-08, the operator's standing decision on the standalone accelerator box: it is never
// delegated to, never serves the fleet and sends nothing to another node). A local-only device is
// published in no /fleet/health, opens no `accel` lane and has no accel job accepted for it, so
// the box's own tools and agent loop keep it and nothing on the fleet can reach it.
//
// The decision is PER DEVICE and this is its one spelling. The Coral Edge TPU and the RK3588 NPU
// are served over the fleet (another box advertises each, ADR 0038), so a guard keyed on "any
// accelerator" or on the standalone box itself would strip a device the fleet routes work to. A
// device joins this list only on an operator decision; a device profiles.json declares without a
// decision fails TestEveryDeclaredAcceleratorHasAFleetVisibilityDecision (internal/tierseed).
var localOnlyAccelerators = []string{"hailo-8l"}

// LocalOnlyAccelerator reports whether id stays on the box that carries it. The id is compared
// trimmed and case-folded so a hand-edited spelling cannot walk past the guard; the registration
// gates (HasAccelerator) stay exact, so such a spelling also registers no tool.
func LocalOnlyAccelerator(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, l := range localOnlyAccelerators {
		if id == l {
			return true
		}
	}
	return false
}

// FleetVisibleAccelerators is ids without the local-only devices: the order is kept (ADR 0037's
// first-listed-owner rule rides on it), the result is always a fresh slice, and it is nil when
// nothing is left, so an `omitempty` health key stays absent.
//
// It is THE filter behind the three fleet-visible points: the /fleet/health accelerators list, the
// `accel` task advertisement and accel job acceptance. It is applied on the way OUT only —
// Accelerators, HasAccelerator and every local registration (the MCP tools, the agent loop, the
// pipeline lanes) keep the device, because the decision is "never leaves the box", not "off".
func FleetVisibleAccelerators(ids []string) []string {
	var out []string
	for _, id := range ids {
		if LocalOnlyAccelerator(id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// LocalOnlyAcceleratorFindings returns the non-fatal findings about fleet_accelerators: an entry
// naming a local-only device asks the fleet for something it never serves, so every forwarded call
// for it would defer. The box that CARRIES the device lists it in accelerators, which is the
// normal case and says nothing here. It loads (a finding, never a refusal), is named once per
// entry with its index, and rides the same two doors as CallDeadlineFindings: a `doctor` FAIL row
// and a startup warning.
func LocalOnlyAcceleratorFindings(c Config) []string {
	var out []string
	for i, id := range c.FleetAccelerators {
		if !LocalOnlyAccelerator(id) {
			continue
		}
		out = append(out, fmt.Sprintf("fleet_accelerators[%d] %q names a local-only device: it stays on the box that carries it, so no node advertises it or accepts its jobs over the fleet and every forwarded call would defer (ADR 0038, register E-08). Remove it from fleet_accelerators; the box that carries the device lists it in accelerators", i, id))
	}
	return out
}
