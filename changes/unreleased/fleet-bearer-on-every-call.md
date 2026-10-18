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
- **Pinned, for clients that do not exist yet.** `TestEveryFleetClientRequestSetsTheBearer` (root package) reads the Go
  source of every fleet client package, and of any file that names a `/fleet/` route, and judges each request on its own.
  It fails on a request built by `http.NewRequest` or `http.NewRequestWithContext` that no bearer-setting call reaches (a
  second request in the same function needs a call of its own, and so does a name that is assigned again); on a call to
  `http.Get`, `http.Post`, `http.Head` or `http.PostForm`, or to the same four methods on an `*http.Client`, which leave
  nowhere to set a header; and on a `Do` of a request that was not built there, was not handed in, and carries no bearer
  call. A bearer-setting call is `Header.Set` or `Header.Add` of `Authorization` with a value that begins `Bearer ` on that
  request, or a call to a function of the same package (or of another package of the module) that does exactly that to
  the request it is given. A call named `auth` counts only if the function it names does that; a string that says
  `Authorization`, a header set on a different request and a value that is not a bearer count for nothing. A request that
  deliberately goes without is listed in `fleetBearerAllowlist` with the reason (the node's own forward to its local
  engine, the identity read of a cascade base that may not be a fleet node, PAIR's loopback endpoints, the terminal
  view's read of the dashboard it runs against); a stale entry fails too, and a second bare request in an allowed
  function is a new site. The lint follows a client through the names it is held under (a parameter, a field, a package
  variable, a package variable that re-exports another package's client, a function that returns one, and a local that
  copies, points to or dereferences any of these), not through types, so a client held under a name it cannot tie to
  `*http.Client`, and a request handed over through an interface or a function value, are not seen. The shapes it must
  report are fixtures in `testdata/fleetbearerlint/red` (27: each shorthand on the package and on a client, on a re-exported
  client, on a client a function hands back and on a local copy of one, a `Do` of a request built by a helper or in its own
  argument, a second request left bare, a helper named `auth` that sets nothing, a helper of another package that sets
  nothing, a stray `Authorization` string, a header set on another request, a value that is not a bearer), and the requests in
  `testdata/fleetbearerlint/green` must pass (a helper of another package that sets the bearer included); one test holds the
  lint to both.
- **Mutant.** Removing the `auth` call from the text client's poll turned both its package test and the source test red. Two more mutants turned the source test red: the accelerator client's `auth` helper setting another header, and the delegator's withdraw request no longer setting the bearer. A `Get` in place of the `Do` in the media client's JSON read, and a `Post` in place of the `Do` in the chat client's node lane, which hold their client as a re-exported package variable and as a local copy of a field, turn it red too (the lint that followed names without copies let both through). One-line changes to the lint itself (functions that return a client, a `Do` on an inline request, a helper of another package, the field rule, the package-variable rule, the dereference and pointer rules, and the package-variable resolution cut to one pass) each turn the fixture test red.
- **Not verified here.** No live node was called; every node in these tests is an in-process loopback server.
