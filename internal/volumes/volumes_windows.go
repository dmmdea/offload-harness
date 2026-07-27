package volumes

import (
	"strings"
	"syscall"
	"unsafe"
)

// Windows enumeration via kernel32 — no third-party dependency, matching the rest
// of the repo (gpu_hide_windows.go takes the same approach).
var (
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procGetLogicalDriveStrings = kernel32.NewProc("GetLogicalDriveStringsW")
	procGetDriveType           = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceEx     = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetVolumeInformation   = kernel32.NewProc("GetVolumeInformationW")
)

// Windows drive types (winbase.h).
const (
	driveRemovable = 2
	driveFixed     = 3
	driveRemote    = 4
	driveCDROM     = 5
	driveRAMDisk   = 6
)

// List enumerates fixed, removable and network drives with their capacity. A drive
// that cannot be queried (an empty card reader, a disconnected share) is skipped
// rather than reported with zeroes, which would look like a full disk.
func List() ([]Volume, error) {
	buf := make([]uint16, 512)
	n, _, err := procGetLogicalDriveStrings.Call(uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if n == 0 {
		return nil, err
	}
	osRoot := strings.ToUpper(osVolumeRoot())

	var out []Volume
	for _, root := range splitNullTerminated(buf[:n]) {
		if root == "" {
			continue
		}
		p, perr := syscall.UTF16PtrFromString(root)
		if perr != nil {
			continue
		}
		dt, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(p)))
		switch dt {
		case driveFixed, driveRemovable, driveRemote, driveRAMDisk:
		default: // CD-ROM and unknown/no-root-dir are never install targets
			continue
		}
		var freeAvail, total, totalFree uint64
		r, _, _ := procGetDiskFreeSpaceEx.Call(
			uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(&freeAvail)),
			uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&totalFree)),
		)
		if r == 0 || total == 0 {
			continue // no media, or access denied: absent beats a phantom full disk
		}
		v := Volume{
			Root:       root,
			TotalBytes: total,
			FreeBytes:  freeAvail, // the caller's quota-aware free space, not the raw total
			IsOS:       strings.EqualFold(strings.TrimSuffix(root, `\`), strings.TrimSuffix(osRoot, `\`)),
			Removable:  dt == driveRemovable || dt == driveRAMDisk,
			Network:    dt == driveRemote,
		}
		v.Label, v.FS = volumeInfo(p)
		out = append(out, v)
	}
	return out, nil
}

// volumeInfo reads the label and filesystem name; both are cosmetic, so any
// failure yields empty strings rather than an error.
func volumeInfo(root *uint16) (label, fs string) {
	nameBuf := make([]uint16, 261)
	fsBuf := make([]uint16, 261)
	r, _, _ := procGetVolumeInformation.Call(
		uintptr(unsafe.Pointer(root)),
		uintptr(unsafe.Pointer(&nameBuf[0])), uintptr(len(nameBuf)),
		0, 0, 0,
		uintptr(unsafe.Pointer(&fsBuf[0])), uintptr(len(fsBuf)),
	)
	if r == 0 {
		return "", ""
	}
	return syscall.UTF16ToString(nameBuf), syscall.UTF16ToString(fsBuf)
}

// osVolumeRoot is the drive Windows booted from, e.g. `C:\`.
func osVolumeRoot() string {
	if sd := syscall.Getenv; sd != nil {
		if v, ok := syscall.Getenv("SystemDrive"); ok && v != "" {
			return strings.ToUpper(v) + `\`
		}
	}
	return `C:\`
}

// splitNullTerminated turns kernel32's "C:\\\x00D:\\\x00\x00" block into paths.
func splitNullTerminated(b []uint16) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c != 0 {
			continue
		}
		if i > start {
			out = append(out, syscall.UTF16ToString(b[start:i]))
		}
		start = i + 1
	}
	return out
}
