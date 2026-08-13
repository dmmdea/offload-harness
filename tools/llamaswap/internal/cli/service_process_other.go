// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//go:build !windows

package cli

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// glueProbeProcess locates the llama-swap process on a POSIX host.
//
// `ps` is used rather than a /proc walk so this works on macOS and BSD as well
// as Linux. A process owned by another user is still listed, so the
// details_readable=false path is rarer here than on Windows — but it is kept,
// because a container or a hardened host can still hide it.
func glueProbeProcess(ctx context.Context) glueProcessInfo {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "ps", "-eo", "pid,lstart,comm")
	hideSpawnedWindow(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return glueProcessInfo{Note: "process probe failed: " + err.Error()}
	}
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		comm := fields[len(fields)-1]
		if !strings.Contains(comm, "llama-swap") {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		info := glueProcessInfo{Found: true, PID: pid}
		// lstart is the middle columns, e.g. "Wed Aug 13 00:27:19 2026".
		stamp := strings.Join(fields[1:len(fields)-1], " ")
		if t, terr := time.Parse("Mon Jan  2 15:04:05 2006", stamp); terr == nil {
			info.StartTime = t.Format(time.RFC3339)
			info.Uptime = time.Since(t).Round(time.Second).String()
			info.DetailsReadable = true
		} else {
			info.Note = "start time unparseable from ps output"
		}
		return info
	}
	return glueProcessInfo{Found: false, Note: "no llama-swap process in ps output"}
}
