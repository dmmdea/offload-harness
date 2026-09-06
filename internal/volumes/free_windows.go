//go:build windows

package volumes

import "golang.org/x/sys/windows"

// FreeBytes reports the bytes available to the caller on the volume holding
// path (GetDiskFreeSpaceEx's caller-available figure, so a quota on the
// account is honoured the same way the store's own writes would meet it).
func FreeBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail, nil
}
