### Security - a fleet node requires the fleet bearer on every lane, and a run-graph manifest may name only pinned node packs and models under models/

- **The defect.** The node gated the agent, vision, text and door lanes on the fleet bearer and admitted every other task type
  with no check: `image-gen`, `video-gen`, `animate`, `audio-gen`, `run-graph`, the legacy `stt` lane when no token was set,
  and the poll of any such job. A `run-graph` manifest names `node_packs`, which the node `git clone`s and pip-installs into
  its ComfyUI environment, so any peer that could reach a node's listener beyond loopback could make the node run code of its
  choosing, with or without a token configured.
- **Every lane is gated.** `tokenGated` returns true for every task type (the special case for the legacy `stt` lane is gone).
  With a `fleet_auth_token` a request without the bearer is `401`; a poll or withdraw of any job, a re-dispatch of a known job id
  and every read of `GET /fleet/media/{name}` follow the same rule, and the jobs feed omits every row's `error` text without it.
  The check runs first, before validation and before the task type is looked up, so a stranger learns nothing about the node's
  lanes. Each refusal is logged with the caller's address and the task type (never the credential or the payload). The journal
  cannot be grown by a caller: the throttle is keyed on the caller's address and the reason, never on the task type (which the
  caller chooses), at most twenty lines are written a minute across callers, and the first refusal after that minute writes one
  line counting the rest.
- **Loopback stays tokenless; everything else fails closed.** A loopback node with no token serves as before, with a
  byte-identical ack and poll (pinned). A node that listens anywhere else with no token refuses every task type with `403 fleet
  lanes require fleet_auth_token on a non-loopback listener` (the status is unchanged; the sentence no longer says "agent lane"),
  and `fleet-serve` refuses to start there, naming `fleet_auth_token`, the listen address and the config file it read (or that
  none was found), so a node that will not start says which file to fix. The startup line now says which
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
- **A manifest's models land under `models/` and come from a listed host.** A manifest's `models[]` makes the node download each
  `source_url` to `path` inside the ComfyUI tree before ComfyUI starts, so a `custom_nodes/` path, a `..` climb, an absolute
  path, a drive or a stream name would place code where ComfyUI loads it or write outside the install. An entry is taken only
  when its path is `models/<class>/<file>` (backslashes read as separators; no empty, `.` or `..` segment; nothing beginning
  with a space or ending with a space or a dot; no control character or `:*?"<>|`) and its `source_url`, when it has one, is
  `https://<host>/...` on a plain DNS name that is, or is a subdomain of, a `model_hosts` entry of `render/node-packs.json`
  (`huggingface.co`, `hf.co`, `civitai.com`, `github.com`, `githubusercontent.com`; no user name, port or IP literal). The node
  refuses at the door with `400` naming the entry (`mediacap.CheckManifestModels`); the runner refuses again, before it looks
  for, hashes or downloads any file, with the typed defer `MODEL_NOT_ALLOWED`, having read every entry before provisioning the
  first. One table of paths and sources drives both tests. A `sha256` is not required, because the manifest carries its own hash
  and one the caller supplies authenticates nothing. A workflow whose models come from another host is refused until the file
  gains a row. A `models[]` entry with no `path` is refused at the door as well (the runner's parser already refused it).
  Left open: a holder can still put a file of any type under `models/<class>/` (a weight file in a pickle-based format is
  code when ComfyUI loads it), and the node follows redirects from a listed host.
- **Rollout.** Delegators first, then nodes: every fleet client already sends the bearer whenever a token is configured
  (the previous entry pins it). The delegator change is deployed fleet-wide first; before the node change is deployed to a
  node, `fleet_auth_token` must be present in the config its `fleet-serve` reads. A node that listens beyond loopback with no
  token will not start after the upgrade, so give it the token first. `setup/install.sh` takes `--token-file PATH` on a node
  install, stores the key in the node's config (readable by its owner only, handed to the `--user` account with `sudo chown`
  when that is not the installing user, never printed) and refuses, before it writes anything, to register the unit without
  one, so a fresh install no longer crash-loops; the Windows launcher template says the key must
  be in the config before the task is registered. ADR 0023 carries the amendment, and `docs/FLEET-NODE.md` and
  `docs/OPERATOR-GUIDE.md` the new rule.
- **A node swap refuses a node that could not start afterwards.** `node-swap` reads the config it resolves and records only
  whether `fleet_auth_token` is there; the engine refuses, as its first step and on a dry run too, a node whose health URL is
  beyond loopback and whose config holds no key (the step is named `fleet-key`, the message names the key and the config). A
  loopback or standalone node is not checked; `--skip-fleet-key-check` (`-SkipFleetKeyCheck` on the Windows launcher) leaves
  the check out for a box whose config is not the node's.
- **Tests, red first.** The failing tests were committed before the change: tokenless remote `run-graph` (401, logged), the
  fail-closed `403` for every task type, loopback with no token allowed, the bearer holder admitted with unchanged bytes,
  unpinned and wrongly-pinned packs refused before any clone (Go and Node, the satisfier asserted never to reach the checkout
  or build a clone command), startup refusal, health without private fields, media files gated; then, in a second red series,
  hostile model paths and sources refused (Go door and Node satisfier, from one shared table), the refusal naming the config
  file, the refusal journal bounded against a caller who varies the task type, and the installer's key precondition. Mutants
  each turn their test red: the `tokenGated` widening removed, the Go pack check removed, the Node pack check removed, the
  startup refusal removed, the Go model check removed, the Node model check removed, the config path dropped from the refusal,
  the journal's task-type key restored, the node-swap key check removed.
- **Pins to confirm.** The MultiGPU pin is the documented one. The VideoHelperSuite and GGUF commits are the ones one reference
  node's checkout was at, not documented anywhere (`"pin": "unverified"` in the file); confirm them against the fleet before a
  workflow depends on them, and add a row for any other pack a workflow needs.
- **Not verified here.** No live tailnet node was reached and no real clone or pack install ran; every node in these tests is
  an in-process loopback listener, and the runner's satisfier ran against injected git, tooling and filesystem fakes. A
  workflow whose manifest names a pack outside the table, or a model outside `models/` or from a host outside the list, is now
  refused until the table gains a row. The installer's key handling ran against a stub binary and a temporary prefix; no unit
  was registered, and no node was swapped (the swap check ran against fakes, and the launchers' forwarding is a source test).
  A node that was installed before this change has its unit and config as they were: the token has to be in its config before
  it is upgraded, or it will not start. That a ComfyUI imports a `custom_nodes` package at start-up was not run here; the model
  rule closes the write whatever it does.
