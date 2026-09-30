package hwdetect

import (
	"errors"
	"reflect"
	"regexp"
	"testing"
)

// The uevent bodies are the reference board's own (an Orange Pi 5, RK3588S, vendor 6.1 kernel): the
// display subsystem is card0 and the NPU, bound by the vendor "RKNPU" platform driver, is card1.
const (
	rknpuVendorUevent  = "DRIVER=RKNPU\nOF_NAME=npu\nOF_FULLNAME=/npu@fdab0000\nOF_COMPATIBLE_0=rockchip,rk3588-rknpu\nOF_COMPATIBLE_N=1\nMODALIAS=of:NnpuT(null)Crockchip,rk3588-rknpu\n"
	rknpuDisplayUevent = "DRIVER=rockchip-drm\nOF_NAME=display-subsystem\nOF_FULLNAME=/display-subsystem\nOF_COMPATIBLE_0=rockchip,display-subsystem\nOF_COMPATIBLE_N=1\nMODALIAS=of:Ndisplay-subsystemT(null)Crockchip,display-subsystem\n"
	// The mainline in-tree driver for the same silicon: a different driver, which the RKNN runtime
	// does not use.
	rknpuRocketUevent = "DRIVER=rocket\nOF_NAME=npu\nOF_COMPATIBLE_0=rockchip,rk3588-rknn-core\nOF_COMPATIBLE_N=1\n"
)

const rknpuCard1 = "/sys/class/drm/card1/device/uevent"

// filesystem stands in for sysfs: a path it holds reads back, any other is "no such file".
func filesystem(files map[string]string, reads *[]string) func(string) (string, error) {
	return func(path string) (string, error) {
		if reads != nil {
			*reads = append(*reads, path)
		}
		if body, ok := files[path]; ok {
			return body, nil
		}
		return "", errors.New("open " + path + ": no such file or directory")
	}
}

// DetectRknpu reports ["rknpu"] iff a DRM card's uevent carries the whole line DRIVER=RKNPU — the
// vendor driver's platform-driver name. Everything else is "no accelerator", never a failure: a
// missing NPU is the normal case.
func TestDetectRknpu(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"vendor driver on card1 beside the display card (the reference board)",
			map[string]string{"/sys/class/drm/card0/device/uevent": rknpuDisplayUevent, rknpuCard1: rknpuVendorUevent}, []string{"rknpu"}},
		{"vendor driver on card0", map[string]string{"/sys/class/drm/card0/device/uevent": rknpuVendorUevent}, []string{"rknpu"}},
		{"vendor driver on the last candidate", map[string]string{"/sys/class/drm/card3/device/uevent": rknpuVendorUevent}, []string{"rknpu"}},
		{"in-tree rocket driver is not the RKNN runtime's driver", map[string]string{rknpuCard1: rknpuRocketUevent}, nil},
		{"only the display card", map[string]string{"/sys/class/drm/card0/device/uevent": rknpuDisplayUevent}, nil},
		{"the driver token is case-sensitive", map[string]string{rknpuCard1: "DRIVER=rknpu\n"}, nil},
		{"a longer driver name is a different driver", map[string]string{rknpuCard1: "DRIVER=RKNPU2\n"}, nil},
		{"the token must be a whole line", map[string]string{rknpuCard1: "XDRIVER=RKNPU\nOF_NAME=DRIVER=RKNPU\n"}, nil},
		{"the compatible string without a bound driver", map[string]string{rknpuCard1: "OF_NAME=npu\nOF_COMPATIBLE_0=rockchip,rk3588-rknpu\n"}, nil},
		{"empty uevent", map[string]string{rknpuCard1: ""}, nil},
		{"no DRM nodes readable (no driver, or not Linux)", nil, nil},
	}
	for _, c := range cases {
		if got := DetectRknpu(filesystem(c.files, nil)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// The probe reads only the four fixed candidates — card0..card3's device/uevent — so a box is judged
// by the same handful of files everywhere, and an unreadable one is skipped, not fatal.
func TestDetectRknpuReadsOnlyTheFixedCandidates(t *testing.T) {
	var reads []string
	if got := DetectRknpu(filesystem(nil, &reads)); got != nil {
		t.Fatalf("got %v on a box with no DRM nodes", got)
	}
	if len(reads) != 4 {
		t.Fatalf("read %v, want the four card0..card3 uevent files", reads)
	}
	shape := regexp.MustCompile(`^/sys/class/drm/card[0-3]/device/uevent$`)
	seen := map[string]bool{}
	for _, p := range reads {
		if !shape.MatchString(p) || seen[p] {
			t.Errorf("read %q: not one of the distinct fixed candidates", p)
		}
		seen[p] = true
	}
	// An unreadable card before the NPU does not hide it.
	files := map[string]string{"/sys/class/drm/card2/device/uevent": rknpuVendorUevent}
	if got := DetectRknpu(filesystem(files, nil)); !reflect.DeepEqual(got, []string{"rknpu"}) {
		t.Errorf("unreadable card0/card1 hid the NPU on card2: got %v", got)
	}
}

// DetectAllAccelerators lists the RKNPU AFTER the Hailo and the Coral. The order is load-bearing (the
// shared-name rule gives a capability name to the first listed owner), so it is asserted, not just the
// set — and the RKNPU probe must not fire on the bare "ALIVE" the Coral test stubs every path with.
func TestDetectAllAcceleratorsListsRknpuLast(t *testing.T) {
	hailoRun := func(args ...string) (string, error) {
		if args[0] == "scan" {
			return "Device: 0001:01:00.0\n", nil
		}
		return "Device Architecture: HAILO8L\n", nil
	}
	noHailo := func(args ...string) (string, error) { return "", errors.New("hailortcli: not found") }
	all := map[string]string{coralStatusPath: "ALIVE\n", rknpuCard1: rknpuVendorUevent}

	if got := DetectAllAccelerators(hailoRun, filesystem(all, nil)); !reflect.DeepEqual(got, []string{"hailo-8l", "coral-edgetpu", "rknpu"}) {
		t.Errorf("all three: got %v, want [hailo-8l coral-edgetpu rknpu] in that order", got)
	}
	if got := DetectAllAccelerators(noHailo, filesystem(all, nil)); !reflect.DeepEqual(got, []string{"coral-edgetpu", "rknpu"}) {
		t.Errorf("coral + rknpu: got %v", got)
	}
	if got := DetectAllAccelerators(noHailo, filesystem(map[string]string{rknpuCard1: rknpuVendorUevent}, nil)); !reflect.DeepEqual(got, []string{"rknpu"}) {
		t.Errorf("rknpu only: got %v", got)
	}
	if got := DetectAllAccelerators(noHailo, filesystem(map[string]string{coralStatusPath: "ALIVE\n"}, nil)); !reflect.DeepEqual(got, []string{"coral-edgetpu"}) {
		t.Errorf("coral only: got %v", got)
	}
}
