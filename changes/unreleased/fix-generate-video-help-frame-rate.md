### Fixed - `generate-video --help` no longer says every clip plays at 16 fps

The `--frames` help named WAN's 16 fps for every family; an LTX-2.5 call plays at the machine's `videogen_fps`. The `--json` help now also says what a deferral looks like to a script: exit 0, `"ok": false`, `"deferred": true`, no file.
