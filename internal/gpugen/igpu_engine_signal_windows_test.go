//go:build windows

package gpugen

// selfSignal is a no-op on Windows (no signal deaths): the rows that need it skip there.
func selfSignal(name string) {}
