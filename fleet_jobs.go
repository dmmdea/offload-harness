package main

import (
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// newFleetJobs builds the fleet node's job store from config: its execution slots
// (fleet_max_concurrent_jobs) and its poll lease (fleet_poll_lease_sec, ADR 0064).
// NewJobs starts a store at the default lease; the config narrows it, or turns the
// abandoned-job rules off with a negative value. One function, so that the wiring
// the serve verb depends on — an operator's lease actually reaching the store — is
// the wiring a test reads back (fleet_jobs_test.go), as chooseSamplerKind is for
// the sampler.
func newFleetJobs(cfg config.Config) *fleetnode.Jobs {
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	jobs.SetPollLease(cfg.FleetPollLease())
	return jobs
}
