package vllmseat

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSeatLauncherDegradesInsteadOfRefusingOnACacheServerFault pins the policy
// the 2026-09-09 outage forced: a cache server that will not mount, cannot be
// probed, or writes under SEAT_L2_MIN_MBPS is a reason to serve the SAME-BOX
// tier (L1 only) loudly — never a reason to refuse the seat. That morning the
// share crawled at ~36 MB/s behind a degraded WSL datapath, the launcher
// refused every start, llama-swap turned each refusal into HTTP 500, and every
// session on the box concluded "delegation is not viable" and did the work in
// the cloud context. The floor still gates L2 (a slow share is never used);
// only the seat's availability stops depending on it.
//
// The launcher is shell rendered verbatim, so this pins its text: the three
// cache-server faults route through degrade_l2, the only remaining refusal is
// the port-already-bound case, and the degraded state is written for readback.
func TestSeatLauncherDegradesInsteadOfRefusingOnACacheServerFault(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, must := range []string{
		"degrade_l2() {",
		`degrade_l2 "share $SEAT_L2_MOUNT_SRC did not mount`,
		`degrade_l2 "share writes at ~${mbps} MB/s, below SEAT_L2_MIN_MBPS=${MIN_MBPS}`,
		`degrade_l2 "cannot write a 64 MiB probe`,
		"CACHE SERVER DEGRADED",
		"seat-l2.status",
	} {
		if !strings.Contains(s, must) {
			t.Errorf("seat_fg.sh lost the degrade path: %q missing", must)
		}
	}
	// Every REFUSING line left must be the port-bound refusal — a cache-server
	// fault may never again end the start.
	for _, line := range regexp.MustCompile(`(?m)^.*REFUSING to start.*$`).FindAllString(s, -1) {
		if !strings.Contains(line, "already bound") {
			t.Errorf("a cache-server fault still refuses the seat: %s", strings.TrimSpace(line))
		}
	}
	// And the degrade helper empties L2 so the MP server never registers a
	// store it cannot reach (an unmounted base_path is a local directory the
	// adapter would happily write into while the tier holds nothing).
	helper := s[strings.Index(s, "degrade_l2() {"):]
	helper = helper[:strings.Index(helper, "\n}")]
	if !strings.Contains(helper, `L2=""`) {
		t.Errorf("degrade_l2 must clear L2; got:\n%s", helper)
	}
}
