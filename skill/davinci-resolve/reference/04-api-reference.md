# 04 — Scripting API condensed catalog (Resolve Studio 21.0.4.5, README Last Updated 24 Jul 2026)

> **21.1 (2026-09-09):** the source is now `DaVinciResolveScript.pyi` (typed, with docstrings) +
> `README.md` in the same folder; `README.txt` is gone. `api-catalog-21.1.md` in this folder is a
> flat extract of the .pyi — grep THAT for a signature. New 21.1 methods and the claims below that
> 21.1 overturned (transcript readback, transitions/fades/speed, multicam, frame-rate lock,
> keyframe-mode return, render-preset export path) are in `09-resolve-21-1.md` §4–§5.

Source: `C:\ProgramData\Blackmagic Design\DaVinci Resolve\Support\Developer\Scripting\README.txt`
on either box. This is a working digest, not a replacement — when a signature matters, grep the
README (`grep -n "MethodName" README.txt`). Lists are Python lists; Lua uses indexed tables.
Indices are **1-based** everywhere (tracks, nodes since 16.2, timelines, takes, comps).
Return conventions: Bool for setters (check it), object or **None** for getters that fail,
`[]`/`{}` for empty lists. Deprecated dict-returning variants (`GetProjectsInCurrentFolder`,
`GetRenderJobs`, `GetItemsInTrack`, `GetFlags`, `GetNumNodes`/`SetLUT` on TimelineItem) still
exist — prefer the list forms and `GetNodeGraph()`.

## Object chain
`resolve → GetProjectManager() → Project → GetMediaPool() / GetCurrentTimeline() / GetGallery()`
`resolve.Fusion()` → Fusion app object (Fusion scripting API, see 07). `resolve.GetMediaStorage()` → file browser.

## Resolve
- `Fusion()`, `GetMediaStorage()`, `GetProjectManager()`
- `OpenPage(name)` / `GetCurrentPage()` → one of `media photo cut edit fusion color fairlight deliver` or **None** (page-null state)
- `GetProductName()` ("DaVinci Resolve Studio"), `GetVersion()` → [major,minor,patch,build,suffix], `GetVersionString()` ("21.0.4.5")
- Layout presets: `GetLayoutPresetList / LoadLayoutPreset / UpdateLayoutPreset / ExportLayoutPreset(name, path) / DeleteLayoutPreset / SaveLayoutPreset / ImportLayoutPreset(path[, name])`
- Render presets (file level): `ImportRenderPreset(path)` (sets it current), `ExportRenderPreset(name, path)` (measured False for the first stock preset on 20.3 — may refuse stock presets)
- Burn-in presets: `GetBurnInPresetList / DeleteBurnInPreset / ImportBurnInPreset / ExportBurnInPreset`
- User prefs presets: `GetUserPreferencesPresetList / Load… / Save… / Delete… / Import…(path[,name]) / Export…(name, path)`
- `GetKeyframeMode()` / `SetKeyframeMode(mode)` — `KEYFRAME_MODE_ALL=0, _COLOR=1, _SIZING=2` (setting to the current value returned False on 20.3)
- `GetFairlightPresets()`, `DisableBackgroundTasksForCurrentResolveSession()` (no re-enable), `Quit()`

## ProjectManager
- `CreateProject(name[, mediaLocationPath])` → Project or None if name exists (needs a page open on our boxes)
- `LoadProject(name)` → Project or None (None also when it is already current); `GetCurrentProject()`; `SaveProject()`; `CloseProject(project)` (no save)
- `DeleteProject(name)` (not the loaded one); `ArchiveProject(name, path, isArchiveSrcMedia=True, isArchiveRenderCache=True, isArchiveProxyMedia=False)` (returned False in the 20.3 probe for .dra and folder paths — treat as GUI-only until measured here)
- Folders: `CreateFolder / DeleteFolder / GetProjectListInCurrentFolder / GetProjectAttributesInCurrentFolder` (attrs: lastModifiedDate, creationDate, notes, liveCollaborationMode) `/ GetFolderListInCurrentFolder / GotoRootFolder / GotoParentFolder / GetCurrentFolder / OpenFolder`
- `ImportProject(path[, name])` (.drp; the working round-trip path), `ExportProject(name, path, withStillsAndLUTs=True)`, `RestoreProject(path[, name])` (returned False on a plain .drp — for .dra archives)
- DB: `GetCurrentDatabase()` → {DbType:'Disk'|'PostgreSQL', DbName, IpAddress?}; `GetDatabaseList()`; `SetCurrentDatabase({...})` (closes the open project!)
- Cloud: `CreateCloudProject / LoadCloudProject / ImportCloudProject / RestoreCloudProject` with `{resolve.CLOUD_SETTING_PROJECT_NAME, …_PROJECT_MEDIA_PATH, …_IS_COLLAB, …_SYNC_MODE (CLOUD_SYNC_NONE|PROXY_ONLY|PROXY_AND_ORIG), …_IS_CAMERA_ACCESS}` — not our workflow

## Project
- `GetMediaPool()`, `GetName()`, `SetName()`, `GetUniqueId()`, `GetGallery()`
- Timelines: `GetTimelineCount()`, `GetTimelineByIndex(i)`, `GetCurrentTimeline()`, `SetCurrentTimeline(tl)` (switches the GUI)
- Settings: `GetSetting([key])` (no key = full dict, 158 keys on 21.0.4.5 — see live dump), `SetSetting(key, valueString)`; `GetPresetList()` / `SetPreset(name)` (project presets: "Current Project", "System Config", "guest default config")
- Render: `GetRenderFormats()` {desc→ext}, `GetRenderCodecs(fmt)` {desc→**id**}, `GetCurrentRenderFormatAndCodec()`, `SetCurrentRenderFormatAndCodec(fmtExt, codecId)`, `GetRenderResolutions([fmt, codec])` [{Width,Height}], `GetCurrentRenderMode()`/`SetCurrentRenderMode(0 individual|1 single)`, `GetRenderPresetList()`, `LoadRenderPreset(name)`, `SaveAsNewRenderPreset(name)`, `DeleteRenderPreset(name)`, `SetRenderSettings({…})` (keys in 06), `AddRenderJob()`→jobId str (may be ""), `DeleteRenderJob(id)`, `DeleteAllRenderJobs()`, `GetRenderJobList()`, `StartRendering([ids], isInteractiveMode=False)` / `StartRendering()` all, `StopRendering()`, `IsRenderingInProgress()`, `GetRenderJobStatus(id)` → {JobStatus, CompletionPercentage, EstimatedTimeRemainingInMs, TimeTakenToRenderInMs, Error} (or an error STRING), `GetQuickExportRenderPresets()`, `RenderWithQuickExport(preset, {TargetDir, CustomName, VideoQuality, EnableUpload})` → status dict or error string (renders the current timeline immediately)
- `RefreshLUTList()` (call after copying a .cube into the LUT folder so `SetLUT` can find it)
- Color groups: `GetColorGroupsList()`, `AddColorGroup(name)`, `DeleteColorGroup(group)`
- `InsertAudioToCurrentTrackAtPlayhead(path, startOffsetInSamples, durationInSamples)` (Fairlight page, selected track; returned False in probes — flaky)
- `LoadBurnInPreset(name)`, `ExportCurrentFrameAsStill(path)` (.jpg/.png/.tif/.dpx/.drx; page-dependent), `ApplyFairlightPresetToCurrentTimeline(name)`
- AI: `GenerateSpeech({TextInput ≤350 chars, VoiceModel "Female 1"|"Male 1"|"Custom Voice", CustomVoiceFile, Speed, Variation, Pitch, GenerationID, Filename, AddToTimeline, AudioTrack}, timecode)` → MediaPoolItem (needs AI Speech Generator extra); `ResetIntellisearchAnalysis()`

## MediaStorage
`GetMountedVolumeList()`, `GetSubFolderList(path)`, `GetFileList(path)` (consolidated sequences), `RevealInStorage(path)`,
`AddItemListToMediaPool(paths…| [paths] | [{media, startFrame, endFrame}])` → [MediaPoolItem] (subclip import), `AddClipMattesToMediaPool(item, [paths], stereoEye)`, `AddTimelineMattesToMediaPool([paths])`

## MediaPool
- `GetRootFolder()`, `AddSubFolder(folder, name)`, `GetCurrentFolder()`, `SetCurrentFolder(folder)`, `RefreshFolders()`, `GetUniqueId()`
- `ImportMedia([paths])` / `ImportMedia([{FilePath:"f_%03d.dpx", StartIndex, EndIndex}])` → [MediaPoolItem] (imports into the CURRENT folder — set it first)
- `CreateEmptyTimeline(name)` → Timeline (None if the name exists or page-null); `CreateTimelineFromClips(name, [clips] | [{mediaPoolItem,startFrame,endFrame,recordFrame}])`
- `AppendToTimeline([clips])` / `AppendToTimeline([{mediaPoolItem, startFrame, endFrame, mediaType 1 video|2 audio, trackIndex, recordFrame}])` → [TimelineItem] — appends to the CURRENT timeline; the ONLY placement primitive
- `ImportTimelineFromFile(path, {timelineName, importSourceClips=True, sourceClipsPath, sourceClipsFolders, interlaceProcessing})` — AAF/EDL/XML/FCPXML/DRT/ADL/OTIO (DRT ignores name/importSourceClips)
- `DeleteTimelines([tl])`, `DeleteClips([items])`, `DeleteFolders([folders])`, `MoveClips([items], folder)`, `MoveFolders`, `ImportFolderFromFile(drbPath[, sourceClipsPath])`
- `RelinkClips([items], folderPath)`, `UnlinkClips([items])`, mattes: `GetClipMatteList / GetTimelineMatteList / DeleteClipMattes`
- `ExportMetadata(csvPath[, clips])`, `GetSelectedClips()` (None when empty), `SetSelectedClip(item)`
- `AutoSyncAudio([items ≥1 video + ≥1 audio], {resolve.AUDIO_SYNC_MODE: AUDIO_SYNC_WAVEFORM|AUDIO_SYNC_TIMECODE, AUDIO_SYNC_CHANNEL_NUMBER: n|AUDIO_SYNC_CHANNEL_AUTOMATIC(-1)|AUDIO_SYNC_CHANNEL_MIX(-2), AUDIO_SYNC_RETAIN_EMBEDDED_AUDIO, AUDIO_SYNC_RETAIN_VIDEO_METADATA})` (content-dependent; returned False on synthetic media)
- `CreateStereoClip(L, R)`

## Folder
`GetClipList()`, `GetName()`, `GetSubFolderList()`, `GetIsFolderStale()`, `GetUniqueId()`, `Export(drbPath)`,
AI (Studio + Extras): `TranscribeAudio(useSpeakerDetection=None)`, `ClearTranscription()`, `PerformAudioClassification()`, `ClearAudioClassification()`, `RemoveMotionBlur({deblurOption})` → [[orig,new]…], `AnalyzeForIntellisearch(identifyFaces, isBetterMode)`, `AnalyzeForSlate(resolve.MARKER_<COLOR>)`

## MediaPoolItem
- `GetName()/SetName()`, `GetMediaId()`, `GetUniqueId()`, `GetTimeline()` (if it is a timeline clip)
- `GetMetadata([key])/SetMetadata(key, v | {dict})`, `GetThirdPartyMetadata/SetThirdPartyMetadata`
- `GetClipProperty([key])` → all clip attributes when no key (e.g. `File Path`, `File Name`, `FPS`, `Duration`, `Frames`, `Resolution`, `Start TC`, `End TC`, `Start`, `End`, `Type`, `Video Codec`, `Audio Ch`, `Alpha mode`, `Super Scale` …) / `SetClipProperty(key, v)` (some read-only; `Super Scale` 1..4 with optional sharpness/noise floats for 2x Enhanced; `FPS` reinterpret; `Alpha mode` "None" fixes transparent VFX clips [community])
- Markers: `AddMarker(frameId, color, name, note, duration[, customData])`, `GetMarkers()` → {frame: {color,duration,note,name,customData}}, `GetMarkerByCustomData`, `UpdateMarkerCustomData`, `GetMarkerCustomData`, `DeleteMarkersByColor(color|"All")`, `DeleteMarkerAtFrame`, `DeleteMarkerByCustomData`
- Flags/colors: `AddFlag(color)`, `GetFlagList()`, `ClearFlags(color|"All")`, `GetClipColor/SetClipColor/ClearClipColor`
- Marks: `GetMarkInOut()` → {video:{in,out}, audio:{in,out}}, `SetMarkInOut(in, out, type="all"|"video"|"audio")`, `ClearMarkInOut(type)`
- Proxy/relink: `LinkProxyMedia(path)`, `LinkFullResolutionMedia(path)` (20+), `UnlinkProxyMedia()`, `ReplaceClip(path)`, `ReplaceClipPreserveSubClip(path)`, `MonitorGrowingFile()`
- `GetAudioMapping()` → JSON string (embedded_audio_channels, linked_audio{…}, track_mapping{…})
- AI: `TranscribeAudio(useSpeakerDetection=None[, transcribeAsNestedClip])` (21.0.4: NO text getter; **21.1: `GetTranscription([useNestedClipTranscription])` → {language, segments[{start,end,text,speaker,words[{start,end,text}]}]}**, synchronous transcribe, page-dependent — 09 §5), `ClearTranscription`, `PerformAudioClassification`, `ClearAudioClassification`, `RemoveMotionBlur({FileName, Format, Codec, EncodingProfile, UseExtremeMode, UseMarkInMarkOut, RenderAtSourceRes, UseMoreGpuMemory, Encoder})` → new item, `AnalyzeForIntellisearch`, `AnalyzeForSlate`; 21.1 also `SetAudioMapping(jsonString)`

## Timeline
- `GetName/SetName`, `GetStartFrame()`, `GetEndFrame()`, `GetStartTimecode()/SetStartTimecode("01:00:00:00")`, `GetUniqueId()`, `GetMediaPoolItem()`
- Tracks: `GetTrackCount(type)` type ∈ video|audio|subtitle; `AddTrack(type[, subType | {audioType, index}])` audio subtypes mono|stereo|lrc|lcr|lrcs|lcrs|quad|5.0|5.0film|5.1|5.1film|7.0|7.0film|7.1|7.1film|adaptive1..36; `DeleteTrack(type, i)`, `GetTrackSubType`, `SetTrackEnable/GetIsTrackEnabled`, `SetTrackLock/GetIsTrackLocked`, `GetTrackName/SetTrackName`
- Items: `GetItemListInTrack(type, i)` (subtitle track items: `GetName()` is the caption text [community]), `GetSelectedClips()` (includes linked audio), `GetCurrentVideoItem()`, `DeleteClips([items], ripple=False)`, `SetClipsLinked([items], bool)`
- Markers: same family as MediaPoolItem (frameId is TIMELINE-relative offset, e.g. 96 = start+96)
- Playhead: `GetCurrentTimecode()`, `SetCurrentTimecode("00:00:10:00")` (Cut/Edit/Color/Fairlight/Deliver pages)
- `GetCurrentClipThumbnailImage()` → {width,height,format,data(base64 RGB8)} (Color page)
- `DuplicateTimeline([name])`, `CreateCompoundClip([items], {startTimecode, name})`, `CreateFusionClip([items])`
- `ImportIntoTimeline(aafPath, {autoImportSourceClipsIntoMediaPool, ignoreFileExtensionsWhenMatching, linkToSourceCameraFiles, useSizingInfo, importMultiChannelAudioTracksAsLinkedGroups, insertAdditionalTracks, insertWithOffset, sourceClipsPath, sourceClipsFolders})` (AAF only)
- `Export(path, exportType[, subtype])` — types: `EXPORT_AAF (subtype EXPORT_AAF_NEW|EXPORT_AAF_EXISTING) · EXPORT_DRT · EXPORT_EDL (EXPORT_CDL|EXPORT_SDL|EXPORT_MISSING_CLIPS|EXPORT_NONE) · EXPORT_FCP_7_XML · EXPORT_FCPXML_1_8/1_9/1_10 · EXPORT_HDR_10_PROFILE_A/B · EXPORT_TEXT_CSV · EXPORT_TEXT_TAB · EXPORT_DOLBY_VISION_VER_2_9/4_0/5_1 · EXPORT_OTIO · EXPORT_ALE · EXPORT_ALE_CDL`
- `GetSetting([key])/SetSetting(key, v)` — timeline overrides; set `useCustomSettings="1"` first (it RESETS several color/resolution keys to defaults [community forum t=212784] — set it FIRST, then the specific keys, then read back)
- Generators/titles: `InsertGeneratorIntoTimeline("Solid Color")`, `InsertFusionGeneratorIntoTimeline(name)`, `InsertFusionCompositionIntoTimeline()`, `InsertOFXGeneratorIntoTimeline(name)`, `InsertTitleIntoTimeline("Text")`, `InsertFusionTitleIntoTimeline("Text+")` → TimelineItem placed at the PLAYHEAD on the first free track, default duration (5 s), no clipInfo
- Stills: `GrabStill()`, `GrabAllStills(1 first|2 middle)`
- AI: `CreateSubtitlesFromAudio({resolve.SUBTITLE_LANGUAGE: AUTO_CAPTION_SPANISH|…, SUBTITLE_CAPTION_PRESET: AUTO_CAPTION_SUBTITLE_DEFAULT|TELETEXT|NETFLIX, SUBTITLE_CHARS_PER_LINE 1..60 (42 default), SUBTITLE_LINE_BREAK: AUTO_CAPTION_LINE_SINGLE|DOUBLE, SUBTITLE_GAP 0..10})` → subtitle track; `DetectSceneCuts()`; `ConvertTimelineToStereo()`; `AnalyzeDolbyVision([items], analysisType)`
- Voice isolation per track: `GetVoiceIsolationState(trackIndex)` / `SetVoiceIsolationState(trackIndex, {isEnabled, amount 0..100})`
- `GetNodeGraph()` (timeline-level grade graph), marks: `GetMarkInOut/SetMarkInOut/ClearMarkInOut`

## TimelineItem
- Identity/geometry: `GetName/SetName` (subtitle text for subtitle items), `GetUniqueId`, `GetTrackTypeAndIndex()` → [type, idx], `GetStart([subframe])`, `GetEnd([subframe])`, `GetDuration([subframe])` (timeline frames; **end is exclusive** in the half-open sense used by AppendToTimeline), `GetLeftOffset()`/`GetRightOffset()` (available handle frames), `GetSourceStartFrame/GetSourceEndFrame/GetSourceStartTime/GetSourceEndTime`, `GetMediaPoolItem()`, `GetLinkedItems()`, `SetClipEnabled(bool)/GetClipEnabled()`
- Inspector-style properties: `GetProperty([key])`, `SetProperty(key, v)` — keys: `Pan, Tilt, ZoomX, ZoomY, ZoomGang, RotationAngle, AnchorPointX, AnchorPointY, Pitch, Yaw, FlipX, FlipY, CropLeft/Right/Top/Bottom, CropSoftness, CropRetain, DynamicZoomEase (0 linear,1 in,2 out,3 in&out), CompositeMode (0 Normal … 27 Darker Color; 28 Foreground,29 Alpha,30 Inverted Alpha,31 Lum,32 Inverted Lum), Opacity 0..100, Distortion -1..1, RetimeProcess (0 project,1 nearest,2 frame blend,3 optical flow), MotionEstimation (0..6), Scaling (0 project,1 crop,2 fit,3 fill,4 stretch), ResizeFilter (0..15)`. Values beyond range are clipped. **Static values only — no keyframes via this API.** Dynamic Zoom itself (the start/end rects) is not exposed; only its ease. **21.1:** `GetProperties()`/`SetProperties(dict)` (all-or-nothing validation; 32 keys incl. `AudioVolume`, `AudioPan`, pitch, voice isolation, dialogue leveler and the `*Enabled` section flags), `GetFades/SetFades({FadeIn,FadeOut})`, `GetSpeed/SetSpeed({Percentage, PitchCorrection, StretchKeyframesToFit, RippleTimeline})`, `AddTransition({type, category, position, alignment, duration})`, `GetType()`, `Get/SetOutputBlanking`, `FlattenMulticam`, `PerformMulticamSmartSwitch` — 09 §4–§5.
- Markers/flags/color: same family as MediaPoolItem (frameId is CLIP-relative)
- Fusion: `GetFusionCompCount()`, `GetFusionCompByIndex(i)`, `GetFusionCompNameList()`, `GetFusionCompByName`, `AddFusionComp()`, `ImportFusionComp(path)`, `ExportFusionComp(path, i)`, `DeleteFusionCompByName`, `LoadFusionCompByName`, `RenameFusionCompByName`
- Color: `AddVersion(name, 0 local|1 remote)`, `GetCurrentVersion()`, `DeleteVersionByName`, `LoadVersionByName`, `RenameVersionByName`, `GetVersionNameList(type)`, `SetCDL({NodeIndex:"1", Slope:"r g b", Offset:"r g b", Power:"r g b", Saturation:"s"})`, `CopyGrades([targets])`, `GetNodeGraph([layerIdx])` → Graph, `GetColorGroup/AssignToColorGroup/RemoveFromColorGroup`, `ExportLUT(resolve.EXPORT_LUT_17PTCUBE|33PTCUBE|65PTCUBE|PANASONICVLUT, path)`, `ResetAllNodeColors()`, `LoadBurnInPreset(name)`, `UpdateSidecar()`
- Takes: `AddTake(item[, start, end])`, `GetSelectedTakeIndex`, `GetTakesCount`, `GetTakeByIndex`, `DeleteTakeByIndex`, `SelectTakeByIndex`, `FinalizeTake`
- Caches: `GetIsColorOutputCacheEnabled/SetColorOutputCache(v)`, `GetIsFusionOutputCacheEnabled/SetFusionOutputCache(v)` v ∈ `CACHE_AUTO_ENABLED=-1, CACHE_DISABLED=0, CACHE_ENABLED=1`
- AI: `CreateMagicMask("F"|"B"|"BI")`, `RegenerateMagicMask()`, `Stabilize()`, `SmartReframe()` (uses project/timeline reframe settings), `GetVoiceIsolationState()/SetVoiceIsolationState({isEnabled, amount})`, `GetSourceAudioChannelMapping()`, stereo getters

## Graph (node graph of an item / timeline / color-group pre/post)
`GetNumNodes()`, `SetLUT(nodeIndex, lutPath)` (absolute, or relative to the LUT folder; must already be discovered — `RefreshLUTList`), `GetLUT(i)`, `SetNodeCacheMode(i, v)`/`GetNodeCacheMode`, `GetNodeLabel(i)`, `GetToolsInNode(i)`, `SetNodeEnabled(i, bool)`, `ApplyGradeFromDRX(path, 0 no keyframes|1 source TC aligned|2 start frames aligned)` (REPLACES the graph), `ApplyArriCdlLut()`, `ResetAllGrades()`

## ColorGroup
`GetName/SetName`, `GetClipsInTimeline([tl])`, `GetPreClipNodeGraph()`, `GetPostClipNodeGraph()`

## Gallery / GalleryStillAlbum
`GetGalleryStillAlbums()`, `GetGalleryPowerGradeAlbums()`, `GetCurrentStillAlbum()/SetCurrentStillAlbum`, `CreateGalleryStillAlbum()`, `CreateGalleryPowerGradeAlbum()`, `GetAlbumName/SetAlbumName`; album: `GetStills()`, `GetLabel/SetLabel`, `ImportStills([paths])`, `ExportStills([stills], folder, prefix, format dpx|cin|tif|jpg|png|ppm|bmp|xpm|drx)` (page-dependent: may return False unless the Color page gallery is open), `DeleteStills`

## Marker colors (all accepted on 20.3+)
Blue Cyan Green Yellow Red Pink Purple Fuchsia Rose Lavender Sky Mint Lemon Sand Cocoa Cream (`resolve.MARKER_BLUE` … constants exist for `AnalyzeForSlate`). Clip/flag colors use the same names. `customData` is invisible in the UI — store JSON there to tag machine-made markers (`{"by":"bridge","kind":"cut","v":1}`) so you can find/delete only yours with `GetMarkerByCustomData`/`DeleteMarkerByCustomData`.

## Project settings keys that matter (full list of 158 in the live dump)
`timelineFrameRate` ("29.97", "29.97 DF", 24.0 …) · `timelinePlaybackFrameRate` · `timelineDropFrameTimecode` · `timelineResolutionWidth/Height` · `timelineOutputResolutionWidth/Height` · `timelineOutputResMatchTimelineRes` · `timelineInputResMismatchBehavior` (scaleToFit|scaleToCrop|stretch|center… — values match the UI menu, verify by snapshot) · `timelineOutputResMismatchBehavior` · `timelinePixelAspectRatio` (square) · `timelineInterlaceProcessing` · `timelineSampleRate` (48000) · `colorScienceMode` (davinciYRGB | davinciYRGBColorManagedv2 | acesccap1…; set FIRST when changing color management) · `colorSpaceInput/Timeline/Output` (+`…Gamma` when `separateColorSpaceAndGamma`=1) · `rcmPresetMode` · `isAutoColorManage` · `inputDRT/outputDRT` · `superScale` (0 auto,1 none,2,3,4; 2x Enhanced = 4 args) · `superScaleSharpness/NoiseReduction` · `imageResizeMode` (sharper|smoother|bicubic|bilinear|bessel|box|catmull-rom|cubic|gaussian|lanczos|mitchell|nearest|quadratic|sinc) · `imageRetimeInterpolation` (nearest|frameBlend|opticalFlow) · `imageMotionEstimationMode` (standardFaster|standardBetter|enhancedFaster|enhancedBetter|speedWarpBetter|speedWarpFaster) · `imageResizingGamma` · `perfProxyMediaMode` (0 off,1 when available,2 when source not available) · `perfProxyResolutionRatio` · `perfOptimisedMediaOn` · `perfOptimisedCodec` (apch = ProRes 422 HQ) · `perfRenderCacheMode` (none|user|auto) · `perfRenderCacheCodec` · `perfCacheClipsLocation` · `perfAutoRenderCacheAfterTime` (5) · `perfAutoRenderCacheFuEffect/Transition/Composite` · `colorGalleryStillsLocation` · `nodeStackLayers` · `limitSubtitleCPL` (60) · `limitSubtitleCaptionDurationSec` (3) · `limitAudioMeterLUFS` (-23) · `transcriptionLanguage` (auto|…) · `speakerDetection` · `videoMonitorFormat` · `projectMediaLocation` · `timelineWorkingLuminance`.
Values are strings in/out except a few numbers (`timelineFrameRate` came back as float 24.0, `superScale` as int 1). Always read back after `SetSetting`; a False means rejected (locked because media/timelines exist, ACES context, "match timeline" behaviour, etc.).

## Timeline settings keys (per-timeline overrides, only honoured when `useCustomSettings`="1")
`useCustomSettings, timelineResolutionWidth/Height, timelineOutputResolutionWidth/Height, timelineFrameRate, timelinePixelAspectRatio, timelineInterlaceProcessing, timelineInputResMismatchBehavior, timelineOutputResMismatchBehavior, superScale…, colorScienceMode, colorSpaceTimeline/Output (+Gamma), isAutoColorManage, inputDRT/outputDRT, useCATransform, disableFusionToneMapping, videoMonitorFormat, hdrDolbyMasterDisplay, timelineWorkingLuminance…`. **Measured 2026-09-01 workstation, 21.0.4.5:** `Timeline.GetSetting()` returns EXACTLY the same 158 keys as `Project.GetSetting()` (set difference empty both ways) and, on a fresh timeline, identical values — the per-timeline dict is a full copy of the project dict, not a subset. `Timeline.SetSetting` rules measured: with `useCustomSettings`="0" every `timelineResolutionWidth/Height` and `timelineFrameRate` write returns False; `SetSetting("useCustomSettings","1")` → True, after which `timelineResolutionWidth/Height` writes → True and read back ("1080"/"1920" for a vertical timeline); `timelineFrameRate` STILL returns False after custom settings (frame rate is fixed at timeline creation — set it on the PROJECT before `CreateEmptyTimeline`, or create via `CreateTimelineFromClips`/`ImportTimelineFromFile` with the rate baked in). **Re-measured on 21.1 (09 §5): on an EMPTY timeline with `useCustomSettings`="1", `SetSetting("timelineFrameRate","25")` → True (start frame 86400→90000); it is locked only once clips exist.** Also 21.1: `Timeline.GetSettings()` with custom settings on returns **69** keys (the project's 158 when off). Fresh timeline facts: start frame 86400 = TC `01:00:00:00` at 24 fps, 1 video + 1 audio track (`GetTrackSubType("audio",1)` = "stereo"), 0 subtitle tracks; `InsertGeneratorIntoTimeline("Solid Color")` → 120 frames (5 s) at the playhead, `GetMediaPoolItem()` False, `GetFusionCompCount()` 0; `InsertFusionTitleIntoTimeline("Text+")` → 120 frames appended after it on V1 (took 6 ms), `GetFusionCompCount()` 1 named "Composition 1"; `GetProperty()` on either item returns the 21-key transform dict (`Pan, Tilt, ZoomX, ZoomY, ZoomGang, RotationAngle, AnchorPointX/Y, Pitch, Yaw, FlipX/Y, CropLeft/Right/Top/Bottom, CropSoftness, CropRetain, DynamicZoomEase, CompositeMode, Opacity`=100.0). `AddMarker(frame,"Blue","ref","note",1,"cd1")` → True and `GetMarkers()` keys are frames RELATIVE to the timeline start (`86410` when start is 86400 — i.e. absolute). `DeleteTimelines([tl])` → True.
