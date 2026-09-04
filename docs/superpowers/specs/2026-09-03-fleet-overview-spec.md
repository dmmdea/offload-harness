# Fleet overview, jobs feed, smoke verb, and placement signals — spec

Date: 2026-09-03. Origin: the NVIDIA Personal AI Router (PAIR) v0.1.1 gap analysis. The operator
liked PAIR's operator surface (node cards with live graphs, a jobs feed naming the serving node, an
errors tab, a headless terminal view, a one-click traffic test) and asked for those ideas ported
to the harness instead of adopting PAIR (which routes only to Ollama/LM Studio and schedules on a
coarser signal than the delegator already uses).

## Goals (the operator's list, verbatim intent)

1. **Overview UI** — one card per node: live GPU utilization, VRAM per device, CPU, RAM graphs,
   agent seat and residency, served model list, cluster status. Served by the delegator,
   tailnet-only, in the safe port band, polling every node's health.
2. **Jobs feed** — cluster-wide: model, node, state, wall time, defer reason. Sourced from the
   nodes' job stores plus the delegator's delegation-log corpus.
3. **Errors feed** — probe failures, job errors, deferred contracts with their reason; severity,
   age, on the same page.
4. **Terminal view** — `local-offload top`, the same feed in a terminal for headless boxes.
   Deliberately last.
5. **Capability gate** — nodes advertise the model set they actually serve; placement filters on
   it before ranking.
6. **`fleet-smoke`** — one tiny contract per configured node, reports where each landed and
   whether it passed. The harness's equivalent of PAIR's "Test" button.
7. **GPU utilization as a placement tie-breaker** — never the primary signal.

## Non-goals

- mDNS discovery, PIN pairing, mTLS. The tailnet and the bearer-gated agent lane already cover
  trust; static `delegate_remotes` stays the roster.
- Port takeover of 11434/1234 or any drop-in OpenAI endpoint (a separate spec if ever).
- Engine management (install/start/pull) — llama-swap and the tier matrix own that.
- Changing `queue_depth` semantics or the three existing ranking keys.

## Constraints

- **Never-cloud, tailnet-only (ADR 0001):** the UI listener defaults to loopback and binds beyond
  it only with `--listen-trusted-network`, exactly like `fleet-serve`; outbound probes ride
  `netguard.SafeTransport`.
- **Additive wire only:** every new `/fleet/health` field is additive and optional; a pre-0.113.0
  node decodes fine and is treated as "unknown", never as a capacity or a zero.
- **Health never blocks:** new samples (utilization, CPU, RAM, served models) are produced by the
  background samplers/cached probes, never inside the handler.
- **No JS frameworks, no CDN:** one embedded HTML file, vanilla JS, inline SVG sparklines. It must
  render with no network access.
- **Port:** the UI binds `127.0.0.1:18813` by default (Qube safe band; 18810–18812 are taken by
  dispatcher/fleet-serve/retalk). The Qube port file gets the row in the same change.
- **Versioning ritual:** VERSION, `internal/buildinfo/buildinfo.go`, `.printing-press.json` move
  together to `0.113.0`; CHANGELOG under Keep-a-Changelog; docs updated in the same PR (house rule).
- **Placement rule holds:** an idle local node still wins unconditionally; the served-model gate
  and the utilization tie-breaker only reshape remote ranking.

## Acceptance

- `curl http://127.0.0.1:18813/api/overview` returns JSON with one entry per configured node,
  each carrying reachability, health fields, a sparkline history, and the last 50 jobs;
  `errors[]` lists probe failures, job errors and deferred delegations with reason and age.
- Loading `/` in a browser shows a card per node with updating graphs and a jobs table naming the
  serving node, with no console errors and no external requests.
- `local-offload fleet-smoke` exits 0 with one PONG row per reachable agent node and non-zero when
  any node fails, printing a table: node, seat, placement, wall_ms, verdict.
- A node that publishes `served_models` without its agent seat in it is never placed on
  (`gate_test.go` pins it); two otherwise-equal nodes rank by lower `gpu_util_pct` only when both
  publish it (`gate_test.go` pins it, including the "unknown never loses to known" case).
- `go test ./...` green; `docs_lint_test.go` green with the new system doc and ADR.
