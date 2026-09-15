//go:build windows

package placement

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	wtsapi32                        = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSQuerySessionInformationW = wtsapi32.NewProc("WTSQuerySessionInformationW")
	shell32                         = windows.NewLazySystemDLL("shell32.dll")
	procSHQueryUserNotificationSt   = shell32.NewProc("SHQueryUserNotificationState")
	user32                          = windows.NewLazySystemDLL("user32.dll")
	procGetLastInputInfo            = user32.NewProc("GetLastInputInfo")
	kernel32                        = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount64              = kernel32.NewProc("GetTickCount64")
)

const (
	wtsCurrentServerHandle = 0          // WTS_CURRENT_SERVER_HANDLE
	wtsSessionInfoEx       = 25         // WTS_INFO_CLASS.WTSSessionInfoEx
	noActiveConsoleSession = 0xFFFFFFFF // WTSGetActiveConsoleSessionId: no session attached to the console
)

// wtsInfoExLevel1 mirrors WTSINFOEX_LEVEL1_W. The explicit pad before the
// LARGE_INTEGER block keeps the layout identical to the C struct on every
// Windows arch (MSVC aligns LARGE_INTEGER to 8; Go/386 would align int64 to 4).
// Verified live 2026-09-10 on the Qube: 232 bytes returned, level 1, the
// user name and the logon/current times decode in place.
type wtsInfoExLevel1 struct {
	SessionID               uint32
	SessionState            int32
	SessionFlags            int32
	WinStationName          [33]uint16
	UserName                [21]uint16
	DomainName              [18]uint16
	_                       [4]byte
	LogonTime               int64
	ConnectTime             int64
	DisconnectTime          int64
	LastInputTime           int64
	CurrentTime             int64
	IncomingBytes           uint32
	OutgoingBytes           uint32
	IncomingFrames          uint32
	OutgoingFrames          uint32
	IncomingCompressedBytes uint32
	OutgoingCompressedBytes uint32
}

// wtsInfoExW mirrors WTSINFOEXW: Level, then the union (8-aligned).
type wtsInfoExW struct {
	Level uint32
	_     [4]byte
	Data  wtsInfoExLevel1
}

// lastInputInfo mirrors LASTINPUTINFO for GetLastInputInfo.
type lastInputInfo struct {
	Size uint32
	Time uint32 // GetTickCount() at the last input event, ms
}

// probeOS reads the console session: (1) which session owns the console,
// (2) its lock state through WTSQuerySessionInformation level WTSSessionInfoEx
// — the only API that reports the lock state of a session other than the
// caller's, which matters because the fleet node can run as a scheduled
// task; (3) the idle time, from WTS's LastInputTime when the OS fills it
// (RDP sessions) and otherwise from GetLastInputInfo, which is per-session
// and is trusted ONLY when the caller's own session is the console session
// (measured 2026-09-10 on the Qube: WTS reports LastInputTime 0 for the local
// console, so a probe built on it alone fails closed forever on the box the
// guard exists for; and from session 0 GetLastInputInfo would describe a
// session that never receives input, reading as "away" forever — fail OPEN);
// (4) the shell's notification state so a full-screen game or a
// presentation counts as "at the desk". Any step that fails leaves
// Known=false and the guard refuses; the OS layer is kept thin and the rule
// lives in classify.
func probeOS(threshold time.Duration) Presence {
	sid := windows.WTSGetActiveConsoleSessionId()
	if sid == noActiveConsoleSession {
		return Presence{Known: false, Note: "no session is attached to the console"}
	}
	var (
		buf *byte
		n   uint32
	)
	r, _, callErr := procWTSQuerySessionInformationW.Call(uintptr(wtsCurrentServerHandle), uintptr(sid), uintptr(wtsSessionInfoEx), uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 || buf == nil {
		return Presence{Known: false, Note: fmt.Sprintf("WTSQuerySessionInformation(session %d, WTSSessionInfoEx) failed: %v", sid, callErr)}
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
	if uintptr(n) < unsafe.Sizeof(wtsInfoExW{}) {
		return Presence{Known: false, Note: fmt.Sprintf("WTSSessionInfoEx returned %d bytes, want ≥ %d", n, unsafe.Sizeof(wtsInfoExW{}))}
	}
	info := (*wtsInfoExW)(unsafe.Pointer(buf))
	if info.Level != 1 {
		return Presence{Known: false, Note: fmt.Sprintf("WTSSessionInfoEx level %d, want 1", info.Level)}
	}
	d := info.Data
	locked := d.SessionFlags == wtsSessionStateLock
	idle, idleOK, idleNote := lastInputIdle(sid, d)
	if !idleOK {
		if locked {
			// A locked session is unambiguously away whatever the idle reader says.
			return Presence{Known: true, Away: true, Locked: true, Note: "console session locked (" + idleNote + ")"}
		}
		return Presence{Known: false, Note: idleNote}
	}
	var quns int32
	hr, _, _ := procSHQueryUserNotificationSt.Call(uintptr(unsafe.Pointer(&quns)))
	if int32(hr) < 0 {
		if locked {
			return Presence{Known: true, Away: true, Locked: true, IdleSec: int(idle.Seconds()), Note: "console session locked (shell state unavailable)"}
		}
		return Presence{Known: false, IdleSec: int(idle.Seconds()), Note: fmt.Sprintf("SHQueryUserNotificationState failed (HRESULT 0x%08x)", uint32(hr))}
	}
	away, note := classify(locked, idle, quns, threshold)
	return Presence{Known: true, Away: away, Locked: locked, IdleSec: int(idle.Seconds()), Note: note + " (" + idleNote + ")"}
}

// lastInputIdle is the idle-time reader with its two sources in order of
// trust: WTS's own LastInputTime for the console session (session-scoped,
// works from any session, but 0 on a local console), then GetLastInputInfo
// when the caller shares the console session (the only case in which it
// describes the operator's input). The note names the source so an operator
// reading a defer can tell which clock refused them.
func lastInputIdle(consoleSID uint32, d wtsInfoExLevel1) (time.Duration, bool, string) {
	if d.LastInputTime > 0 && d.CurrentTime >= d.LastInputTime {
		return time.Duration(d.CurrentTime-d.LastInputTime) * 100 * time.Nanosecond, true, "idle from WTS LastInputTime"
	}
	var own uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &own); err != nil {
		return 0, false, fmt.Sprintf("WTS reports no last-input time and the caller's session is unknown: %v", err)
	}
	if own != consoleSID {
		return 0, false, fmt.Sprintf("WTS reports no last-input time and the caller runs in session %d, not the console session %d — GetLastInputInfo would not describe the operator", own, consoleSID)
	}
	li := lastInputInfo{Size: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if r, _, err := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&li))); r == 0 {
		return 0, false, fmt.Sprintf("GetLastInputInfo failed: %v", err)
	}
	if li.Time == 0 {
		return 0, false, "GetLastInputInfo reports no input since boot in this session"
	}
	tick, _, _ := procGetTickCount64.Call()
	// dwTime is a 32-bit tick; modular subtraction is exact until 49.7 days of idle.
	return time.Duration(uint32(tick)-li.Time) * time.Millisecond, true, "idle from GetLastInputInfo"
}
