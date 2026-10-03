//go:build windows

package volumes

// OSDriveRoot is the drive Windows booted from, e.g. `C:\`. It is the one volume the
// harness keeps data off: C: holds Windows and program installs, never data.
func OSDriveRoot() string { return osVolumeRoot() }
