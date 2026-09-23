package config

import (
	"encoding/json"
	"testing"
)

// TestCascadeSeatGuardDefaultsOn: the guard that keeps a Tier-1 cascade call
// from evicting a loaded vLLM seat is ON unless a config says otherwise, and
// an absent key (every config written before the key existed) must not read as
// an opt-out — the EmbedMemoEnabled posture.
func TestCascadeSeatGuardDefaultsOn(t *testing.T) {
	if !Default().CascadeSeatGuardOn() {
		t.Fatal("the default config must have the cascade seat guard on")
	}
	for body, want := range map[string]bool{
		`{}`:                            true,
		`{"cascade_seat_guard": true}`:  true,
		`{"cascade_seat_guard": false}`: false,
	} {
		var c Config
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got := c.CascadeSeatGuardOn(); got != want {
			t.Errorf("%s: CascadeSeatGuardOn() = %v, want %v", body, got, want)
		}
	}
}
