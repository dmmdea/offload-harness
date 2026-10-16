//go:build !windows

package gpulease

import "os"

// openRecordShared is os.Open off Windows: a POSIX open never blocks, and is never blocked by, the rename that replaces the file.
func openRecordShared(path string) (*os.File, error) { return os.Open(path) }
