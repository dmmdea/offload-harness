### Fixed - the Windows installer ships the render runners beside the harness, so the sd.cpp and hyperframes routes resolve on a fresh install

- **What was wrong.** Every relative render binding the config carries (`render/sdcpp-generate.mjs` on
  the tiers that seed the sd.cpp image lane, `render/compose-hyperframes.mjs` wherever the installer
  binds the compose lane) resolves against the harness executable's directory, and `install.sh` copies
  the `render/` tree to `<prefix>/bin/render` for that reason. `install.ps1` never did: a fresh Windows
  install built `harness\local-offload.exe`, installed hyperframes, bound both scripts, and `doctor`
  then reported both routes `BOUND-BUT-MISSING` (`script not found at <home>\harness\render\...`),
  found on the `maxwell-3` reference box on 2026-10-28; `generate_image` deferred on its first call.
- **The fix.** A new Step 7a copies the repo's `render/` tree to `<home>\harness\render` on every
  install (never skipped, like a rebuilt exe), fails loudly when no runner lands, and
  `SETUP-AGENT.md` lists the directory among what the installer lays down. Boxes that bound an absolute
  `compose_script` by hand keep working; a relative binding now resolves on Windows as it does on Linux.
- **Check.** `install.ps1` re-run on the reference box: `doctor` reports `generate_image` and
  `compose_video` `CONFIGURED`, and `local-offload generate-image` renders a 512x512 through sd.cpp.
