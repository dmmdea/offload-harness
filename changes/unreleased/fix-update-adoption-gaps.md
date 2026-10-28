### Fixed - `update` adopts the nodes as they are: sd.cpp's `sd-cli`, a build family per component, repoint of unnumbered and source-tree directories, the root from any installer layout

- **The gaps.** A read-only pass over every node, with the verb's own dry runs, found five things that stopped the
  verb from taking over what the nodes run:
  - sd.cpp's program was looked up as `sd`. Every release above the floor ships `sd-cli` and `sd-server`, so the
    offline self-check would reject the newest build, and a rejected id is never retried.
  - The build family came from the node's backend for every component. A maxwell-3 node serves llama.cpp on CUDA and
    sd.cpp on Vulkan (the only family measured to render on its card), and its sd.cpp would have been replaced by a
    CUDA build with no code for the card.
  - `--repoint` matched only build-numbered directory names, so a llama.cpp kept as `/opt/llama-vulkan` could not be
    repointed. It also kept the `bin` segment of a source tree (`llamacpp-b<N>/bin/llama-server`), which pointed at
    a path the flat publisher archive does not have.
  - Repoint's `LD_LIBRARY_PATH` pass replaced a whole repointed list, dropping `:${LD_LIBRARY_PATH:-}`.
  - The install root was found beside the binary only when it sat in `<home>/bin`. The Windows installer puts it in
    `<home>/harness`, so a node set up by the installer needed `--home` on every call.
- **What it does.**
  - sd.cpp's program is `sd-cli`.
  - The family is chosen per component: `update.variant.<component>`, else the `ggml-cuda` / `ggml-vulkan` library
    beside the live build, else the node's backend. The verb says which one it used.
  - `--repoint` takes `--from <dir>`, which matches whole path components only, and drops a `bin` segment when the
    live build is flat.
  - A line the directory pass rewrote keeps the rest of its `LD_LIBRARY_PATH` list.
  - The root is `--home`, `$OFFLOAD_HOME`, `update.home` in `config.json`, then the nearest folder holding
    `installed.json` among the binary's folder and the three above it, then `~/offload-stack`. The verb prints which
    of these it used.
- **Check.** This branch's binary, on the nodes themselves:
  - A Windows maxwell-3 node, binary placed in `<home>/harness`, no `--home`, no update config: `install root ...
    (installed.json above this binary)`. `sdcpp: build family vulkan (the live build carries the ggml Vulkan
    backend)` resolved `sd-master-<sha7>-bin-win-vulkan-x64.zip`. `llama: build family cuda12`. whisper (a house
    build outside the verb) read `not-installed`.
  - On two Linux nodes, `--repoint` on copies of their yamls:
    - `--from /opt/llama-vulkan` rewrote all 8 seat lines and kept `:${LD_LIBRARY_PATH:-}` on the macro.
    - A source-tree yaml rewrote 16 lines to `<live>/llama-server` with the `bin` segment dropped.
  - The publisher's newest sd.cpp Windows build prints `stable-diffusion.cpp version unknown, commit <sha7>` from
    `sd-cli.exe --version` and exits 0.

  Docs: `docs/systems/update.md` (Usage, Algorithm step 2, the config table, Repoint).
