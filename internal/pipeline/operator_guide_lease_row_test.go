package pipeline

// The OPERATOR-GUIDE's "engine + lease" row (register C-33) said "a 5,400 s video run outlives the default lease; size the
// lease --for window to the job", put the GPU lease TTL at 3,600 s "against" the media timeouts, and quoted one box's
// deployed values (600 / 5,400 / 600,000) as if they were the defaults. The code does something else:
//
//   - a media call stamps ITS OWN timeout as the lease window (acquireMediaLease takes the same duration the render runs
//     under), and the render is killed at that same value, so a render cannot outlive the lease the pipeline took for it;
//   - gpulease.DefaultTTL (3,600 s) is only the fallback for a lease acquired with no window, which no media call is;
//   - gpu_wait_ms is the QUEUE wait (how long a call waits behind a holder before it defers), not a lease length;
//   - the one place a window can be shorter than the job is a `gpu reserve` wrapper, whose --for term a call running
//     inside it inherits.
//
// These tests read the guide's row and refuse the retired claims, ask for the corrected sentences, and tie every figure the
// row quotes to the code (config.Default and gpulease.DefaultTTL), so the row cannot drift back to one machine's config.
// The lease windows themselves are pinned in mediaadmit_lease_window_test.go.

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const operatorGuidePath = "../../docs/OPERATOR-GUIDE.md"

// guideProse reads the guide and flattens it the way a quote wraps across lines: typographic apostrophes straightened,
// whitespace collapsed.
func guideProse(t *testing.T, raw string) string {
	t.Helper()
	return strings.Join(strings.Fields(strings.NewReplacer("’", "'", "‘", "'").Replace(raw)), " ")
}

// leaseRowOf returns the guide's "engine + lease" table row, flattened, and the whole guide flattened. The row is one table
// line; finding it is the blind-gate control, so a renamed or split row fails here instead of passing every check below.
func leaseRowOf(t *testing.T) (row, whole string) {
	t.Helper()
	raw, err := os.ReadFile(operatorGuidePath)
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "| engine + lease |") {
			rows = append(rows, line)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("%s has %d rows that start \"| engine + lease |\", want exactly 1: this check is blind", operatorGuidePath, len(rows))
	}
	return guideProse(t, rows[0]), guideProse(t, string(raw))
}

// commas writes n the way the guide writes a figure: 90,000.
func commas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func TestOperatorGuideLeaseRowStatesTheMediaLeaseIsTheTimeout(t *testing.T) {
	row, whole := leaseRowOf(t)

	for _, retired := range []string{
		"outlives the default lease",               // a media call's lease window IS its timeout: the render is killed at the same value
		"size the lease `--for` window to the job", // the pipeline sizes its own lease; only a gpu reserve wrapper has a --for to size
		"lease TTL (3,600 s default)",              // 3,600 s is the fallback for a lease with no window, not the lease a media call takes
		"`gpu_wait_ms` 600,000",                    // one box's deployed value, and the key is a queue wait, not a lease length
	} {
		if strings.Contains(row, retired) {
			t.Errorf("the engine + lease row still says %q", retired)
		}
	}
	for _, want := range []struct{ phrase, why string }{
		{"the call's own timeout", "a media call's lease window is its own timeout"},
		{"queue wait", "gpu_wait_ms is the queue wait, not a lease length"},
		{"fallback", "the 3,600 s DefaultTTL is only the fallback for a lease with no window"},
		{"`gpu reserve`", "a reserve wrapper is the one place a window can be shorter than the job"},
	} {
		if !strings.Contains(row, want.phrase) {
			t.Errorf("the engine + lease row does not say %q (%s)", want.phrase, want.why)
		}
	}

	// The doctor finding is a fact about a value in a config file, not about the machine the guide was written on.
	const control = "`gpu_wait_ms` more than 3x `vision_gpu_wait_sec`"
	if !strings.Contains(whole, control) {
		t.Fatalf("the guide no longer names the gpu_wait_ms doctor finding (%q): this check is blind", control)
	}
	if strings.Contains(whole, "the deployed 600,000 ms") {
		t.Error(`the doctor paragraph still says "the deployed 600,000 ms", which reads as the current default; it is "a deployed" value`)
	}
	if !strings.Contains(whole, "a deployed 600,000 ms") {
		t.Error(`the doctor paragraph does not say "a deployed 600,000 ms"`)
	}
}

func TestOperatorGuideQuotesCodeDefaultsForMediaTimeouts(t *testing.T) {
	row, _ := leaseRowOf(t)
	d := config.Default()

	// Every figure the row gives for these keys is the code default, and the row gives each at least once.
	for _, k := range []struct {
		key  string
		want int
	}{
		{"imagegen_timeout_sec", d.ImageGenTimeoutSec},
		{"videogen_timeout_sec", d.VideoGenTimeoutSec},
		{"gpu_wait_ms", d.GPUWaitMs},
	} {
		re := regexp.MustCompile("`" + k.key + "` ([0-9]+(?:,[0-9]{3})*)")
		found := re.FindAllStringSubmatch(row, -1)
		if len(found) == 0 {
			t.Errorf("the engine + lease row quotes no figure for `%s` (the code default is %s)", k.key, commas(k.want))
		}
		for _, m := range found {
			if m[1] != commas(k.want) {
				t.Errorf("the engine + lease row quotes `%s` %s; the code default is %s (a deployed or seeded value is not the default)", k.key, m[1], commas(k.want))
			}
		}
	}

	// The lease library's own fallback window.
	fallback := fmt.Sprintf("%s s", commas(int(gpulease.DefaultTTL/time.Second)))
	if !strings.Contains(row, fallback) {
		t.Errorf("the engine + lease row does not quote the lease library's fallback window (gpulease.DefaultTTL, %s)", fallback)
	}
}
