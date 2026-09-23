#!/usr/bin/env bash
# repatch-lmcache-overlay.sh — build the LMCache overlay a vLLM seat loads through SEAT_LMCACHE_PYTHONPATH: a full copy of
# the LMCache that is CURRENTLY installed in the seat venv, with the ordered patches in $PATCHES applied on top
# (default: the directory this script ships in, lmcache-patches/ — named apart from the overlay it builds, whose default
# path is <seat dir>/lmcache-overlay). Nothing in site-packages is touched.
#
# Why an overlay instead of editing site-packages: the seat loads LMCache via PYTHONPATH=$OVERLAY (SEAT_LMCACHE_PYTHONPATH
# in the seat env), so the installed package stays pristine and dropping the overlay is one env line. seat_fg.sh starts
# BOTH the engine and its lmcache-mp server (a transient systemd-run unit, re-created every seat start) from that one
# variable, so server and engine always load the same overlay.
# Because the overlay is a FULL copy of the installed package it must be REBUILT after any lmcache upgrade — run this.
#
# Shipped patch set (base LMCache 0.5.5; ordered, applied with patch -p1 against the installed package):
#   01-pr4253.diff                  LMCache PR #4253: fp8-KV / FlashInfer store fix on hybrid models (#4247)
#   02-house-pp-layout.diff         per-rank L2->L1 buffer layouts for pipeline-parallel seats (HOUSE-PP-LAYOUT): the
#                                   layout registry is keyed by (model, world size) and kept one rank's layout, so a
#                                   pipeline seat whose stages hold different layer mixes got 0 L2 hits
#   04-house-pp-register-bind.diff  bind each rank's layouts at REGISTER (optional 8th REGISTER_KV_CACHE payload, opted in
#                                   for that request type only), so the FIRST request after an MP server start hits too;
#                                   the store-time bind stays as the fallback AND the check
#   05-backport-pr4709.diff         report a failed retrieve's blocks to vLLM (LMCache PR #4709, vLLM adapter hunk only).
#                                   vLLM recomputes them ONLY with kv_load_failure_policy "recompute" (seat env:
#                                   SEAT_KV_LOAD_FAILURE_POLICY=recompute); its default "fail" fails the request. The
#                                   deploy gate below refuses to put 05 into an overlay a seat env names without it.
#   06-backport-pr5249.diff         Mamba align/all: exclude the last prompt token from the lookup range (LMCache PR #5249)
#   smoke-overlay.py                CPU-only test of every patch (no server, model or GPU), run on the NEW tree before the
#                                   swap; it must exit 0
# (03 — the vLLM >= 0.29 kv-layout resolution — is upstream in LMCache 0.5.5 and no longer shipped.)
# The engine and its MP server must run the same patch set: an engine with 04 against a server without it does not
# start (the old server drops the 8-frame REGISTER and the engine's register times out after mq_timeout).
# A patch whose change is already in the base (patch -R --dry-run applies) is SKIPPED and recorded, never forced.
#
#   bash repatch-lmcache-overlay.sh                                          # rebuild the default overlay
#   VENV=/root/g7/vllm-env-030 OVERLAY=/root/g7/lmcache-overlay-055 bash repatch-lmcache-overlay.sh
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VENV="${VENV:-/root/g7/vllm-env}"
OVERLAY="${OVERLAY:-/root/g7/lmcache-overlay}"
PATCHES="${PATCHES:-$HERE}"
MP_UNITS="${MP_UNITS:-lmcache-mp}"   # systemd units that may load the overlay; the swap refuses while one that does is active
SEAT_FG="${SEAT_FG:-/root/g7/seat_fg.sh}"
SEAT_ENVS="${SEAT_ENVS:-/root/g7/seat-*.env}"
PY="$VENV/bin/python"

SP="$("$PY" -c 'import lmcache,os;print(os.path.dirname(lmcache.__file__))' 2>/dev/null)"
VER="$("$PY" -c 'import lmcache;print(lmcache.__version__)' 2>/dev/null)"
[ -n "$SP" ] || { echo "repatch: lmcache not importable in $VENV"; exit 1; }
ls "$PATCHES"/0*.diff >/dev/null 2>&1 || { echo "repatch: no patches in $PATCHES"; exit 1; }
echo "repatch: installed lmcache $VER at $SP; patches from $PATCHES"

NEW="$OVERLAY.new"
rm -rf "$NEW"; mkdir -p "$NEW"
cp -r "$SP" "$NEW/lmcache"
PROV="lmcache $VER from $VENV, built $(date -u +%Y-%m-%dT%H:%M:%SZ)"
applied=0
for p in "$PATCHES"/0*.diff; do
  name="$(basename "$p")"; sum="$(sha256sum "$p" | cut -c1-16)"
  if ( cd "$NEW" && patch -p1 -R --dry-run -s -f <"$p" >/dev/null 2>&1 ); then
    echo "repatch: $name already in the base — skipped"; PROV="$PROV
skip $name $sum (already in base)"; continue
  fi
  if ! ( cd "$NEW" && patch -p1 --forward -s <"$p" ); then
    echo "repatch: $name did NOT apply cleanly onto lmcache $VER — overlay left untouched; inspect $NEW"; exit 1
  fi
  find "$NEW" -name '*.orig' -delete
  echo "repatch: applied $name"; PROV="$PROV
apply $name $sum"; applied=$((applied+1))
done

# Verify: the overlay imports from the NEW tree and carries the marker of every patch in the set (applied or in the base).
OUT="$(cd /tmp && PYTHONPATH="$NEW" CUDA_VISIBLE_DEVICES="" "$PY" -B - <<'PYEOF' 2>&1
import inspect, os
import lmcache
import lmcache.integration.vllm.kv_cache_group_edits as g
from lmcache.v1.multiprocess.engine_context import LayoutDescRegistry
root = os.path.realpath(os.environ["PYTHONPATH"])
print("from_overlay=%s" % os.path.realpath(lmcache.__file__).startswith(root))
print("fused_kv=%s" % ("_FUSED_KV_NDIM" in dir(g)))
print("pp_layout=%s" % hasattr(LayoutDescRegistry, "bind_rank_layouts"))
def marker(name, fn):
    try:
        print("%s=%s" % (name, bool(fn())))
    except Exception as e:  # a missing symbol is a False marker, not a crash
        print("%s=False (%r)" % (name, e))
def _register_bind():
    from lmcache.v1.multiprocess.modules.lmcache_driven_transfer import LMCacheDrivenTransferModule as M
    from lmcache.v1.multiprocess.protocol import get_min_payload_count, get_payload_classes
    from lmcache.v1.multiprocess.protocols.base import RequestType
    rk = RequestType.REGISTER_KV_CACHE
    return ("kv_worker_id" in inspect.signature(M.register_kv_cache).parameters
            and hasattr(M, "_house_store_bind")
            and get_min_payload_count(rk) == len(get_payload_classes(rk)) - 1
            and get_min_payload_count(RequestType.PING) == len(get_payload_classes(RequestType.PING)))
def _pr4709():
    import lmcache.integration.vllm.vllm_multi_process_adapter as a
    import lmcache.integration.vllm.lmcache_mp_connector as c
    return (inspect.getsource(a.LMCacheMPWorkerAdapter.get_finished).count("error_block_ids.update(r_block_ids)") >= 2
            and "kv_load_failure_policy" in inspect.getsource(c.LMCacheMPConnector.__init__))
def _pr5249():
    from lmcache.integration.vllm.vllm_multi_process_adapter import LMCacheMPSchedulerAdapter as S
    return "reserve_last_token" in inspect.signature(S.maybe_submit_lookup_request).parameters
marker("register_bind", _register_bind)
marker("pr4709", _pr4709)
marker("pr5249", _pr5249)
print("lmcache=%s" % lmcache.__version__)
PYEOF
)"
echo "$OUT" | grep -E '^[a-z0-9_]+='
WANT="from_overlay=True fused_kv=True pp_layout=True"
[ -e "$PATCHES/04-house-pp-register-bind.diff" ] && WANT="$WANT register_bind=True"
[ -e "$PATCHES/05-backport-pr4709.diff" ] && WANT="$WANT pr4709=True"
[ -e "$PATCHES/06-backport-pr5249.diff" ] && WANT="$WANT pr5249=True"
for want in $WANT; do
  echo "$OUT" | grep -qx "$want" || { echo "repatch: VERIFY FAILED ($want missing) — overlay left untouched; inspect $NEW"; exit 1; }
done
if [ -f "$PATCHES/smoke-overlay.py" ]; then
  SOUT="$(cd /tmp && PYTHONPATH="$NEW" CUDA_VISIBLE_DEVICES="" "$PY" -B "$PATCHES/smoke-overlay.py" 2>&1)"; src=$?
  echo "$SOUT" | grep -E '^FAIL |^SMOKE TOTAL'
  [ "$src" = 0 ] || { echo "repatch: VERIFY FAILED (smoke-overlay.py rc=$src) — overlay left untouched; inspect $NEW"; exit 1; }
  PROV="$PROV
smoke smoke-overlay.py $(sha256sum "$PATCHES/smoke-overlay.py" | cut -c1-16) $(echo "$SOUT" | grep '^SMOKE TOTAL' | tail -1)"
fi
printf '%s\n' "$PROV" > "$NEW/.overlay-provenance"

# Deploy gate for 05 (#4709): an overlay a seat env names is a deployment. It carries 05 only when EVERY seat that names
# it asks vLLM to recompute failed loads — SEAT_KV_LOAD_FAILURE_POLICY=recompute in that env (the last assignment wins, as
# when seat_fg.sh sources it), or an older launcher that hard-codes the policy in --kv-transfer-config; otherwise the first
# failed retrieve would fail a user request.
SEAT_NAMES="$(grep -lsE "^SEAT_LMCACHE_PYTHONPATH=$OVERLAY/?([[:space:]]|$)" $SEAT_ENVS 2>/dev/null | tr '\n' ' ')"
if [ -n "$SEAT_NAMES" ] && grep -q "^apply 05-backport-pr4709.diff" "$NEW/.overlay-provenance"; then
  NO_RECOMPUTE=""
  for env in $SEAT_NAMES; do
    pol="$(grep -E '^SEAT_KV_LOAD_FAILURE_POLICY=' "$env" | tail -1 | sed -E 's/^[^=]*=["'\'']?([a-z]*).*/\1/')"
    [ "$pol" = recompute ] && continue
    grep -qE 'kv_load_failure_policy[\\"]*:[\\"]*recompute' "$SEAT_FG" 2>/dev/null && continue
    NO_RECOMPUTE="$NO_RECOMPUTE $env"
  done
  if [ -n "$NO_RECOMPUTE" ]; then
    if [ "${REPATCH_ALLOW_FAIL_POLICY:-0}" != 1 ]; then
      echo "repatch: built $NEW but NOT swapped — $OVERLAY is named by$NO_RECOMPUTE, which does not set"
      echo "  SEAT_KV_LOAD_FAILURE_POLICY=recompute (05 would turn a failed retrieve into a failed request). Set it there, or"
      echo "  rerun with REPATCH_ALLOW_FAIL_POLICY=1 to accept vLLM's \"fail\" policy."
      exit 3
    fi
    echo "repatch: WARNING — REPATCH_ALLOW_FAIL_POLICY=1:$NO_RECOMPUTE keep(s) vLLM's \"fail\" policy; a failed retrieve fails the request"
  fi
fi

# Swap only while no running MP server has THIS overlay loaded (a live server keeps the old tree's modules in memory).
# A unit that loads a different overlay path is not affected by this swap.
for u in $MP_UNITS; do
  if systemctl is-active --quiet "$u" 2>/dev/null; then
    uenv="$(systemctl show -p Environment "$u" 2>/dev/null)"
    if echo "$uenv" | grep -qE "PYTHONPATH=([^ ]*:)?$OVERLAY/?(:| |$)"; then
      echo "repatch: built $NEW but NOT swapped — $u is running from $OVERLAY. Stop the seat, then: mv $OVERLAY $OVERLAY.prev && mv $NEW $OVERLAY"
      exit 2
    fi
    echo "repatch: $u is running but does not load $OVERLAY ($(echo "$uenv" | grep -oE 'PYTHONPATH=[^ ]*' || echo 'no PYTHONPATH')) — swap is safe"
  fi
done
rm -rf "$OVERLAY.prev"; [ -e "$OVERLAY" ] && mv "$OVERLAY" "$OVERLAY.prev"
mv "$NEW" "$OVERLAY"
echo "repatch: OK — $applied patch(es) applied; previous overlay kept at $OVERLAY.prev"
cat "$OVERLAY/.overlay-provenance"
