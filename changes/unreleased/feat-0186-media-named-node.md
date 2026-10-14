### Added - A video, animation or audio call can name the one fleet node that runs it, and a node publishes the family rows of those lanes

- **What changed.** `offload_generate_video`, `offload_animate_character` and `offload_generate_audio` (CLI `--node` on `generate-video`,
  `animate-character`, `generate-audio`) take `node`: a node id (the `node_id` its `/fleet/health` publishes, in any case) or one of the
  `delegate_remotes` URLs. The named node is the only candidate. A box that has the lane itself still sends the call there, no other
  node is tried and this machine's lane is not asked; `route local` with a node is refused, and so is a node on an image or a graph call made
  through the library or the CLI (the image and graph verbs have no `--node`; the image and graph MCP tools list no `node` and drop one sent to
  them like any unlisted argument, so no door advertises a parameter it would drop). A call with no `node` is placed
  exactly as before and reads no row. `meta.placement` reads `remote: named node <name>`.
- **Refused before anything is sent.** A node that cannot take the call is refused by a word in `data.refusal`, beside `node`, `lane`,
  `family`, `missing[]` and `roster[]`, before the POST, so no job exists on it. `node_unknown` (it lists the ids that answered) and
  `node_is_self` are `contract` defers. `node_lacks_route` (the lane is not served or its route is not CONFIGURED, naming the files of its
  row), `node_lacks_media_job` (input files and the door is closed), `node_lacks_model` (the row of the family the call renders lists a file
  with `bytes` -1, or no such row) and `node_below_min_vram` are `capacity` defers with `err_class` `no_node_configured`, the pairing that
  err class already has, narrowed to the node the caller chose. A node that does not answer, or holds a text lease, is an ordinary
  `capacity` defer with no err class (it may take the call later), and so is a node id that no answering node has while some roster entry
  did not answer. Nothing falls back to another node or to a local run. A named call that ran on a node whose row judged it adds
  `rendered_family` and `rendered_files` to the result.
- **The rows.** `/fleet/health` gains `media_recipes[]`, additive: one row `{lane, name, default, engine, digest, files[{role, name,
  bytes}], license, commercial_use}` per family a ComfyUI or sd.cpp binding of the node names (video: the default family always, another
  only when `videogen_families` binds it or every file is on the node, an sd.cpp family by its own name; `wan-animate2`; `acestep`), with
  `bytes` -1 for a file the node does not hold. A row is a statement of sizes, not an identity: no fingerprint, an empty `digest`, a stat per
  file, memoised for a minute; ComfyUI names keep their subfolders and an absolute, drive-lettered or `..` name is published as its file name.
  Voice, the audio.cpp engines and the sd.cpp animation lane publish no row, which a reader takes as unknown: a lane the node publishes no
  row for goes to the node's own door, as a forced call does. A `model` is matched by row name exactly and otherwise folded the way the node
  folds it (`mediacap.VideoBinding`), `ace` has no row. `delegate.NodeView.MediaRecipes` decodes it.
- **`min_vram_gb` on video and animation** (`--min-vram-gb`): the caller's number filters the remote candidates of any placement that leaves
  the machine by the node's largest card, as a graph's does; a malformed value is refused by name on every route. The Wan 720p default floor
  of a graph is not applied here.
- **Why.** The standing order for the fleet is that any machine holding the model takes a video, animation or audio call. That is three
  releases of work, and naming a node needs no identity, so it comes first: a caller who knows which node has the weights can say so, and a
  node that lacks them is refused in under a health read instead of by that node's own 503 after the job was sent. D2 is recorded as
  decided in ADR 0082 Amendment 3 (these lanes overflow like images, on by default, `route local` the escape); the identity release (a
  digest of the files' content and the settings that change pixels, the busy-lane probe, one-shot overflow) and the release for tickets with
  input files are written down there and are not built here. A node that has not opened the media-job door stays as its operator left it: that is
  a `node_lacks_media_job` for a call with input files, not something this change designs around.
- **Tests, written first.** `internal/mediacap/lane_rows_test.go`, `internal/fleetnode/media_recipes_test.go`,
  `internal/delegate/nodeview_mediarecipes_test.go`, `internal/mediaremote/named_test.go`, `internal/mcpserver/medianode_test.go` and
  `main_node_cli_test.go`, committed red on behaviour-free seams before the fix. Red before: `one family bound and present, want one video row,
  got []`; `rows per lane = map[] (rows []), want one video, one animate and one music row`; `rows = []` (the delegator's decoder);
  `node "render-b": {OK:true ... "node":"render-a"` (the call went to another node); `a named node must take the call even when this machine
  has the lane: {OK:true ... Data:{"local":true}}`; `want a deferred answer, got {OK:true ...` for every refusal; `min_vram_gb 24: ok=true,
  want false`; `offload_generate_video: node advertised=false, want true`; `flag provided but not defined: -node`. On the tests as first
  committed: 21 failing tests in `internal/mediaremote`, 11 in `internal/mediacap`, 3 in `internal/fleetnode`, 1 in `internal/delegate`, 4 in
  `internal/mcpserver`, 5 in the root package. One of them (the fleetnode memo test) panicked on an empty list in its first red run; it was
  changed to fail on its assertion and its changed form run red against the same seam. **Not red first, and the history says so:** the
  test that exactly one default video row exists when the default family is sd.cpp was committed red before the fix; the pins that the
  default row is the one marked default, that an older node is not judged for a model, and that an sd.cpp family is matched by its own
  name were added green after the fix, to give three mutants a test to die on; the redaction of a URL typed with a credential was written
  with its fix in one commit (its test is red under mutant `mr33`); and the task-list half of `node_lacks_route` got its test after a mutant
  survived (`mr10`). Guards that pass before and after by design: a blank node is no node, a call with no node never reads a row, a node of an
  older build decodes to nothing, a node with no lane publishes no key.
- **Mutation checks.** 74 single-line mutants through `scripts/mutation-check.sh`, run on
  `git archive` copies of the committed tree (each landed on exactly one line, compiled, and was restored byte for byte), the last verdict of each
  `MUTATION-VERDICT: RED`; the 33 `internal/mediaremote` mutants were run again in full against the final Go code, all RED. First pass: 59 RED; 9 did not compile (`mc16`, `mr04`, `mr17`, `mr20`, `rt10`-`rt14`: a variable left unused) and 4 did
  not land because the marker matched more than one line (`mcp02`, `rt07`-`rt09`), all replaced by mutants that do; 2 survived, `mr33` because the
  run set omitted the new redaction test, and `mr10` (the task-list half of `node_lacks_route`) because no test held it, which a test now does.
  By rule. mediacap (17): the size of a missing file (-1 to 0); the default family always listed; another family only when bound or complete
  (guard removed, guard disabled; the all-files-present test disabled); an sd.cpp family replacing a ComfyUI one of its name; an absolute or
  climbing name cut to its file name; a digest on a row; the animation and music engine guards; the music and video binding gates; the license
  pair; the default flag; the sd.cpp default taking the ComfyUI default; the role prefix; an sd.cpp file published by path; the animation row
  name. fleetnode (3): the minute memo, the publication, `omitempty`. delegate (1): the decode. mediaremote (33): `auto` with a node
  bypassing a local lane; route local with a node; a node on an image or a graph call; a node that is not a string; this machine's own node
  (case-sensitive); the id and URL match (case-sensitive, URL dropped); an id nobody answered to while an entry is down; the roster list; the
  task and route refusal, the files it names, the media-job refusal, the model refusal; a lane with no row judged; the model fold, `ace`
  judged, the exact-name match, the default row, the music-only rule; `min_vram_gb` on a named node, in `refuses`, on video and animation, and
  its precheck; the text lease; `rendered_family`; the typed data, the err class and the defer class of a refusal; the named branch of
  `callWith` and the placement string; the URL redaction. mcpserver (6): the `node` and `min_vram_gb` schemas, `node` on the image tool, the two
  plumbing lines, `min_vram_gb` on the audio tool. Root (14): the two plumbing lines, the `--min-vram-gb` check, the value-flag entry of each
  of the three verbs, the three usage lines, the parsed fields of each verb.
- **Docs.** ADR 0082 Amendment 3 (the rules, the six refusals and their classes, what a row is and is not, the named limits, and the two
  releases that follow), `docs/systems/media-generation.md` ("Naming a node"), `docs/systems/fleet-node.md`, `docs/FLEET-NODE.md`, the
  glossary and the operator guide.
