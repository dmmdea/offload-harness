package hwdetect

import (
	"errors"
	"reflect"
	"testing"
)

// DetectCoral (Coral design D4): ["coral-edgetpu"] iff the apex status node
// reads ALIVE. Everything else — absent driver, a read error, any other status —
// is "no accelerator", never a failure: a missing TPU is the normal case.
func TestDetectCoral(t *testing.T) {
	cases := []struct {
		name string
		body string
		err  error
		want []string
	}{
		{"alive", "ALIVE\n", nil, []string{"coral-edgetpu"}},
		{"alive without newline", "ALIVE", nil, []string{"coral-edgetpu"}},
		{"other status", "SLEEP\n", nil, nil},
		{"empty", "", nil, nil},
		{"read error (no driver, or Windows)", "", errors.New("open /sys/class/apex/apex_0/status: no such file or directory"), nil},
	}
	for _, c := range cases {
		got := DetectCoral(func(path string) (string, error) {
			if path != coralStatusPath {
				t.Fatalf("%s: read %q, want %q", c.name, path, coralStatusPath)
			}
			return c.body, c.err
		})
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// DetectAllAccelerators is the union of the probes IN ORDER — Hailo first, then
// Coral. The order is load-bearing (the shared-name rule gives a name to the
// first listed owner), so it is asserted, not just the set.
func TestDetectAllAcceleratorsUnionAndOrder(t *testing.T) {
	hailoRun := func(args ...string) (string, error) {
		if args[0] == "scan" {
			return "Device: 0001:01:00.0\n", nil
		}
		return "Device Architecture: HAILO8L\n", nil
	}
	noHailo := func(args ...string) (string, error) { return "", errors.New("hailortcli: not found") }
	alive := func(string) (string, error) { return "ALIVE", nil }
	dead := func(string) (string, error) { return "", errors.New("no apex") }

	if got := DetectAllAccelerators(hailoRun, alive); !reflect.DeepEqual(got, []string{"hailo-8l", "coral-edgetpu"}) {
		t.Errorf("both: got %v, want [hailo-8l coral-edgetpu] in that order", got)
	}
	if got := DetectAllAccelerators(noHailo, alive); !reflect.DeepEqual(got, []string{"coral-edgetpu"}) {
		t.Errorf("coral only: got %v", got)
	}
	if got := DetectAllAccelerators(hailoRun, dead); !reflect.DeepEqual(got, []string{"hailo-8l"}) {
		t.Errorf("hailo only: got %v", got)
	}
	if got := DetectAllAccelerators(noHailo, dead); got != nil {
		t.Errorf("neither: got %v, want nil", got)
	}
}
