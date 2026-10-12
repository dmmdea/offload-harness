//go:build linux

package proctree

import (
	"os"
	"strconv"
	"strings"
)

// tableMembers reads /proc: every process's parent and group from its stat line, and selects the
// members of the tree (membersByTable). A process that exits mid-walk is simply absent.
func tableMembers(pgid, root int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	ppid := map[int]int{}
	group := map[int]int{}
	for _, e := range entries {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil {
			continue
		}
		parent, g, ok := statParentGroup(pid)
		if !ok {
			continue
		}
		ppid[pid], group[pid] = parent, g
	}
	return membersByTable(pgid, root, ppid, group), nil
}

// statParentGroup parses the ppid and pgrp out of /proc/<pid>/stat. The command name is
// parenthesised and may itself contain spaces and parentheses, so the fields are counted from the
// last closing parenthesis: after it come the state, the ppid and the pgrp.
func statParentGroup(pid int) (ppid, pgrp int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 3 {
		return 0, 0, false
	}
	pp, err1 := strconv.Atoi(f[1])
	pg, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return pp, pg, true
}
