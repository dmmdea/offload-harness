package gpuprobe

// HostFreeRAMGiB reports the host's free (available-to-allocate) physical
// RAM in GiB. The composite tier's `host_ram` guard needs it because the
// triple layer's MoE seats hold tens of GB of experts in system RAM
// (--n-cpu-moe): VRAM headroom alone says nothing about whether such a seat
// can load, and a load that pushes the host into swap stalls every other
// seat on the box. Windows reads kernel32.GlobalMemoryStatusEx (AvailPhys),
// Linux /proc/meminfo MemAvailable; any other platform — or a failed read —
// returns ok=false so the guard fails CLOSED rather than admitting on a
// number it never had.
//
// It is the available-RAM half of ReadHostMemory (hostmemory.go), which also carries the
// commit charge the lease admission rule reads (HostRAMAdmits): the same reading, so a seat's
// guard and a lease's gate cannot be looking at two different host states.
func HostFreeRAMGiB() (float64, bool) {
	m, ok := ReadHostMemory()
	if !ok {
		return 0, false
	}
	return m.AvailableGiB, true
}
