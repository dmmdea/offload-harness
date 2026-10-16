### Security - a fleet node requires the fleet bearer on every lane, and a run-graph manifest may name only pinned node packs

- **The defect.** The node gated the agent, vision, text and door lanes on the fleet bearer and admitted every other task type
  with no check: `image-gen`, `video-gen`, `animate`, `audio-gen`, `run-graph`, the legacy `stt` lane when no token was set,
  and the poll of any such job. A `run-graph` manifest names `node_packs`, which the node `git clone`s and pip-installs into
  its ComfyUI environment, so any peer that could reach a node's listener beyond loopback could make the node run code of its
  choosing, with or without a token configured.
- **Every lane is gated.** `tokenGated` returns true for every task type (the special case for the legacy `stt` lane is gone).
  With a `fleet_auth_token` a request without the bearer is `401`; a poll or withdraw of any job, a re-dispatch of a known job id
  and every read of `GET /fleet/media/{name}` follow the same rule, and the jobs feed omits every row's `error` text without it.
  The check runs first, before validation and before the task type is looked up, so a stranger learns nothing about the node's
  lanes. Each refusal is logged with the caller's address and the task type (never the credential or the payload), at most once
  per caller, task type and reason every ten seconds.
- **Loopback stays tokenless; everything else fails closed.** A loopback node with no token serves as before, with a
  byte-identical ack and poll (pinned). A node that listens anywhere else with no token refuses every task type with `403 fleet
  lanes require fleet_auth_token on a non-loopback listener` (the status is unchanged; the sentence no longer says "agent lane"),
  and `fleet-serve` refuses to start there, naming `fleet_auth_token` and the listen address. The startup line now says which
  lanes are open: `loopback only: every fleet lane is tokenless on <addr>`, or `every lane requires the fleet bearer`. The
  `UNAUTHENTICATED` warning is gone because it is no longer true. `local-offload doctor`'s token rows say the same.
- **Health stays open, with its private fields withheld.** Without the bearer, on a node that has a token, `/fleet/health`
  still carries identity, versions, capacity, counts, lanes and model names, and drops the lease's free-text `reason`, the store
  steward's status (its `root` is a path on the node's disk) and the weight-file rows (`image_recipes`, `media_recipes`). A
  bearer holder, and a caller of a loopback node with no token, read the whole answer.
- **A token holder can have the node render, not run code.** A manifest may name a node pack only when its `repo` AND `commit`
  are a row of `render/node-packs.json`: VideoHelperSuite, ComfyUI-MultiGPU at its documented archival commit (upstream and
  mirror) and ComfyUI-GGUF. The match is exact (no case folding, trimming, `.git` suffix or abbreviated commit), keys are read
  as the runner reads them, and the directory name must be the row's or the one the repository gives. The node refuses at the
  door with `400` naming the pack, before a job, a temp file or a lease exists (`mediacap.CheckManifestNodePacks`, called by
  `buildRunGraph`); the runner refuses again before its first tooling probe, model download or git command (typed defer
  `NODE_PACK_NOT_ALLOWED`; a table that cannot be read refuses every pack). The Go table is a copy pinned to the JSON by a test.
  Adding a pack is a reviewed edit to both.
- **Rollout.** Delegators first, then nodes: every fleet client already sends the bearer whenever a token is configured
  (the previous entry pins it). A node that listens beyond loopback with no token will not start after the upgrade, so give
  it the token first. ADR 0023 carries the amendment, and `docs/FLEET-NODE.md` and `docs/OPERATOR-GUIDE.md` the new rule.
- **Tests, red first.** The failing tests were committed before the change: tokenless remote `run-graph` (401, logged), the
  fail-closed `403` for every task type, loopback with no token allowed, the bearer holder admitted with unchanged bytes,
  unpinned and wrongly-pinned packs refused before any clone (Go and Node, the satisfier asserted never to reach the checkout
  or build a clone command), startup refusal, health without private fields, media files gated. Four mutants each turn their
  test red: the `tokenGated` widening removed, the Go pack check removed, the Node pack check removed, the startup refusal
  removed.
- **Pins to confirm.** The MultiGPU pin is the documented one. The VideoHelperSuite and GGUF commits are the ones one reference
  node's checkout was at, not documented anywhere (`"pin": "unverified"` in the file); confirm them against the fleet before a
  workflow depends on them, and add a row for any other pack a workflow needs.
- **Not verified here.** No live tailnet node was reached and no real clone or pack install ran; every node in these tests is
  an in-process loopback listener, and the runner's satisfier ran against injected git, tooling and filesystem fakes. A
  workflow whose manifest names a pack outside the table is now refused until the table gains a row. Nodes installed with the
  stock Linux service unit do not set `fleet_auth_token` in the unit: the token has to be in the node's config before it is
  upgraded, or it will not start.
