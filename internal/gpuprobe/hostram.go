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
func HostFreeRAMGiB() (float64, bool) {
	return hostFreeRAMGiB()
}
