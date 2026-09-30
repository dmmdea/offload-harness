package main

// bareClientAllowlist: every place in the tree that builds its own HTTP client,
// reviewed 2026-09-30 against ADR 0042 (a client that dials a CALLER-NAMED host
// goes through netguard's pinned transport). Regenerate the key list with
// BARE_CLIENT_LINT_PRINT=1 go test . -run TestNoNewBareHTTPClient -v, then
// review each new key by hand: the reason is the review.
//
// Kinds of reason:
//   - "configured": dials an endpoint from the operator's config (llama-swap,
//     a seat, a sidecar, a fleet node in delegate_remotes) — never a host a
//     request or a model names.
//   - "guarded": dials a caller-named host through a netguard guard (named).
//   - "open": dials a caller-named host without a dial guard; the register row
//     that closes it is named.
var bareClientAllowlist = map[string]struct {
	count int
	why   string
}{
	"gpu_drain.go:<file-scope>":                                  {1, "configured: the local llama-swap endpoint (drain and warm-back)"},
	"internal/accelclient/accelclient.go:NewDevice":              {1, "configured: the local accelerator sidecar"},
	"internal/accelremote/accelremote.go:<file-scope>":           {1, "configured: fleet nodes from delegate_remotes (accelerator lane)"},
	"internal/agent/client.go:NewLLMClient":                      {1, "configured: the agent seat endpoint (llama-swap or a seat_endpoints entry under the tailnet guard)"},
	"internal/agent/fetchtool.go:newFetchClient":                 {2, "guarded: the agent's web_fetch dials through netguard.PublicDialControl (ADR 0042)"},
	"internal/agent/memory.go:NewMemoryClient":                   {1, "configured: the memory authority endpoint from config"},
	"internal/agent/props.go:<file-scope>":                       {1, "configured: the seat's /props on the local llama-swap"},
	"internal/agent/window.go:probeWindow":                       {2, "configured: the seat's served-window probe on the local llama-swap"},
	"internal/delegate/nodeview.go:<file-scope>":                 {1, "configured: fleet nodes from delegate_remotes (health reads)"},
	"internal/delegate/run.go:<file-scope>":                      {2, "configured: fleet nodes from delegate_remotes (dispatch and poll)"},
	"internal/fleetnode/chat_lane.go:<file-scope>":               {1, "configured: the node's own llama-swap (chat lane proxy)"},
	"internal/fleetnode/claimloop.go:Server.StartClaimLoop":      {1, "configured: the fleet queue holder from config"},
	"internal/fleetnode/ingress.go:newIngressClient":             {1, "guarded: every ref is checked against the IP-literal allowlist before any dial, redirects refused, no proxy (ingress.go)"},
	"internal/fleetnode/kvslots.go:<file-scope>":                 {1, "configured: the node's own llama-swap (kv-slot lane)"},
	"internal/fleetnode/server.go:<file-scope>":                  {1, "configured: the node's own local services"},
	"internal/fleetview/poller.go:NewPoller":                     {1, "configured: fleet nodes from delegate_remotes (overview page)"},
	"internal/fleetview/topmodel.go:NewTop":                      {1, "configured: fleet nodes from delegate_remotes (top view)"},
	"internal/gpuactivity/snapshot.go:Snapshot":                  {1, "configured: the local llama-swap /running"},
	"internal/gpugen/gpugen.go:freeComfyVRAM":                    {1, "configured: the local ComfyUI endpoint"},
	"internal/judge/embed.go:NewEmbedder":                        {1, "configured: the embedding seat endpoint"},
	"internal/llamaclient/client.go:New":                         {1, "configured: the cascade endpoint (llama-swap)"},
	"internal/llamaclient/endpoints.go:Client.WithSeatEndpoints": {1, "configured: seat_endpoints entries (tailnet guard at config load)"},
	"internal/llamaclient/lanes.go:Client.WithRemoteLanes":       {1, "configured: remote lanes from config"},
	"internal/llamaclient/lanes.go:FleetLaneGates":               {1, "configured: fleet nodes from delegate_remotes (lane gates)"},
	"internal/llamaclient/lanes.go:LocalSwapBusy":                {1, "configured: the local llama-swap /running"},
	"internal/mcpserver/mcpserver.go:localSeatView":              {1, "configured: the local llama-swap (status view)"},
	"internal/mediacap/routeneeds.go:LiveNodeChecker":            {1, "configured: fleet nodes from delegate_remotes (media route readiness)"},
	"internal/modelaffinity/upstream.go:upstreamResident":        {1, "configured: the local llama-swap /running (the upstream fence)"},
	"internal/nimclient/nimclient.go:New":                        {1, "open: offload_nim dials a caller-named base; the key is bound to NVIDIA's exact hosts since 0.143.1 (S-01), the base itself is not yet allowlisted — register SF S-30 (audit-first allowlist) closes it"},
	"internal/nodeswap/deps.go:readHealth":                       {1, "configured: the swapped node's own --health-url"},
	"internal/pairworkloads/pairworkloads.go:New":                {1, "configured: the local PAIR telemetry endpoint"},
	"internal/pairworkloads/seatwatch.go:NewSeatWatcher":         {1, "configured: the local llama-swap"},
	"internal/pipeline/agenttask.go:<file-scope>":                {1, "configured: the agent seat on the local llama-swap"},
	"internal/pipeline/browse.go:newBrowseDecisionClient":        {2, "configured: the loopback browse decision endpoint (ADR 0060)"},
	"internal/pipeline/liveness.go:engineActivityProbe":          {1, "configured: the seat's own address from llama-swap /running (ADR 0061)"},
	"internal/placement/live.go:<file-scope>":                    {1, "configured: fleet nodes from delegate_remotes (placement reads)"},
	"internal/research/fetch.go:Fetch":                           {1, "guarded: wrapped in netguard.PublicTransport before the first dial (ADR 0042)"},
	"internal/seatguard/guard.go:New":                            {1, "configured: the local llama-swap"},
	"internal/sttclient/sttclient.go:New":                        {1, "configured: the speech-to-text endpoint from config"},
	"internal/swapclient/swapclient.go:NewGuarded":               {1, "configured: the local llama-swap (guarded constructor)"},
	"internal/tokclient/tokclient.go:New":                        {1, "configured: the tokenizer endpoint (llama-swap upstream)"},
	"internal/ttsclient/ttsclient.go:Speak":                      {1, "configured: the text-to-speech endpoint from config"},
	"internal/visionremote/visionremote.go:<file-scope>":         {1, "configured: fleet nodes from delegate_remotes (vision lane)"},
}
