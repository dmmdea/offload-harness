# Shared font kit

The runner copies this directory into every materialized template as `shared/`. Templates point
their `@font-face` rules at `shared/fonts/…`.

| file | face | sha256 |
|---|---|---|
| `fonts/inter-latin-400-normal.woff2` | Inter 400, latin | `8909904ab6c872eb994093482a88a28eca2cd95912d7b6fecd72103b0dc07edc` |
| `fonts/inter-latin-700-normal.woff2` | Inter 700, latin | `6f56409fd3d64bb85f7d070bce20749db2d66b6d63cec586cc22d1c761be2491` |
| `fonts/inter-latin-900-normal.woff2` | Inter 900, latin | `d5c0ed7b8b5dde97d48b97947d740bbd8ad3ba9f2c5cc6b8280f16acba2d828e` |

**Source.** The files come from `@fontsource/inter@5.2.8` (npm, `files/`). They are byte-identical to
the faces the HyperFrames 0.8.61 producer embeds for `Inter`: the three hashes above were compared
against the base64 payloads in `node_modules/hyperframes/dist/cli.js`.

**Why they are vendored.** If the page does not declare a family with `@font-face`, the HyperFrames
compiler resolves it by requesting the Google Fonts CSS API at render time, even for families it
embeds. A declared face skips that request, so the render stays offline and gives the same result on
a disconnected node.

**License.** The Inter font is licensed under the SIL Open Font License 1.1. The full text,
including the copyright line, is in [`fonts/OFL.txt`](fonts/OFL.txt) and travels with the files.
