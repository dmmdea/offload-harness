package main

import (
	"fmt"
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

// pollLeaseNote is the start-up line that says which poll lease the node runs under.
// The lease in force is not always the one written: 0 takes the default, a positive
// value under the floor is raised to it, and a negative one turns the abandoned-job
// rules off. A substituted value with no runtime signal is a setting the operator
// believes in and is not running; the first evidence would be a job that was reaped
// too soon, or one that never was.
func pollLeaseNote(cfg config.Config) string {
	secs := int(cfg.FleetPollLease() / time.Second)
	const rule = "an accepted agent job nobody polls for this long is skipped and reaped"
	switch {
	case cfg.FleetPollLeaseSec < 0:
		return fmt.Sprintf("poll lease OFF (fleet_poll_lease_sec=%d): an accepted job nobody polls is never skipped or reaped; a delegator's own withdraw still works", cfg.FleetPollLeaseSec)
	case cfg.FleetPollLeaseSec == 0:
		return fmt.Sprintf("poll lease %d s (the default; fleet_poll_lease_sec is unset): %s", secs, rule)
	case cfg.FleetPollLeaseSec < config.FleetPollLeaseSecFloor:
		return fmt.Sprintf("poll lease %d s: fleet_poll_lease_sec=%d is under the %d s floor and was raised to it (a delegator's gap between polls must fit inside the lease): %s",
			secs, cfg.FleetPollLeaseSec, config.FleetPollLeaseSecFloor, rule)
	}
	return fmt.Sprintf("poll lease %d s (fleet_poll_lease_sec=%d): %s", secs, cfg.FleetPollLeaseSec, rule)
}
