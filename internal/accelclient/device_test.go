package accelclient

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The Device label (Coral design D2) is what makes a defer read "<device>: ..."
// whichever lane produced it. New() must stay the Hailo constructor byte-for-
// byte in behaviour, so the Hailo path and its tests are untouched.
func TestDeviceLabelOnErrors(t *testing.T) {
	// A closed port: the transport error must name the device.
	coral := NewDevice("coral-edgetpu", "http://127.0.0.1:1", 500*time.Millisecond)
	if coral.Device() != "coral-edgetpu" {
		t.Fatalf("Device() = %q", coral.Device())
	}
	_, err := coral.Health(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "coral-edgetpu sidecar unreachable") {
		t.Errorf("coral error does not lead with the device: %v", err)
	}
	hailo := New("http://127.0.0.1:1", 500*time.Millisecond)
	if hailo.Device() != "hailo-8l" {
		t.Fatalf("New() must remain the hailo-8l constructor, got %q", hailo.Device())
	}
	_, err = hailo.Health(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "hailo-8l sidecar unreachable") {
		t.Errorf("hailo error does not lead with the device: %v", err)
	}
}

// ErrNoSidecarCmd stays ONE sentinel for every device (callers compare identity);
// the device rides on the wrapping error text from Ensure's spawn path instead.
func TestSidecarSpawnErrorNamesDevice(t *testing.T) {
	c := NewDevice("coral-edgetpu", "http://127.0.0.1:1", 300*time.Millisecond)
	sc := NewSidecar(c, func() error { return errBoom }, time.Second)
	err := sc.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "starting coral-edgetpu sidecar") {
		t.Errorf("spawn error does not name the device: %v", err)
	}
}

type boomErr struct{}

func (boomErr) Error() string { return "boom" }

var errBoom error = boomErr{}
