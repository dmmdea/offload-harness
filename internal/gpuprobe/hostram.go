package gpuprobe

import "fmt"

// HostFreeRAMGiB reports the host's free (available-to-allocate) physical
// RAM in GiB. The composite tier's `host_ram` guard needs it because the
// triple layer's MoE seats hold tens of GB of experts in system RAM
// (--n-cpu-moe): VRAM headroom alone says nothing about whether such a seat
// can load, and a load that pushes the host into swap stalls every other
// seat on the box. Windows reads kernel32.GlobalMemoryStatusEx (AvailPhys),
// Linux /proc/meminfo MemAvailable; any other platform — or a failed read —
// returns ok=false so the guard fails CLOSED rather than admitting on a
// number it never had.
func HostFreeRAMGiB() (float64, bool) {
	return hostFreeRAMGiB()
}

// RAMHeadroom is the host-RAM term of the card allocator: a job that declares needGiB of
// host RAM (an MoE seat's experts, a ComfyUI instance's pinned memory) fits when free RAM
// covers the need AND still leaves headroomGiB for everything else on the box. A load
// that pushes the host into swap stalls every seat, which VRAM headroom never shows.
//
// freeOK=false (the reader could not say) refuses when a need was declared, because the
// guards fail CLOSED on a number they never had; with no declared need it does not refuse,
// because an unreadable counter must not turn every reservation away.
func RAMHeadroom(freeGiB float64, freeOK bool, needGiB, headroomGiB float64) (ok bool, why string) {
	if needGiB < 0 {
		needGiB = 0
	}
	if headroomGiB < 0 {
		headroomGiB = 0
	}
	if !freeOK {
		if needGiB > 0 {
			return false, "host free RAM is unreadable and the job declares a RAM need"
		}
		return true, ""
	}
	if freeGiB < needGiB+headroomGiB {
		return false, fmt.Sprintf("host free RAM %.1f GiB is below the job's %.1f GiB plus the %.1f GiB headroom", freeGiB, needGiB, headroomGiB)
	}
	return true, ""
}
