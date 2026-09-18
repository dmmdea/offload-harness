# 07 — Fusion (titles, motion), color, audio, captions

## Fusion scripting model (FusionScript, Lua/Python identical except `:`→`.`) [doc: Fusion 8 Scripting Guide + Resolve README]
Get a comp from a timeline item: `comp = item.GetFusionCompByIndex(1)` (create one with
`item.AddFusionComp()` on plain clips; inserted Text+ items already have one). The Fusion page's
active comp is `resolve.Fusion().GetCurrentComp()` (None when no comp is open — do not rely on it
from automation; always go through the timeline item).

Composition (comp) essentials:
- `comp.Lock()` / `comp.Unlock()` — batch mode: no dialogs, no re-render on every change. Wrap every scripted mutation in Lock/Unlock.
- `comp.StartUndo("label")` / `comp.EndUndo(True)` — one undo event so the editor can Ctrl+Z your change.
- `comp.GetToolList(selected=False[, regid])` → dict {index: tool}; filter by RegID (`"TextPlus"`, `"Merge"`, `"Transform"`, `"Background"`, `"MediaIn"`, `"MediaOut"`, `"Loader"`, `"Saver"`).
- `comp.FindTool(name)`, `comp.FindToolByID(regid[, prev])`, `comp.ActiveTool`, `comp.SetActiveTool(tool)`.
- `comp.AddTool(regid[, defsettings][, xpos, ypos])` (−32768,−32768 = auto-place next to selected and auto-connect); Python sugar: `bg = comp.Background()`, `mg = comp.Merge({"Background": bg, "Foreground": fg})`.
- `comp.CurrentTime` (get/set), `comp.GetAttrs()` → `COMPN_RenderStart/RenderEnd/GlobalStart/GlobalEnd`, `COMPB_Locked`, `COMPS_Name`; `comp.SetAttrs({...})`.
- `comp.Copy(tool|[tools])`, `comp.Paste([settings])` (paste a `.setting`/macro from `bmd.readfile(path)`), `comp.CopySettings(tool)` → table, `comp.Save(path)`, `comp.Render(...)` (Fusion-internal render, not the Deliver page), `comp.Execute("!Py3: …")`, `comp.MapPath("Comp:...")`.
- `comp.GetPrefs()/SetPrefs({"Comp.FrameFormat.Name": "HDTV 1080"})`.

Tool (Operator) essentials:
- `tool.ID` (RegID), `tool.Name`, `tool.GetAttrs()` (`TOOLS_RegID`, `TOOLB_PassThrough`, `TOOLNT_Region_Start/End`, `TOOLIT_Clip_Length`…), `tool.SetAttrs({"TOOLB_PassThrough": True})` to bypass.
- `tool.GetInputList([type])` → dict {id: Input}; each `inp.GetAttrs()` has `INPS_ID`, `INPS_Name`, `INPS_DataType` (Number|Text|Point|Image|Gradient…), `INPB_Connected`, `INPN_MinAllowed/MaxAllowed`.
- `tool.GetInput(id[, time])` / `tool.SetInput(id, value[, time])`; sugar `tool.Size = 0.08`, `tool.Size[24] = 0.12` (index = frame). Third arg `0`/omitted = static value (ignores keyframes).
- `tool.ConnectInput("Foreground", otherTool)` or `tool.Input = other.Output`; `tool.Input = None` disconnects; `inp.GetConnectedOutput()`.
- `tool.AddModifier("Size", "BezierSpline")` → returns Bool; then `tool.Size[0] = a; tool.Size[24] = b` creates keyframes. Equivalent: `tool.Size = comp.BezierSpline(); tool.Size[0]=…`.
- `inp.GetKeyFrames()` (nil if not animated), `inp.SetExpression("Gain*0.7")` / `GetExpression()`, spline: `spl = inp.GetConnectedOutput().GetTool(); spl.GetKeyFrames()` → {time: {value, LH={x,y}, RH={x,y}}}; `spl.SetKeyFrames(table, replace)`; `spl.AdjustKeyFrames(start,end,x,y,"set"|"offset"|"scale")`; `spl.DeleteKeyFrames(start,end)`.
- `tool.SaveSettings(path)` → `.setting` file (Lua-table text); `tool.LoadSettings(path|table)`; `tool.Refresh()` (returns a NEW handle — reassign); `tool.Delete()`.
- Group/macro: `GroupOperator` with published `Inputs = ordered() { … }`; `group.GetChildrenList()`; LoadSettings on a group may not refresh Edit-page published-input order until reselected in the GUI [community/kernel].

Frame numbers inside a comp are COMP-local (0 = first frame of the clip, unless the comp's GlobalStart is the timeline frame — read `COMPN_GlobalStart`). Timeline items' Fusion comps in Resolve run from 0.

## Text+ (TextPlus) — the workhorse for titles [MEASURED 2026-09-01 workstation 21.0.4.5 — see the verified block after the sample]
```python
comp = item.GetFusionCompByIndex(1)
comp.Lock(); comp.StartUndo("bridge title")
tp = list(comp.GetToolList(False, "TextPlus").values())[0]
tp.SetInput("StyledText", "LAMBORGHINI REVUELTO")
tp.SetInput("Font", "League Gothic"); tp.SetInput("Style", "Regular")
tp.SetInput("Size", 0.12)                # fraction of frame height (0.08 ≈ 8 %)
tp.SetInput("Red1", 1.0); tp.SetInput("Green1", 0.85); tp.SetInput("Blue1", 0.0); tp.SetInput("Alpha1", 1.0)   # shading element 1 = fill
tp.SetInput("Center", {1: 0.5, 2: 0.68})  # Point: table {x, y} in 0..1 (0.5,0.5 = frame center); some builds want {"X":..,"Y":..} — probe GetInput("Center") first
tp.SetInput("HorizontalJustification", 1)  # 0 left? 1 center 2 right — verify per build (note.com lists HorizontalAnchor -1/0/1)
tp.SetInput("VerticalJustification", 1)
tp.SetInput("LineSpacing", 1.0); tp.SetInput("CharacterSpacing", 1.05)
comp.EndUndo(True); comp.Unlock()
```
Other useful Text+ inputs seen in the wild: `Tracking`, `LineSpacing`, `Enabled1..8` + `ElementShape1` (shading layers: 1 fill, 2 outline "Outline", 3 shadow), `Opacity1`, `Thickness2` (outline width), `ShadowOffsetX/Y`, `WriteOn` (0..1, the "Write On" reveal — animate it with a BezierSpline for word-by-word), `LayoutType`, `Wrap`. Hover a control in the Fusion Inspector: the status bar shows `Text1: <ID>`.

### Verified Text+ facts [measured 2026-09-01, workstation, Studio 21.0.4.5, title inserted by `InsertFusionTitleIntoTimeline("Text+")`]
- The tool is named **`Template`** (`TOOLS_Name`), `TOOLS_RegID` = `TextPlus`; comp tools are `{Template: TextPlus, MediaOut1: Saver}`. Comp `CustomData.TEMPLATE_ID` = "Text+". Select it with `comp.GetToolList(False, "TextPlus")`, never by name "Text1".
- `GetInputList()` returns **309** inputs keyed 1..309 (NOT by ID); read values with `tp.GetInput("<ID>")`, never `GetInput(k)` with the numeric key (returns None). Full ID list: `live-dump-2026-09-01.json` → `textplus_input_ids`.
- Defaults: `StyledText`="Custom Title", `Font`="Open Sans", `Style`="Semibold", `Size`=0.09, `Center`={1:0.5, 2:0.5, 3:0.0}, `Red1/Green1/Blue1/Alpha1`=1.0, `VerticalJustificationNew`=3, `HorizontalJustificationNew`=3, `VerticalJustification`=3, `HorizontalJustification`=**None** (do not use the un-suffixed horizontal ID), `LineSpacing`=1, `CharacterSpacing`=1, `Wrap`=0, `Layout`=1, `LayoutType`=0, `Enabled1`=1, `Enabled2..4`=0, `Name1..4` = "White Solid Fill" / "Red Outline" / "Black Shadow" / "Blue Border", `UseFrameFormatSettings`=1, `Width/Height`=1920/1080, `GlobalIn/Out`=0/119, `Background`=1 with `Red/Green/Blue/Alpha`=0.
- `SetInput("StyledText", s)` returns **None** even on success — verify with `GetInput`. Same for every SetInput.
- **Center** accepts `{1: x, 2: y}` AND `[x, y]`; read-back is always `{1: x, 2: y, 3: 0.0}`. `tp.Center = [x, y]` (attribute assignment) and `SetInput("Center", 0.5, 0)` (scalar) are silently IGNORED — use SetInput with a dict/list.
- Colours: `Red1/Green1/Blue1` set 1.0/0.2/0.1 read back exactly.
- **Justification**: `HorizontalJustificationNew` accepts 0,1,2,3 and reads back the same; the default 3 is what the GUI shows as "Center". The Fusion combo order is Left/Center/Right + a "New/Justified" mode; which integer is Left vs Right was NOT rendered and checked — set it, render one frame, look, then record here. Same for `VerticalJustificationNew`.
- **Shading elements**: `Enabled2` (outline) reads null-ish for its sub-inputs (`Thickness2`, `Red2`, `ElementShape2`…) until enabled; after `SetInput("Enabled2",1)` `Thickness2`=0.05 writes and reads back and `ElementShape2`=1. Shadow = element 3, border = element 4, per `Name3/Name4`.
- **Keyframes from script WORK**: `tp.Size = comp.BezierSpline(); tp.Size[0] = 0.05; tp.Size[24] = 0.12` → `tp.Size.GetKeyFrames()` = `{1: 0.0, 2: 24.0}` (a table of frame numbers, not values), `GetInput("Size", t)` at 0/12/24 = 0.05/0.085/0.12 (linear interpolation), `tp.Size.ID` = "Size". The exported `.setting` shows the modifier as `TemplateSize = BezierSpline { KeyFrames = { [0] = { 0.05, RH = {8, 0.0733}, Flags = { Linear = true } }, [24] = { 0.12, LH = {16, 0.0967}, Flags = { Linear = true } } } }` and `Size = Input { SourceOp = "TemplateSize", Source = "Value" }`.
- `item.ExportFusionComp(path, 1)` → True and writes a Lua-table `.setting` (`Composition { … Tools = { Template = TextPlus { Inputs = { … } }, TemplateSize = BezierSpline {…}, MediaOut1 = Saver {…} } }`, `RenderRange = {0, 119}`). Only NON-default inputs are serialized — a good way to see exactly which IDs a GUI-authored template uses.
- `TimelineItem.GetProperty("ZoomX"/"Pan"/"Opacity")` on the title = 1.0/0.0/100.0 — clip-level transform is separate from the Fusion `Center`.

Adding a title from script: `item = tl.InsertFusionTitleIntoTimeline("Text+")` inserts at the playhead (set with `tl.SetCurrentTimecode`) on the lowest empty video track, default 5 s, and returns the TimelineItem. It accepts no clipInfo (position/duration cannot be set through it) [community]. Alternatives: (1) prepare a Text+ generator in the media pool once (GUI: drag Text+ from Effects → Media Pool) and then `AppendToTimeline([{mediaPoolItem: textplusItem, startFrame, endFrame, recordFrame, trackIndex}])` for exact geometry, then edit its comp [community "Script Hack" forum t=190118]; (2) render titles to ProRes 4444 with alpha and treat them as media (deterministic, brand-safe, what v11 did).
Text+ templates: `item.ImportFusionComp(path.setting)` loads a saved comp onto the item; `item.ExportFusionComp(path, 1)` saves one. Author the animated brand template ONCE in the GUI (Write-On, scale pop, glow), save the `.setting`, then only `SetInput` the published text/colour values from scripts. `AddFusionComp()` results were reported not to persist in some builds — prefer titles inserted by Resolve (which already have a comp) or ImportFusionComp.

## Motion inside a clip (punch-in, push, whip) via Fusion
For a video clip: `comp = item.AddFusionComp()` (or the existing one); the graph is `MediaIn1 → MediaOut1`. Insert `xf = comp.Transform()` , `xf.Input = comp.MediaIn1.Output; comp.MediaOut1.Input = xf.Output`; animate `xf.Size` with `AddModifier("Size","BezierSpline")` and frame-indexed assignments; `xf.Center` for pans (Point). Ease: edit spline handles via `SetKeyFrames` (LH/RH) or use `AdjustKeyFrames`. Cheaper for reels: FFmpeg `zoompan`/`crop` upstream — the Fusion route costs a Fusion render per frame on an 8 GB card.

## Color
- LUT on a node: `g = item.GetNodeGraph(); g.SetLUT(1, "MyTools/Cinematic 10.cube")` (path relative to the LUT root or absolute; `project.RefreshLUTList()` after copying a new cube). `g.GetLUT(1)` reads back.
- CDL: `item.SetCDL({"NodeIndex":"1","Slope":"1 1 1","Offset":"0 0 0","Power":"1 1 1","Saturation":"1"})` — strings, space-separated triples.
- Stills/DRX: grab a hero grade (`tl.GrabStill()`, export via the album with format `drx`, or `project.ExportCurrentFrameAsStill(path.drx)` on the Color page), then `g.ApplyGradeFromDRX(path, 0)` on targets — replaces the whole node graph.
- Copy a grade: `hero.CopyGrades([others])`. Groups: `grp = project.AddColorGroup("A-roll")`, `item.AssignToColorGroup(grp)`, grade `grp.GetPostClipNodeGraph()` once.
- Versions: `item.AddVersion("v2", 0)`, `LoadVersionByName`. Node count/labels/tools are readable; individual grade controls (lift/gamma/gain) are NOT — compute looks offline (LUT/CDL) and apply.
- Color management: set `colorScienceMode` first, then `colorSpaceInput/Timeline/Output`; ACES contexts lock custom spaces. MyTools default = DaVinci YRGB, Rec.709 Gamma 2.4, LUT look.
- Caches on an 8 GB card: `item.SetColorOutputCache(1)` for NR/grain-heavy items; `SetFusionOutputCache(1)` for Fusion titles.

## Audio (what exists, what doesn't)
- Exists: tracks (add/delete/name/lock/enable, subtype), `AutoSyncAudio`, per-track and per-item Voice Isolation (`Timeline.SetVoiceIsolationState(1, {"isEnabled": True, "amount": 40})` measured True on 21.1), `GetAudioMapping`/`GetSourceAudioChannelMapping` (+ 21.1 setters `SetAudioMapping`/`SetSourceAudioChannelMapping`, JSON `track_mapping`), `ApplyFairlightPresetToCurrentTimeline`, `GenerateSpeech` (TTS Extras pack; returns an error STRING when it is missing), `TranscribeAudio` + **21.1 `GetTranscription()` readback**, `CreateSubtitlesFromAudio`, **21.1 `Timeline.NormalizeAudioLevel([items], {"targetLevel": -12.0})` / `GetNormalizeAudioModes()` (14 modes)**, 21.1 static per-item `AudioVolume` (-100..30 dB) / `AudioPan` / pitch / dialogue-leveler properties via `SetProperties`, `SetFades` on audio items.
- Does NOT exist: volume/pan AUTOMATION (keyframes), EQ, compressor, bus routing, ducking, IntelliCut, Music Remixer, Dialogue Separator, loudness metering readback.
- Therefore the MyTools audio chain is FFmpeg upstream (vo-process → audio-mix → two-pass loudnorm) and Resolve only places finished stems. Keep music/VO/SFX on separate audio tracks (A1 VO, A2 music, A3 SFX) for the editor's benefit even though we mix upstream.

## Captions
- Kinetic karaoke (short-form): ASS via pysubs2, burned by FFmpeg AFTER the Resolve render (grade first, burn last). Fonts: Montserrat/Anton bold, white + yellow active word, 3 px black outline, ≤3 words, 64 pt @1080×1920, ~68 % height, inside the 900×1400 safe box.
- Long-form soft captions: `tl.CreateSubtitlesFromAudio({resolve.SUBTITLE_LANGUAGE: resolve.AUTO_CAPTION_SPANISH, resolve.SUBTITLE_CHARS_PER_LINE: 42, resolve.SUBTITLE_LINE_BREAK: resolve.AUTO_CAPTION_LINE_DOUBLE})` (Studio, language pack may need the Extras manager) → read back with `for it in tl.GetItemListInTrack("subtitle", 1): (it.GetStart(), it.GetDuration(), it.GetName())` → export SRT via render `ExportSubtitle: True, SubtitleFormat: "SeparateFile"` (or write the SRT yourself from the walk). Spanish QA: ≤17 CPS, ≤42 CPL, ≥0.8 s on screen, `¿¡` and car-model nouns preserved.
- Word-level timing for karaoke always comes from harness STT (`offload_transcribe` on a ≤30 s stem, hotwords = car lexicon); Resolve's transcript is a cross-check only.
