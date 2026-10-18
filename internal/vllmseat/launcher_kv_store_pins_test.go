package vllmseat

import (
	"strings"
	"testing"
)

// TestLauncherKVNeedKnobIsWiredAheadOfTheExec pins the text of the B-54 fix on every platform (the run that proves its
// behaviour is Linux-only, launcher_kv_store_run_test.go): the knob exists and is validated, the context length the exec
// passes is a variable the pin block may set to -1 and not a literal, the pin's banner is still printed before the decision,
// and the decision sits between the pin and the exec.
func TestLauncherKVNeedKnobIsWiredAheadOfTheExec(t *testing.T) {
	s := readLauncher(t)
	start := strings.Index(s, "# KV pool PINNED FROM FREE MEMORY")
	banner := strings.Index(s, `echo "seat_fg: KV pool PINNED at ${pool_gib} GiB/worker`)
	execAt := strings.Index(s, `exec "$VENV/bin/vllm" serve`)
	if start < 0 || banner < start || execAt < banner {
		t.Fatalf("launcher lost a landmark: pin block %d, pin banner %d, exec %d", start, banner, execAt)
	}
	pin := s[start:execAt]
	if !strings.Contains(pin[banner-start:], "SEAT_KV_NEED_GIB") {
		t.Fatal("the pin block does not read SEAT_KV_NEED_GIB after the pin banner is printed")
	}
	for _, must := range []string{
		`MAXLEN="${SEAT_MAX_LEN:-131072}"`, // the configured length, with the default the exec used to carry
		`MAXLEN=-1`,                        // the one place the auto-fit is chosen
		`--max-model-len -1`,               // the line that says so
	} {
		if !strings.Contains(pin, must) {
			t.Errorf("the pin block lost %q", must)
		}
	}
	if !strings.Contains(s[execAt:], `--max-model-len "$MAXLEN"`) {
		t.Error(`the exec no longer passes --max-model-len "$MAXLEN"`)
	}
	if strings.Contains(s[execAt:], `--max-model-len "${SEAT_MAX_LEN`) {
		t.Error("the exec passes the configured length directly again: the auto-fit decision cannot reach it")
	}
}

// TestSeatEnvTemplateDocumentsTheKVPinKnobs: the template seat env carries the pin family as a documented, commented-out
// block (they are per-box measured figures, so no renderer emits them), and SEAT_KV_NEED_GIB says where its value comes from.
func TestSeatEnvTemplateDocumentsTheKVPinKnobs(t *testing.T) {
	env := readSeatScript(t, "windows-wsl/seat.env")
	for _, must := range []string{
		"\n#SEAT_KV_HEADROOM_GIB=",
		"\n#SEAT_KV_NEED_GIB=",
		"KV cache is needed",
	} {
		if !strings.Contains(env, must) {
			t.Errorf("windows-wsl/seat.env lost %q", strings.TrimSpace(must))
		}
	}
	// Commented out: a rendered seat must not pin a pool nobody measured.
	for _, l := range strings.Split(env, "\n") {
		if strings.HasPrefix(l, "SEAT_KV_") || strings.HasPrefix(l, "SEAT_NONKV_") {
			t.Errorf("the template seat env enables a KV pin knob: %q", l)
		}
	}
}

// TestLauncherStoreAddressChoiceIsBoundedAndScoped pins the text of the B-55 fix on every platform (the run that proves its
// behaviour is Linux-only, launcher_kv_store_run_test.go). The mDNS lookup is an embedded helper, never a file the installer
// must carry; it runs only for a cifs mount whose source is a .local name and whose source address is pinned; it sits between
// the source-address resolution and the ONE mount command; it is bounded by `timeout` (a seat start must never hang on a
// lookup); the query is the raw one the fix is for (QU bit, the mDNS group); the two test seams exist and say they are
// tests'; and the chosen address reaches the mount, the status file and the degrade messages.
func TestLauncherStoreAddressChoiceIsBoundedAndScoped(t *testing.T) {
	s := readLauncher(t)
	srcaddr := strings.Index(s, `case "${SEAT_L2_MOUNT_SRCADDR:-}" in`)
	pick := strings.Index(s, "l2_pick_address\n")
	mount := strings.Index(s, `mount -t "${SEAT_L2_MOUNT_TYPE:-cifs}"`)
	if srcaddr < 0 || pick < 0 || mount < 0 {
		t.Fatalf("launcher lost a landmark: source-address case %d, l2_pick_address call %d, mount %d", srcaddr, pick, mount)
	}
	if !(srcaddr < pick && pick < mount) {
		t.Errorf("the address choice must sit between the source-address resolution (%d) and the mount (%d); it is at %d", srcaddr, mount, pick)
	}
	if n := strings.Count(s, `mount -t "${SEAT_L2_MOUNT_TYPE:-cifs}"`); n != 1 {
		t.Errorf("the launcher must run ONE mount command with a variable source, found %d", n)
	}
	if !strings.Contains(s[mount:], `"$MOUNT_SRC" "$SEAT_L2_MOUNT_DIR" -o "$MOUNT_OPTS"`) {
		t.Error(`the mount no longer takes its source from "$MOUNT_SRC"`)
	}
	for _, must := range []string{
		`timeout -s KILL 3 "$py" - "$1" "$2" <<'PYEOF'`, // the hard bound: 3 s, then SIGKILL, around the embedded helper
		`timeout -s KILL 3 "$SEAT_FG_TEST_MDNS_CMD"`,    // and around the test seam, so a stand-in cannot hold a start either
		`224.0.0.251:5353`,                              // the mDNS group
		`0x8001`,                                        // class IN with the QU (unicast-response) bit
		`SEAT_FG_TEST_MDNS_DEST`,                        // the helper's destination, a test seam
		`*.local) ;;`,                                   // only .local names are looked up
		`"${SEAT_L2_MOUNT_TYPE:-cifs}" = cifs ]`,        // and only for a cifs mount
		`[ -n "$SRCADDR" ]`,                             // and only when a source address is pinned
		`${L2_ADDR:+ addr=$L2_ADDR}`,                    // the status file records the chosen address
	} {
		if !strings.Contains(s, must) {
			t.Errorf("seat_fg.sh lost %q", must)
		}
	}
	// The seams are for tests: the script says so where it reads them.
	for _, seam := range []string{"SEAT_FG_TEST_MDNS_CMD", "SEAT_FG_TEST_MDNS_DEST"} {
		i := strings.Index(s, seam)
		if i < 0 || !strings.Contains(s[max(0, i-400):i+len(seam)], "test") {
			t.Errorf("%s is not documented as a test-only seam where the launcher reads it", seam)
		}
	}
	// Nothing runs the helper unbounded: every python call in the lookup goes through the timeout line above.
	fnAt := strings.Index(s, "l2_mdns_answers() {")
	if fnAt < 0 {
		t.Fatal("seat_fg.sh has no l2_mdns_answers function")
	}
	fn := s[fnAt:]
	end := strings.Index(fn, "\nPYEOF\n")
	if end < 0 {
		t.Fatal("the embedded helper's here-doc is not closed by a PYEOF line")
	}
	fn = fn[:end]
	if strings.Count(fn, `"$py" -`) != 1 || !strings.Contains(fn, `timeout -s KILL 3 "$py" -`) {
		t.Error("the embedded helper must be started in exactly one place, under timeout -s KILL 3")
	}
	// A chosen address is said where the reader of a degrade needs it, and the messages keep the text the other test pins.
	for _, must := range []string{
		`degrade_l2 "share $SEAT_L2_MOUNT_SRC did not mount at $SEAT_L2_MOUNT_DIR: $why"`,
		`degrade_l2 "share writes at ~${mbps} MB/s, below SEAT_L2_MIN_MBPS=${MIN_MBPS}`,
		`${L2_ADDR:+`,
	} {
		if !strings.Contains(s, must) {
			t.Errorf("seat_fg.sh lost %q", must)
		}
	}
}
