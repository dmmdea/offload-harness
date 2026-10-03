//go:build !windows && !linux

package gpulease

import "errors"

// processTreeImpl has no process-table reader on this platform: the evidence rule then reads
// the lease's recorded command alone, and a legacy lease stays whole-node unless that names a
// card.
func processTreeImpl(root int) ([]TreeProc, error) {
	return nil, errors.New("gpulease: no process-tree reader on this platform")
}

// startUnixMs cannot convert a start identity on this platform; the recycled-pid check is
// skipped there (the tree reader does not exist either).
func startUnixMs(int64) (int64, bool) { return 0, false }
