package config

import "testing"

// The node-side stt keys read 0 as the built-in (the fleet_compose_bundle_max_mb convention): 48 MiB
// of decoded audio, which is 64 MiB on the wire after base64 and about 4.6 h of 32 kbps Opus, and one
// stt job at a time.
func TestEffectiveSTTFleetKeys(t *testing.T) {
	var c Config
	if got := c.EffectiveSTTUploadMaxBytes(); got != 48<<20 {
		t.Errorf("EffectiveSTTUploadMaxBytes() = %d, want %d", got, 48<<20)
	}
	if got := c.EffectiveSTTMaxConcurrent(); got != 1 {
		t.Errorf("EffectiveSTTMaxConcurrent() = %d, want 1", got)
	}
	c.FleetSTTUploadMaxMB, c.FleetSTTMaxConcurrent = 10, 3
	if got := c.EffectiveSTTUploadMaxBytes(); got != 10<<20 {
		t.Errorf("EffectiveSTTUploadMaxBytes() = %d, want %d", got, 10<<20)
	}
	if got := c.EffectiveSTTMaxConcurrent(); got != 3 {
		t.Errorf("EffectiveSTTMaxConcurrent() = %d, want 3", got)
	}
	// A negative value is a typo, not "unlimited": both keys guard a single-slot whisper upstream
	// and a disk, so they fall back to the built-in.
	c.FleetSTTUploadMaxMB, c.FleetSTTMaxConcurrent = -5, -1
	if c.EffectiveSTTUploadMaxBytes() != 48<<20 || c.EffectiveSTTMaxConcurrent() != 1 {
		t.Errorf("a negative key must read as the built-in, got %d / %d", c.EffectiveSTTUploadMaxBytes(), c.EffectiveSTTMaxConcurrent())
	}
}
