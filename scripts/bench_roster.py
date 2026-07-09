#!/usr/bin/env python3
"""bench_roster.py — warm throughput bench for the local-offload model roster.

Measures each text-generation model THROUGH the live llama-swap serving path
(:11436), so the numbers reflect the exact deployed config (ngl / ctx / flash-attn
/ KV-quant), not a synthetic llama-bench invocation. It reads llama.cpp's own
`timings` block from the response, which measures the compute phases only:
  - prompt_per_second   -> PP tok/s (prefill)
  - predicted_per_second-> TG tok/s (decode)  <- the headline number
Cold-load (swap) cost is derived as: first-request wall - (prompt_ms+predicted_ms).

Methodology (per Benchmark-Table addenda): run on a CLEAN GPU only. Check
`nvidia-smi --query-compute-apps` first; kill Discord/Edge-WebView/browsers; a run
with competing VRAM consumers is CONTAMINATED and must be labelled as such.

Usage:
  python bench_roster.py gemma4-e2b offload-e4b gemma4-26b-a4b
  python bench_roster.py --runs 5 --max-tokens 256 offload-e4b
  python bench_roster.py --out ../.testrun/roster-bench.json <aliases...>
Each alias is a llama-swap model alias (see ~/llama-swap/config.yaml).
"""
import argparse
import json
import statistics
import subprocess
import time
import urllib.request

# 127.0.0.1, NOT localhost: on Windows, urllib tries IPv6 ::1 first, which stalls
# ~21s before falling back to IPv4 (llama-swap binds 127.0.0.1 only). That stall
# silently inflated wall-clock metrics; server-side `timings` were immune.
ENDPOINT = "http://127.0.0.1:11436/v1/chat/completions"

# A fixed ~300-token passage so PP has a meaningful prefill to measure, identical
# across every model so the comparison is apples-to-apples.
PROMPT = (
    "The following is a technical passage about local language-model inference. "
    "On consumer GPUs the dominant cost during decoding is memory bandwidth, not "
    "raw compute: each generated token requires streaming the full set of model "
    "weights and the growing key-value cache out of VRAM. Quantization to 4 bits "
    "shrinks the weight footprint roughly fourfold, which is why a 9-billion "
    "parameter model in Q4_K_M can run on an 8 GB card while its FP16 form cannot. "
    "Prompt processing, by contrast, is compute-bound and parallel across the "
    "prompt tokens, so it reports a much higher tokens-per-second figure than "
    "decoding. Flash attention reduces the memory traffic of the attention step, "
    "and quantizing the KV cache trades a small amount of quality for a larger "
    "usable context window. Mixture-of-experts models activate only a fraction of "
    "their parameters per token, so a 26B-A4B model decodes closer to the speed of "
    "a 4B dense model while retaining the knowledge of a much larger one, at the "
    "cost of holding all experts resident or paging them from system RAM. "
    "Summarize the key throughput trade-offs described above in detail, covering "
    "bandwidth, quantization, prompt processing, and mixture-of-experts."
)


def gpu_used_mib():
    """Return GPU MiB in use (None if nvidia-smi unavailable)."""
    try:
        out = subprocess.run(
            ["nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits"],
            capture_output=True, text=True, timeout=10).stdout.strip().splitlines()
        return int(out[0])
    except Exception:
        return None


def one_call(model, max_tokens):
    """One chat completion; returns (wall_ms, timings_dict_or_None, content_len)."""
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": PROMPT}],
        "max_tokens": max_tokens,
        "temperature": 0,
        "stream": False,
        # Disable prompt caching: an identical prompt across runs would otherwise hit
        # the server's prompt cache (prompt_n collapses to ~4), making PP tok/s measure
        # 4 tokens of overhead instead of the real ~300-token prefill.
        "cache_prompt": False,
    }).encode()
    req = urllib.request.Request(ENDPOINT, data=body, headers={"Content-Type": "application/json"})
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=600) as r:
        d = json.loads(r.read())
    wall_ms = (time.perf_counter() - t0) * 1000
    timings = d.get("timings")
    content = (d.get("choices", [{}])[0].get("message", {}) or {}).get("content", "")
    return wall_ms, timings, len(content or "")


def bench_model(model, runs, max_tokens):
    vram_before = gpu_used_mib()
    # Warmup: triggers the llama-swap load; its wall - compute = cold-load proxy.
    warm_wall, warm_t, warm_len = one_call(model, max_tokens)
    vram_after = gpu_used_mib()
    load_ms = None
    if warm_t:
        compute = warm_t.get("prompt_ms", 0) + warm_t.get("predicted_ms", 0)
        load_ms = max(0.0, warm_wall - compute)
    pp, tg, prompt_n, pred_n = [], [], None, None
    for _ in range(runs):
        _, t, _ = one_call(model, max_tokens)
        if not t:
            continue
        if t.get("prompt_per_second"):
            pp.append(t["prompt_per_second"])
        if t.get("predicted_per_second"):
            tg.append(t["predicted_per_second"])
        prompt_n = t.get("prompt_n", prompt_n)
        pred_n = t.get("predicted_n", pred_n)
    return {
        "model": model,
        "timings_available": warm_t is not None,
        "pp_tok_s_median": round(statistics.median(pp), 1) if pp else None,
        "tg_tok_s_median": round(statistics.median(tg), 1) if tg else None,
        "tg_tok_s_max": round(max(tg), 1) if tg else None,
        "prompt_n": prompt_n,
        "predicted_n": pred_n,
        "cold_load_ms": round(load_ms) if load_ms is not None else None,
        "vram_before_mib": vram_before,
        "vram_after_mib": vram_after,
        "vram_model_mib": (vram_after - vram_before) if (vram_before is not None and vram_after is not None) else None,
        "warmup_content_chars": warm_len,
        "runs": len(tg),
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("aliases", nargs="+", help="llama-swap model aliases to bench")
    ap.add_argument("--runs", type=int, default=3, help="measured runs per model (median)")
    ap.add_argument("--max-tokens", type=int, default=256)
    ap.add_argument("--out", default=None, help="write full JSON results here")
    args = ap.parse_args()

    print(f"# bench_roster :: {len(args.aliases)} models :: runs={args.runs} max_tokens={args.max_tokens}")
    print(f"# GPU used at start: {gpu_used_mib()} MiB  (CLEAN run needs the card near-idle)")
    print(f"{'model':<18}{'PP tok/s':>10}{'TG tok/s':>10}{'TG max':>9}{'prompt_n':>10}{'load ms':>9}{'VRAM MiB':>10}")
    results = []
    for a in args.aliases:
        try:
            r = bench_model(a, args.runs, args.max_tokens)
        except Exception as e:
            print(f"{a:<18}  ERROR: {e}")
            results.append({"model": a, "error": str(e)})
            continue
        results.append(r)
        print(f"{r['model']:<18}{str(r['pp_tok_s_median']):>10}{str(r['tg_tok_s_median']):>10}"
              f"{str(r['tg_tok_s_max']):>9}{str(r['prompt_n']):>10}{str(r['cold_load_ms']):>9}{str(r['vram_model_mib']):>10}")
    if args.out:
        with open(args.out, "w") as f:
            json.dump({"results": results, "max_tokens": args.max_tokens, "runs": args.runs}, f, indent=2)
        print(f"\n# wrote {args.out}")


if __name__ == "__main__":
    main()
