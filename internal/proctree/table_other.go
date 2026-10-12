//go:build !windows && !linux

package proctree

import "errors"

// tableMembers has no process-table reader on this platform: the tree is stopped through its
// group, but its members cannot be listed.
func tableMembers(pgid, root int) ([]int, error) {
	return nil, errors.New("proctree: no process-table reader on this platform")
}
