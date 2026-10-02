package hwdetect

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"testing"
)

// The uevent bodies are the reference board's own (<node-d>, RK3588S). The vendor 6.1 BSP kernel
// binds the NPU as a DRM node — the display subsystem is card0 and the NPU card1 — so the driver name
// is on the DRM card's device. The mainline 7.0 kernel with the rknpu 0.9.8 DKMS module names it on the
// NPU's three core platform devices instead: the GPU (panthor) takes card1, and the NPU's DRM card2
// hangs off a virtual device whose uevent is EMPTY.
const (
	rknpuVendorUevent  = "DRIVER=RKNPU\nOF_NAME=npu\nOF_FULLNAME=/npu@fdab0000\nOF_COMPATIBLE_0=rockchip,rk3588-rknpu\nOF_COMPATIBLE_N=1\nMODALIAS=of:NnpuT(null)Crockchip,rk3588-rknpu\n"
	rknpuDisplayUevent = "DRIVER=rockchip-drm\nOF_NAME=display-subsystem\nOF_FULLNAME=/display-subsystem\nOF_COMPATIBLE_0=rockchip,display-subsystem\nOF_COMPATIBLE_N=1\nMODALIAS=of:Ndisplay-subsystemT(null)Crockchip,display-subsystem\n"
	rknpuPanthorUevent = "DRIVER=panthor\nOF_NAME=gpu\nOF_FULLNAME=/gpu@fb000000\nOF_COMPATIBLE_0=rockchip,rk3588-mali\nOF_COMPATIBLE_1=arm,mali-valhall-csf\nOF_COMPATIBLE_N=2\nMODALIAS=of:NgpuT(null)Crockchip,rk3588-maliCarm,mali-valhall-csf\n"
)

const (
	rknpuCard0 = "/sys/class/drm/card0/device/uevent"
	rknpuCard1 = "/sys/class/drm/card1/device/uevent"
	rknpuCard2 = "/sys/class/drm/card2/device/uevent"
)

// rknpuCoreAddr are the register addresses of the RK3588's three NPU cores; each is a platform device
// named <address>.npu.
var rknpuCoreAddr = [3]string{"fdab0000", "fdac0000", "fdad0000"}

// rknpuCore is the sysfs path of NPU core n's platform-device uevent.
func rknpuCore(n int) string {
	return "/sys/bus/platform/devices/" + rknpuCoreAddr[n] + ".npu/uevent"
}

// coreUevent is the uevent of NPU core n on the mainline kernel, bound to driver. The RKNPU body is the
// reference board's, read from fdab0000.npu; the other cores' files differ only in the address. The
// rocket body is the same file with the in-tree driver's name — the board blacklists that module, so it
// was not read there.
func coreUevent(driver string, n int) string {
	return fmt.Sprintf("DRIVER=%s\nOF_NAME=npu\nOF_FULLNAME=/npu@%s\nOF_COMPATIBLE_0=rockchip,rk3588-rknn-core\nOF_COMPATIBLE_N=1\nMODALIAS=of:NnpuT(null)Crockchip,rk3588-rknn-core\n", driver, rknpuCoreAddr[n])
}

// mainlineBoard is the reference board on the mainline kernel with every NPU core bound to driver.
func mainlineBoard(driver string) map[string]string {
	files := map[string]string{rknpuCard0: rknpuDisplayUevent, rknpuCard1: rknpuPanthorUevent, rknpuCard2: ""}
	for n := range rknpuCoreAddr {
		files[rknpuCore(n)] = coreUevent(driver, n)
	}
	return files
}

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

// DetectRknpu reports ["rknpu"] iff a candidate's uevent carries the whole line DRIVER=RKNPU — the
// platform driver's name, on a DRM card's device (vendor kernel) or on an NPU core's platform device
// (mainline kernel with the DKMS driver). Everything else is "no accelerator", never a failure: a
// missing NPU is the normal case.
func TestDetectRknpu(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"vendor kernel: the NPU is the DRM card's device (card1 beside the display card)",
			map[string]string{rknpuCard0: rknpuDisplayUevent, rknpuCard1: rknpuVendorUevent}, []string{"rknpu"}},
		{"vendor kernel, NPU on card0", map[string]string{rknpuCard0: rknpuVendorUevent}, []string{"rknpu"}},
		{"vendor kernel, NPU on the last card candidate", map[string]string{"/sys/class/drm/card3/device/uevent": rknpuVendorUevent}, []string{"rknpu"}},

		{"mainline kernel + rknpu DKMS: the driver is on the core platform devices, card2's uevent is empty (the reference board today)",
			mainlineBoard("RKNPU"), []string{"rknpu"}},
		{"mainline + DKMS, only core 0 bound", map[string]string{rknpuCard2: "", rknpuCore(0): coreUevent("RKNPU", 0)}, []string{"rknpu"}},
		{"mainline + DKMS, only core 2 bound", map[string]string{rknpuCard2: "", rknpuCore(2): coreUevent("RKNPU", 2)}, []string{"rknpu"}},

		{"mainline + the in-tree rocket driver on every core is not the RKNN runtime's driver", mainlineBoard("rocket"), nil},
		{"mainline board whose NPU driver is not loaded: GPU, display and an empty DRM device only",
			map[string]string{rknpuCard0: rknpuDisplayUevent, rknpuCard1: rknpuPanthorUevent, rknpuCard2: ""}, nil},
		{"the NPU's DRM card alone: an empty uevent proves nothing", map[string]string{rknpuCard2: ""}, nil},
		{"only the display card", map[string]string{rknpuCard0: rknpuDisplayUevent}, nil},

		{"the driver token is case-sensitive", map[string]string{rknpuCard1: "DRIVER=rknpu\n"}, nil},
		{"a longer driver name is a different driver", map[string]string{rknpuCard1: "DRIVER=RKNPU2\n"}, nil},
		{"the token must be a whole line", map[string]string{rknpuCard1: "XDRIVER=RKNPU\nOF_NAME=DRIVER=RKNPU\n"}, nil},
		{"the token must be a whole line on a core device too", map[string]string{rknpuCore(0): "XDRIVER=RKNPU\n"}, nil},
		{"the compatible string without a bound driver", map[string]string{rknpuCard1: "OF_NAME=npu\nOF_COMPATIBLE_0=rockchip,rk3588-rknpu\n"}, nil},
		{"the compatible string of a core device without a bound driver", map[string]string{rknpuCore(0): "OF_NAME=npu\nOF_COMPATIBLE_0=rockchip,rk3588-rknn-core\n"}, nil},
		{"empty uevent", map[string]string{rknpuCard1: ""}, nil},
		{"no NPU nodes readable (no driver, or not Linux)", nil, nil},
	}
	for _, c := range cases {
		if got := DetectRknpu(filesystem(c.files, nil)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// The probe reads only the seven fixed candidates — card0..card3's device/uevent (vendor kernel) and the
// three NPU cores' platform uevent (mainline) — so a box is judged by the same handful of files
// everywhere, and an unreadable one is skipped, not fatal.
func TestDetectRknpuReadsOnlyTheFixedCandidates(t *testing.T) {
	var reads []string
	if got := DetectRknpu(filesystem(nil, &reads)); got != nil {
		t.Fatalf("got %v on a box with no NPU nodes", got)
	}
	if len(reads) != 7 {
		t.Fatalf("read %v, want the four DRM card uevent files and the three NPU core uevent files", reads)
	}
	shape := regexp.MustCompile(`^/sys/(class/drm/card[0-3]/device|bus/platform/devices/fd(ab|ac|ad)0000\.npu)/uevent$`)
	seen := map[string]bool{}
	for _, p := range reads {
		if !shape.MatchString(p) || seen[p] {
			t.Errorf("read %q: not one of the distinct fixed candidates", p)
		}
		seen[p] = true
	}
	// An unreadable candidate before the NPU does not hide it, on either layout.
	for name, files := range map[string]map[string]string{
		"vendor (card2)":    {"/sys/class/drm/card2/device/uevent": rknpuVendorUevent},
		"mainline (core 2)": {rknpuCore(2): coreUevent("RKNPU", 2)},
	} {
		if got := DetectRknpu(filesystem(files, nil)); !reflect.DeepEqual(got, []string{"rknpu"}) {
			t.Errorf("%s: unreadable earlier candidates hid the NPU: got %v", name, got)
		}
	}
}

// DetectAllAccelerators lists the RKNPU AFTER the Hailo and the Coral, on either kernel layout. The order
// is load-bearing (the shared-name rule gives a capability name to the first listed owner), so it is
// asserted, not just the set. The RKNPU probe must also not fire on the bare "ALIVE" the Coral test stubs
// every path with: a read that succeeds proves nothing.
func TestDetectAllAcceleratorsListsRknpuLast(t *testing.T) {
	hailoRun := func(args ...string) (string, error) {
		if args[0] == "scan" {
			return "Device: 0001:01:00.0\n", nil
		}
		return "Device Architecture: HAILO8L\n", nil
	}
	noHailo := func(args ...string) (string, error) { return "", errors.New("hailortcli: not found") }
	layouts := map[string]map[string]string{
		"vendor":   {rknpuCard1: rknpuVendorUevent},
		"mainline": mainlineBoard("RKNPU"),
	}
	for name, npu := range layouts {
		withCoral := map[string]string{coralStatusPath: "ALIVE\n"}
		for k, v := range npu {
			withCoral[k] = v
		}
		if got := DetectAllAccelerators(hailoRun, filesystem(withCoral, nil)); !reflect.DeepEqual(got, []string{"hailo-8l", "coral-edgetpu", "rknpu"}) {
			t.Errorf("%s, all three: got %v, want [hailo-8l coral-edgetpu rknpu] in that order", name, got)
		}
		if got := DetectAllAccelerators(noHailo, filesystem(withCoral, nil)); !reflect.DeepEqual(got, []string{"coral-edgetpu", "rknpu"}) {
			t.Errorf("%s, coral + rknpu: got %v", name, got)
		}
		if got := DetectAllAccelerators(noHailo, filesystem(npu, nil)); !reflect.DeepEqual(got, []string{"rknpu"}) {
			t.Errorf("%s, rknpu only: got %v", name, got)
		}
	}
	if got := DetectAllAccelerators(noHailo, filesystem(map[string]string{coralStatusPath: "ALIVE\n"}, nil)); !reflect.DeepEqual(got, []string{"coral-edgetpu"}) {
		t.Errorf("coral only: got %v", got)
	}
	alive := func(string) (string, error) { return "ALIVE\n", nil }
	if got := DetectRknpu(alive); got != nil {
		t.Errorf("the bare ALIVE stub read as an RKNPU: %v", got)
	}
}
