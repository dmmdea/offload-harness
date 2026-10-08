package config

import "testing"

// DisplayFloorGiB is the number the card allocator holds an opened display card to: the
// layers' own desktop floor, read from the declaration the display layer's guard reads.
func TestDisplayFloorGiBIsTheLargestFloorOfTheDisplayFloorGuardedLayers(t *testing.T) {
	if got := (Config{}).DisplayFloorGiB(); got != 0 {
		t.Fatalf("a plain box declares no floor, got %v", got)
	}
	if got := CompositeFixture().DisplayFloorGiB(); got != 4 {
		t.Fatalf("the fixture's display and triple layers both keep 4 GiB, got %v", got)
	}

	// The largest one wins: a card kept for two layers keeps the more demanding floor.
	c := CompositeFixture()
	for i := range c.Layers {
		if c.Layers[i].Name == "triple" {
			c.Layers[i].DisplayFloorGiB = 6
		}
	}
	if got := c.DisplayFloorGiB(); got != 6 {
		t.Fatalf("the larger of two floors must win, got %v", got)
	}

	// A number with no guard behind it applies nothing: the guard is what puts the floor to work,
	// so a layer that carries display_floor_gib but not display_floor must not close a card.
	c = CompositeFixture()
	for i := range c.Layers {
		c.Layers[i].Guards = []string{"presence"}
	}
	if got := c.DisplayFloorGiB(); got != 0 {
		t.Fatalf("a floor without the display_floor guard is not in force, got %v", got)
	}

	// The guard with no number: nothing to keep.
	c = CompositeFixture()
	for i := range c.Layers {
		c.Layers[i].DisplayFloorGiB = 0
	}
	if got := c.DisplayFloorGiB(); got != 0 {
		t.Fatalf("the guard without a floor keeps nothing, got %v", got)
	}
}
