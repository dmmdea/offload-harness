//go:build windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// shortName is the Windows 8.3 alias of p's last element ("" when the volume makes
// none).
func shortName(t *testing.T, p string) string {
	t.Helper()
	long, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, 1024)
	n, err := syscall.GetShortPathName(long, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		return ""
	}
	s := filepath.Base(syscall.UTF16ToString(buf[:n]))
	if strings.EqualFold(s, filepath.Base(p)) {
		return ""
	}
	return s
}

// TestShortNamesDoNotBypassTheFloor (SF-07 review B1): an 8.3 alias is another name
// for the same secret file or credential directory, and enforce refuses it for
// read_file and search_files alike.
func TestShortNamesDoNotBypassTheFloor(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env.production")
	if err := os.WriteFile(envFile, []byte("TOKEN=abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte("TOKEN=sshcfg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shortEnv, shortSSH := shortName(t, envFile), shortName(t, sshDir)
	if shortEnv == "" || shortSSH == "" {
		t.Skip("this volume generates no 8.3 short names")
	}
	res, _ := buildForFloor(t, dir, "enforce", "")
	read := floorTool(t, res.Tools, "read_file")
	for _, p := range []string{shortEnv, shortSSH + "/config"} {
		if out, err := read.Exec(context.Background(), `{"path":"`+p+`"}`); err == nil || strings.Contains(out, "TOKEN=") {
			t.Errorf("read_file %s (short name) returned %q, %v; want a refusal", p, out, err)
		}
	}
	out, err := floorTool(t, res.Tools, "search_files").Exec(context.Background(), `{"pattern":"TOKEN","path":"`+shortSSH+`"}`)
	if err == nil || strings.Contains(out, "sshcfg") {
		t.Errorf("search_files under %s (short name) returned %q, %v; want a refusal", shortSSH, out, err)
	}

	// Under warn nothing is refused, so only the resolved name can tell the floor
	// that the short name is .env.production: the read must leave a warn row.
	wres, audit := buildForFloor(t, dir, "warn", "")
	if _, err := floorTool(t, wres.Tools, "read_file").Exec(context.Background(), `{"path":"`+shortEnv+`"}`); err != nil {
		t.Fatal(err)
	}
	if rows := readRows(t, audit); len(rows) != 1 || rows[0].Decision != "warn" {
		t.Errorf("a warn-mode read through the short name %s left rows %+v, want one warn row", shortEnv, rows)
	}
}
