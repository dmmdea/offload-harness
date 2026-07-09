#!/usr/bin/env python3
"""bench_aux.py — throughput/latency bench for the non-chat roster models served
by llama-swap (:11436): the always-loaded CPU memory stack.

  embed   -> embeddinggemma  : /v1/embeddings   (latency + embeddings/sec + dim)
  rerank  -> bge-reranker-v2-m3 : /v1/rerank     (latency + docs/sec)

These are CPU models (-ngl 0), so GPU contamination is irrelevant — but CPU
contention IS (run on an idle system for valid latency). Whisper RTF is measured
separately (WSL-side curl against /upstream/whisper-stt/inference with jfk.wav).

Usage:
  python bench_aux.py embed  --model embeddinggemma     --batch 32 --runs 5
  python bench_aux.py rerank --model bge-reranker-v2-m3 --docs 32  --runs 5
"""
import argparse
import json
import statistics
import time
import urllib.request

# 127.0.0.1, NOT localhost: Windows urllib stalls ~21s on the IPv6 ::1 attempt
# before falling back to IPv4 (llama-swap binds 127.0.0.1 only).
BASE = "http://127.0.0.1:11436"

# Realistic short memory-style texts; varied by index so nothing is a trivial dup.
def texts(n):
    base = [
        "The escalation tier runs a dense 9B escalation model in Q4_K_M.",
        "mem0 stores durable decisions and brand context for the ecosystem.",
        "The reasoning tier reclaims a deferral before falling through to Opus.",
        "Embeddings on the memory stack use EmbeddingGemma on the CPU.",
        "Whisper large-v3-turbo transcribes Spanish audio with low word error.",
        "The GPU is an RTX 3070 Mobile with eight gigabytes of VRAM.",
        "Prompt processing is compute-bound; decoding is memory-bandwidth-bound.",
        "The harness never calls a cloud model; it defers on low confidence.",
    ]
    return [f"[{i}] {base[i % len(base)]}" for i in range(n)]


def post(path, body):
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=120) as r:
        d = json.loads(r.read())
    return (time.perf_counter() - t0) * 1000, d


def bench_embed(model, batch, runs):
    inp = texts(batch)
    _ = post("/v1/embeddings", {"model": model, "input": inp})  # warmup
    lat, dim = [], None
    for _ in range(runs):
        ms, d = post("/v1/embeddings", {"model": model, "input": inp})
        lat.append(ms)
        if d.get("data"):
            dim = len(d["data"][0]["embedding"])
    med = statistics.median(lat)
    print(f"embeddinggemma  batch={batch}  median {med:.1f} ms/batch  "
          f"{batch / (med/1000):.1f} embeddings/s  dim={dim}  (runs={runs})")
    return {"model": model, "batch": batch, "median_ms": round(med, 1),
            "embeddings_per_s": round(batch / (med/1000), 1), "dim": dim, "runs": runs}


def bench_rerank(model, ndocs, runs):
    docs = texts(ndocs)
    query = "Which model runs the local reasoning tier?"
    # llama.cpp exposes reranking at /v1/rerank (fallback /rerank on older builds).
    path = "/v1/rerank"
    body = {"model": model, "query": query, "documents": docs, "top_n": ndocs}
    try:
        _ = post(path, body)
    except Exception:
        path = "/rerank"
        _ = post(path, body)
    lat = []
    for _ in range(runs):
        ms, _ = post(path, body)
        lat.append(ms)
    med = statistics.median(lat)
    print(f"bge-reranker    docs={ndocs}   median {med:.1f} ms  "
          f"{ndocs / (med/1000):.1f} docs/s  via {path}  (runs={runs})")
    return {"model": model, "docs": ndocs, "median_ms": round(med, 1),
            "docs_per_s": round(ndocs / (med/1000), 1), "endpoint": path, "runs": runs}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("mode", choices=["embed", "rerank"])
    ap.add_argument("--model", required=True)
    ap.add_argument("--batch", type=int, default=32)
    ap.add_argument("--docs", type=int, default=32)
    ap.add_argument("--runs", type=int, default=5)
    args = ap.parse_args()
    if args.mode == "embed":
        bench_embed(args.model, args.batch, args.runs)
    else:
        bench_rerank(args.model, args.docs, args.runs)


if __name__ == "__main__":
    main()
