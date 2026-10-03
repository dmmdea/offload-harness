//go:build !windows

package volumes

// OSDriveRoot is "" off Windows. The rule it serves is about the drive letter Windows
// boots from; a Unix host has no such drive, and an empty root makes OnDrive match
// nothing, so no caller needs its own GOOS check.
func OSDriveRoot() string { return "" }
