package pipeline

// A1 config-pinning coverage (0.81.0): the wire result pins the seat's served
// config + the harness build EXACTLY when the seat demonstrably served, and
// never as a side effect that could cold-start a model.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/core"
)

func pinPropsFixture() map[string]any {
	return map[string]any{
		"default_generation_settings": map[string]any{
			"n_ctx": 65536,
			"params": map[string]any{
				"temperature": 0.8, "top_k": 40, "top_p": 0.95, "min_p": 0.05,
				"reasoning_format": "none", "chat_format": "Content-only",
				"samplers": []string{"top_k", "temperature"},
			},
		},
		"total_slots": 1,
		"model_path":  "/models/seat.gguf",
		"model_ftype": "Q4_K - Medium",
		"build_info":  "b322-4df29be",
		"chat_template": "{% t %}",
		"modalities":  map[string]bool{"vision": false},
	}
}

func hashTestBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestRunAgentTaskStampsPinsWhenSeatServed(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		props:     pinPropsFixture(),
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if wire.HarnessVersion != buildinfo.Version {
		t.Errorf("harness_version = %q, want %q", wire.HarnessVersion, buildinfo.Version)
	}
	// Compare against an INDEPENDENT hash of the running binary — "non-empty"
	// would also pass a hash of the wrong file.
	if want := hashTestBinary(t); wire.HarnessBuildSHA256 != want {
		t.Errorf("harness_build_sha256 = %q, want the running binary's sha256 %q", wire.HarnessBuildSHA256, want)
	}
	if len(wire.SeatConfigSHA256) != 64 {
		t.Errorf("seat_config_sha256 = %q, want a sha256 hex", wire.SeatConfigSHA256)
	}
	if wire.SeatConfigBasis == "" {
		t.Error("seat_config_basis empty on a served run with a healthy props endpoint")
	}
}

// TestRunAgentTaskAbstentionCarriesPins: a re-pack abstention is a run whose
// seat SERVED (the loop finished; the model answered and got the shape
// wrong) — precisely the rows a paired experiment scores, so they must be
// pairable.
func TestRunAgentTaskAbstentionCarriesPins(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `not json at all` },
		props:     pinPropsFixture(),
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("want an abstention defer, got deferred=%v class=%q (%s)", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if wire.HarnessVersion == "" || wire.SeatConfigSHA256 == "" {
		t.Errorf("abstention lost its pins: version=%q seat=%q", wire.HarnessVersion, wire.SeatConfigSHA256)
	}
}

// TestRunAgentTaskPreLoopDeferCarriesNoPins: a seat that never served must
// not be pinned — and, load-bearing, must not even be PROBED: /props against
// a non-resident seat would cold-start a model as a telemetry side effect.
func TestRunAgentTaskPreLoopDeferCarriesNoPins(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{"some-other-seat"},
		loop:      func(int64) string { return doneChat("unreached") },
		repack:    func(int64) string { return `{}` },
		props:     pinPropsFixture(),
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassConfig {
		t.Fatalf("want a config defer (seat unserved), got deferred=%v class=%q", wire.Deferred, wire.DeferClass)
	}
	if wire.HarnessVersion != "" || wire.HarnessBuildSHA256 != "" || wire.SeatConfigSHA256 != "" || wire.SeatConfigBasis != "" {
		t.Errorf("pre-loop defer carries pins: %+v", wire)
	}
	if n := fake.propsCNT.Load(); n != 0 {
		t.Errorf("props probed %d time(s) on a run whose seat never served — a cold-start side effect waiting to happen", n)
	}
}

// TestRunAgentTaskAbsentPropsLeavesSeatPinAbsent: no props endpoint (the
// pre-0.81 fleet, or an evicted seat) → the seat pin stays ABSENT — never a
// sentinel that could pair two failures with each other — while the
// harness-side pins, which need no network, still stamp.
func TestRunAgentTaskAbsentPropsLeavesSeatPinAbsent(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		// props nil: the fake 404s the passthrough.
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if wire.SeatConfigSHA256 != "" || wire.SeatConfigBasis != "" {
		t.Errorf("seat pin invented without a props answer: %q / %q", wire.SeatConfigSHA256, wire.SeatConfigBasis)
	}
	if wire.HarnessVersion != buildinfo.Version || wire.HarnessBuildSHA256 == "" {
		t.Errorf("harness pins missing on a served run: version=%q build=%q", wire.HarnessVersion, wire.HarnessBuildSHA256)
	}
}

// TestEntryFromArm: the ledger row carries the process's experimental arm —
// and, just as load-bearing, carries NOTHING when the env is unset (ordinary
// traffic must never read as part of an arm).
func TestEntryFromArm(t *testing.T) {
	t.Setenv("OFFLOAD_DELEGATE_ARM", "  t2-27b-r1  ")
	e := entryFrom(core.TaskAgentRun, core.Meta{}, false, 0)
	if e.Arm != "t2-27b-r1" {
		t.Errorf("arm = %q, want trimmed %q", e.Arm, "t2-27b-r1")
	}
	t.Setenv("OFFLOAD_DELEGATE_ARM", "")
	if e := entryFrom(core.TaskAgentRun, core.Meta{}, false, 0); e.Arm != "" {
		t.Errorf("arm = %q with the env unset, want empty", e.Arm)
	}
}
