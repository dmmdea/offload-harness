package pipeline

import "github.com/dmmdea/offload-harness/internal/gpuprobe"

// roomyHostMem is the stand-in host the pipeline's tests run against: 256 GiB physical with 40 GiB
// committed, so a media call that declares host RAM is admitted unless a test says otherwise. TestMain
// installs it as the host-memory reader, because a test must not depend on what the machine running it
// is doing: a media admission reads the host before it grants a lease.
var roomyHostMem = gpuprobe.HostMemory{PhysicalGiB: 256, AvailableGiB: 200, CommitUsedGiB: 40, CommitLimitGiB: 400}
