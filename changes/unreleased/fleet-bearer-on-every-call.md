### Changed - every call a delegator makes to a fleet node provably carries the fleet bearer

- **What this is.** A node is about to require the fleet bearer on every lane it serves (the next entry in this release),
  so the order of the rollout is delegators first, nodes second. This entry is the delegators' half: no client code
  needed a change, because every client already set `Authorization: Bearer <fleet_auth_token>` whenever the token is
  configured, and what was missing was a test that says so. Each fleet client was read request by request: the media,
  compose, vision, text, stt and accelerator clients (dispatch, job poll, output fetch, and the input-carrying media-job
  and compose-project doors), the delegator (agent dispatch, job poll, withdraw, queue submit, queued-job poll, node
  health view), the roster probe and its token probe, the fleet view's poller, the PAIR relay and the node's own claim
  loop. No `render/` script and no integration talks to a node directly.
- **Pinned, per client.** A new `TestEveryRequestCarriesTheFleetBearer` in each client package drives that package's
  request builders against a recording node (`rostertest.AuthNode`) for every task type and door the package sends, and
  fails unless the node received at least one request and every one carried exactly `Bearer <token>`.
- **Pinned, for clients that do not exist yet.** `TestEveryFleetClientRequestSetsTheBearer` (root package) reads the
  Go source: a function in a fleet client package, or in any file that names a `/fleet/` route, that builds an HTTP
  request without calling the package's `auth` helper or setting `Authorization` fails the test, unless it is listed in
  `fleetBearerAllowlist` with a reason (the node's own forward to its local engine, the identity read of a cascade base
  that may not be a fleet node, PAIR's loopback endpoints). A stale allowlist entry fails too.
- **Mutant.** Removing the `auth` call from the text client's poll turned both its package test and the source test red.
- **Not verified here.** No live node was called; every node in these tests is an in-process loopback server.
