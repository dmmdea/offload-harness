## TypedDict AAFImportOptions: autoImportSourceClipsIntoMediaPool, ignoreFileExtensionsWhenMatching, linkToSourceCameraFiles, useSizingInfo, importMultiChannelAudioTracksAsLinkedGroups, insertAdditionalTracks, insertWithOffset, sourceClipsPath, sourceClipsFolders
## TypedDict AppendClipInfo: mediaPoolItem, startFrame, endFrame, mediaType, trackIndex, recordFrame
## TypedDict AudioSyncSettings: syncMode, channelNumber, retainEmbeddedAudio, retainVideoMetadata
## TypedDict AutoAlignOptions: SyncUsing, UseTrack
## TypedDict AutoCaptionSettings: language, captionPreset, charsPerLine, lineBreak, gap
## TypedDict CDL: NodeIndex, Slope, Offset, Power, Saturation
## TypedDict CloneStatus: JobStatus, CompletionPercentage, Error
## TypedDict CloneToolSettings: PreserveFolderName, ChecksumType
## TypedDict CloudSettings: projectName, projectMediaPath, isCollab, syncMode, isCameraAccess
## TypedDict CompoundClipOptions: startTimecode, name
## TypedDict CreateTimelineClipInfo: mediaPoolItem, startFrame, endFrame, recordFrame
## TypedDict DatabaseInfo: DbType, DbName, IpAddress
## TypedDict DeblurOptions: FileName, Format, Codec, EncodingProfile, Encoder, UseExtremeMode, UseMarkInMarkOut, RenderAtSourceRes, UseMoreGpuMemory
## TypedDict EncryptDCTLOptions: Name, Expiry, OutputFolder
## TypedDict FadeInfo: FadeIn, FadeOut
## TypedDict FloatingWindowParams: left, right, top, bottom
## TypedDict ImportClipInfo: FilePath, StartIndex, EndIndex
## TypedDict ImportOptions: timelineName, importSourceClips, sourceClipsPath, sourceClipsFolders, interlaceProcessing
## TypedDict MarkInOut: video, audio
## TypedDict MarkerInfo: color, duration, note, name, customData
## TypedDict MediaStorageItemInfo: media, startFrame, endFrame
## TypedDict MulticamOptions: name, startTimecode, frameRate, angleSyncMode, channelConfig, multicamAudioMode, angleNameMode, splitAtGaps, useFullClipExtents, createBinForSourceClips, detectSameCameraClipsMode
## TypedDict NormalizeAudioOptions: normalizationMode, targetLevel, targetLoudness, setLevelMode
## TypedDict OutputBlanking: Top, Bottom, Left, Right
## TypedDict ProjectAttributes: lastModifiedDate, creationDate, notes, liveCollaborationMode
## TypedDict ProjectSettings: timelineResolutionWidth, timelineResolutionHeight, timelinePixelAspectRatio, timelineFrameRate, timelineDropFrameTimecode, timelineInterlaceProcessing, timelinePlaybackFrameRate, timelineOutputResMatchTimelineRes, timelineOutputResolutionWidth, timelineOutputResolutionHeight, timelineOutputPixelAspectRatio, timelineInputResMismatchBehavior, timelineOutputResMismatchBehavior, timelineFrameRateMismatchBehavior, timelineInputResMismatchUseCustomPreset, timelineInputResMismatchCustomPreset, timelineOutputResMismatchUseCustomPreset, timelineOutputResMismatchCustomPreset, timelineSampleRate, timelineSaveThumbsInProject, imageRetimeInterpolation, imageMotionEstimationMode, imageMotionEstimationRange, imageResizeMode, imageDeinterlaceQuality, imageEnableFieldProcessing, perfCacheClipsLocation, perfOptimisedMediaOn, perfProxyMediaMode, perfRenderCacheMode, perfOptimizedResolutionRatio, perfAutoRenderCacheEnable, perfAutoRenderCacheAfterTime, perfAutoRenderCacheTransition, perfAutoRenderCacheComposite, perfAutoRenderCacheFuEffect, perfOptimisedCodec, perfProxyResolutionRatio, perfRenderCacheCodec, isAutoColorManage, rcmPresetMode, separateColorSpaceAndGamma, colorScienceMode, colorSpaceTimeline, colorSpaceTimelineGamma, colorSpaceInput, colorSpaceInputGamma, colorSpaceOutput, colorSpaceOutputGamma, colorSpaceOutputToneMapping, inputDRT, outputDRT, useInverseDRT, timelineWorkingLuminanceMode, timelineWorkingLuminance, inputDRTSatRolloffStart, inputDRTSatRolloffLimit, outputDRTSatRolloffStart, outputDRTSatRolloffLimit, colorSpaceOutputGamutMapping, imageResizingGamma, graphicsWhiteLevel, useCATransform, disableFusionToneMapping, useColorSpaceAwareGradingTools, colorAcesIDT, colorAcesGamutCompressType, colorAcesODT, colorAcesNodeLUTProcessingSpace, colorSpaceOutputToneLuminanceMax, colorSpaceOutputGamutSaturationKnee, colorSpaceOutputGamutSaturationMax, colorKeyframeDynamicsStartProfile, colorKeyframeDynamicsEndProfile, colorLuminanceMixerDefaultZero, colorUseLegacyLogGrades, colorUseContrastSCurve, colorUseStereoConvergenceForEffects, colorUseLocalVersionsAsDefault, colorUseBGRPixelOrderForDPX, colorGalleryStillsLocation, colorGalleryStillsNamingEnabled, colorGalleryStillsNamingPattern, colorGalleryStillsNamingCustomPattern, colorGalleryStillsNamingWithStillNumber, colorVersion1Name, colorVersion2Name, colorVersion3Name, colorVersion4Name, colorVersion5Name, colorVersion6Name, colorVersion7Name, colorVersion8Name, colorVersion9Name, colorVersion10Name, hdrMasteringOn, hdrMasteringLuminanceMax, hdrDolbyControlsOn, hdrDolbyVersion, hdrDolbyAnalysisTuning, hdrDolbyMasterDisplay, hdr10PlusControlsOn, audioOutputHasTimecode, videoMonitorFormat, videoMonitorUseStereoSDI, videoMonitorUse444SDI, videoMonitorSDIConfiguration, videoDataLevels, videoDataLevelsRetainSubblockAndSuperWhiteData, videoMonitorBitDepth, videoMonitorScaling, videoMonitorUseHDROverHDMI, videoMonitorUseMatrixOverrideFor422SDI, videoMonitorMatrixOverrideFor422SDI, videoDeckFormat, videoDeckUseStereoSDI, videoMonitorUseLevelA, videoDeckUse444SDI, videoDeckSDIConfiguration, videoDeckBitDepth, videoDeckUseAudoEdit, videoDeckNonAutoEditFrames, videoDeckPrerollSec, videoDeckOutputSyncSource, videoDeckAdd32Pulldown, videoCaptureMode, videoCaptureFormat, videoCaptureCodec, videoCaptureIngestHandles, audioCaptureNumChannels, videoPlayoutMode, videoPlayoutShowSourceTimecode, videoPlayoutShowLTC, videoPlayoutLTCFramesOffset, videoPlayoutAudioFramesOffset, audioPlayoutNumChannels, videoPlayoutBatchHeadDuration, videoPlayoutBatchTailDuration, limitBroadcastSafeOn, limitBroadcastSafeLevels, limitAudioMeterLUFS, limitAudioMeterLoudnessScale, limitAudioMeterAlignLevel, limitAudioMeterHighLevel, limitAudioMeterLowLevel, limitAudioMeterDisplayMode, limitSubtitleCPL, limitSubtitleCaptionDurationSec, superScale, superScaleSharpness, superScaleNoiseReduction, superScaleSharpnessStrength, superScaleNoiseReductionStrength, transcriptionLanguage, speakerDetection, nodeStackLayers, cloudProjectMediaLocation, projectMediaLocation
## TypedDict ProjectSettingsPresetInfo: Name, Width, Height
## TypedDict QuickExportRenderSettings: TargetDir, CustomName, VideoQuality, EnableUpload
## TypedDict QuickExportRenderStatus: JobStatus, CompletionPercentage, TimeTakenToRenderInMs, Error
## TypedDict RenderJobInfo: JobId, RenderJobName, TimelineName, TargetDir, IsExportVideo, IsExportAudio, FormatWidth, FormatHeight, FrameRate, PixelAspectRatio, MarkIn, MarkOut, AudioBitDepth, AudioSampleRate, ExportAlpha, AlphaMode, OutputFilename, RenderMode, PresetName, VideoFormat, VideoCodec, AudioCodec, EncodingProfile, MultiPassEncode, NetworkOptimization, UploadStatus, ClipStartFrame, TimelineStartTimecode, ReplaceExistingFilesInPlace
## TypedDict RenderJobStatus: JobStatus, CompletionPercentage, TimeTakenToRenderInMs, EstimatedTimeRemainingInMs, Error
## TypedDict RenderSettings: SelectAllFrames, MarkIn, MarkOut, TargetDir, CustomName, UseUniqueFilenames, UniqueFilenameStyle, ExportVideo, ExportAudio, FormatWidth, FormatHeight, FrameRate, PixelAspectRatio, VideoQuality, AudioFormat, AudioCodec, AudioBitDepth, AudioSampleRate, ColorSpaceTag, GammaTag, ExportAlpha, AlphaMode, EncodingProfile, MultiPassEncode, NetworkOptimization, ClipStartFrame, TimelineStartTimecode, ReplaceExistingFilesInPlace, ExportSubtitle, SubtitleFormat, UseFullExtents, AddFrameHandles, DataBurnIn
## TypedDict ResolutionInfo: Width, Height
## TypedDict SmartSwitchSettings: minEditDuration, editChangeDelay, isAutoDetectWideAngle, analysisMode, wideAngleID, wideAngleFrequency, isUseWideAngleForIntroOutro, isUseWideAngleForSilence, switchOnVideoOnly, quality
## TypedDict SpeechSettings: TextInput, VoiceModel, CustomVoiceFile, Speed, Variation, Pitch, GenerationID, Filename, AddToTimeline, AudioTrack
## TypedDict SpeedOptions: Percentage, PitchCorrection, StretchKeyframesToFit, RippleTimeline
## TypedDict TakeInfo: startFrame, endFrame, mediaPoolItem
## TypedDict ThumbnailData: width, height, format, data
## TypedDict TimelineItemProperties: TransformEnabled, Pan, Tilt, ZoomX, ZoomY, ZoomGang, RotationAngle, AnchorPointX, AnchorPointY, Pitch, Yaw, FlipX, FlipY, CroppingEnabled, CropLeft, CropRight, CropTop, CropBottom, CropSoftness, CropRetain, DynamicZoomEnabled, DynamicZoomEase, CompositeEnabled, CompositeMode, Opacity, LensCorrectionEnabled, Distortion, RetimeAndScalingEnabled, RetimeProcess, MotionEstimation, Scaling, ResizeFilter, AudioVolumeEnabled, AudioVolume, AudioPanEnabled, AudioPan, AudioPitchEnabled, AudioPitchSemiTones, AudioPitchCents, AudioVoiceIsolationEnabled, AudioVoiceIsolationAmount, AudioDialogueLevelerEnabled, AudioDialogueLevelerMode, AudioDialogueLevelerReduceLoudDialogue, AudioDialogueLevelerLiftSoftDialogue, AudioDialogueLevelerBackgroundReduction, AudioDialogueLevelerOutputGain
## TypedDict TimelineSettings: useCustomSettings, timelineResolutionWidth, timelineResolutionHeight, timelinePixelAspectRatio, timelineInputResMismatchBehavior, timelineFrameRate, timelineDropFrameTimecode, timelineInterlaceProcessing, timelineOutputResMatchTimelineRes, timelineOutputResolutionWidth, timelineOutputResolutionHeight, timelineOutputPixelAspectRatio, timelineOutputResMismatchBehavior, superScale, videoMonitorFormat, videoMonitorUse444SDI, videoMonitorUseLevelA, videoMonitorUseStereoSDI, videoMonitorSDIConfiguration, videoDataLevels, videoDataLevelsRetainSubblockAndSuperWhiteData, videoMonitorBitDepth, videoMonitorScaling, videoMonitorUseHDROverHDMI, videoMonitorUseMatrixOverrideFor422SDI, videoMonitorMatrixOverrideFor422SDI, colorScienceMode, acesVersion, isAutoColorManage, rcmPresetMode, separateColorSpaceAndGamma, colorSpaceTimeline, colorSpaceTimelineGamma, colorAcesGamutCompressType, colorAcesODT, colorAcesMidGray, colorSpaceOutput, colorSpaceOutputGamma, use203NitsReference, colorSpaceOutputGamutLimit, colorAcesNodeLUTProcessingSpace, hdrMasteringOn, hdrMasteringLuminanceMax, outputDRT, colorSpaceOutputToneMapping, colorSpaceOutputGamutMapping, colorSpaceOutputGamutSaturationKnee, colorSpaceOutputGamutSaturationMax, useInverseDRT, colorSpaceOutputToneLuminanceMax, outputDRTSatRolloffStart, outputDRTSatRolloffLimit, inputDRT, inputDRTSatRolloffStart, inputDRTSatRolloffLimit, useCATransform, useColorSpaceAwareGradingTools, imageResizingGamma, graphicsWhiteLevel, timelineWorkingLuminance, timelineWorkingLuminanceMode, hdrDolbyControlsOn, hdrDolbyVersion, hdrDolbyMasterDisplay, hdrDolbyUseExternalCMU, hdr10PlusControlsOn, hdrVividControlsOn, hdrVividMasterDisplay, disableFusionToneMapping
## TypedDict Transcription: language, segments
## TypedDict TranscriptionSegment: start, end, text, speaker, words
## TypedDict TranscriptionWord: start, end, text
## TypedDict TransitionOptions: type, category, position, alignment, duration
## TypedDict VersionInfo: versionName, versionType
## TypedDict VoiceIsolationState: isEnabled, amount
## class Resolve -- The Resolve application: pages, the current project and application-wide presets.
- GetProjectManager() -> ProjectManager  # Returns the project manager object for currently open database
- GetMediaStorage() -> MediaStorage  # Returns the media storage object to query and act on media locations
- Fusion() -> Fusion  # Starting point for Fusion scripts
- GetCurrentProject() -> Project  # Returns the currently loaded Resolve project
- GetCurrentTimeline() -> Timeline  # Returns the currently loaded timeline
- GetMediaPool() -> MediaPool  # Returns the MediaPool object for the current project
- GetGallery() -> Gallery  # Returns the Gallery object for the current project
- OpenPage(pageName: str) -> bool  # Switches DaVinci Resolve Page. pageName can be: 'media', 'photo', 'cut', 'edit', 'fusion', 'color', 'fairlight', 'deliver'
- GetCurrentPage() -> str  # Returns current DaVinci Resolve Page: 'media', 'photo', 'cut', 'edit', 'fusion', 'color', 'fairlight', 'deliver'
- SetHighPriority(highPriority: bool) -> bool  # Sets the script execution priority to high or normal
- GetVersion() -> list[int | str]  # Returns list of product version fields in [major, minor, patch, build, suffix] format
- GetVersionString() -> str  # Returns product version in major.minor.patch[suffix].build format
- GetProductName() -> str  # Returns product name
- IsStudio() -> bool  # Returns whether this is the Studio version of the product
- GetLayoutPresetList() -> list[str]  # Returns a list of available UI layout preset names
- LoadLayoutPreset(presetName: str) -> bool  # Loads UI layout from saved preset
- UpdateLayoutPreset(presetName: str) -> bool  # Overwrites preset named 'presetName' with current UI layout
- ExportLayoutPreset(presetName: str, presetFilePath: str) -> bool  # Exports preset named 'presetName' to path 'presetFilePath'
- DeleteLayoutPreset(presetName: str) -> bool  # Deletes preset named 'presetName'
- SaveLayoutPreset(presetName: str) -> bool  # Saves current UI layout as a preset
- ImportLayoutPreset(presetFilePath: str, presetName: str | None=None) -> bool  # Imports UI layout preset from file
- Quit() -> bool  # Quits the Resolve App
- ImportRenderPreset(presetPath: str) -> bool  # Import a render preset from a file and select it
- ExportRenderPreset(presetName: str, exportPath: str) -> bool  # Export a render preset to a file
- GetBurnInPresetList() -> list[str]  # Returns a list of available data burn in preset names
- DeleteBurnInPreset(presetName: str) -> bool  # Deletes the named data burn in preset
- ImportBurnInPreset(presetPath: str) -> bool  # Import a data burn in preset from a file
- ExportBurnInPreset(presetName: str, exportPath: str) -> bool  # Export a data burn in preset to a file
- GetKeyboardPresetList() -> list[str]  # Returns a list of available keyboard preset names
- LoadKeyboardPreset(presetName: str) -> bool  # Loads the named keyboard preset
- DeleteKeyboardPreset(presetName: str) -> bool  # Deletes the named keyboard preset
- GetCurrentKeyboardPreset() -> str  # Returns the name of the currently active keyboard preset
- ImportKeyboardPreset(filePath: str, presetName: str | None=None) -> bool  # Imports a keyboard preset from file. Uses file base name as preset name if not specified.
- ExportKeyboardPreset(presetName: str, exportPath: str) -> bool  # Exports the named keyboard preset to the specified file path
- GetKeyframeMode() -> int  # Returns the currently set keyframe mode, one of the resolve.KEYFRAME_MODE_* constants. Color Page only.
- SetKeyframeMode(keyframeMode: KeyframeMode) -> bool  # Set keyframe mode
- GetFairlightPresets() -> list[str]  # Returns a list of Fairlight presets by name
- DisableBackgroundTasksForCurrentResolveSession()  # Disables all background tasks for current Resolve session
- ValidateDCTL(dctlSource: str) -> str | None  # Validates DCTL source code. Returns None on success, error string on failure.
- EncryptDCTL(inputPath: str, encryptDCTLOptions: EncryptDCTLOptions | None=None) -> bool  # Encrypts the DCTL at inputPath and writes it to an output folder.
- GetUserPreferencesPresetList() -> list[str]  # Returns a list of available user preferences preset names
- LoadUserPreferencesPreset(presetName: str) -> bool  # Loads the named user preferences preset
- SaveUserPreferencesPreset(presetName: str) -> bool  # Saves current user preferences as a preset with the given name
- DeleteUserPreferencesPreset(presetName: str) -> bool  # Deletes the named user preferences preset
- ImportUserPreferencesPreset(filePath: str, presetName: str | None=None) -> bool  # Imports a user preferences preset from file. Uses file base name as preset name if not specified.
- ExportUserPreferencesPreset(presetName: str, exportPath: str) -> bool  # Exports the named user preferences preset to the specified file path
## class ProjectManager -- Creates, loads and organizes projects, project folders and databases. See README.md section 'Cloud Projects Settings'.
- LoadProject(projectName: str) -> Project | None  # Loads and returns a project. Returns None if project was not found.
- CreateProject(projectName: str, mediaLocationPath: str | None=None) -> Project | None  # Creates and returns a project. Returns None if projectName exists.
- DeleteProject(projectName: str) -> bool  # Delete project in the current folder. Project must not be currently loaded.
- SaveProject() -> bool  # Saves the currently loaded project with its own name.
- GetCurrentProject() -> Project  # Returns the currently loaded Resolve project
- CreateFolder(folderName: str) -> bool  # Creates a folder. Returns False if it already existed.
- GetProjectListInCurrentFolder() -> list[str]  # Returns a list of project names in current folder
- GetFolderListInCurrentFolder() -> list[str]  # Returns a list of folder names in current folder
- GotoRootFolder() -> bool  # Opens root folder in database
- GotoParentFolder() -> bool  # Opens parent folder of current folder in database. Returns False if current folder has no parent.
- OpenFolder(folderName: str) -> bool  # Opens folder
- ImportProject(filePath: str, projectName: str | None=None) -> bool  # Imports a project from the file
- ExportProject(projectName: str, filePath: str, withStillsAndLUTs=True) -> bool  # Exports project to a file.
- ArchiveProject(projectName: str, filePath: str, isArchiveSrcMedia=True, isArchiveRenderCache=True, isArchiveProxyMedia=False) -> bool  # Archives project to a file
- RestoreProject(filePath: str, projectName: str | None=None) -> bool  # Restores a project from the file
- GetProjectLastModifiedTime(projectName: str) -> int  # Returns the last modified time of the project as an epoch timestamp
- GetProjectAttributesInCurrentFolder() -> dict[str, ProjectAttributes]  # Returns a dict of project names mapped to their attributes (lastModifiedDate, creationDate, notes, liveCollaborationMode) for all projects in the current folder
- CloseProject(project: Project) -> bool  # Closes the specified project without saving
- GetCurrentFolder() -> str  # Returns the current folder name
- DeleteFolder(folderName: str) -> bool  # Deletes the specified folder
- GetCurrentDatabase() -> DatabaseInfo  # Returns a dictionary (with keys 'DbType', 'DbName' and optional 'IpAddress') corresponding to the current database connection
- GetDatabaseList() -> list[DatabaseInfo]  # Returns a list of dictionary items (with keys 'DbType', 'DbName' and optional 'IpAddress') corresponding to all the databases added to Resolve
- SetCurrentDatabase(dbInfo: DatabaseInfo) -> bool  # Switches current database connection to the database specified by the keys below, and closes any open project
- LoadCloudProject(cloudSettings: CloudSettings) -> Project | None  # Loads and returns a cloud project with the given cloud settings. Returns None if not found
- CreateCloudProject(cloudSettings: CloudSettings) -> Project | None  # Creates and returns a cloud project
- ImportCloudProject(filePath: str, cloudSettings: CloudSettings) -> bool  # Imports a cloud project from the file path with given cloud settings
- RestoreCloudProject(folderPath: str, cloudSettings: CloudSettings) -> bool  # Restores a cloud project from the folder path with given cloud settings
## class Project -- A project: timelines, settings, presets and render jobs. See README.md section 'Looking up Project and Clip properties'.
- GetMediaPool() -> MediaPool  # Returns the MediaPool object
- GetCurrentTimeline() -> Timeline  # Returns the currently loaded timeline
- SetCurrentTimeline(timeline: Timeline) -> bool  # Sets given timeline as current timeline for the project
- GetTimelineCount() -> int  # Returns the number of timelines in the project
- GetTimelineByIndex(idx: int) -> Timeline | None  # Returns timeline at the given index, 1 <= idx <= project.GetTimelineCount()
- GetGallery() -> Gallery  # Returns the Gallery object
- GetName() -> str  # Returns project name
- SetName(projectName: str) -> bool  # Sets project name if given projectName is unique
- GetProjectSettingsPresetList() -> list[ProjectSettingsPresetInfo]  # Returns a list of project settings presets and their information
- SetProjectSettingsPreset(presetName: str) -> bool  # Sets project settings preset by given name into project
- DeleteProjectSettingsPreset(presetName: str) -> bool  # Deletes the project settings preset with the given name
- SaveCurrentProjectSettingsAsNewPreset(presetName: str) -> bool  # Saves the current project settings as a new preset with the given name
- UpdateProjectSettingsPreset(presetName: str) -> bool  # Updates the given project settings preset with current settings
- ExportProjectSettingsPreset(presetName: str, exportPath: str) -> bool  # Exports the given project settings preset to the specified file path
- ImportProjectSettingsPreset(presetFilePath: str, presetName: str | None=None) -> bool  # Imports a project settings preset from file. Uses file base name as preset name if not specified.
- GetRenderJobList() -> list[RenderJobInfo]  # Returns a list of render jobs and their information
- GetRenderPresetList() -> list[str]  # Returns a list of render preset names
- LoadRenderPreset(presetName: str) -> bool  # Loads a render preset by name
- SaveAsNewRenderPreset(presetName: str) -> bool  # Saves current render settings as a new preset with the given name
- DeleteRenderPreset(presetName: str) -> bool  # Deletes given render preset
- UpdateRenderPreset(presetName: str) -> bool  # Updates given render preset with current render settings
- SetQuickExportEnabledForRenderPreset(presetName: str, isEnabled: bool) -> bool  # Enables or disables quick export for the named render preset
- StartRendering(jobIds: list[str], isInteractiveMode=False) -> bool  # Starts rendering jobs indicated by the input job ids
- StopRendering()  # Stops any current render processes
- IsRenderingInProgress() -> bool  # Returns True if a rendering is in progress
- AddRenderJob() -> str  # Adds a render job based on current render settings to the render queue
- DeleteRenderJob(jobId: str) -> bool  # Deletes render job for input job id
- DeleteAllRenderJobs() -> bool  # Deletes all render jobs in the queue
- SetRenderSettings(settings: RenderSettings) -> bool  # Sets given settings for rendering
- GetRenderResolutions(format: str | None=None, codec: str | None=None) -> list[ResolutionInfo]  # Returns list of resolutions applicable for the given render format and codec
- GetSettings() -> ProjectSettings  # Returns a dict with all project settings. See README.md section 'Looking up Project and Clip properties'.
- SetSettings(settings: ProjectSettings) -> bool  # Sets the project settings with specified dict of setting names and values. See README.md section 'Looking up Project and Clip properties'.
- GetRenderJobStatus(jobId: str) -> RenderJobStatus  # Returns a dict with job status and completion percentage
- GetQuickExportRenderPresets() -> list[str]  # Returns a list of quick export render presets
- RenderWithQuickExport(quickExportPresetName: str, presetInfo: QuickExportRenderSettings) -> QuickExportRenderStatus  # Renders current timeline with quick export preset
- GetRenderFormats() -> dict  # Returns a dict (format -> file extension) of available render formats
- GetAudioRenderFormats() -> dict  # Returns a dict (format -> file extension) of available audio render formats
- GetRenderCodecs(renderFormatFileExtension: str) -> dict  # Returns a dict (codec description -> codec name) of available codecs
- GetAudioRenderCodecs(audioRenderFormatFileExtension: str) -> dict  # Returns a dict (codec description -> codec name) of available audio codecs for the given format
- GetCurrentRenderFormatAndCodec() -> dict[str, str]  # Returns a dict with currently selected format and render codec
- SetCurrentRenderFormatAndCodec(format: str, codec: str) -> bool  # Sets given render format and render codec as options for rendering
- GetCurrentRenderMode() -> int  # Returns the render mode: 0 - Individual clips, 1 - Single clip
- SetCurrentRenderMode(renderMode: int) -> bool  # Sets the render mode: 0 for Individual clips, 1 for Single clip
- RefreshLUTList() -> bool  # Refreshes LUT List
- GetUniqueId() -> str  # Returns a unique ID for the project item
- InsertAudioToCurrentTrackAtPlayhead(mediaPath: str, startOffsetInSamples: int, durationInSamples: int) -> bool  # Inserts the media with startOffset and duration in samples to the current track at the playhead
- LoadBurnInPreset(presetName: str) -> bool  # Loads user defined data burn in preset
- ExportCurrentFrameAsStill(filePath: str) -> bool  # Exports current frame as still to supplied filePath
- GetColorGroupsList() -> list[ColorGroup]  # Returns a list of all group objects in the timeline
- AddColorGroup(groupName: str) -> ColorGroup  # Creates a new ColorGroup with unique groupName
- DeleteColorGroup(colorGroup: ColorGroup) -> bool  # Deletes the given ColorGroup and sets clips to ungrouped
- ApplyFairlightPresetToCurrentTimeline(presetName: str) -> bool  # Applies Fairlight preset to current timeline
- ResetIntellisearchAnalysis() -> bool  # Resets intellisearch analysis for the project
- GenerateSpeech(speechSettings: SpeechSettings) -> MediaPoolItem  # Generates speech for given speechSettings dict
## class MediaPool -- The media pool of a project: its folders, its clips and the timelines created from them.
- AddSubFolder(folder: Folder, name: str) -> Folder  # Adds new subfolder under specified Folder object with the given name
- GetCurrentFolder() -> Folder  # Returns currently selected Folder
- RefreshFolders() -> bool  # Updates the folders in collaboration mode
- SetCurrentFolder(folder: Folder) -> bool  # Sets current folder by given Folder
- GetRootFolder() -> Folder  # Returns root Folder of Media Pool
- CreateTimelineFromClips(name: str, clipInfos: list[CreateTimelineClipInfo]) -> Timeline  # Creates new timeline with specified name, and appends the specified MediaPoolItem objects
- AppendToTimeline(clipInfos: list[AppendClipInfo]) -> list[TimelineItem]  # Appends specified MediaPoolItem objects in the current timeline. Returns the list of appended timelineItems
- CreateEmptyTimeline(name: str) -> Timeline  # Adds new timeline with given name
- ImportTimelineFromFile(filePath: str, importOptions: ImportOptions | None=None) -> Timeline  # Creates timeline based on parameters within given file (AAF/EDL/XML/FCPXML/DRT/ADL/OTIO) and optional importOptions dict
- DeleteTimelines(timelines: list[Timeline]) -> bool  # Deletes specified timelines in the media pool
- ExportMetadata(fileName: str, clips: list[MediaPoolItem] | None=None) -> bool  # Exports metadata of specified clips to 'fileName' in CSV format. If no clips are specified, all clips from media pool will be used
- DeleteClips(clips: list[MediaPoolItem]) -> bool  # Deletes specified clips or timeline mattes in the media pool
- ImportFolderFromFile(filePath: str, sourceClipsPath: str | None=None) -> bool  # Imports a DRB folder from the given file path
- DeleteFolders(subfolders: list[Folder]) -> bool  # Deletes specified subfolders in the media pool
- MoveClips(clips: list[MediaPoolItem], targetFolder: Folder) -> bool  # Moves specified clips to target folder
- MoveFolders(folders: list[Folder], targetFolder: Folder) -> bool  # Moves specified folders to target folder
- GetClipMatteList(mediaPoolItem: MediaPoolItem) -> list[str]  # Get mattes for specified MediaPoolItem, as a list of paths to the matte files
- GetTimelineMatteList(folder: Folder) -> list[MediaPoolItem]  # Get mattes in specified Folder, as list of MediaPoolItems
- DeleteClipMattes(mediaPoolItem: MediaPoolItem, paths: list[str]) -> bool  # Delete mattes based on their file paths, for specified MediaPoolItem
- RelinkClips(clips: list[MediaPoolItem], folderPath: str) -> bool  # Update the folder location of specified media pool clips with the specified folder path
- UnlinkClips(clips: list[MediaPoolItem]) -> bool  # Unlink specified media pool clips
- ImportMedia(clipInfos: list[ImportClipInfo]) -> list[MediaPoolItem]  # Imports specified file/folder paths into current Media Pool folder. Returns a list of the MediaPoolItems created
- GetUniqueId() -> str  # Returns a unique ID for the media pool
- CreateStereoClip(leftMediaPoolItem: MediaPoolItem, rightMediaPoolItem: MediaPoolItem) -> MediaPoolItem  # Takes in two existing media pool items and creates a new 3D stereoscopic media pool entry replacing the input media
- CreateMulticamClip(clips: list[MediaPoolItem], multicamOptions: MulticamOptions) -> list[MediaPoolItem]  # Creates Multicam clips from the specified MediaPoolItems and options
- AutoSyncAudio(mediaPoolItems: list[MediaPoolItem], audioSyncSettings: AudioSyncSettings) -> bool  # Syncs audio for specified MediaPoolItems. The list must contain at least one video and one audio clip.
- GetSelectedClips() -> list[MediaPoolItem]  # Returns the current selected MediaPoolItems
- SetSelectedClip(mediaPoolItem: MediaPoolItem) -> bool  # Sets the selected MediaPoolItem to the given MediaPoolItem
## class MediaPoolItem -- A clip in the media pool. See README.md section 'Looking up Project and Clip properties'.
- GetName() -> str  # Returns the clip name.
- SetName(name: str) -> bool  # Sets the clip's name to name(string).
- GetTimeline() -> Timeline  # Returns the timeline object if the mpItem is a timeline clip
- GetMetadata(metadataType: str | None=None) -> str | dict  # Returns the metadata value for the key 'metadataType'. If no argument is specified, a dict of all set metadata properties is returned.
- SetMetadata(metadata: dict) -> bool  # Sets the item metadata with specified dict of key-value pairs
- GetThirdPartyMetadata(metadataType: str | None=None) -> str | dict  # Returns the third party metadata value for the key 'metadataType'. If no argument, a dict of all set third party metadata properties is returned.
- SetThirdPartyMetadata(metadata: dict) -> bool  # Sets/Add the item third party metadata with specified dict of key-value pairs
- GetMediaId() -> str  # Returns the unique ID for the MediaPoolItem.
- AddMarker(frameId: int, color: MarkerColor, name: str, note: str, duration: int, customData: str | None=None) -> bool  # Creates a new marker at given frameId position. 'customData' is optional.
- DeleteMarkersByColor(color: MarkerColor | Literal['All']) -> bool  # Delete all markers of the specified color. 'All' as argument deletes all color markers.
- DeleteMarkerAtFrame(frameNum: int) -> bool  # Delete marker at frame number from the media pool item.
- DeleteMarkerByCustomData(customData: str) -> bool  # Delete first matching marker with specified customData.
- GetMarkers() -> dict[int, MarkerInfo]  # Returns a dict (frameId -> {information}) of all markers.
- GetMarkerByCustomData(customData: str) -> MarkerInfo  # Returns marker {information} for the first matching marker with specified customData.
- UpdateMarkerCustomData(frameId: int, customData: str) -> bool  # Updates customData for the marker at given frameId position.
- GetMarkerCustomData(frameId: int) -> str  # Returns customData string for the marker at given frameId position.
- AddFlag(color: FlagColor) -> bool  # Adds a flag with given color (string).
- GetFlagList() -> list[str]  # Returns a list of flag colors assigned to the item.
- ClearFlags(color: FlagColor | Literal['All']) -> bool  # Clears the flag of the given color if one exists. An 'All' argument is supported and clears all flags.
- GetClipColor() -> ClipColor | Literal['']  # Returns the item color as a string.
- SetClipColor(colorName: ClipColor) -> bool  # Sets the item color based on the colorName (string).
- ClearClipColor() -> bool  # Clears the item color.
- LinkFullResolutionMedia(fullResMediaPath: str) -> bool  # Links proxy media to full resolution media files specified via its path.
- LinkProxyMedia(proxyMediaFilePath: str) -> bool  # Links proxy media located at path specified by arg 'proxyMediaFilePath' with the current clip.
- UnlinkProxyMedia() -> bool  # Unlinks any proxy media associated with clip.
- ReplaceClip(filePath: str) -> bool  # Replaces the underlying asset and metadata of MediaPoolItem with the specified absolute clip path.
- ReplaceClipPreserveSubClip(filePath: str) -> bool  # Replaces the underlying asset and metadata preserving original sub clip extents.
- GetClipProperty(propertyName: str | None=None) -> str | ClipProperties  # Returns the property value for the key 'propertyName'. If no argument, a dict of all clip properties is returned.
- SetClipProperty(propertyName: str, propertyValue: str) -> bool  # Sets the given property to propertyValue (string).
- GetUniqueId() -> str  # Returns a unique ID for the media pool item
- TranscribeAudio(useSpeakerDetection: bool | None=None, transcribeAsNestedClip=False) -> bool  # Transcribes audio of the MediaPoolItem
- ClearTranscription(clearNestedClipTranscription=False) -> bool  # Clears audio transcription of the MediaPoolItem.
- PerformAudioClassification() -> bool  # Analyzes and classifies the audio of a MediaPoolItem.
- ClearAudioClassification() -> bool  # Clears audio classification of the MediaPoolItem.
- GetAudioMapping() -> str  # Returns a string with MediaPoolItem's audio mapping information (JSON format).
- SetAudioMapping(audioMapping: str) -> bool  # Sets audio mapping from a JSON string.
- GetMarkInOut() -> MarkInOut  # Returns dict of in/out marks set.
- SetMarkInOut(markIn: int, markOut: int, markType: MarkType | None=None) -> bool  # Sets mark in/out of type MarkType (default: 'all').
- ClearMarkInOut(markType: MarkType | None=None) -> bool  # Clears mark in/out of type MarkType (default: 'all').
- MonitorGrowingFile() -> bool  # Monitor a file as long as it keeps growing.
- RemoveMotionBlur(deblurOption: DeblurOptions) -> MediaPoolItem  # Apply Motion Deblur on MediaPoolItem, Returns newly created MediaPoolItem.
- AnalyzeForIntellisearch(identifyFaces: bool, isBetterMode: bool) -> bool  # Perform Intellisearch analysis on the MediaPoolItem.
- AnalyzeForSlate(markerColor: SlateMarkerColor) -> bool  # Perform Slate analysis on the MediaPoolItem.
- GetTranscription(useNestedClipTranscription=False) -> Transcription  # Returns transcription data for the media pool item if available.
## class Timeline -- A timeline: tracks, items, markers and export. See README.md section 'Looking up timeline export properties'.
- GetName() -> str  # Returns the timeline name.
- SetName(timelineName: str) -> bool  # Sets the timeline name if timelineName (string) is unique.
- GetStartFrame() -> int  # Returns the frame number at the start of timeline.
- GetEndFrame() -> int  # Returns the frame number at the end of timeline.
- GetTrackCount(trackType: TrackType) -> int  # Returns the number of tracks for the given TrackType.
- GetItemListInTrack(trackType: TrackType, index: int) -> list[TimelineItem]  # Returns a list of timeline items on specified track.
- GetSelectedClips() -> list[TimelineItem]  # Returns the currently selected timeline items
- GetCurrentTimecode() -> str  # Returns a string timecode representation for the current playhead position.
- SetCurrentTimecode(timecode: str) -> bool  # Sets current playhead position from input timecode.
- GetCurrentVideoItem() -> TimelineItem | None  # Returns the current video timeline item.
- AddMarker(frameId: int, color: MarkerColor, name: str, note: str, duration: int, customData: str | None=None) -> bool  # Creates a new marker at given frameId position.
- DeleteMarkersByColor(color: MarkerColor | Literal['All']) -> bool  # Deletes all timeline markers of the specified color.
- DeleteMarkerAtFrame(frameNum: int) -> bool  # Deletes the timeline marker at the given frame number.
- DeleteMarkerByCustomData(customData: str) -> bool  # Delete first matching marker with specified customData.
- GetMarkers() -> dict[int, MarkerInfo]  # Returns a dict (frameId -> {information}) of all markers.
- GetMarkerByCustomData(customData: str) -> MarkerInfo  # Returns marker {information} for the first matching marker with specified customData.
- UpdateMarkerCustomData(frameId: int, customData: str) -> bool  # Updates customData for the marker at given frameId position.
- GetMarkerCustomData(frameId: int) -> str  # Returns customData string for the marker at given frameId position.
- GetCurrentClipThumbnailImage() -> ThumbnailData  # Returns a dict with data containing raw thumbnail image data for current media in the Color Page.
- AddTrack(trackType: TrackType, subTrackType: str | None=None) -> bool  # Adds track of TrackType. Optional argument subTrackType.
- DeleteTrack(trackType: TrackType, trackIndex: int) -> bool  # Deletes track of trackType and given trackIndex. 1 <= trackIndex <= GetTrackCount(trackType).
- GetTrackSubType(trackType: TrackType, trackIndex: int) -> str  # Returns an audio track's format.
- SetTrackEnable(trackType: TrackType, trackIndex: int, enabled: bool) -> bool  # Enables/Disables track with given trackType and trackIndex
- GetIsTrackEnabled(trackType: TrackType, trackIndex: int) -> bool  # Returns True if track with given trackType and trackIndex is enabled.
- SetTrackLock(trackType: TrackType, trackIndex: int, locked: bool) -> bool  # Locks/Unlocks track with given trackType and trackIndex
- GetIsTrackLocked(trackType: TrackType, trackIndex: int) -> bool  # Returns True if track with given trackType and trackIndex is locked.
- DeleteClips(timelineItems: list[TimelineItem], rippleDelete=False) -> bool  # Deletes specified TimelineItems from the timeline, performing ripple delete if second argument is True.
- SetClipsLinked(timelineItems: list[TimelineItem], linked: bool) -> bool  # Links or unlinks the specified TimelineItems depending on second argument.
- NormalizeAudioLevel(timelineItems: list[TimelineItem], normalizeAudioOptions: NormalizeAudioOptions | None=None) -> bool  # Normalizes the audio level of specified TimelineItems using the given normalizeAudioOptions.
- AutoAlignClips(timelineItems: list[TimelineItem], autoAlignOptions: AutoAlignOptions | None=None) -> bool  # Aligns specified TimelineItems using the given options. Returns True if successful, False otherwise.
- GetNormalizeAudioModes() -> list[str]  # Returns the list of valid normalizationMode strings for NormalizeAudioLevel.
- GetTrackName(trackType: TrackType, trackIndex: int) -> str  # Returns the track name for track indicated by trackType and index.
- SetTrackName(trackType: TrackType, trackIndex: int, name: str) -> bool  # Sets the track name for track indicated by trackType and index.
- DuplicateTimeline(timelineName: str) -> Timeline  # Duplicates the timeline and returns the created timeline.
- GrabStill() -> GalleryStill  # Grabs still from the current video clip. Returns a GalleryStill object.
- GrabAllStills(stillFrameSource: int) -> list[GalleryStill]  # Grabs stills from all clips at 'stillFrameSource' (1=First frame, 2=Middle frame).
- CreateCompoundClip(timelineItems: list[TimelineItem], clipInfo: CompoundClipOptions | None=None) -> TimelineItem  # Creates a compound clip of input timeline items.
- CreateFusionClip(timelineItems: list[TimelineItem]) -> TimelineItem  # Creates a Fusion clip of input timeline items.
- Export(fileName: str, exportType: TimelineExportType, exportSubtype: TimelineExportSubtype) -> bool  # Exports timeline to 'fileName' as per input exportType & exportSubtype format. See README.md section 'Looking up timeline export properties'.
- GetSettings() -> TimelineSettings | ProjectSettings  # Returns a dict with all timeline settings, or the project settings when useCustomSettings is '0'. See README.md section 'Looking up Project and Clip properties'.
- SetSettings(settings: TimelineSettings) -> bool  # Sets the timeline settings with specified dict of setting names and values. See README.md section 'Looking up Project and Clip properties'.
- GetStartTimecode() -> str  # Returns the start timecode for the timeline.
- SetStartTimecode(timecode: str) -> bool  # Set the start timecode of the timeline to the string 'timecode'.
- ImportIntoTimeline(filePath: str, importOptions: AAFImportOptions | None=None) -> bool  # Imports timeline items from an AAF file.
- InsertGeneratorIntoTimeline(generatorName: str) -> TimelineItem  # Inserts a generator into the timeline.
- InsertFusionGeneratorIntoTimeline(generatorName: str) -> TimelineItem  # Inserts a Fusion generator into the timeline.
- InsertFusionCompositionIntoTimeline() -> TimelineItem  # Inserts a Fusion composition into the timeline.
- InsertOFXGeneratorIntoTimeline(generatorName: str) -> TimelineItem  # Inserts an OFX generator into the timeline.
- InsertTitleIntoTimeline(titleName: str) -> TimelineItem  # Inserts a title into the timeline.
- InsertFusionTitleIntoTimeline(titleName: str) -> TimelineItem  # Inserts a Fusion title into the timeline.
- CreateSubtitlesFromAudio(autoCaptionSettings: AutoCaptionSettings | None=None) -> bool  # Creates subtitles from audio for the timeline.
- GetUniqueId() -> str  # Returns a unique ID for the timeline
- DetectSceneCuts() -> bool  # Detects and makes scene cuts along the timeline.
- ConvertTimelineToStereo() -> bool  # Converts timeline to stereo.
- GetNodeGraph() -> Graph  # Returns the timeline's node graph object.
- AnalyzeDolbyVision(timelineItems: list[TimelineItem], analysisType: DolbyVisionAnalysisType) -> bool  # Analyzes Dolby Vision on clips present on the timeline.
- GetMediaPoolItem() -> MediaPoolItem | None  # Returns the media pool item corresponding to the timeline
- GetMarkInOut() -> MarkInOut  # Returns dict of in/out marks set.
- SetMarkInOut(markIn: int, markOut: int, markType: MarkType | None=None) -> bool  # Sets mark in/out of type MarkType (default: 'all')
- ClearMarkInOut(markType: MarkType | None=None) -> bool  # Clears mark in/out of type MarkType (default: 'all')
- GetVoiceIsolationState(trackIndex: int) -> VoiceIsolationState  # Returns the Voice Isolation State as a dict.
- SetVoiceIsolationState(trackIndex: int, voiceIsolationState: VoiceIsolationState) -> bool  # Sets Voice Isolation state of audio track.
- SetOutputBlanking(outputBlanking: OutputBlanking) -> bool  # Sets the output blanking for the timeline. Accepts a dictionary with keys 'Top', 'Bottom', 'Left' and 'Right'. The values are in pixels.
- GetOutputBlanking() -> OutputBlanking  # Returns the output blanking for the timeline as a dictionary with keys 'Top', 'Bottom', 'Left' and 'Right'. The values are in pixels.
## class TimelineItem -- A clip, title, generator or transition on a timeline track. See README.md section 'Looking up Timeline item properties'.
- GetType() -> str  # Returns the type of the item: 'video', 'audio', 'generator' or 'transition'
- AddTransition(transitionOptions: TransitionOptions) -> TimelineItem | None  # Adds a transition of the given type/category to the start or end of this item. Returns the created transition item or None on failure.
- GetName() -> str  # Returns the item name
- SetName(name: str) -> bool  # Sets the clip's name to name.
- GetStart(subframePrecision=False) -> float  # Returns the start frame position on the timeline. Returns fractional frames if subframe_precision is True
- GetEnd(subframePrecision=False) -> float  # Returns the end frame position on the timeline. Returns fractional frames if subframe_precision is True
- GetSourceStartFrame() -> int  # Returns the start frame position of the media pool clip in the timeline clip
- GetSourceEndFrame() -> int  # Returns the end frame position of the media pool clip in the timeline clip
- GetSourceStartTime() -> float  # Returns the start time position of the media pool clip in the timeline clip
- GetSourceEndTime() -> float  # Returns the end time position of the media pool clip in the timeline clip
- GetDuration(subframePrecision=False) -> float  # Returns the item duration. Returns fractional frames if subframe_precision is True
- GetLeftOffset(subframePrecision=False) -> float  # Returns the maximum extension by frame for clip from left side. Returns fractional frames if subframe_precision is True
- GetRightOffset(subframePrecision=False) -> float  # Returns the maximum extension by frame for clip from right side. Returns fractional frames if subframe_precision is True
- GetFusionCompCount() -> int  # Returns number of Fusion compositions associated with the timeline item
- GetFusionCompNameList() -> list[str]  # Returns a list of Fusion composition names associated with the timeline item
- GetFusionCompByIndex(compIndex: int) -> FusionComp | None  # Returns the Fusion composition object based on given index. 1 <= compIndex <= timelineItem.GetFusionCompCount()
- GetFusionCompByName(compName: str) -> FusionComp | None  # Returns the Fusion composition object based on given name
- AddFusionComp() -> FusionComp  # Adds a new Fusion composition associated with the timeline item
- GetMediaPoolItem() -> MediaPoolItem | None  # Returns the media pool item corresponding to the timeline item if one exists
- AddMarker(frameId: int, color: MarkerColor, name: str, note: str, duration: int, customData: str | None=None) -> bool  # Creates a new marker at given frameId position and with given marker information. 'customData' is optional and helps to attach user specific data to the marker
- DeleteMarkersByColor(color: MarkerColor | Literal['All']) -> bool  # Deletes all markers of the specified color from the timeline item. 'All' as argument deletes all color markers
- DeleteMarkerAtFrame(frameNum: int) -> bool  # Deletes marker at frame number from the timeline item
- DeleteMarkerByCustomData(customData: str) -> bool  # Deletes first matching marker with specified customData
- GetMarkers() -> dict[int, MarkerInfo]  # Returns a dict (frameId -> {information}) of all markers and dicts with their information
- GetMarkerByCustomData(customData: str) -> MarkerInfo  # Returns marker {information} for the first matching marker with specified customData
- UpdateMarkerCustomData(frameId: int, customData: str) -> bool  # Updates customData (string) for the marker at given frameId position. CustomData is not exposed via UI and is useful for scripting developer to attach any user specific data to markers
- GetMarkerCustomData(frameId: int) -> str  # Returns customData string for the marker at given frameId position
- SetProperties(properties: TimelineItemProperties) -> bool  # Sets the item properties with specified dict of property keys and values. See README.md section 'Looking up Timeline item properties'.
- GetProperties() -> TimelineItemProperties  # Returns a dict with all supported item properties. See README.md section 'Looking up Timeline item properties'.
- SetSpeed(speedOptions: SpeedOptions) -> bool  # Sets the Clip Speed
- GetSpeed() -> SpeedOptions  # Returns the clip speed options
- AddFlag(color: FlagColor) -> bool  # Adds a flag with given color (string)
- GetFlagList() -> list[str]  # Returns a list of flag colors assigned to the item
- ClearFlags(color: FlagColor | Literal['All']) -> bool  # Clears flags of the specified color. An 'All' argument is supported to clear all flags
- GetStereoConvergenceValues() -> dict[int, float]  # Returns a dict (offset -> value) of keyframe offsets and respective convergence values
- GetStereoLeftFloatingWindowParams() -> dict[int, FloatingWindowParams]  # For the LEFT eye -> returns a dict (offset -> dict) of keyframe offsets and respective floating window params
- GetStereoRightFloatingWindowParams() -> dict[int, FloatingWindowParams]  # For the RIGHT eye -> returns a dict (offset -> dict) of keyframe offsets and respective floating window params
- GetClipColor() -> ClipColor | Literal['']  # Returns the item color as a string
- SetClipColor(colorName: ClipColor) -> bool  # Sets the item color based on the colorName (string)
- ClearClipColor() -> bool  # Clears the item color
- ImportFusionComp(path: str) -> FusionComp  # Imports a Fusion composition from given file path by creating and adding a new composition for the item
- ExportFusionComp(path: str, compIndex: int) -> bool  # Exports the Fusion composition based on given index to the path provided
- DeleteFusionCompByName(compName: str) -> bool  # Deletes the named Fusion composition
- LoadFusionCompByName(compName: str) -> FusionComp  # Loads the named Fusion composition as the active composition
- RenameFusionCompByName(oldName: str, newName: str) -> bool  # Renames the Fusion composition identified by oldName
- RenameVersionByName(oldName: str, newName: str, versionType: int) -> bool  # Renames the color version identified by oldName and versionType (0 - local, 1 - remote)
- DeleteVersionByName(versionName: str, versionType: int) -> bool  # Deletes a color version by name and versionType (0 - local, 1 - remote)
- LoadVersionByName(versionName: str, versionType: int) -> bool  # Loads a named color version as the active version. versionType: 0 - local, 1 - remote
- AddVersion(versionName: str, versionType: int) -> bool  # Adds a new color version for a video clip based on versionType (0 - local, 1 - remote)
- GetVersionNameList(versionType: int) -> list[str]  # Returns a list of all color versions for the given versionType (0 - local, 1 - remote)
- SetCDL(CDL: CDL) -> bool  # Sets CDL values on the node. Keys of map are: 'NodeIndex', 'Slope', 'Offset', 'Power', 'Saturation'
- AddTake(mediaPoolItem: MediaPoolItem, startFrame: int | None=None, endFrame: int | None=None) -> bool  # Adds mediaPoolItem as a new take. Initializes a take selector for the timeline item if needed. By default, the full clip extents is added. startFrame and endFrame are optional arguments used to specify the extents
- GetSelectedTakeIndex() -> int  # Returns the index of the currently selected take, or 0 if the clip is not a take selector
- GetTakesCount() -> int  # Returns the number of takes in take selector, or 0 if the clip is not a take selector
- GetTakeByIndex(idx: int) -> TakeInfo | None  # Returns a dict with take info for specified index
- DeleteTakeByIndex(idx: int) -> bool  # Deletes a take by index, 1 <= idx <= number of takes
- SelectTakeByIndex(idx: int) -> bool  # Selects a take by index, 1 <= idx <= number of takes
- FinalizeTake() -> bool  # Finalizes take selection
- CopyGrades(tgtTimelineItems: list[TimelineItem]) -> bool  # Copies the current node stack layer grade to the same layer for each item in tgtTimelineItems.
- GetClipEnabled() -> bool  # Gets clip enabled status
- SetClipEnabled(enabled: bool) -> bool  # Sets clip enabled based on argument
- GetCurrentVersion() -> VersionInfo  # Returns the current version of the video clip. The returned value will have the keys versionName and versionType (0 - local, 1 - remote)
- UpdateSidecar() -> bool  # Updates sidecar file for BRAW clips or RMD file for R3D clips
- GetUniqueId() -> str  # Returns a unique ID for the timeline item
- LoadBurnInPreset(presetName: str) -> bool  # Loads user defined data burn in preset for clip when supplied presetName (string).
- CreateMagicMask(mode: str) -> bool  # Creates a magic mask. mode can be 'F' (forward), 'B' (backward), or 'BI' (bidirectional)
- RegenerateMagicMask() -> bool  # Regenerates the magic mask
- Stabilize() -> bool  # Performs stabilization on the clip
- SmartReframe() -> bool  # Performs Smart Reframe.
- GetNodeGraph(layerIdx: int | None=None) -> Graph  # Returns the clip's node graph object at layerIdx (int, optional). Returns the first layer if layerIdx is skipped. 1 <= layerIdx <= project.GetSetting('nodeStackLayers')
- GetColorGroup() -> ColorGroup | None  # Returns the clip's color group if one exists
- AssignToColorGroup(colorGroup: ColorGroup) -> bool  # Assigns the clip to the given ColorGroup. ColorGroup must be an existing group in the current project
- RemoveFromColorGroup() -> bool  # Removes the clip from its ColorGroup
- ExportLUT(exportType: ExportLutType, path: str) -> bool  # Exports a LUT of the size given by 'exportType', saving it in the provided 'path'
- GetLinkedItems() -> list[TimelineItem]  # Returns a list of linked timeline items
- GetTrackTypeAndIndex() -> list[str | int]  # Returns a list of two values that correspond to the TimelineItem's trackType (string) and trackIndex (int) respectively
- GetSourceAudioChannelMapping() -> str  # Returns a string with TimelineItem's audio mapping information
- SetSourceAudioChannelMapping(audioMapping: str) -> bool  # Sets source audio channel mapping from a JSON string.
- GetIsColorOutputCacheEnabled() -> bool  # Returns if the cache corresponding to cache_type is enabled
- GetIsFusionOutputCacheEnabled() -> str  # Returns if the cache corresponding to cache_type is enabled (or auto)
- SetColorOutputCache(enabled: bool) -> bool  # Sets caching to enabled or disabled. Equivalent to clip context menu action 'Render Cache Color Output'
- SetFusionOutputCache(cacheValue: str) -> bool  # Sets caching to auto, enabled or disabled. Equivalent to clip context menu action 'Render Cache Fusion Output'
- GetVoiceIsolationState() -> VoiceIsolationState  # Returns the Voice Isolation State as a dict {isEnabled, amount}, of the timelineItem
- SetVoiceIsolationState(state: VoiceIsolationState) -> bool  # Sets Voice Isolation state of the timelineItem to the given VoiceIsolationState of {isEnabled (bool), amount (int)}. amount is in range of [0, 100].
- ResetAllNodeColors() -> bool  # Resets node color for all nodes in the active version of the clip.
- SetOutputBlanking(outputBlanking: OutputBlanking) -> bool  # Sets the output blanking for the clip. Accepts a dictionary with keys 'Top', 'Bottom', 'Left' and 'Right'. The values are in pixels.
- GetOutputBlanking() -> OutputBlanking  # Returns the output blanking for the clip as a dictionary with keys 'Top', 'Bottom', 'Left' and 'Right'. The values are in pixels. The dictionary will be empty if the timeline's output blanking is used.
- SetUseTimelineForOutputBlanking(useTimelineOutputBlanking: bool) -> bool  # Sets the flag to use the timeline's output blanking for the clip.
- GetUseTimelineForOutputBlanking() -> bool  # Gets the flag to use the timeline's output blanking for the clip.
- PerformMulticamSmartSwitch(smartSwitchSettings: SmartSwitchSettings) -> bool  # Performs Multicam SmartSwitch on the multicam TimelineItem using the given smartSwitchSettings
- FlattenMulticam(gradeOption: FlattenMulticamGrade) -> bool  # Flattens the multicam TimelineItem, using the grade source specified by gradeOption
- GetFades() -> FadeInfo  # Returns a dict {FadeIn, FadeOut} of the fade durations (in frames) for the item's video or audio fader
- SetFades(fades: FadeInfo) -> bool  # Sets the fade durations (in frames) for the item's video or audio fader from a dict {FadeIn, FadeOut}
## class ColorGroup -- A group of clips sharing a pre-clip and a post-clip grade.
- GetName() -> str  # Returns the name of the ColorGroup
- SetName(groupName: str) -> bool  # Renames ColorGroup to groupName
- GetClipsInTimeline(timeline: Timeline | None=None) -> list[TimelineItem]  # Returns a list of TimelineItems in the ColorGroup for the given Timeline
- GetPreClipNodeGraph() -> Graph  # Returns the ColorGroup Pre-clip graph
- GetPostClipNodeGraph() -> Graph  # Returns the ColorGroup Post-clip graph
## class Folder -- A media pool folder: its clips, its subfolders and their analysis.
- GetName() -> str  # Returns the media folder name
- GetSubFolderList() -> list[Folder]  # Returns a list of subfolders in the folder
- GetClipList() -> list[MediaPoolItem]  # Returns a list of clips (items) within the folder
- GetIsFolderStale() -> bool  # Returns true if folder is stale in collaboration mode
- GetUniqueId() -> str  # Returns a unique ID for the media pool folder
- Export(filePath: str) -> bool  # Exports the folder as a DRB file to filePath
- TranscribeAudio(useSpeakerDetection: bool | None=None, transcribeAsNestedClip=False) -> bool  # Transcribes audio of the MediaPoolItems within the folder and nested folders.
- ClearTranscription() -> bool  # Clears audio transcription of the MediaPoolItems within the folder and nested folders.
- PerformAudioClassification() -> bool  # Analyzes and classifies the audio of the MediaPoolItems within the folder and nested folders into categories and subcategories
- ClearAudioClassification() -> bool  # Clears audio classification of the MediaPoolItems within the folder and nested folders
- RemoveMotionBlur(deblurOption: DeblurOptions | None=None) -> list[list[MediaPoolItem]]  # Apply Motion Deblur on MediaPoolItems in Folder, Returns a list of original to newly created MediaPoolItems
- AnalyzeForIntellisearch(identifyFaces: bool, isBetterMode: bool) -> bool  # Perform Intellisearch analysis to all the MediaPoolItems in the folder.
- AnalyzeForSlate(markerColor: SlateMarkerColor) -> bool  # Perform Slate analysis with current settings and use the stated markerColor to all the MediaPoolItems in the folder.
## class Gallery -- The gallery of a project: its still albums and PowerGrade albums.
- GetAlbumName(galleryStillAlbum: GalleryStillAlbum) -> str  # Returns the name of a GalleryStillAlbum object
- SetAlbumName(galleryStillAlbum: GalleryStillAlbum, albumName: str) -> bool  # Sets the name of a GalleryStillAlbum object
- GetCurrentStillAlbum() -> GalleryStillAlbum  # Returns current album as a GalleryStillAlbum object
- SetCurrentStillAlbum(galleryStillAlbum: GalleryStillAlbum) -> bool  # Sets current album to the given GalleryStillAlbum object
- CreateGalleryStillAlbum() -> GalleryStillAlbum  # Creates a new gallery still album
- CreateGalleryPowerGradeAlbum() -> GalleryStillAlbum  # Creates a new gallery power grade album
- GetGalleryStillAlbums() -> list[GalleryStillAlbum]  # Returns the gallery still albums as a list of GalleryStillAlbum objects
- GetGalleryPowerGradeAlbums() -> list[GalleryStillAlbum]  # Returns the gallery PowerGrade albums as a list of GalleryStillAlbum objects
## class GalleryStill -- A still of a gallery album, used as a handle by the GalleryStillAlbum functions.
## class GalleryStillAlbum -- An album of gallery stills, which can be labelled, imported and exported.
- GetStills() -> list[GalleryStill]  # Returns the list of GalleryStill objects in the album
- GetLabel(galleryStill: GalleryStill) -> str  # Returns the label of the galleryStill
- SetLabel(galleryStill: GalleryStill, label: str) -> bool  # Sets the new label to a GalleryStill object
- ImportStills(filePaths: list[str]) -> bool  # Imports GalleryStill from each filePath in the list
- ExportStills(galleryStill: list[GalleryStill], folderPath: str, filePrefix: str, format: str) -> bool  # Exports list of GalleryStill objects to a directory
- DeleteStills(galleryStill: list[GalleryStill]) -> bool  # Deletes specified list of GalleryStill objects
## class Graph -- The node graph of a clip or of a color group: its nodes, LUTs and grades. See README.md section 'Cache Mode information'.
- GetNumNodes() -> int  # Returns the number of nodes in the graph
- SetLUT(nodeIndex: int, lutPath: str) -> bool  # Sets LUT on the node mapping the node index provided, 1 <= nodeIndex <= GetNumNodes()
- GetLUT(nodeIndex: int) -> str  # Gets relative LUT path based on the node index provided, 1 <= nodeIndex <= GetNumNodes()
- SetNodeCacheMode(nodeIndex: int, cacheValue: CacheMode) -> bool  # Sets the cache mode type on the node mapping the node index provided
- GetNodeCacheMode(nodeIndex: int) -> int  # Returns the cache mode type on the node mapping the node index provided
- GetNodeLabel(nodeIndex: int) -> str  # Returns the label of the node at nodeIndex
- GetToolsInNode(nodeIndex: int) -> list[str]  # Returns toolsList of the tools used in the node indicated by given nodeIndex
- SetNodeEnabled(nodeIndex: int, isEnabled: bool) -> bool  # Sets the node at the given nodeIndex to isEnabled, 1 <= nodeIndex <= GetNumNodes()
- ApplyArriCdlLut() -> bool  # Applies ARRI CDL and LUT.
- ApplyGradeFromDRX(path: str, gradeMode: int) -> bool  # Loads a still from given file path and applies grade to graph with gradeMode (0=No keyframes, 1=Source Timecode aligned, 2=Start Frames aligned)
- ResetAllGrades() -> bool  # Resets all grades in the graph
## class MediaStorage -- Browses the volumes of the file system and adds media files to the media pool.
- GetMountedVolumeList() -> list[str]  # Returns list of folder paths corresponding to mounted volumes displayed in Resolve's Media Storage
- GetSubFolderList(folderPath: str) -> list[str]  # Returns list of folder paths in the given absolute folder path
- GetFileList(folderPath: str) -> list[str]  # Returns list of media and file listings in the given absolute folder path
- RevealInStorage(path: str) -> bool  # Expands and displays given file/folder path in Resolve's Media Storage
- AddItemListToMediaPool(itemInfos: list[MediaStorageItemInfo]) -> list[MediaPoolItem]  # Adds specified file/folder paths from Media Storage into current Media Pool folder. Returns a list of the MediaPoolItems created
- AddClipMattesToMediaPool(mediaPoolItem: MediaPoolItem, paths: list[str], stereoEye: str | None=None) -> bool  # Adds specified media files as mattes for the specified MediaPoolItem. stereoEye is 'left' or 'right' for stereo clips
- AddTimelineMattesToMediaPool(paths: list[str]) -> list[MediaPoolItem]  # Adds specified media files as timeline mattes in current media pool folder. Returns a list of created MediaPoolItems
- StartCloneMedia(sourceDir: str, targetDirs: str | list[str]) -> bool  # Starts cloning media from sourceDir to targetDirs. Use SetCloneToolSettings to configure PreserveFolderName/ChecksumType beforehand.
- SetCloneToolSettings(cloneToolSettings: CloneToolSettings | None=None) -> bool  # Sets the PreserveFolderName/ChecksumType options used by subsequent StartCloneMedia calls (and by the Clone Tool UI).
- StopCloneMedia() -> bool  # Stops the currently in-progress clone job started via StartCloneMedia. Returns False if no clone job is in progress.
- GetCloneStatus() -> CloneStatus  # Returns a dict with the status of the current (or most recently started) clone job
## class Fusion -- 
## class FusionComp -- 