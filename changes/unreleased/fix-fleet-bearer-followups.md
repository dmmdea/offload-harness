### Fixed - the tests that hold a fleet node's bearer rules cover every route, and the texts that describe the rules say what holds

- **A new route cannot skip the bearer rules.** The node now registers its routes, and the durable queue's, through one small
  interface, and a test records every pattern the node serves and compares it with the tables the credential tests run over. A
  route that takes a body (POST, PUT, PATCH, or a pattern that names no method) must be a row of the door table, which is what
  puts it under the checks that a caller without the bearer costs the node no body byte and is refused with one journal line; any
  other route must be a row of that table or an entry of the two public routes (health and the jobs feed), which are read-only. A
  row that names no route fails too. The hand-kept door count that stood in for this is gone. Nothing a caller sees changes.
- **"Answered the same way" is pinned.** For `/fleet/health` and the jobs feed, a request that presents a bearer the node does not
  hold (a wrong one of the key's length and of another, the scheme in lower case, `Basic`, a bare `Bearer` with and without a
  space, a 64 KiB bearer, a 64 KiB value with no scheme, and the right key under another scheme) must get the same status, the same
  response headers (the `Date` and `Content-Length` a server sets per response aside) and the same body as a request with none, on
  the same node, and the view it is compared with must be the one that withholds the private fields. The one thing a presented
  credential changes is the journal, which gets one throttled line in the usual shape. Nothing a caller sees changes.
- **The list of sentences that must stay out of the guides is checked against what they replaced.** Each row of the door-claims
  lint now carries the words of its file as they stood before the bearer covered every lane, and the lint is run over them as if
  they were the file, so a row that matched nothing in any version fails. Two rows that did are replaced (the operator guide's real
  sentence about the health route) or dropped (the decision record is held by the amendment test), and the list now covers the
  health route, the media lanes, the config row that named the agent lane as the only gated one, the flow that called the contract
  unauthenticated, the systems page's three sentences, the poll handler's comment and two test comments in the node's package.
- **The systems page says every lane needs the bearer.** Its Auth section no longer says the token gates exactly two lanes, and it
  no longer says a node with no token is unchanged or that the legacy speech-to-text lane rides the bearer only when the node has
  one: a node with none answers only on loopback and refuses everything else with `403`.
- **The release floors are the first release whose client sends the bearer.** Withdraw 0.146.0, text 0.154.0, compose 0.163.0, stt
  0.165.0, the roster probe's token check 0.167.0 and media 0.173.0, in the node guide, the operator guide and the entry that
  introduced them; four of the six were wrong. The speech-to-text asker, its upload door and their two config keys are dated 0.165.0
  in the six files that named an earlier release.
- **The run-graph door is described as it is.** The cost rationale for capping the missing-weights list says a holder of the fleet
  bearer can send a graph that names thousands of files, in the node guide, the media systems page and the comments in the
  package that checks the files, where it said any peer could.
- **The refusal of a composition's own code says what the bearer does.** A dispatch of `compose-video` that carries its own `html`
  or `project_dir` is refused as before. The error says the door renders the node's vetted templates and points at the project
  door of a node that opted in, instead of saying the bearer is limited to templates, which it is not on such a node. The
  composition guide says the same.
- **The operator guide's health example keeps the token off the command line.** It reads the `Authorization` header from a file
  only the operator can read (`curl -H @file`).
- **Tests, red first.** The lint's own proof of its rows failed on the two rows that matched nothing, the extended list failed on
  eight stale sentences and then on three more, and the tightened composition refusal test failed on the old wording, each before
  the change; the route table and the same-answer matrix pass today by design and were shown to fail on a route added without a row,
  on a challenge and a verdict header added for a presented bearer, and on the task list emptied for one.
- **Not verified here.** No live node was reached: the route table and the same-answer matrix run against in-process nodes, and the
  release numbers were read from the repository's history, not from a deployed fleet.
