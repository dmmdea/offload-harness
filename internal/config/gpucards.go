package config

// DefaultGPUHostRAMHeadroomGiB is the host RAM the card allocator keeps free when
// gpu_host_ram_headroom_gib is unset. It is a floor against swapping, not a measurement
// of any one job; plan P13 tunes it from the measured RAM of N concurrent ComfyUI
// instances.
const DefaultGPUHostRAMHeadroomGiB = 4.0

// GPUHostRAMHeadroom is gpu_host_ram_headroom_gib with the default applied.
func (c Config) GPUHostRAMHeadroom() float64 {
	if c.GPUHostRAMHeadroomGiB > 0 {
		return c.GPUHostRAMHeadroomGiB
	}
	return DefaultGPUHostRAMHeadroomGiB
}
