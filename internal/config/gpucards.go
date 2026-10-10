package config

import "github.com/dmmdea/offload-harness/internal/gpuprobe"

// DefaultGPUHostRAMHeadroomGiB is the host RAM every GPU lease grant, and the card allocator, keep
// uncommitted beyond what a lease declares, when gpu_host_ram_headroom_gib is unset. It is the
// probe's own default (8 GiB, gpuprobe.DefaultHostRAMHeadroomGiB: it was 4 while only the
// `--cards` path read it, and the paging incident of 2026-10-09 showed 4 is inside the noise of the
// desktop's own growth). A floor against paging, not a measurement of any one job.
const DefaultGPUHostRAMHeadroomGiB = gpuprobe.DefaultHostRAMHeadroomGiB

// GPUHostRAMHeadroom is gpu_host_ram_headroom_gib with the default applied.
func (c Config) GPUHostRAMHeadroom() float64 {
	if c.GPUHostRAMHeadroomGiB > 0 {
		return c.GPUHostRAMHeadroomGiB
	}
	return DefaultGPUHostRAMHeadroomGiB
}
