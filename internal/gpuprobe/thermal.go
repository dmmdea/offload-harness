package gpuprobe

import (
	"context"
	"errors"
)

// Thermal is one card's row of the optional thermal query (ReadThermal): the temperature and the active clock
// event reasons, keyed by the card's UUID. A nil TemperatureC or an empty ThrottleReasons is "not read", never
// a value.
type Thermal struct {
	Index           int
	UUID            string
	TemperatureC    *int
	ThrottleReasons []string
}

// thermalRun runs nvidia-smi with args and returns what it wrote to stdout. A test injects its own.
type thermalRun func(ctx context.Context, args []string) (string, error)

var errThermalStub = errors.New("gpuprobe: thermal reader not implemented")

// ParseThermal is a stub.
func ParseThermal(out string) ([]Thermal, error) { return nil, errThermalStub }

// ThrottleReasonNames is a stub.
func ThrottleReasonNames(mask uint64) []string { return nil }

// MergeThermal is a stub.
func MergeThermal(devs []Device, rows []Thermal) {}

// ReadThermal is a stub.
func ReadThermal(ctx context.Context) ([]Thermal, error) { return nil, errThermalStub }

func readThermalWith(ctx context.Context, run thermalRun) ([]Thermal, error) {
	return nil, errThermalStub
}

// ResetThermalState is a stub.
func ResetThermalState() {}

func refusesField(out string, err error, field string) bool { return false }
