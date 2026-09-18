# 10 — The bridge sidecar (146 commands) and the printed CLI

Built 2026-09-08/09 on `video-pipeline` branch `feat/resolve-http-sidecar` (PR #5),
verified live on the workstation against Studio 21.1.0.14 with `_ref_scratch`. Tags: [measured] unless
stated. The old 17-command `resolve.cmd` reference in 03 still applies to those 17 verbs; every
other verb below is new.

## 1. Shape
- `python resolve_cli.py <command> [args] [--json] [--yes] [--timeout S]` — same watchdog, exit
  codes (0/1/2/3/4) and `--yes` guard as before (03).
- `python resolve_cli.py serve [--port 18800]` — loopback HTTP sidecar over the SAME handlers.
  `GET /healthz`, `GET /v1/commands` (self-describing: every command's method, path, `help` and
  `args[{name, kind, required, choices, default, help}]`), `GET /v1/<read>?a=b`,
  `POST /v1/<write>` with a JSON body that MUST carry `"yes": true`. Replies
  `{ok, exit_code, data, lines, context}` (`context` = the project/timeline/page the command
  acted on, for callers that keep history); on a non-zero exit the handler's `data`/`lines` are kept when
  it produced any (e.g. `validate-dctl` on bad source, a partial settings apply).
- One request at a time (one blocking IPC underneath); a watchdog trip answers 503 and **stops the
  sidecar** (the abandoned call would otherwise land later, unobserved) — start it again.
- Argument kinds: `text` · `bool` (`--flag`; `true/false/1/0` on the wire) · `int` · `float` ·
  `list` (repeat the flag / positional list; a JSON array string in a query works) · `json`
  (an object on the wire; a JSON string or `@file` on the CLI, `@file` refused over HTTP).
  Tri-state options (`--enable true|false`, `--enabled`, `--lock`, `--voice-isolation`, …) are
  text with choices so "unset" is expressible.
- Refs: a **clip** is a name, unique id, or file path (`--bin A/B` scopes it; ambiguity is an
  error, never a silent first match); a **timeline item** is `V1:3` (track role + 1-based
  position), a unique id, or a name; tracks are `V2` / `A1` / `S1` or `video:2`.
- Read-safety: `check_readonly.py` (default-deny AST gate) scans every module; a read command may
  only call `Get*/Is*/Has*` (+ `Fusion`, `FindTool*`, `ValidateDCTL`). Every write handler calls
  `guard()` before touching Resolve — unit-tested with a Resolve stub that explodes on any access.

## 2. Command groups (what each measured on 21.1)
| Group | Commands | Measured notes |
|---|---|---|
| status / project manager | `status` (+studio, database, keyboard preset, rendering), `database`, `set-database`, `folders`, `folder create/delete/open/root/parent`, `save-project`, `close-project [--save]`, `rename-project`, `export-project` (.drp), `import-project`, `restore-project` (.dra), `cloud-project` (unverified: no cloud library), `archive-project` (**refuses**: crashes 21.1) | `.drp` export/import round-trip works |
| project settings | `project-settings [--key]` (158 keys), `set-project-settings --settings JSON` (one key per call, per-key result + readback, `useCustomSettings` first), `project-settings-presets`, `project-settings-preset apply/save/update/delete/export/import` | save/delete of a preset verified |
| media storage | `storage-volumes`, `storage-browse PATH [--files]`, `storage-import PATHS [--start-frame --end-frame] [--matte-for CLIP] [--timeline-mattes]`, `clone-status`, `clone-media --source --targets [--checksum md5…] [--stop]` | paths are normalised to backslashes (forward slashes answer nothing) |
| bins & clips | `bins`, `bin create/delete/set-current/move/refresh` (create is idempotent), `import-media PATHS [--bin] [--force] [--sequence …]` (skips paths already in the pool), `delete-clips [--all-matches]`, `move-clips`, `relink-clips [--folder|--unlink]`, `select-clip`, `stereo-clip`, `sync-audio` (content-dependent; synthetic media → False), `rename-clip` | idempotent import measured: second call reports `skipped` |
| clip data | `clip-info` (all properties, metadata, flags, colour, marks, markers, audio mapping), `set-clip-property KEY VALUE [--sharpness --noise-reduction]`, `set-clip-metadata --metadata JSON [--third-party]`, `clip-markers`, `add-clip-marker`, `delete-clip-marker (--frame|--custom-data|--color)`, `clip-flag --add/--remove`, `clip-color COLOR|clear`, `clip-mark --mark-in --mark-out|--clear`, `proxy --link/--full-res/--unlink`, `replace-clip`, `monitor-growing`, `set-audio-mapping`, **`transcription`** (21.1 readback), `clip-ai transcribe/clear-transcription/classify-audio/clear-classification/deblur/intellisearch/slate [--folder BIN]` | markers' `customData` round-trips as JSON; `clip-ai` switches off fusion/color/deliver and needs `--timeout ≥120` |
| timelines | `timeline-info`, `create-timeline`, `create-timeline-from-clips NAME CLIPS… [--batch JSON]`, `duplicate-timeline`, `delete-timeline`, `switch-timeline`, `rename-timeline`, `append --clip … [--start-frame --end-frame --record-frame --track-index --media-type] | --batch JSON` (readback-verified), `export-timeline --path --type otio|fcpxml|edl|aaf|drt|csv|…` (reports the bundle's inner file), `import-timeline --path [--name] [--no-import-source-clips] [--into]` (resolves bundle dirs, retries without source import) | frame rate changeable on an EMPTY custom-settings timeline, locked after clips |
| tracks | `tracks`, `add-track TYPE [--subtype]`, `delete-track V2`, `set-track V2 [--name] [--enable] [--lock] [--voice-isolation --voice-isolation-amount]` | voice isolation on A1 reads back `{isEnabled: true, amount: 40}` |
| items | `items [--type]` (refs, type incl. `transition`/`generator`), `item-info` (32 properties, speed, fades, blanking, markers, comps, version, caches, linked items), `delete-items [--ripple]`, `link-items [--unlink]`, `set-item` (name, enabled, colour, flags, `--speed`, `--fade-in/--fade-out`, `--blanking`, caches, audio mapping, voice isolation), `set-item-properties --properties JSON` (dict call, per-key fallback), **`add-transition`** (position+alignment+duration; handles!), **`multicam create/flatten/smart-switch`**, `add-marker`/`delete-marker` (`--item` for clip-relative), `timecode`, `set-timecode --playhead/--start`, `timeline-mark`, `timeline-settings`, `set-timeline-settings`, `set-blanking`, `insert-generator NAME --kind generator|fusion-title|title|ofx-generator|fusion-generator|fusion-composition [--at TC]`, `compound-clip`, `fusion-clip`, `grab-still [--all first|middle]`, `normalize-modes`, `timeline-ai subtitles/scene-cuts/stereo/dolby-vision/normalize/auto-align`, `item-fusion list/add/import/export/delete/load/rename` | `Text+` inserted at the playhead for 5 s; multicam item lands as `<name> - Angle 1` |
| colour | `color-info` (versions, group, node graph: labels/LUT/tools/cache), `timeline-graph`, `color-version add/delete/load/rename`, `apply-lut ITEMS --lut PATH [--node]`, `apply-cdl`, `apply-drx`, `copy-grade --source`, `set-node [--enabled --cache --reset-all --reset-colors --arri-cdl]`, `export-lut [--cube 17|33|65|vlut]` (switches to Color and back), `refresh-luts`, `color-groups`, `color-group create/delete/rename/assign/remove`, `gallery`, `gallery-album create/create-powergrade/set-current/rename/import/export/delete-stills` | per-node lift/gamma/gain is not in the API — stated, not hidden |
| Fusion | `fusion-tools [--regid] [--read-inputs IDS] [--all-inputs]`, `fusion-set --tool --inputs JSON [--time]` (Lock/StartUndo…EndUndo/Unlock, readback = verification, warns when a spline overrides a static value), `fusion-keyframes --tool --input --keys {frame:value} | --clear` (BezierSpline), `fusion-tool add/delete/bypass/unbypass [--regid --name --connect-from --connect-to]`, `set-title --text --font --style --size --color r,g,b --center x,y --outline w [--inputs JSON]` | `Center` takes `[x, y]`; the TextPlus tool is named `Template` |
| render | `render-formats [--format]` (codec desc→ID, audio formats/codecs, resolutions), `set-render-format EXT CODEC` (descriptions translated to IDs), `set-render-mode single|individual`, `set-render-settings --settings JSON` (per key; `EnableUpload` refused), `render-preset load/save/update/delete/export/import/quick-export-on/quick-export-off` (export path = folder), `render-jobs`, `render-job-status JOB`, `add-render-job [--preset --format --codec --settings --out --name]` (retry on ""), `delete-render-job JOB|--all-jobs`, `start-render [JOBS] [--wait]`, `stop-render`, `quick-export-presets`, `quick-export PRESET --out --name [--quality]` (upload hard-off), `verify-render PATH [--codec --width --height --fps --audio --min-duration]` (ffprobe; the only render readback), `render`, `smoke` | 270-frame 720p NVENC job: 1.8 s; quick export `.mov` 1.5 s; verify-render caught a 25-vs-24 fps mismatch |
| system | `presets` (layout/burn-in/prefs/keyboard(+current)/fairlight), `preset KIND ACTION [--name --path]`, `keyframe-mode`, `set-keyframe-mode all|color|sizing`, `disable-background-tasks`, `quit [--save]`, `validate-dctl --source|--path`, `encrypt-dctl PATH [--name --expiry --out]`, `tts --text --voice …` (Extras pack), `insert-audio`, `thumbnail [--out FILE.ppm]` (Color page), `export-frame PATH`, `page NAME` | `validate-dctl` returns the compiler's message on bad source |

## 3. Live-verification record (2026-09-09, headless and GUI 21.1.0.14, `_ref_scratch`)
Driver: `scratchpad/drive.py` phases project → media → timeline → color → render → more →
cleanup. Final clean pass (headless 21.1.0.14, fresh `_ref_scratch`): **197 calls OK, 14
non-OK, 13 of them the documented limits the driver asserts on purpose** (invalid DCTL text,
`sync-audio` on synthetic media, `transcription` before transcribing / on a sine tone, the
`clip-ai` timeout floor, the frame-rate lock with clips present, a centre transition without a
head handle, deleting the current grade version, `thumbnail` None, `clone-media` False, `tts`
without the Extras pack, `archive-project` refused, layout-preset save headless) and one driver
teardown ordering bug. Not verified live: `cloud-project`, `restore-project` (.dra),
`stereo-clip`, `set-audio-mapping` / `--audio-mapping` (needs multi-track media),
`timeline-ai subtitles/scene-cuts/stereo/dolby-vision/auto-align`, `insert-audio`,
`clip-ai deblur/intellisearch/slate` (Extras packs), `export-timeline aaf/drt/…` beyond otio,
fcpxml and edl. Outputs under `D:\Editing\_pp_smoke\ref\`
(`ref_render.mp4`, `ref_quick.mov`, `_ref_tl.otio`, `_ref_tl.fcpxml\Info.fcpxml`, `_ref_tl.edl`,
`_ref_v1.cube`, `_ref_title.setting`, `_ref_preset.xml\_ref_preset.xml`, `_ref_thumb.ppm`,
`_ref_scratch.drp`).

## 4. The printed CLI (`davinci-resolve-pp-cli`)
Generated by printing-press (v4.32) from a spec DERIVED from the sidecar's `/v1/commands`
catalog (`research/make_spec.py` in run `20260908-013656-8c5431ca`; re-run it after any bridge
change and regenerate — the CLI cannot drift from the bridge). 11 resources (`system project
storage pool clip timeline item color fusion render audio`), 147 endpoint commands:
`davinci-resolve-pp-cli <resource> <verb> --flag …` maps 1:1 onto `/v1/<command>`; writes need
`--yes` exactly like the bridge; `json` kind args are passed as JSON strings. Default
`--data-source live` (a control surface has no meaningful local mirror).

**History layer (hand-authored, `internal/oplog` + `internal/cli/oplog_hook.go`):** every
sidecar round-trip is recorded in `<data dir>/history.db` (Windows:
`%USERPROFILE%\.local\share\davinci-resolve-pp-cli\history.db`) with the sidecar's context
stamp, and derived tables feed the seven commands Resolve itself cannot answer: `runs list/show`
(every call, exit code, busy trips), `marker search <q>` (FTS across projects), `render history`
(queued/started/quick-export/verified-by-file), `color audit` (newest grade version per item
vs the project's latest), `fusion titles search <q>` (every Text+ StyledText written),
`timeline log <name>` (append/delete/insert/transition trail), `doctor history [--record]`
(environment snapshots + drift). All verified live 2026-09-09. Base URL `http://127.0.0.1:18800`;
reach a remote workstation sidecar with an SSH local-forward. Shipcheck 2026-09-09 (run 6): 7/7 legs, scorecard 93/100
(A); the full test suite passes with `GOTMPDIR=%USERPROFILE%\go-tmp` (Windows Defender flags
Go test binaries built under `%TEMP%` as `Trojan:Win32/Bearfoos.B!ml`, a known heuristic
false positive — redirecting Go's temp dir out of `%TEMP%` avoids it without an exclusion).

**Where it lives [measured 2026-09-09]:** promoted with `printing-press lock promote` to the LOCAL
library `%USERPROFILE%\printing-press\library\davinci-resolve` (source + `spec.yaml` + `SKILL.md`;
`go build ./cmd/davinci-resolve-pp-cli` there yields the binary). Deliberately NOT published to the
public printing-press library — the standing rule is never publish without asking. History DB
default: `%USERPROFILE%\.local\share\davinci-resolve-pp-cli\history.db`.

**Phase 5 live dogfood (mandatory here — no-auth API) [measured 2026-09-09]:** 511/511 tests over
175 commands PASS (`proofs/phase5-dogfood.json` + `phase5-acceptance.json` in the run dir). Three
runs were needed; what each taught, so the next regeneration does not relearn it:
- `happy_args` in the internal spec use the generator grammar `--flag=value;--flag2=value` —
  SEMICOLON-separated. Space-separated args reach the CLI as one flag value ("position must be an
  integer"). `make_spec.py` now emits `;`.
- `timeline log <unknown>` exits 3 (not found) instead of 0 with an empty list; dogfood's
  invalid-argument probe requires a non-zero exit.
- `color audit`'s "latest version" is advanced only by `color version --action add|load|rename`;
  a plain `color info` read never advances it (an old version seen on one item used to mark every
  other item stale).
- The media-pool fixture is not durable: something between sessions emptied `_ref_scratch`'s
  bins. Fixture recipe (all through the sidecar): `import-media` `D:/Editing/_pp_smoke/ref/ref_a.mp4`;
  `append --clip ref_a.mp4` onto `_ref_cli_tl` (Text+ generator at `V1:1`, clip lands on `V1:2`/`A1:1`);
  `clip-ai --op transcribe --clip ref_a.mp4` (AI passes return False on the Color page — the bridge
  switches to Edit itself). Whitelisted real writes run only on this disposable project; every
  other write is `--dry-run`.

**Generator fixes this CLI forced (<dev>/cli-printing-press fork, PR #1, merged 2026-09-09):**
the SKILL template advertised `credentials.toml`/cookies/auth sidecars for `auth: none` CLIs (now
gated on `HasAuthCommand` like the README), and `naming.OneLine` cut long descriptions at a period
INSIDE a token (`ProjectManager.ArchiveProject` → `Short: "REFUSED on purpose: ProjectManager."`).
The local `printing-press.exe` is built from that merged main. Known non-blocking shipcheck
warnings: `verb info→get` naming hints on the four `*-info` commands (kept to match the bridge
names), and a false "command-depth mismatch" on `runs list`/`marker search` (the checker matches
the `list`/`search` leaf name across parents; both commands are registered and verified live).

## 5. Rig status
**2026-09-10 [measured]:** the rig came back online and was upgraded in place: Studio
**21.1.0.14** installed from the workstation's `Downloads\DaVinci_Resolve_Studio_21.1_Windows` pair
(`Install Resolve 21.1.exe` + `.dat`, 10.3 GB, scp'd over MagicDNS in 4m47s; silent install
`"Install Resolve 21.1.exe" /i /q /noreboot` from the elevated SSH shell, exit 0, Resolve not
running at the time). Bridge build `c099f222a` deployed with `deploy.ps1` (default remote mode,
verify PASS: `resolve.cmd --version` answers the commit). What is STILL UNVERIFIED on the rig:
scripting under 21.1 with the bundled `python312\` embeddable (compatibility is per-machine — run
`_probe.py` there first), the licence state after the upgrade (`LeManager | License Key:` in the
log), and a read pass. All three need Resolve open in the console session; it was not launched
because the editor was active at the console (house rule: never launch a GUI into someone's
session unasked).
