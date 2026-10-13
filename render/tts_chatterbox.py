#!/usr/bin/env python
# tts_chatterbox.py — Chatterbox Multilingual TTS worker (MIT license, commercial-safe,
# zero-shot voice cloning). Invoked by render/tts.mjs inside the .tts-venv. Clones the --ref
# voice if given (reproduces that speaker's accent — e.g. a reference Spanish voice); lang
# defaults to 'es'. Reconciled against the real installed API: ChatterboxMultilingualTTS
# .from_pretrained(device) -> .generate(text, language_id, audio_prompt_path=..., exaggeration=, cfg_weight=,
# temperature=, repetition_penalty=, min_p=, top_p=). The six sampling keywords are the --exaggeration /
# --cfg-weight / --temperature / --repetition-penalty / --min-p / --top-p flags render/tts.mjs forwards (the same
# names tts_chatterbox_ft.py reads); a flag that is not given is not passed, so the model keeps its own default.
import os
# Windows blocks symlinks without Developer Mode/elevation → HF cache os.symlink raises
# WinError 1314. Tell huggingface_hub to COPY into the cache instead. Must be set before any
# huggingface_hub import (chatterbox pulls it in), so it sits above that import.
os.environ.setdefault("HF_HUB_DISABLE_SYMLINKS", "1")
os.environ.setdefault("HF_HUB_DISABLE_SYMLINKS_WARNING", "1")
import argparse, math, sys, torch, torchaudio
# The wav is delivered through atomic_out.save_atomic (a sibling module): a save straight onto --out
# truncates it first, so a full disk left a zero-byte wav that read as finished and a failed save
# destroyed a previous good one. The script directory goes on the path explicitly so the import also
# works when the interpreter runs isolated (-I / -P).
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from atomic_out import save_atomic
from chatterbox.mtl_tts import ChatterboxMultilingualTTS


def log(*a):
    print(*a, file=sys.stderr, flush=True)


# generate()'s sampling keywords: the argparse dest of each flag below. Unset stays None and is NOT passed.
SAMPLING = ("temperature", "cfg_weight", "exaggeration", "repetition_penalty", "min_p", "top_p")


def finite_float(s):
    """argparse type: a float that is neither nan nor inf (float() alone reads 'nan', 'inf' and '1e999')."""
    try:
        v = float(s)
    except ValueError:
        v = math.nan
    if not math.isfinite(v):
        raise argparse.ArgumentTypeError(f"not a finite number: {s!r}")
    return v


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--text", required=True)
    ap.add_argument("--lang", default="es")
    ap.add_argument("--ref", default=None, help="reference wav to clone the voice from")
    # sampling knobs, the same flags (and dests) as tts_chatterbox_ft.py; None = leave the model's own default
    ap.add_argument("--temperature", type=finite_float, default=None)
    ap.add_argument("--cfg-weight", dest="cfg_weight", type=finite_float, default=None)
    ap.add_argument("--exaggeration", type=finite_float, default=None)
    ap.add_argument("--repetition-penalty", dest="repetition_penalty", type=finite_float, default=None)
    ap.add_argument("--min-p", dest="min_p", type=finite_float, default=None)
    ap.add_argument("--top-p", dest="top_p", type=finite_float, default=None)
    args = ap.parse_args()

    # J4 seam: TTS_DEVICE overrides torch device selection ("cpu", "cuda", "cuda:1",
    # "xpu", ...) — the AMD/CPU boxes' escape hatch. Unset = the old auto-pick.
    device = os.environ.get("TTS_DEVICE") or ("cuda" if torch.cuda.is_available() else "cpu")
    log(f"[chatterbox] loading model on {device} (first run downloads weights ~1GB)…")
    model = ChatterboxMultilingualTTS.from_pretrained(device=device)

    kw = {"audio_prompt_path": args.ref} if args.ref else {}
    sampling = {k: getattr(args, k) for k in SAMPLING if getattr(args, k) is not None}
    kw.update(sampling)
    log(f"[chatterbox] generating lang={args.lang}" + (f", cloning {args.ref}" if args.ref else "")
        + (f", sampling {sampling}" if sampling else ", model default sampling"))
    wav = model.generate(args.text, language_id=args.lang, **kw)

    if not torch.is_tensor(wav):
        wav = torch.as_tensor(wav)
    if wav.dim() == 1:
        wav = wav.unsqueeze(0)
    save_atomic(args.out, lambda p: torchaudio.save(p, wav.detach().cpu().float(), model.sr))
    log(f"[chatterbox] wrote {args.out} @ {model.sr} Hz")


if __name__ == "__main__":
    main()
