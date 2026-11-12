package ledger

import "github.com/dmmdea/offload-harness/internal/buildinfo"

// stampBuild puts this process's build on a row, on the one path every writer takes (Record), so no record site
// can forget it. It reads the same source the delegation log does (buildinfo.BuildSHA256, computed once per
// process) and uses the delegation log's field names.
//
// Which field depends on whose work the row records:
//
//   - a delegation row (task agent_delegate, its dispatch marker included) and any row naming a remote node are
//     written by the DELEGATOR, so this process's build is delegator_build_sha256, as in the delegation log;
//   - every other row records work this process ran, so it is harness_build_sha256.
//
// A caller that holds the executing node's own build (the node's result carries it) sets harness_build_sha256
// itself; a value already on the row is never replaced. A process whose self-hash failed stamps nothing (the
// failure was logged once where it happened): the row is then unstamped, never stamped with a guess.
func stampBuild(e *Entry) {
	build := buildinfo.BuildSHA256()
	if build == "" {
		return
	}
	if e.Task == delegationTask || e.Node != "" || e.NodeID != "" {
		if e.DelegatorBuildSHA256 == "" {
			e.DelegatorBuildSHA256 = build
		}
		return
	}
	if e.HarnessBuildSHA256 == "" {
		e.HarnessBuildSHA256 = build
	}
}
