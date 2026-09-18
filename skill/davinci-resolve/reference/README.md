# DaVinci Resolve autonomous-editing reference (skill documentation)

Built 2026-09-01 from: the rig's own scripting `README.txt` (Last Updated 24 Jul 2026, the
authoritative doc — newer than every web mirror), the bridge source
(`video-pipeline/engine/resolve_bridge/`), the vendored davinci-resolve-mcp kernels,
the Fusion 8 Scripting Guide, live read-only dumps from Resolve Studio 21.0.4.5, and web
research (community references, forum threads, export guides). Every claim is tagged:
**[measured]** = observed live on our machines, **[doc]** = official Blackmagic text,
**[community]** = third-party report, **[inferred]** = my reasoning, verify before relying.

**2026-09-09 — Resolve 21.1 (Studio 21.1.0.14) measured on the workstation.** Read `09-resolve-21-1.md`
FIRST: it holds the native MCP server findings, the new API (transitions, fades, speed, multicam,
transcript readback, settings presets, clone tool, DCTL validation), the interpreter matrix
(built-in Python 3.14, per-machine pin no longer reproduced), a crash (`ArchiveProject`), and
the table of older rules it overturns. The older chapters keep their 21.0.4 measurements with a
pointer at every overturned line. The scripting docs are now `README.md` + `CHANGELOG.md` +
`DaVinciResolveScript.pyi` (typed; `api-catalog-21.1.md` here is a flat extract of it).

Purpose: stop re-researching, stop guessing, drive Resolve fully, translate editor
instructions into API operations, and keep working offline / over a slow link.

## Reading order (load only what the task needs — this folder is NOT auto-loaded)

| File | Load when |
|---|---|
| `01-hosts-hardware.md` | any session: which box, GPU/RAM/disks, paths, where things live, license state |
| `02-connection-transport.md` | connecting, interpreter crashes, headless, watchdog, page-null, cross-user launch |
| `03-cli-reference.md` | using `resolve.cmd` (every command, exit codes, gotchas, recovery) |
| `04-api-reference.md` | writing any Python against the API (condensed catalog + constants + settings keys) |
| `05-editing-playbook.md` | turning an editor's instruction into operations; frame math; what the API cannot do and the workaround |
| `06-render-delivery.md` | rendering, codecs, YouTube/Shorts numbers, NVENC, verify-by-file |
| `07-fusion-titles-color-audio.md` | Text+ titles, Fusion scripting, keyframes, LUT/CDL/grades, audio limits, captions |
| `08-failure-modes.md` | anything returned None/False/hung; the full catalog of known traps |
| `09-resolve-21-1.md` | **21.1**: native MCP server (stdio zipapp, 14 tools), built-in Python, new API, overturned rules, crash list, measured behaviours |
| `10-sidecar-and-cli.md` | the 146-command bridge/sidecar and the printed `davinci-resolve` CLI: routes, argument kinds, refs (`V1:3`), what each group measured |
| `api-catalog-21.1.md` | every class/method/signature/docstring of `DaVinciResolveScript.pyi` (21.1), flat, greppable |
| `live-dump-2026-09-01.json` | exact `Project.GetSetting()` keys (= `Timeline.GetSetting()` keys), the 309 Text+ input IDs with defaults, render formats/codec ids, preset names on 21.0.4.5 |

## The ten rules (memorize; the rest of the folder is detail)

1. Read first (`status --json`); `"page": null` means every write silently returns None — `load-project NAME --yes` (or `CreateProject` from script) is the way out; `Resolve.exe -nogui` never shows the trap at all. **[measured on TWO machines; -nogui measured on the workstation 2026-09-01]**
2. Studio must be RUNNING; the free edition answers `scriptapp()` with None, it does not crash. A 0xC0000005 crash is the wrong CPython, measured per machine on 21.0.4 — **on 21.1 every 64-bit CPython 3.11–3.14 bound, and Resolve ships its own `ResolvePython.exe` (3.14.4) that needs no env vars** (09 §2). **[measured]**
3. Every API call can hang forever behind a GUI modal. Always run under a watchdog; exit 3 means "busy, retry later", never a fact about the project. **On 21.1 `ArchiveProject` from a script stalls and then CRASHES Resolve — never call it** (09 §5). **[measured]**
4. The API is silent on failure: methods return False/None with no message — and sometimes an error STRING (`GetRenderJobStatus`, `GenerateSpeech`, `ValidateDCTL`). Check every return by type. **[doc + measured]**
5. No trim/move/razor/keyframe on a placed clip: pre-resolve geometry into `AppendToTimeline` clipInfo, then read back `GetStart/GetDuration`. **21.1 adds `AddTransition` (needs media handles), `SetFades`, `SetSpeed`, `SetProperties`** — those are the ONLY post-placement edits (09 §5). **[doc + measured]**
6. `SetRenderSettings` one key per call, fail loud on False; `AddRenderJob` may return "" first time; completion is proven by the output FILE only. **[measured]**
7. Codec ids ≠ codec descriptions: `GetRenderCodecs` maps description→id and every setter wants the id (`H264`, `H264_NVIDIA`, `H265_NVIDIA`, `ProRes422HQ`). **[measured]**
8. Titles: `InsertFusionTitleIntoTimeline("Text+")` takes no clipInfo; drive text via the item's Fusion comp (`GetFusionCompByIndex(1)` → `GetToolList(False,"TextPlus")` → `SetInput("StyledText", …)`); keyframes come from `BezierSpline` or a pre-animated `.setting` template. **[measured 2026-09-01: SetInput, Center dict/list, colours, Enabled2 outline, BezierSpline keyframes and ExportFusionComp all verified live]**
9. **21.1: `MediaPoolItem.GetTranscription()` returns per-word timecodes** after a synchronous `TranscribeAudio()` (works on media/cut/edit/fairlight, returns False on fusion/color/deliver; 20 s cold). Harness STT (hotwords) stays the karaoke source; Resolve's transcript is now a real cross-check, not a missing feature (09 §5). **[measured 2026-09-09; the 21.0.4 "no readback" rule is retired]**
10. Never disturb the editor's live session: reads any time, writes only in an idle/supervised window, `smoke` disables auto-backup for the rest of that Resolve session.
