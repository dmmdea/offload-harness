package gpuactivity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// A sample whose full query failed once for a reason that does not name display_attached is answered from the query
// without it, and cannot say which card the monitor is on. The card table (gpuprobe.Read) marks every device of such a
// reading AttachedUnknown; this sample feeds offload_status's per-card table, so it carries the same mark, or the
// brief would show no display card at all on that reading and read as "nothing to judge".

const (
	smiTwoCardsFallback = "0, GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee, NVIDIA GeForce RTX 5060 Ti, 0, 100, 16311, Disabled\r\n" +
		"1, GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff, NVIDIA GeForce RTX 5070 Ti, 0, 900, 16303, Enabled\r\n"
	smiTwoCardsFull = "0, GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee, NVIDIA GeForce RTX 5060 Ti, 0, 100, 16311, Disabled, No\r\n" +
		"1, GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff, NVIDIA GeForce RTX 5070 Ti, 0, 900, 16303, Disabled, Yes\r\n"
)

func sampleWith(t *testing.T, run func(withAttached bool) (string, error)) []GPU {
	t.Helper()
	gpuprobe.ResetDisplayAwareState()
	t.Cleanup(gpuprobe.ResetDisplayAwareState)
	oldRun := smiRun
	t.Cleanup(func() { smiRun = oldRun })
	smiRun = func(_ context.Context, args ...string) (string, error) {
		return run(strings.Contains(strings.Join(args, " "), "display_attached"))
	}
	gpus, err := SampleGPUs(context.Background())
	if err != nil {
		t.Fatalf("SampleGPUs: %v", err)
	}
	return gpus
}

func TestSampleGPUsMarksEveryCardWhenTheReadingCouldNotSayWhichCardTheMonitorIsOn(t *testing.T) {
	transient := sampleWith(t, func(with bool) (string, error) {
		if with {
			return "", errors.New("exit status 9: nvidia-smi was busy") // does not name the field
		}
		return smiTwoCardsFallback, nil
	})
	if len(transient) != 2 {
		t.Fatalf("the fallback's two cards: %+v", transient)
	}
	for _, g := range transient {
		if !g.AttachedUnknown {
			t.Errorf("card %d: a reading from the fallback after an unexplained failure must say it cannot tell the monitor's card", g.Index)
		}
	}

	// The positive controls: a healthy driver, and a driver that refuses the field (it never had it), are NOT unknown.
	healthy := sampleWith(t, func(with bool) (string, error) {
		if !with {
			t.Error("a healthy driver answers the full query; the fallback must not run")
		}
		return smiTwoCardsFull, nil
	})
	for _, g := range healthy {
		if g.AttachedUnknown {
			t.Errorf("card %d: a full reading is not unknown", g.Index)
		}
	}
	if len(healthy) != 2 || !healthy[1].DisplayAttached {
		t.Fatalf("control: the full reading marks card 1 as attached: %+v", healthy)
	}
	refused := sampleWith(t, func(with bool) (string, error) {
		if with {
			return "", errors.New(`exit status 2: Field "display_attached" is not a valid field to query.`)
		}
		return smiTwoCardsFallback, nil
	})
	for _, g := range refused {
		if g.AttachedUnknown {
			t.Errorf("card %d: a driver that does not know the field rests the rule on display_active alone; that is not unknown", g.Index)
		}
	}
}
