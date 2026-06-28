#!/usr/bin/env python
# tts_chatterbox.py — Chatterbox Multilingual TTS worker (MIT license, commercial-safe,
# zero-shot voice cloning). Invoked by render/tts.mjs inside the .tts-venv. Clones the --ref
# voice if given (reproduces that speaker's accent — e.g. a reference Spanish voice); lang
# defaults to 'es'. Reconciled against the real installed API: ChatterboxMultilingualTTS
# .from_pretrained(device) -> .generate(text, language_id, audio_prompt_path=...).
import os
# Windows blocks symlinks without Developer Mode/elevation → HF cache os.symlink raises
# WinError 1314. Tell huggingface_hub to COPY into the cache instead. Must be set before any
# huggingface_hub import (chatterbox pulls it in), so it sits above that import.
os.environ.setdefault("HF_HUB_DISABLE_SYMLINKS", "1")
os.environ.setdefault("HF_HUB_DISABLE_SYMLINKS_WARNING", "1")
import argparse, sys, torch, torchaudio
from chatterbox.mtl_tts import ChatterboxMultilingualTTS


def log(*a):
    print(*a, file=sys.stderr, flush=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--text", required=True)
    ap.add_argument("--lang", default="es")
    ap.add_argument("--ref", default=None, help="reference wav to clone the voice from")
    args = ap.parse_args()

    device = "cuda" if torch.cuda.is_available() else "cpu"
    log(f"[chatterbox] loading model on {device} (first run downloads weights ~1GB)…")
    model = ChatterboxMultilingualTTS.from_pretrained(device=device)

    kw = {"audio_prompt_path": args.ref} if args.ref else {}
    log(f"[chatterbox] generating lang={args.lang}" + (f", cloning {args.ref}" if args.ref else ""))
    wav = model.generate(args.text, language_id=args.lang, **kw)

    if not torch.is_tensor(wav):
        wav = torch.as_tensor(wav)
    if wav.dim() == 1:
        wav = wav.unsqueeze(0)
    torchaudio.save(args.out, wav.detach().cpu().float(), model.sr)
    log(f"[chatterbox] wrote {args.out} @ {model.sr} Hz")


if __name__ == "__main__":
    main()
