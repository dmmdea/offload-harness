package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLauncherPinsTheCacheServerMountSource pins the source-address fix: on a box with two NICs on one
// subnet the CIFS mount was measured leaving from the Wi-Fi address and the store's allow-list refused it.
// SEAT_L2_MOUNT_SRCADDR ("auto" or an address) must be resolved BEFORE the mount and reach the mount's -o.
func TestLauncherPinsTheCacheServerMountSource(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	resolve := strings.Index(s, `case "${SEAT_L2_MOUNT_SRCADDR:-}" in`)
	mount := strings.Index(s, `mount -t "${SEAT_L2_MOUNT_TYPE:-cifs}"`)
	if resolve < 0 || mount < 0 {
		t.Fatalf("launcher lost a landmark: srcaddr=%d mount=%d", resolve, mount)
	}
	if resolve > mount {
		t.Fatal("SEAT_L2_MOUNT_SRCADDR must be resolved before the mount runs")
	}
	if !strings.Contains(s, `MOUNT_OPTS="${MOUNT_OPTS:+$MOUNT_OPTS,}srcaddr=$SRCADDR"`) {
		t.Fatal("the resolved source address is no longer appended as srcaddr=")
	}
	if !strings.Contains(s[mount:], `-o "$MOUNT_OPTS"`) {
		t.Fatal("the mount no longer uses the pinned options ($MOUNT_OPTS)")
	}

	envRaw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "windows-wsl", "seat.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ReplaceAll(string(envRaw), "\r\n", "\n"), "\nSEAT_L2_MOUNT_SRCADDR=auto\n") {
		t.Fatal("the rendered seat env no longer defaults SEAT_L2_MOUNT_SRCADDR to auto")
	}
}
