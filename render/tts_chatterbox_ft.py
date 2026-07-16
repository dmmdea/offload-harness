#!/usr/bin/env python
# tts_chatterbox_ft.py — FINE-TUNED Chatterbox voice worker (SKELETON).
# Spawned by render/tts.mjs when --engine finetuned. THIS SESSION ships the arg CONTRACT
# + path validation only. The real load path — vendored src/chatterbox_ engine, English
# ChatterboxTTS + T3 vocab override to 2454 + merged strict-load + EnTokenizer, recipe as
# kwargs, NO language_id (mirrors chatterbox-finetuning inference.py::load_finetuned_engine_full) —
# lands in a later "build + tune" session. Until then it DEFERS-not-crashes: validate inputs,
# then exit 3 with a distinct marker so the Go gpugen wrapper maps it to a clean defer
# (never cloud). Stdlib-only on purpose (no torch/chatterbox import) so this runs anywhere.
import argparse
import os
import sys

MARKER = "FT_ENGINE_NOT_VENDORED"


def log(*a):
    print(*a, file=sys.stderr, flush=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--text", required=True)
    ap.add_argument("--lang", default="es")  # metadata only; the FT English engine omits language_id
    ap.add_argument("--ref", default=None)
    ap.add_argument("--model", required=True, help="merged fine-tuned T3 safetensors")
    ap.add_argument("--base-dir", dest="base_dir", required=True, help="ChatterboxTTS.from_local base dir")
    # recipe knobs — accepted now for a stable contract; the real impl binds them as generate() kwargs.
    ap.add_argument("--temperature", type=float, default=None)
    ap.add_argument("--cfg-weight", dest="cfg_weight", type=float, default=None)
    ap.add_argument("--exaggeration", type=float, default=None)
    ap.add_argument("--repetition-penalty", dest="repetition_penalty", type=float, default=None)
    ap.add_argument("--min-p", dest="min_p", type=float, default=None)
    ap.add_argument("--top-p", dest="top_p", type=float, default=None)
    args = ap.parse_args()

    if not os.path.exists(args.model):
        log(f"[chatterbox-ft] {MARKER}: model not found: {args.model}")
        sys.exit(3)
    if not os.path.isdir(args.base_dir):
        log(f"[chatterbox-ft] {MARKER}: base-dir not found: {args.base_dir}")
        sys.exit(3)

    # Contract validated; the real vendored-engine load path is the next-session build.
    log(
        f"[chatterbox-ft] {MARKER}: fine-tuned engine not yet vendored — deferring "
        f"(model={args.model}, base_dir={args.base_dir}, ref={args.ref}). "
        "Next session: vendor src/chatterbox_ + load_finetuned_engine_full."
    )
    sys.exit(3)


if __name__ == "__main__":
    main()
