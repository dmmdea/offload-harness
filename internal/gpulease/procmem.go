package gpulease

import (
	"strconv"
	"strings"
)

// parseStatusPrivate reads RssAnon and VmSwap (kB) out of /proc/<pid>/status text and returns
// their sum in bytes. RssAnon is missing before Linux 4.5, which leaves the process unreadable
// rather than guessing from VmRSS (which counts shared file pages that are not this process's
// own commit). Platform-neutral so the parser is tested on every host.
func parseStatusPrivate(text string) (uint64, bool) {
	var anon, swap uint64
	haveAnon := false
	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "RssAnon":
			anon, haveAnon = kb, true
		case "VmSwap":
			swap = kb
		}
	}
	if !haveAnon {
		return 0, false
	}
	return (anon + swap) * 1024, true
}
