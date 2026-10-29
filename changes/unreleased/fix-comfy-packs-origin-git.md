### Fixed - comfy-packs matches a pack origin that ends in .git or /

- **What it does.** `update --component comfy-packs` compares an installed pack's git origin with its `render/node-packs.json` row after one trailing `/` and one trailing `.git` are dropped from each side; nothing else is normalised (case, scheme and host must match exactly), an empty origin never matches, and `ls-remote` and `fetch` still use the origin as the checkout reports it.
- **Why.** A node whose packs were cloned with a `.git` suffix had every pack skipped as `ORIGIN_NOT_ALLOWED`, so the verb covered none of them there.
- **Check.** The patched `render/` in `--dry-run --json` on that node matched ComfyUI-GGUF and ComfyUI-VideoHelperSuite (`current`, before `ORIGIN_NOT_ALLOWED`). Details: `docs/systems/update.md`.
