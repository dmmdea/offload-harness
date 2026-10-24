"""Timestral forecast server: one GPU model behind a loopback HTTP port, run by llama-swap.

llama-swap starts it on demand (`--port ${PORT}`), waits for GET /health, proxies
`/upstream/<model id>/v1/forecast` to it and stops it after the entry's ttl. It never
listens beyond 127.0.0.1 and has no authentication: the harness and llama-swap are its
only callers.

Wire contract
  GET  /health       -> 200 {"status": "ok", model, device, gpu, dtype, params, ...}
  POST /v1/forecast  -> 200 {"model", "prediction_length", "quantile_levels", "median",
                             "quantiles": {"<level>": ...}, "shape", "wall_ms", ...}
                        400 {"error": "bad_request", "detail"} for an input the model refuses
                        413 {"error": "too_large", "detail"} past the body or point caps
                        500 {"error": "forecast_failed", "detail"} when the model raises

Request body
  series               (T,), (B, T) or (B, V, T) numbers; null marks a missing value. Rows
                       of different lengths are left-padded with missing values.
  prediction_length    horizon in steps (1..max_horizon; longer horizons decode block by block)
  quantiles            levels to return, each one of the model's 21; default [0.1, 0.5, 0.9]
  past_covariates      (B, C, T) or, for one series, (C, T): known over the context only
  known_covariates     (B, C, T + prediction_length) or (C, T + prediction_length)
  categorical_past_covariates / categorical_known_covariates   the same shapes, category codes
  symmetric_averaging, clip_nonnegative   the forecaster's switches (both default true)

The output keeps the input's rank: (T,) in gives (H,) out, (B, T) gives (B, H), (B, V, T)
gives (B, V, H).

Memory: the model packs every target and covariate row of a batch into one attention pass, and
its variate mask grows with the square of the packed rows. A request is therefore run in chunks
of whole series whose rows times context patches stay under --max-row-patches; the answer is the
same as one pass, only the peak memory is bounded. The model loads and runs one warm-up forecast
before the port opens, so llama-swap's health check passes only on a server that can answer.
"""

import argparse
import json
import math
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import numpy as np
import torch

sys.stdout.reconfigure(encoding="utf-8", errors="replace")
sys.stderr.reconfigure(encoding="utf-8", errors="replace")

DEFAULT_QUANTILES = (0.1, 0.5, 0.9)
MAX_BODY = 64 << 20  # 64 MiB of JSON
MAX_POINTS = 4_000_000  # context values across the whole batch, covariates included
# Rows x context patches per forward pass. Measured on a 16 GB card (2026-10-24): 20 rows x 128
# patches peaked at 0.96 GiB allocated with the 0.79 GiB of fp32 weights, 140 rows x 128 patches
# at 2.46 GiB, about 0.1 MiB per row-patch, so 16384 row-patches stay near 2.4 GiB.
DEFAULT_MAX_ROW_PATCHES = 16384


class BadRequest(Exception):
    pass


def _as_array(value, name):
    """Nested lists of numbers/null to a float64 array; a ragged last axis is left-padded with NaN."""
    if value is None:
        return None
    if not isinstance(value, list) or not value:
        raise BadRequest(f"{name} must be a non-empty array")

    def depth(v):
        d = 0
        while isinstance(v, list):
            if not v:
                raise BadRequest(f"{name} holds an empty array")
            v = v[0]
            d += 1
        return d

    d = depth(value)
    if d > 3:
        raise BadRequest(f"{name} has {d} levels of nesting; at most 3 (series, variates, time)")

    def row(v):
        try:
            return np.array([math.nan if x is None else float(x) for x in v], dtype=np.float64)
        except (TypeError, ValueError):
            raise BadRequest(f"{name} must hold numbers or null")

    def pad(rows):
        n = max(len(r) for r in rows)
        return np.stack([np.concatenate([np.full(n - len(r), math.nan), r]) for r in rows])

    if d == 1:
        return row(value)
    if d == 2:
        return pad([row(r) for r in value])
    mats = [pad([row(r) for r in m]) for m in value]
    if len({m.shape[0] for m in mats}) != 1:
        raise BadRequest(f"{name}: every series must carry the same number of rows")
    n = max(m.shape[1] for m in mats)
    return np.stack([np.concatenate([np.full((m.shape[0], n - m.shape[1]), math.nan), m], axis=1) for m in mats])


def _covariates(value, name, batch):
    arr = _as_array(value, name)
    if arr is None:
        return None
    if arr.ndim == 1:
        arr = arr[None, None]
    elif arr.ndim == 2:
        if batch != 1:
            raise BadRequest(f"{name} must be (B, C, length) when several series are forecast")
        arr = arr[None]
    return arr


class Engine:
    def __init__(self, model_dir, device, model_id, max_row_patches=DEFAULT_MAX_ROW_PATCHES):
        from timestral import TimestralForecaster

        import timestral

        t0 = time.perf_counter()
        self.forecaster = TimestralForecaster.from_pretrained(model_dir, device=device, local_files_only=True)
        # The first forecast on a fresh CUDA context pays lazy kernel loading (measured 14.9 s
        # against 0.2 s warm); pay it here, before the port opens and llama-swap sees /health.
        self.forecaster.forecast(np.sin(np.arange(1024, dtype=np.float32) / 7.0), prediction_length=64)
        if device.type == "cuda":
            torch.cuda.synchronize()
        self.load_s = time.perf_counter() - t0
        self.model_id = model_id
        self.model_dir = str(model_dir)
        self.device = device
        self.max_row_patches = max(1, int(max_row_patches))
        self.lock = threading.Lock()
        self.levels = tuple(float(q) for q in self.forecaster.quantile_levels)
        cfg = self.forecaster.config
        self.health = {
            "status": "ok",
            "model": model_id,
            "model_dir": self.model_dir,
            "device": str(device),
            "gpu": torch.cuda.get_device_name(device) if device.type == "cuda" else None,
            "dtype": str(next(self.forecaster.model.parameters()).dtype).replace("torch.", ""),
            "params": sum(p.numel() for p in self.forecaster.model.parameters()),
            "context_length": int(cfg.context_length),
            "patch_size": int(cfg.patch_size),
            "max_horizon_per_pass": int(cfg.block_size * cfg.patch_size),
            "quantile_levels": list(self.levels),
            "supports_covariates": bool(self.forecaster.supports_covariates),
            "supports_known_covariates": bool(self.forecaster.supports_known_covariates),
            "timestral_version": getattr(timestral, "__version__", ""),
            "torch": torch.__version__,
            "load_s": round(self.load_s, 2),
        }

    def forecast(self, body, max_horizon):
        if not isinstance(body, dict):
            raise BadRequest("the body must be a JSON object")
        series = _as_array(body.get("series"), "series")
        if series is None:
            raise BadRequest("series is required")
        rank = series.ndim
        ctx = series if rank == 3 else (series[:, None] if rank == 2 else series[None, None])
        batch, _, t = ctx.shape
        if not np.isfinite(ctx).any(axis=-1).all():
            raise BadRequest("every series needs at least one observed value")
        h = body.get("prediction_length")
        if not isinstance(h, int) or isinstance(h, bool) or h < 1:
            raise BadRequest("prediction_length must be a positive integer")
        if h > max_horizon:
            raise BadRequest(f"prediction_length {h} is above this server's cap {max_horizon}")
        qs = body.get("quantiles", list(DEFAULT_QUANTILES))
        if not isinstance(qs, list) or not qs:
            raise BadRequest("quantiles must be a non-empty array of levels")
        wanted = []
        for q in qs:
            match = next((lv for lv in self.levels if isinstance(q, (int, float)) and abs(lv - q) < 1e-9), None)
            if match is None:
                raise BadRequest(f"quantile {q!r} is not one of the model's levels {list(self.levels)}")
            if match not in wanted:
                wanted.append(match)
        past = _covariates(body.get("past_covariates"), "past_covariates", batch)
        known = _covariates(body.get("known_covariates"), "known_covariates", batch)
        cat_past = _covariates(body.get("categorical_past_covariates"), "categorical_past_covariates", batch)
        cat_known = _covariates(body.get("categorical_known_covariates"), "categorical_known_covariates", batch)
        points = ctx.size + sum(a.size for a in (past, known, cat_past, cat_known) if a is not None)
        if points > MAX_POINTS:
            raise TooLarge(f"{points} input values; the cap is {MAX_POINTS}")
        for name, arr, length in (("past_covariates", past, t), ("categorical_past_covariates", cat_past, t),
                                  ("known_covariates", known, t + h),
                                  ("categorical_known_covariates", cat_known, t + h)):
            if arr is not None and (arr.shape[0] != batch or arr.shape[-1] != length):
                raise BadRequest(f"{name} must be (B={batch}, C, {length}); got {list(arr.shape)}")
        sym = body.get("symmetric_averaging", True)
        clip = body.get("clip_nonnegative", True)
        if not isinstance(sym, bool) or not isinstance(clip, bool):
            raise BadRequest("symmetric_averaging and clip_nonnegative must be booleans")

        # Whole series per chunk, rows (targets + every covariate row) x patches under the cap.
        rows_per_series = ctx.shape[1] + sum(a.shape[1] for a in (past, known, cat_past, cat_known) if a is not None)
        patches = math.ceil(min(t, self.forecaster.max_context) / self.forecaster.patch_size)
        per_chunk = max(1, self.max_row_patches // max(1, rows_per_series * patches))

        def part(a, lo, hi):
            return None if a is None else a[lo:hi]

        with self.lock:
            t0 = time.perf_counter()
            pieces = []
            try:
                for lo in range(0, batch, per_chunk):
                    hi = min(batch, lo + per_chunk)
                    out = self.forecaster.forecast(
                        ctx[lo:hi].astype(np.float32),
                        prediction_length=h,
                        past_covariates=None if past is None else past[lo:hi].astype(np.float32),
                        known_covariates=None if known is None else known[lo:hi].astype(np.float32),
                        categorical_past_covariates=part(cat_past, lo, hi),
                        categorical_known_covariates=part(cat_known, lo, hi),
                        symmetric_averaging=sym,
                        clip_nonnegative=clip,
                    )
                    pieces.append(out.quantiles.float().cpu().numpy())  # (b, V, H, Q)
            except (ValueError, AssertionError) as e:
                raise BadRequest(str(e) or type(e).__name__)
            except torch.cuda.OutOfMemoryError as e:
                torch.cuda.empty_cache()
                raise TooLarge(f"the card ran out of memory ({e}); send fewer or shorter series")
            q = np.concatenate(pieces, axis=0)
            if self.device.type == "cuda":
                torch.cuda.synchronize()
            wall_ms = (time.perf_counter() - t0) * 1000.0

        def shaped(a):  # a: (B, V, H) back to the input's rank
            if rank == 1:
                a = a[0, 0]
            elif rank == 2:
                a = a[:, 0]
            return a.tolist()

        median = q[..., self.levels.index(0.5)]
        result = {
            "model": self.model_id,
            "prediction_length": h,
            "context_length_used": int(min(t, self.forecaster.max_context)),
            "shape": [int(x) for x in median.shape] if rank == 3 else ([batch, h] if rank == 2 else [h]),
            "quantile_levels": wanted,
            "median": shaped(median),
            "quantiles": {f"{lv:g}": shaped(q[..., self.levels.index(lv)]) for lv in wanted},
            "wall_ms": round(wall_ms, 1),
            "chunks": len(pieces),
            "device": self.health["gpu"] or str(self.device),
        }
        return result


class TooLarge(Exception):
    pass


def make_handler(engine, max_horizon):
    class Handler(BaseHTTPRequestHandler):
        server_version = "timestral-forecast/1"

        def log_message(self, fmt, *args):
            sys.stderr.write("forecast-server: " + (fmt % args) + "\n")

        def _send(self, code, payload):
            data = json.dumps(payload, allow_nan=False).encode("utf-8")
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            if self.path.split("?", 1)[0] in ("/health", "/v1/health"):
                self._send(200, engine.health)
            elif self.path.split("?", 1)[0] == "/v1/models":
                self._send(200, {"object": "list", "data": [{"id": engine.model_id, "object": "model"}]})
            else:
                self._send(404, {"error": "not_found", "detail": self.path})

        def do_POST(self):
            if self.path.split("?", 1)[0] != "/v1/forecast":
                self._send(404, {"error": "not_found", "detail": self.path})
                return
            n = int(self.headers.get("Content-Length") or 0)
            if n <= 0:
                self._send(400, {"error": "bad_request", "detail": "empty body"})
                return
            if n > MAX_BODY:
                self._send(413, {"error": "too_large", "detail": f"body of {n} bytes; the cap is {MAX_BODY}"})
                return
            try:
                body = json.loads(self.rfile.read(n))
            except (ValueError, UnicodeDecodeError) as e:
                self._send(400, {"error": "bad_request", "detail": f"invalid JSON: {e}"})
                return
            try:
                self._send(200, engine.forecast(body, max_horizon))
            except BadRequest as e:
                self._send(400, {"error": "bad_request", "detail": str(e)})
            except TooLarge as e:
                self._send(413, {"error": "too_large", "detail": str(e)})
            except Exception as e:  # the model raised: report, keep serving
                self._send(500, {"error": "forecast_failed", "detail": f"{type(e).__name__}: {e}"})

    return Handler


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    ap.add_argument("--model", required=True, help="local checkpoint directory (config.json + model.safetensors)")
    ap.add_argument("--model-id", default="timestral-209m", help="the id the server reports")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--device", default="cuda", help="cuda (default), cuda:N, xpu or mps; never cpu unless --allow-cpu")
    ap.add_argument("--allow-cpu", action="store_true", help="permit a CPU device (for a box with no card)")
    ap.add_argument("--max-horizon", type=int, default=4096, help="largest prediction_length served")
    ap.add_argument("--max-row-patches", type=int, default=DEFAULT_MAX_ROW_PATCHES,
                    help="rows x context patches per forward pass (bounds peak card memory)")
    args = ap.parse_args()

    if args.host not in ("127.0.0.1", "::1", "localhost"):
        ap.error("the server binds loopback only")
    device = torch.device(args.device)
    if device.type == "cpu" and not args.allow_cpu:
        ap.error("refusing a CPU device: inference belongs on a card (pass --allow-cpu to override)")
    if device.type == "cuda" and not torch.cuda.is_available():
        ap.error("no CUDA device is visible to this process")
    if not (Path(args.model) / "model.safetensors").is_file():
        ap.error(f"{args.model} holds no model.safetensors")

    engine = Engine(Path(args.model), device, args.model_id, args.max_row_patches)
    httpd = ThreadingHTTPServer((args.host, args.port), make_handler(engine, args.max_horizon))
    httpd.daemon_threads = True
    print(json.dumps({"event": "ready", "port": args.port, **engine.health}), flush=True)
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
