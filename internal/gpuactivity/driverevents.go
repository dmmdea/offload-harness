package gpuactivity

// driverevents.go: the NVIDIA driver's own fault events for the leased cards (C-83-B, A-128 part 3).
// (STUB, red-first: the types, the seams and the entry points exist; nothing is parsed or read yet.)

import (
	"context"
	"errors"
	"time"
)

// DriverEvent is one event the NVIDIA driver logged.
type DriverEvent struct {
	At     time.Time `json:"at"`
	Source string    `json:"source"`
	ID     int       `json:"id"`
	Bus    string    `json:"bus,omitempty"`
	Card   int       `json:"card"`
	UUID   string    `json:"uuid,omitempty"`
	Text   string    `json:"text,omitempty"`
}

// BusCard is one row of the PCI bus table: the card at a bus and device.
type BusCard struct {
	Index int
	UUID  string
}

// ErrNoDriverEventSource says this OS has no driver-event source.
var ErrNoDriverEventSource = errors.New("gpuactivity: no driver event source on this OS")

var (
	driverEventsGOOS = ""
	driverNow        = time.Now
	runChild         = func(ctx context.Context, name string, args ...string) (string, error) { return "", nil }
)

func resetDriverEvents() {}

// ParseWinEvents parses a sequence of Windows event XML records.
func ParseWinEvents(out string) ([]DriverEvent, error) { return nil, nil }

// ParseKernelLog parses kernel-log lines for NVRM Xid events.
func ParseKernelLog(out string) []DriverEvent { return nil }

// ParseBusTable parses `nvidia-smi --query-gpu=index,uuid,pci.bus_id` rows.
func ParseBusTable(out string) map[string]BusCard { return nil }

// AttributeEvents names the card behind each event's bus.
func AttributeEvents(evs []DriverEvent, table map[string]BusCard) []DriverEvent { return evs }

// ReadDriverEvents reads the recent driver events of this machine.
func ReadDriverEvents(ctx context.Context) ([]DriverEvent, error) { return nil, nil }
