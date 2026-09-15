package placement

import (
	"fmt"
	"time"
)

// Shell notification states (SHQueryUserNotificationState's QUERY_USER_
// NOTIFICATION_STATE). Named here, not in the Windows file, so the pure
// classifier and its tests can reason about them on every platform.
const (
	qunsNotPresent           int32 = 1 // screen saver, locked, or a non-interactive session
	qunsBusy                 int32 = 2 // a full-screen window is up ("busy")
	qunsD3DFullScreen        int32 = 3 // a Direct3D exclusive full-screen app (a game)
	qunsPresentationMode     int32 = 4 // presentation mode
	qunsAcceptsNotifications int32 = 5 // normal desktop
	qunsQuietTime            int32 = 6 // the first hour after a new logon
	qunsApp                  int32 = 7 // a Windows Store app is running
)

// WTS session lock flags, as WTSINFOEX_LEVEL1.SessionFlags reports them. The
// constants are INVERTED from what the names suggest — WTS_SESSIONSTATE_LOCK
// is 0 and UNLOCK is 1 — which is exactly the kind of fact a probe gets wrong
// silently; presence_test pins them.
const (
	wtsSessionStateLock   int32 = 0
	wtsSessionStateUnlock int32 = 1
)

// defaultIdleThreshold is the last-input threshold applied when the caller
// passes 0: config.OperatorIdle's 15-minute default, restated here so the
// probe can never be handed "0 seconds" and read every keystroke gap as away.
const defaultIdleThreshold = 15 * time.Minute

func idleThreshold(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultIdleThreshold
	}
	return d
}

// ProbePresence answers the presence guard for a configured mode. present and
// away are operator overrides that never touch the OS (present is the default:
// the display card stays closed until the operator opts in); auto reads the
// console session — lock state, last input against the idle threshold, and the
// shell's notification state so a game or a presentation counts as "at the
// desk" even with the mouse untouched. Any probe failure is Known=false, which
// the guard refuses.
func ProbePresence(mode string, idle time.Duration) Presence {
	switch mode {
	case "", "present":
		return Presence{Mode: "present", Known: true, Away: false, Note: "operator override: present (the default)"}
	case "away":
		return Presence{Mode: "away", Known: true, Away: true, Note: "operator override: away"}
	case "auto":
		p := probeOS(idleThreshold(idle))
		p.Mode = "auto"
		return p
	}
	return Presence{Mode: mode, Known: false, Note: fmt.Sprintf("presence mode %q is not one of present, away, auto", mode)}
}

// classify is the pure rule behind mode auto, tested branch by branch:
// a locked console is away; a shell that reports NOT_PRESENT is away; a busy,
// D3D-fullscreen or presentation shell is at the desk whatever the idle time;
// otherwise away iff last input is at least the threshold ago.
func classify(locked bool, idle time.Duration, quns int32, threshold time.Duration) (away bool, note string) {
	if locked {
		return true, "console session locked"
	}
	switch quns {
	case qunsNotPresent:
		return true, "shell reports the user not present"
	case qunsBusy:
		return false, fmt.Sprintf("shell busy (full-screen window); idle %s", idle.Round(time.Second))
	case qunsD3DFullScreen:
		return false, fmt.Sprintf("Direct3D fullscreen app running; idle %s", idle.Round(time.Second))
	case qunsPresentationMode:
		return false, fmt.Sprintf("presentation mode; idle %s", idle.Round(time.Second))
	}
	if idle >= threshold {
		return true, fmt.Sprintf("idle %s ≥ threshold %s", idle.Round(time.Second), threshold)
	}
	return false, fmt.Sprintf("idle %s < threshold %s", idle.Round(time.Second), threshold)
}
