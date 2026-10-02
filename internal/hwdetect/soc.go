package hwdetect

import "strings"

// socCompatiblePath is the device tree's root `compatible` property: the board's
// identity as a NUL-separated list, most specific first (board, then SoC). It exists
// on a Linux board that booted from a device tree. On x86 and Windows the read fails,
// which is "not an SoC", never an error — the probe is inert there by construction,
// so it needs no OS switch.
const socCompatiblePath = "/proc/device-tree/compatible"

// rk3588Tokens are the SoC-level tokens of the RK3588 family, and a board carries
// exactly one of them. The vendor 6.1 kernel's device tree names the board
// `rockchip,rk3588s-exampleboard-5` and then the SoC `rockchip,rk3588`; a mainline device
// tree names the board `vendor,exampleboard-5` and then the SoC `rockchip,rk3588s`. Both
// spellings must match, and EXACTLY: a prefix match would also accept the board token
// `rockchip,rk3588s-exampleboard-5` by itself, and every RK3588 variant nobody has measured.
var rk3588Tokens = map[string]bool{"rockchip,rk3588": true, "rockchip,rk3588s": true}

// DetectSoC recognises a supported SoC from the device tree and reports the Facts it
// implies. ok is false — and the machine left exactly as the PCI probes found it — for
// anything else: an x86 or Windows box (no device tree, so the read fails), another
// ARM board, or a property that cannot be read. read is injected so the rule is
// testable without a board.
//
// An SoC has no PCI adapter for probeFallbackGPU to find (its Mali GPU and NPU sit on
// the platform bus), so without this an RK3588 fell through to "cpu" and was handed a
// CPU-inference serving config, which no model on this fleet may run. Its GPU and NPU
// draw from the same RAM as the CPU: VRAMGb stays 0 and UMA is true.
func DetectSoC(read func(path string) ([]byte, error)) (Facts, bool) {
	b, err := read(socCompatiblePath)
	if err != nil {
		return Facts{}, false
	}
	for _, tok := range strings.Split(string(b), "\x00") {
		if rk3588Tokens[tok] {
			return Facts{
				Vendor: "rockchip", Arch: "rk3588", GPUName: "Mali-G610", GPUCount: 1,
				// One entry per counted GPU, like every other probe path (allArchs).
				Archs: []string{"rk3588"}, UMA: true,
			}, true
		}
	}
	return Facts{}, false
}
