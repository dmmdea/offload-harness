//go:build !windows

package volumes

import (
	"bufio"
	"os"
	"strings"
	"syscall"
)

// pseudoFS never holds an install: kernel and container plumbing, snap loopbacks,
// and RAM-backed mounts. Enumerating them would bury the two or three real
// filesystems in forty rows of noise (a stock Ubuntu box has ~30 of these).
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true,
	"cgroup": true, "cgroup2": true, "securityfs": true, "pstore": true, "efivarfs": true,
	"bpf": true, "debugfs": true, "tracefs": true, "configfs": true, "fusectl": true,
	"hugetlbfs": true, "mqueue": true, "ramfs": true, "squashfs": true, "autofs": true,
	"binfmt_misc": true, "rpc_pipefs": true, "nsfs": true, "overlay": true, "fuse.gvfsd-fuse": true,
	"fuse.portal": true, "cifs": false, "nfs": false, // network types are kept, then flagged
}

// networkFS marks mounts that can disappear with the network.
var networkFS = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smbfs": true, "smb3": true,
	"fuse.sshfs": true, "afs": true, "9p": true,
}

// List reads /proc/mounts and stats each real filesystem. Falls back to "/" alone
// when /proc is unavailable (a container, a BSD), which still gives Pick something
// truthful to work with rather than an empty list.
func List() ([]Volume, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		v, serr := statVolume("/", "", true)
		if serr != nil {
			return nil, serr
		}
		return []Volume{v}, nil
	}
	defer f.Close()

	seen := map[string]bool{} // dedupe bind mounts of one filesystem
	var out []Volume
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		device, mount, fsType := fields[0], unescapeMount(fields[1]), fields[2]
		if v, ok := pseudoFS[fsType]; ok && v {
			continue
		}
		if !strings.HasPrefix(device, "/") && !networkFS[fsType] && fsType != "zfs" && fsType != "btrfs" {
			continue // not a block device, not a filesystem we can install onto
		}
		vol, serr := statVolume(mount, fsType, mount == "/")
		if serr != nil {
			continue // a mount we cannot stat is not a candidate
		}
		vol.Network = networkFS[fsType]
		// One entry per (device, capacity): ZFS datasets share a pool but report
		// their own quota/avail, so they are genuinely distinct targets; identical
		// bind mounts are not.
		key := device + "|" + mount
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, vol)
	}
	return out, sc.Err()
}

// statVolume fills a Volume from statfs. FreeBytes uses the UNPRIVILEGED figure
// (Bavail), not Bfree: the reserved blocks a root-only pool keeps back are not
// space an install can use, and counting them has shipped "plenty of room"
// verdicts onto disks that then filled.
func statVolume(mount, fsType string, isOS bool) (Volume, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mount, &st); err != nil {
		return Volume{}, err
	}
	bs := uint64(st.Bsize)
	return Volume{
		Root:       mount,
		FS:         fsType,
		TotalBytes: st.Blocks * bs,
		FreeBytes:  st.Bavail * bs,
		IsOS:       isOS,
	}, nil
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces and tabs.
func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}
