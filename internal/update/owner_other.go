//go:build !windows

package update

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// foreignRootOwner reports whether this process runs as root while home belongs to another user. A missing
// home, an unreadable owner or a non-root process is not a mismatch.
func foreignRootOwner(home string) (uid int, name string, mismatch bool) {
	if os.Geteuid() != 0 {
		return 0, "", false
	}
	fi, err := os.Stat(home)
	if err != nil {
		return 0, "", false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return 0, "", false
	}
	uid = int(st.Uid)
	if u, uerr := user.LookupId(strconv.Itoa(uid)); uerr == nil && u.Username != "" {
		name = u.Username
	} else if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
		name = su
	} else {
		name = "uid " + strconv.Itoa(uid)
	}
	return uid, name, true
}
