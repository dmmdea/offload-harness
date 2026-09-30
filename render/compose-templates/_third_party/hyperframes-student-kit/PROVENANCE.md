# hyperframes-student-kit: provenance and licence handling

Several templates in `render/compose-templates/` are adapted from card designs in a community
teaching kit for HyperFrames. This folder keeps the kit's licence texts exactly as received and records
what was taken from it and what was not. The kit is a reference, never an install: none of its
scripts, skills or CLI calls run anywhere in this repository.

| | |
|---|---|
| Source repository | `https://github.com/nateherkai/hyperframes-student-kit` |
| Pinned commit | `0d30152a82b9ceb93cfdd9bdbf46f0d5ab3cde86` (2026-09-28) |
| Retained verbatim | [`LICENSE`](LICENSE) (MIT, for the kit's original material) and [`PIPELINE-USE-PERMISSION.txt`](PIPELINE-USE-PERMISSION.txt) (the kit's use permission for its style library, templates and pipeline skills) |
| Checked by | `render/compose-hyperframes.test.mjs`, which hashes both files (line endings folded) against the copies at the pinned commit |

## What was adapted

| template here | source card id | source file at the pinned commit | source sha256 |
|---|---|---|---|
| `stat-card` | `kallaway.t1.stat.figure` | `style-library/02-kallaway/cards/tier1/t1-stat-figure.html` | `e1294d1e4bb16594bb4f767fbe11f1de20189d12cf518c88f4e11a4495a12903` |
| `section-title` | `kallaway.t1.section.breath` | `style-library/02-kallaway/cards/tier1/t1-section-breath.html` | `4e82a81883447b1b32ce68b050748be68f3bc2b7bc9ae72d24cb47df219ba085` |
| `callout-label` | `kallaway.t2.label.callout` | `style-library/02-kallaway/cards/tier2/t2-lb-callout.html` | `556ba6656714b39517a9dc92a4d0d040b42353e3a5d5c06ce6ec20b2dc9d26d9` |
| `checklist-card` | `kallaway.t1.overview.checklist` | `style-library/02-kallaway/cards/tier1/t1-overview-checklist.html` | `1779764df07023e6c1038ca9fae11622b8b3fb6662900a8646ee5daf54a8f5d7` |

Each template is a rewrite of the card's layout and motion, not a copy of its file. Each template's
`README.md` carries the provenance line and lists its own changes. The changes common to all four:

- Motion is CSS `@keyframes` only. The kit's cards load a script animation library from a CDN and
  build their motion in script; the templates here load nothing and run no timeline library.
- Text arrives through declared variables (`data-composition-variables`, `data-var-text`) instead of
  free-form slots, and every string variable has a length limit.
- Where a card put emphasis inside a slot with markup, the text is split into `*_pre`, `*_em` and
  `*_post` variables, because a variable is text only.
- Fonts are Inter from `render/compose-templates/_shared/fonts`, declared with `@font-face`; the kit's
  own faces and its Google Fonts import are not used.
- The palette is recoloured (a different default accent and canvas), the creator, style and brand
  names are removed from the templates, and all placeholder copy is replaced with neutral copy. The
  card ids in the table above are the kit's registry ids: they are kept only in this record and in each
  README's provenance line, so that a port can be traced to its source card.
- Not carried over: the grain texture, the frosted-glass blur, the count-up script and every
  time-based or random source.

## What was not taken

The kit's brand assets and brand tokens, its example projects and showcase videos, its seven proprietary
fonts, its scripts (which call cloud services), its CLI workflow, and every one of its agent skills. The
kit's own licence text excludes its brand assets and example-project content from reuse, and none of
that content is here.

## How the licences apply

- The MIT text and the use permission are both retained in this folder. The use permission asks that
  its notice be kept when substantial portions of the kit's source materials are redistributed; this
  folder is that notice, and each adapted template's README links to it.
- The use permission says third-party notices override its grants for their respective assets. The one
  third-party asset the templates touch is the Inter font, which travels with its own licence text in
  `render/compose-templates/_shared/fonts/OFL.txt`.
- The use permission ends by pointing to the kit's `THIRD_PARTY_NOTICES.md`, the place it names for the
  licences of third-party software, fonts and assets. That file is not retained here because none of what it
  covers is redistributed: the templates carry no kit script, no kit font and no kit asset, and the one
  third-party asset they touch, the Inter font, travels with its own licence text (the point above).
- No Apache-2.0 notice is kept here because no text from the kit's Apache-2.0 skills was reused.

## Re-checking the source

```
git clone -c core.longpaths=true --filter=blob:none --no-checkout https://github.com/nateherkai/hyperframes-student-kit kit
cd kit && git sparse-checkout set --no-cone /LICENSE /licenses/ /style-library/
git checkout 0d30152a82b9ceb93cfdd9bdbf46f0d5ab3cde86
```

Run that in a throwaway folder outside any repository, on a shell that does not rewrite leading slashes.
Nothing else from the kit is needed, and nothing from it should be installed.
