package gpuactivity

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Register C-60 (0.143.0): the local seat gate counts a run still in
// admission as a WAITER (FIFO), and anything past admission as a slot
// holder. A door that registers its run without the admission phase
// defaults to "running" and counts as a holder while it waits — the livelock
// that refused 92 local runs on 2026-09-29 then survives for that door (the
// MCP agent_run door did exactly that until this release). Every
// registration site in the binary must say PhaseAdmission.
func TestEveryRunRegistersInAdmission(t *testing.T) {
	root := filepath.Join("..", "..")
	start := regexp.MustCompile(`gpuactivity\.Start\(`)
	var sites, bad []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "tools":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !start.MatchString(line) {
				continue
			}
			where := path + ":" + itoaLine(i+1)
			sites = append(sites, where)
			if !strings.Contains(line, "Phase: gpuactivity.PhaseAdmission") {
				bad = append(bad, where)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) == 0 {
		t.Fatal("found no gpuactivity.Start call at all: the scan is broken, not the code")
	}
	if len(bad) > 0 {
		t.Fatalf("these runs register without Phase: gpuactivity.PhaseAdmission and would count as slot holders while they wait: %v", bad)
	}
}

func itoaLine(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
