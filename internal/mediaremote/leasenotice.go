package mediaremote

import "github.com/dmmdea/offload-harness/internal/config"

// InheritedLeaseNotice is the one stderr line a media call prints when it was asked to use the fleet and cannot: this process inherited a
// GPU lease (GPU_LEASE_* in its environment, `gpu reserve -- local-offload ...`), so its card was chosen for it and the router never
// asks the fleet (chooseBatchMode and the lane probe both return the plain local call for an inherited lease). It returns "" when there
// is nothing to say: no inherited lease, or the caller asked for neither the fleet nor a spill.
//
// route is the route the caller ASKED for ("" when it gave none, so the default never nags); only auto and remote count. spill is the
// caller's --spill. A call with no fleet to turn off (no delegate_remotes in cfg and no remotes named for the call) has nothing to say
// either. The batch verb prints it today; the edit lane can print the same line.
func InheritedLeaseNotice(cfg config.Config, remotes []string, route string, spill bool) string {
	if !ambientLease() {
		return ""
	}
	if len(cfg.DelegateRemotes) == 0 && len(remotes) == 0 {
		return ""
	}
	asked := spill
	if r, ok := NormalizeRoute(route); ok && route != "" && (r == RouteAuto || r == RouteRemote) {
		asked = true
	}
	if !asked {
		return ""
	}
	return "this call runs inside an inherited GPU lease (GPU_LEASE_* is set), so fleet ticketing and --spill are off and it renders on the lease's own card; run it outside `gpu reserve` to use them"
}
