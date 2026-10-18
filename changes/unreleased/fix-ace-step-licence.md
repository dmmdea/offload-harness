### Fixed - the amd-gcn seed records the ACE-Step 1.5 licence as the MIT its source states instead of "not verified", and the licence map says what each ACE-Step repack states (register ACE-LICENCE)

The amd-gcn profile's notes said "ACE-STEP LICENCE IS NOT VERIFIED" and left the music lane's licence unset, a plan page assumed Apache-2.0, and the licence map tagged both Comfy-Org ACE-Step repacks `apache-2.0`
without saying how that relates to the model they repackage. Read at the sources on 2026-10-18:

- **The ACE-Step 1.5 project.** The `LICENSE` file of its GitHub repository is the MIT text ("Copyright (c) 2026 ACEStep"), and its README says the project is licensed under MIT.
- **The model repository `ACE-Step/Ace-Step1.5`.** The Hugging Face API returns `cardData.license` `mit` (tag `license:mit`), the card's own line reads "License: [MIT]", and the card says the generated music may be used commercially.
  The model repository holds no `LICENSE` file (the raw URL answers 404), so the card and the GitHub file are the sources.
- **The package the seed binds.** `audio-cpp/audio.cpp-gguf` is tagged `license:other` at the repository level, and its README table lists the original-model licence of `ACE-Step1.5-GGUF` (family `ace_step`) as MIT;
  audio.cpp's model licence reference lists `ace_step` as MIT with commercial use allowed.
- **The repacks the map names.** `Comfy-Org/ace_step_1.5_ComfyUI_files` states `apache-2.0` and names `ACE-Step/Ace-Step1.5` as its base model, so it states a different licence from the model it repackages.
  `Comfy-Org/ACE-Step_ComfyUI_repackaged` states `apache-2.0` and repackages the ACE-Step v1 checkpoint (`ACE-Step/ACE-Step-v1-3.5B`), whose card states `apache-2.0` too.

What changed:

- **`setup/templates/profiles.json` (amd-gcn notes) and the generated `docs/tiers/amd-gcn.md`.** The unverified clause is now the verified record: MIT, with the three sources above. The page is generated from the notes
  (`TestTierDocsAreCurrent`); the same text was written to both.
- **`render/templates-license-map.json`.** Each repack is tagged exactly as it states itself (`apache-2.0`, class `permissive`, unchanged), `fetched_on` moves to the re-fetch (2026-10-18), and a `note` on each says what its
  base model states; the 1.5 repack's note says the tag is the repo's statement and not the base model's. The top-level `evidence` text says the two entries were re-fetched.
- **`setup/install.ps1`.** The comment above the amd-gcn media pins no longer says the licence is unverified.
- **`internal/tierseed/amdgcn_media_seed_test.go`.** The assertion that the notes call the licence unverified now asserts the verified MIT record (and that the old clause is gone); it fails on the unmodified profile.

What did not change, and why: **no `commercial_use` key is seeded for the music lane.** The audio lanes (`audiocpp_*`, `musicgen_engine`, `voicegen_engine`) have no `license` or `commercial_use` key in the config schema, and the
seed test forbids a licence key on them, so the record lives in the notes. Adding a key would be a schema change with its own design (ADR 0058 tags image, edit and video families only); it is not part of this change.
The two repack tags stay `apache-2.0` because that is what each states. Not verified: the Hugging Face API and the raw files were read once, on one day; a later change of any of them is not seen until the entries are fetched again.
