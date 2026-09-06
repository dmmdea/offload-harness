//go:build !windows

package volumes

import "syscall"

// FreeBytes reports the bytes available to an unprivileged writer on the
// volume holding path. It is the store steward's view of "how much may the
// store keep": the number the kernel would refuse a write beyond, not the
// raw block count (root-reserved blocks are excluded on purpose).
func FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
