### Fixed - HyperFrames always runs against the browser its installed release wants, not the Chrome version the config named

- **The defect.** `hyperframes_browser_path` held the absolute path of one chrome-headless-shell version
  (`.../win64-152.../chrome-headless-shell.exe`) and the runner exported it as `HYPERFRAMES_BROWSER_PATH`. `local-offload
  update` moved HyperFrames to the newest npm release and carried the old browser cache over, but never ran
  `browser ensure`, so a new release kept rendering against the old Chrome. The installers wrote the resolved versioned
  path, which is the pin.
- **`auto`.** `--browser auto` (config `hyperframes_browser_path: "auto"`, now what `install.ps1` and `install.sh`
  write) makes `render/compose-hyperframes.mjs` ask the installed release for its managed browser at render time:
  `browser path` under the harness-owned HOME, and when that file is not on disk `browser ensure` then `browser path`
  again. The render result carries `browser_path` and `chrome_version`. An explicit path works as before.
  `config.BrowserAuto` and `Config.HyperframesBrowserAuto()` name the value; `doctor`/`offload_status` report
  `browser=auto (<newest downloaded browser, or "none downloaded yet; the first composition fetches it">)`, and
  `install hyperframes` notes only an explicit path that differs from the release's own browser, suggesting `auto`.
- **`update`.** The staged HyperFrames generation now runs the install check (`npm rebuild esbuild`, the runner's
  `browser` and `version` ops) before it can go live, so the browser its release wants is fetched while the live
  generation still serves; a failure rejects the stage and leaves the live generation untouched. Browser versions the
  new release no longer resolves are then removed from the staged generation's own cache (the previous generation keeps
  its copy for rollback). With `compose_script` unset the browser is not refreshed and `update` logs that.
  `hfinstall.Verify` is the shared check; `Install` uses it unchanged.
- **Docs.** The operator guide, the media-generation system page and the setup agent guide say the same.
- **Check.** `render --browser auto` on a real install rendered a title card with the release's browser (chrome
  152.0.7977.30); the `browser` op returned the same path.
