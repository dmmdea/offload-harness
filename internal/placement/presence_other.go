//go:build !windows

package placement

import "time"

// probeOS has no console-session reader off Windows: the presence guard
// fails closed (Known=false) rather than guessing that an operator with no
// desktop session is away.
func probeOS(threshold time.Duration) Presence {
	return Presence{Known: false, Note: "presence probe: no console-session reader on this platform"}
}
