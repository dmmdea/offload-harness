# Morning Summary — 2026-06-16 (overnight autonomous build)

> Running log of the overnight local-offload media build. Honest status: what shipped + was **live-verified**, what's flagged, what needs Daniel.

## ✅ Phase A.2 — STT / `offload_transcribe` — BUILT + LIVE-VERIFIED (merge pending review)

**What shipped** (branch `feat/offload-transcribe`, 6 commits, +2160/-7):
- `transcribe` CLI + `offload_transcribe` MCP tool. Converts any local audio **or video** → 16 kHz mono WAV (ffmpeg, `-vn` drops video) → whisper.cpp `whisper-server` (CUDA, large-v3-turbo) → returns the fastcontext **`{gist, segments[]}`** citation pattern + writes `.srt`/`.txt`/`.segments.json` to the media dir. Defers to Opus on any failure.
- Two new pure-Go packages: `internal/audioio` (ffmpeg converter) + `internal/sttclient` (multipart POST to whisper-server via llama-swap `/upstream/whisper-stt/inference`, `verbose_json` parse, force-unload, SRT writer). `runTranscribe` is its own branch in `pipeline.Run` (no prompt, no grammar — audio never touches the Gemma text cascade).
- `--hq` flag routes to a second `whisper-stt-hq` upstream (large-v3) for hard/noisy clips.

**How it was built (min-maxer discipline):**
- 5-agent live-web frontier research sweep BEFORE building. It caught real wins I'd have shipped wrong from memory — chiefly: **flash-attn is default-ON since whisper.cpp v1.8.0 and DEGRADES non-English/noisy audio (#3020)**, so whisper-server runs with `-nfa`. Also: the `/upstream/<model>/inference` routing, `verbose_json` segment shape, per-request decode params, Silero VAD v6.2.0 (not v5.1.2), `-l` defaulting to `en` (so the client must send `language=auto` explicitly).
- Decode profile (research-tuned, sent per-request): Silero VAD + beam 5 + best-of 5 + `entropy_thold 2.8` + `max_context 64` (kills repetition loops) + temperature-fallback.
- Hardware-harmony: not latency-starved (5–8× realtime) → spend GPU slack on quality (`-nfa`, beam, VAD); `-t 8` uses all cores; both turbo + large-v3 page-cached in 64GB RAM so escalation is cheap.

**The load-bearing llama-swap edit (the ritual — done safely):** baseline-verified the memory stack healthy → backed up config → **additive-only** edit (diff confirmed: only insertions) → YAML-validated → restarted → **verified all three: embeddinggemma 768 dims, bge-reranker scores, whisper-stt transcribes**. The persistent CPU memory-stack never wavered. whisper registered as its own `exclusive:true` group (`ttl:300`), so it evicts the Gemma/vision cascade but leaves embeddinggemma+bge-reranker running. (Note: live llama-swap binary is **v202**, not v211 — all routing/group semantics re-verified live on v202.)

**Live verification (real, measured — not eyeballed):**
- **EN** (JFK sample): end-to-end harness → `ok`, correct transcript, valid SRT, all pointer files written. 1585 ms incl. ffmpeg + cold load + transcribe + unload.
- **ES** (synthetic TTS, **WER self-tested clean = 0 on identical strings**):
  - es_ES (Spain voice): turbo & large-v3 **0.0% WER accent-folded** (1.9% with accents — the lone diff is `video→vídeo`, an accent variant).
  - es_MX voice / **Colombian** vocabulary (parceros, camioneta, huecos, gasolina — all correct): **turbo 6.5%** accent-folded (one real miss: clipped final word "arranquemos"→garbled); **large-v3 `--hq` 0.0%** accent-folded (fixed it). With accents, every remaining error is a dropped diacritic (qué→que) — cosmetic, not content. **This is a real demonstration of the turbo→`--hq` escalation value.**
- **Hardware-harmony measured:** cold reload (after force-unload) ≈ **6.0 s**, warm ≈ **0.6 s**. Zero-always-warm costs ~5.4 s/cold-call; set `stt_unload_after:false` to keep warm for a batch.

**Process honesty (corrections Daniel caught mid-build):**
- I started the whisper build before research returned — corrected: research gates finalization, not the clock.
- I skipped writing `pipeline_transcribe_test.go` in the first pass and leaned on a green `go test` "ok" that only ran the *existing* suite. Fixed: tests written + verified to actually run (`-v`), defer-paths covered. (Happy path was always covered by the live smoke.)
- I reported "WER ≈ 0 / it corrected me" by eyeball; then my first WER script was encoding-broken (cp1252 mojibake). Fixed: UTF-8 decode + self-test; numbers above are trustworthy.
- Found + fixed a real UTF-8 bug myself: `preview()` byte-sliced the gist mid-rune (would corrupt Spanish á/ñ) — now rune-safe + tested.

## ✅ Phase S — ik_llama.cpp benchmark spike — DONE — VERDICT: REJECT ik (non-mutating; nothing adopted)

Built ik_llama.cpp in its own dir + benchmarked vs the live mainline engine on scratch ports. **Live `~/llama-swap/config.yaml` untouched; live memory stack + whisper-stt verified healthy after.** Write-up: `docs/PHASE-S-ik_llama-benchmark-2026-06-16.md`.

- **ik is faster on PP** (26B-A4B: +27% f16, **+140% with q8_0 KV**) and the feared #1765 token-gen regression **did NOT reproduce** on our current build (likely fixed by ik's recent CUDA-FA work). The raw benchmark alone would have said "adopt."
- **But ik FAILS the sacred GBNF grammar gate** on the 26B CPU-MoE path — HTTP 500 `invalid UTF-8` on trivial enum grammars (classify/triage) + repetition loops, where **mainline produces valid JSON**. Grammar is non-negotiable → **REJECT**. (ik's grammar is clean on the fully-GPU E4B, so the bug is its CPU-resident-expert path = the #1693 class.)
- **Free win surfaced (no ik):** mainline `--n-cpu-moe 24` lifts the live 26B escalation TG ~**+11%** (15.4→17.2 t/s), grammar-safe. **Proposed for Daniel** — it's a live-config edit, so verify VRAM fit + grammar gate first; your call.
- Verify-then-assert paid off twice: the benchmark said adopt, the gate said no; and #1765 said reject-for-TG, wrong on our build.

## ⚠️ Flagged for Daniel
- **ES tests are CLEAN synthetic TTS, not real noisy field audio.** Capability is proven; noisy chest-cam ES robustness + VAD-threshold tuning need ONE short real Colombian clip (I did NOT touch the Danmar footage per guardrail). Point me at one and I'll benchmark turbo-vs-`--hq` on it.
- **No es_CO (Colombian) TTS voice exists in Piper** — used es_MX (Mexican, the closest LatAm). For **Phase 2 (TTS generation)** the channel's generated voice should be sourced Colombian/LatAm (cultural fit); for STT this phase, whisper is accent-robust so it's not blocking.
- whisper drops some written accents (qué→que). Cosmetic; a Spanish accent-restoration post-pass (or just accepting it) is a future nicety.
- Installed in WSL for testing (no spend): Piper TTS in `~/piper-venv` + es_ES/es_MX voices in `~/piper-voices` (for generating test audio). Not part of the harness.

## ✅ /printing-press evaluation (Daniel-requested) — DONE
Write-up: `docs/PRINTING-PRESS-EVAL-2026-06-16.md`. **Verdict: printing-press can't *build* this harness** (it generates API-client CLIs from a spec; the harness is a bespoke local-inference orchestrator with no API spec — generating would discard its value). **But its quality rubric surfaced one genuinely good, on-philosophy improvement:** add **`--select`/`--compact` field-filtering to the verbose outputs** (`transcribe` segments, `video-describe`, `extract`) — it's literally the fastcontext citation-pattern the harness already espouses, letting the agent pull only needed fields. Plus two minor polish items (MCP `readOnlyHint`, richer `--help` examples). All are small future PRs, not a regeneration. Did NOT run printing-press (wrong tool / no API target).

## ⏳ Next (not done tonight — teed up)
- Phase A.2 STT **SHIPPED**; Phase S **DONE** (REJECT ik); `/printing-press` eval **DONE**.
- **Phase 2 generation scaffolding** (ComfyUI Wan 2.2/Hunyuan video gen — already on disk in `C:\ComfyUI` — + the GPU single-slot file-lock scheduler) is the clear next major item. Deliberately NOT half-built at this hour (quality-first); design is in `CHAPTER-video`. Ready for a fresh research→plan→build cycle.
- Optional small PR from the eval above: `--select`/`--compact` on verbose outputs.

## ❗ Needs Daniel's decision / spend (NOT done autonomously)
- **Phase S proposal — mainline `--n-cpu-moe 24` for the live 26B escalation tier** (~+11% TG, free, grammar-safe). It's a live-`~/llama-swap/config.yaml` edit, so I did NOT apply it — your call. Verify VRAM fit + grammar gate first. (ik itself = rejected, no action.)
- DaVinci Resolve Studio $295 — Phase 3, with Daniel.
- Docker leftovers (open-webui keep/kill) — Daniel's call.
- A real Colombian field clip for representative ES field-audio benchmarking.
- `~/ik_llama.cpp` build (~few GB) kept for the watch-list re-test — remove if you need the disk.
