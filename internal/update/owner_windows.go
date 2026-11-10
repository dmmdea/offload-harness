//go:build windows

package update

// foreignRootOwner has no meaning on Windows: there is no root user whose writes leave a foreign owner behind.
func foreignRootOwner(home string) (uid int, name string, mismatch bool) {
	return 0, "", false
}
