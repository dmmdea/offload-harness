package main

import "github.com/dmmdea/offload-harness/internal/gpuprobe"

// roomyTestHost is the stand-in host the root package's tests run against: 256 GiB physical with
// 40 GiB committed, so a lease that declares host RAM is admitted unless a test says otherwise. TestMain
// installs it as the host-memory reader (runIsolated), because a test must not depend on what the
// machine running it is doing: a reservation that declares host RAM reads the host.
var roomyTestHost = gpuprobe.HostMemory{PhysicalGiB: 256, AvailableGiB: 200, CommitUsedGiB: 40, CommitLimitGiB: 400}
