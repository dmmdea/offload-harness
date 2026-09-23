"""CPU-only smoke tests for the LMCache 0.5.5 overlay (patches-055: 01, 02, 04-06 + review fixes F1-F5).

Ported from patches-v2/smoke-overlay.py (0.5.4) to the 0.5.5 APIs (2026-09-22):
- REGISTER goes through a transport RequestClient (ZmqMultiprocessClient) instead of
  send_request(mq_client, ...); the 8th-frame rule is checked on that client.
- MessageQueueClient.process_outbound_task sets a payload-count error on the request's
  future instead of raising it.
- IPCCacheServerKey carries num_kv_readers (lookup requires it); _create_key takes
  request_configs; the connector __init__ runs more vLLM-config helpers before the checks.

No server, no model, no GPU work. Run with PYTHONPATH=<overlay root> CUDA_VISIBLE_DEVICES="".
"""
import importlib
import os
import queue
import sys
import threading
from types import SimpleNamespace
from unittest.mock import MagicMock

import torch

ROOT = os.path.realpath(os.environ["PYTHONPATH"].split(":")[0])
PASS = FAIL = 0


def check(name, cond, detail=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print(f"PASS {name}")
    else:
        FAIL += 1
        print(f"FAIL {name} {detail}")


# ---- 1. imports of every changed module, from the overlay ----
MODS = [
    "lmcache.v1.distributed.transfer_channel.impl",  # 07: imports with a broken mooncake install
    "lmcache.v1.multiprocess.protocols.base",
    "lmcache.v1.multiprocess.protocols.engine",
    "lmcache.v1.multiprocess.protocol",
    "lmcache.v1.multiprocess.mq",
    "lmcache.v1.multiprocess.transfer_context.worker_transfer",
    "lmcache.v1.multiprocess.transport.base",
    "lmcache.v1.multiprocess.transport.zmq_impl.client",
    "lmcache.v1.multiprocess.engine_context",
    "lmcache.v1.multiprocess.modules.lmcache_driven_transfer",
    "lmcache.v1.multiprocess.modules.lookup",
    "lmcache.v1.distributed.storage_controllers.prefetch_controller",
    "lmcache.v1.mp_observability.subscribers.metrics.l2_failures",
    "lmcache.integration.vllm.vllm_multi_process_adapter",
    "lmcache.integration.vllm.lmcache_mp_connector",
]
for m in MODS:
    try:
        mod = importlib.import_module(m)
        check(f"import {m}", os.path.realpath(mod.__file__).startswith(ROOT), mod.__file__)
    except Exception as e:  # noqa: BLE001
        check(f"import {m}", False, repr(e))

from lmcache.v1.distributed.api import (  # noqa: E402
    DEFAULT_ATTN_WINDOW_DESC,
    MemoryLayoutDesc,
    ObjectKey,
    ipc_key_to_object_keys,
)
from lmcache.v1.multiprocess.custom_types import IPCCacheServerKey  # noqa: E402
from lmcache.v1.multiprocess.engine_context import LayoutDescRegistry  # noqa: E402
import lmcache.v1.multiprocess.mq as mq  # noqa: E402
from lmcache.v1.multiprocess.protocol import get_payload_classes  # noqa: E402
from lmcache.v1.multiprocess.protocols.base import (  # noqa: E402
    HandlerType,
    ProtocolDefinition,
    RequestType,
)
import lmcache.v1.multiprocess.modules.lmcache_driven_transfer as ldt  # noqa: E402
import lmcache.v1.multiprocess.transfer_context.worker_transfer as wt  # noqa: E402
from lmcache.v1.distributed.storage_controllers import prefetch_controller as pc  # noqa: E402
from lmcache.v1.distributed.error import L1Error  # noqa: E402
from lmcache.v1.distributed.storage_controllers.prefetch_controller import PrefetchMode  # noqa: E402
import msgspec  # noqa: E402
from lmcache.utils import EngineType  # noqa: E402


def lay(n):
    return MemoryLayoutDesc(shapes=[torch.Size([2, n, 256, 1024])], dtypes=[torch.bfloat16])


# ---- 2. LayoutDescRegistry register / bind / find ----
r = LayoutDescRegistry()
check("bind before register returns False", r.bind_rank_layouts("m", 3, 0, {0: lay(16)}) is False)
check("find on missing pair", r.find_rank_layouts("m", 3) == (False, {}))
r.register("m", 3, lay(16), DEFAULT_ATTN_WINDOW_DESC, group_layout_descs={0: lay(16)})
check("heterogeneous False after 1 register", r.find_rank_layouts("m", 3)[0] is False)
r.register("m", 3, lay(16), DEFAULT_ATTN_WINDOW_DESC, group_layout_descs={0: lay(16)})
check("heterogeneous False for identical layouts", r.find_rank_layouts("m", 3)[0] is False)
r.register("m", 3, lay(17), DEFAULT_ATTN_WINDOW_DESC, group_layout_descs={0: lay(17)})
check("heterogeneous True for differing layouts", r.find_rank_layouts("m", 3)[0] is True)
check("bind after register returns True", r.bind_rank_layouts("m", 3, 5, {0: lay(17)}) is True)
het, ranks = r.find_rank_layouts("m", 3)
check("find returns bound rank", het and ranks.get(5, {}).get(0) == lay(17), str(ranks))
check("bind on other pair returns False", r.bind_rank_layouts("m", 2, 5, {0: lay(17)}) is False)

# ---- 3. payload-count rule: opt-in per request type (F3) ----
clss = get_payload_classes(RequestType.REGISTER_KV_CACHE)
check("REGISTER_KV_CACHE has 8 payload classes", len(clss) == 8, str(clss))
check("REGISTER_KV_CACHE min payload count is 7", mq.min_payload_count(RequestType.REGISTER_KV_CACHE) == 7)
loosened = [rt.name for rt in RequestType if mq.min_payload_count(rt) != len(get_payload_classes(rt))]
check(f"F3: only REGISTER_KV_CACHE is loosened (of {len(list(RequestType))} request types)",
      loosened == ["REGISTER_KV_CACHE"], str(loosened))
ping_clss = get_payload_classes(RequestType.PING)
check("F3: PING keeps its exact count", mq.min_payload_count(RequestType.PING) == len(ping_clss) == 1, str(ping_clss))
try:
    mq.unwrap_request_payloads([], ping_clss, mq._handler_min_count(RequestType.PING, ping_clss))
    check("F3: server rejects a 0-frame PING", False)
except ValueError:
    check("F3: server rejects a 0-frame PING", True)
check("F3: _handler_min_count(PING) is None (exact)", mq._handler_min_count(RequestType.PING, ping_clss) is None)
check("F3: _handler_min_count(REGISTER_KV_CACHE) == 7", mq._handler_min_count(RequestType.REGISTER_KV_CACHE, clss) == 7)
srv = object.__new__(mq.MessageQueueServer)
srv.handlers = {}
srv.add_blocking_handler(RequestType.PING, ping_clss, lambda x: None)
srv.add_sync_handler(RequestType.REGISTER_KV_CACHE, clss, lambda *a: None)
check("F3: server handlers carry the per-type min count",
      srv.handlers[RequestType.PING].min_count is None and srv.handlers[RequestType.REGISTER_KV_CACHE].min_count == 7)
try:
    ProtocolDefinition(payload_classes=[int, str], response_class=None, handler_type=HandlerType.SYNC,
                       optional_trailing_payloads=1)
    check("F3: optional trailing payload must admit None", False)
except ValueError:
    check("F3: optional trailing payload must admit None", True)
try:
    ProtocolDefinition(payload_classes=[int], response_class=None, handler_type=HandlerType.SYNC,
                       optional_trailing_payloads=2)
    check("F3: optional count out of range rejected", False)
except ValueError:
    check("F3: optional count out of range rejected", True)
for rt in (RequestType.STORE, RequestType.RETRIEVE, RequestType.REGISTER_Q_CACHE, RequestType.LOOKUP):
    c = get_payload_classes(rt)
    try:
        mq.unwrap_request_payloads([b"\xc0"] * (len(c) - 1), c, mq._handler_min_count(rt, c))
        check(f"{rt.name}: one frame short rejected", False)
    except ValueError:
        check(f"{rt.name}: one frame short rejected", True)

# The KVCache payload needs real device IPC handles; swap its codec for a plain msgpack one for
# this test only (the rest of the payload list goes through the real codecs).
KVC = clss[1]
saved = mq._SPECIAL_ENCODER_DECODERS[KVC]
mq._SPECIAL_ENCODER_DECODERS[KVC] = (msgspec.msgpack.Encoder(), msgspec.msgpack.Decoder())
try:
    mc = mq._handler_min_count(RequestType.REGISTER_KV_CACHE, clss)
    vals = [4242, [], "m", 3, EngineType.VLLM, {}, []]
    old = [mq.msgspec_encode(v, cls=c) for v, c in zip(vals, clss[:7])]
    dec_old = mq.unwrap_request_payloads(old, clss, mc)
    check("old client (7 frames) decodes, kv_worker_id=None", len(dec_old) == 8 and dec_old[7] is None and dec_old[0] == 4242, str(dec_old))
    new = old + [mq.msgspec_encode(2, cls=clss[7])]
    dec_new = mq.unwrap_request_payloads(new, clss, mc)
    check("new client (8 frames) decodes kv_worker_id=2", dec_new[7] == 2 and type(dec_new[7]) is int, str(dec_new))
    newnone = old + [mq.msgspec_encode(None, cls=clss[7])]
    check("explicit None 8th frame decodes None", mq.unwrap_request_payloads(newnone, clss, mc)[7] is None)
    for bad, label in ((old[:6], "6 frames"), (new + [b"\xc0"], "9 frames")):
        try:
            mq.unwrap_request_payloads(bad, clss, mc)
            check(f"{label} rejected", False)
        except ValueError:
            check(f"{label} rejected", True)

    # client outbound check (the same per-type rule on the sending side). 0.5.5: a refused
    # request's error is set on its future (not raised); re-raise it here.
    def outbound(rt, payloads):
        cl = object.__new__(mq.MessageQueueClient)
        cl.input_queue = queue.Queue()
        cl.pending_futures = {}
        cl.socket = MagicMock()
        fut = MagicMock()
        cl.input_queue.put(mq.MessageQueueClient.WrappedRequest(1, fut, rt, payloads))
        cl.process_outbound_task()
        if fut.set_exception.called:
            raise fut.set_exception.call_args.args[0]
        return cl.socket.send_multipart.call_args.args[0]

    frames = outbound(RequestType.REGISTER_KV_CACHE, list(vals))
    check("client sends a 7-payload REGISTER_KV_CACHE", len(frames) == 2 + 7, str(len(frames)))
    frames = outbound(RequestType.REGISTER_KV_CACHE, list(vals) + [5])
    check("client sends an 8-payload REGISTER_KV_CACHE", len(frames) == 2 + 8, str(len(frames)))
    try:
        outbound(RequestType.PING, [])
        check("F3: client refuses a 0-payload PING", False)
    except ValueError:
        check("F3: client refuses a 0-payload PING", True)
finally:
    mq._SPECIAL_ENCODER_DECODERS[KVC] = saved

# ---- 3b. worker register omits the 8th frame when kv_worker_id is None (F2) ----
wt._get_kv_device = lambda kv: torch.device("cpu")
wt.get_event_ipc_backend = lambda d: SimpleNamespace(check_event_support=lambda d: None)
wt.wrap_kv_caches = lambda kv: "KV"
from lmcache.v1.multiprocess.transport.zmq_impl.client import ZmqMultiprocessClient  # noqa: E402


class FakeMQClient:
    """Stands in for MessageQueueClient under the real ZmqMultiprocessClient."""

    def __init__(self):
        self.sent = []

    def submit_request(self, rt, payloads, response_cls=None):
        self.sent.append((rt, list(payloads)))
        return SimpleNamespace(result=lambda timeout=None: None)


for wid, want in ((None, 7), (0, 8), (2, 8)):
    fmq = FakeMQClient()
    ctxw = wt.LMCacheDrivenTransferContext()
    ctxw.register(1, {"l0": torch.zeros(1)}, "m", 3, 16, ZmqMultiprocessClient(fmq), 1.0, kv_worker_id=wid)
    sent = fmq.sent
    check(f"F2: worker register kv_worker_id={wid} sends {want} payloads",
          len(sent) == 1 and sent[0][0] == RequestType.REGISTER_KV_CACHE and len(sent[0][1]) == want
          and (wid is None or sent[0][1][7] == wid), str(sent))
    # the payload list the transport built passes the client's outbound per-type rule
    if len(sent) == 1:
        KVC0 = clss[1]
        saved0 = mq._SPECIAL_ENCODER_DECODERS[KVC0]
        mq._SPECIAL_ENCODER_DECODERS[KVC0] = (msgspec.msgpack.Encoder(), msgspec.msgpack.Decoder())
        try:
            fr = outbound(RequestType.REGISTER_KV_CACHE, [4242, [], "m", 3, EngineType.VLLM, {}, []] + sent[0][1][7:])
            check(f"F2: kv_worker_id={wid} REGISTER leaves the client as {want} payload frames", len(fr) == 2 + want, str(len(fr)))
        finally:
            mq._SPECIAL_ENCODER_DECODERS[KVC0] = saved0


class LegacyRequestClient:
    """A RequestClient whose register_kv_cache predates the kv_worker_id keyword."""

    def __init__(self):
        self.calls = 0

    def register_kv_cache(self, instance_id, kv_cache, model_name, world_size, engine_type, layout_hints,
                          engine_group_infos):
        self.calls += 1
        return SimpleNamespace(result=lambda timeout=None: None)


lrc = LegacyRequestClient()
try:
    wt.LMCacheDrivenTransferContext().register(1, {"l0": torch.zeros(1)}, "m", 3, 16, lrc, 1.0)
    check("F2: kv_worker_id=None works with a RequestClient without the keyword", lrc.calls == 1)
except TypeError as e:
    check("F2: kv_worker_id=None works with a RequestClient without the keyword", False, repr(e))
import inspect as _insp0  # noqa: E402
from lmcache.v1.multiprocess.transport.base import RequestClient  # noqa: E402
check("transport: RequestClient.register_kv_cache declares kv_worker_id=None",
      _insp0.signature(RequestClient.register_kv_cache).parameters.get("kv_worker_id") is not None
      and _insp0.signature(RequestClient.register_kv_cache).parameters["kv_worker_id"].default is None)

# Server-side handler signature check (the MQ server refuses to register a mismatching handler).
mod_obj = object.__new__(ldt.LMCacheDrivenTransferModule)
check(
    "register_kv_cache handler signature matches the 8-payload protocol",
    mq.MessageQueueServer._inspect_handler_signature(None, RequestType.REGISTER_KV_CACHE, mod_obj.register_kv_cache),
)

# ---- 4. register-time bind, end to end on CPU fakes ----
reg = LayoutDescRegistry()
mod_obj._ctx = SimpleNamespace(layout_desc_registry=reg, chunk_size=256, separate_object_groups=False, full_sw_kv=False)
mod_obj._cache_contexts = {}
mod_obj._lock = threading.Lock()
LAYERS = {7001: 16, 7002: 17, 7003: 15}  # PP=3: every rank has a different layer count


class FakeCC:
    def __init__(self, n):
        self.n = n
        self.device = torch.device("cpu")
        self.num_layers = n
        self.kv_layer_groups_manager = SimpleNamespace(num_object_groups=1, get_attn_desc=lambda: DEFAULT_ATTN_WINDOW_DESC)


_next = {}
ldt.create_cache_context = lambda kv, chunk, **kw: FakeCC(_next["n"])
ldt.get_event_ipc_backend = lambda dev: SimpleNamespace(check_event_support=lambda d: None)
ldt.get_layout_desc = lambda cc, n, object_group_id=0: lay(cc.n)
for wid, (iid, n) in enumerate(LAYERS.items()):
    _next["n"] = n
    mod_obj.register_kv_cache(iid, [], "m", 3, None, {}, [], wid)
het, ranks = reg.find_rank_layouts("m", 3)
RB = [ObjectKey.ComputeKVRank(world_size=3, global_rank=w, local_world_size=3, local_rank=w) for w in range(3)]
exp = {RB[w]: {0: lay(n)} for w, n in enumerate(LAYERS.values())}
check("register-bind: heterogeneous flag set", het is True)
check("register-bind: all 3 ranks bound before any store", ranks == exp, f"{ranks.keys()} vs {exp.keys()}")
check("register-bind: bound map = instance -> its rank",
      mod_obj._house_bound_ranks() == {iid: RB[w] for w, iid in enumerate(LAYERS)}, str(mod_obj._house_bound_ranks()))
for w in range(3):
    k = IPCCacheServerKey(model_name="m", world_size=3, worker_id=w, token_ids=(), start=0, end=0, request_id="r")
    kr = ipc_key_to_object_keys(k, [b"\x01" * 8], [0])[0][0].kv_rank
    check(f"kv_rank(worker {w}) at register == store key kv_rank", kr == RB[w] and ranks[kr] == {0: lay(list(LAYERS.values())[w])})
# old client: no kv_worker_id -> nothing bound at register (store-time fallback remains)
reg2 = LayoutDescRegistry()
mod_obj._ctx.layout_desc_registry = reg2
mod_obj._cache_contexts = {}
mod_obj.__dict__.pop("_house_rank_bound", None)
_next["n"] = 16
mod_obj.register_kv_cache(8001, [], "m", 3, None, {}, [])
check("old client register: nothing bound, instance not marked", reg2.find_rank_layouts("m", 3)[1] == {} and 8001 not in mod_obj._house_bound_ranks())
# F4: an out-of-range kv_worker_id is not trusted at register
mod_obj.register_kv_cache(8002, [], "m", 3, None, {}, [], 3)
mod_obj.register_kv_cache(8003, [], "m", 3, None, {}, [], -1)
check("F4: kv_worker_id outside [0, world_size) -> nothing bound, not marked",
      reg2.find_rank_layouts("m", 3)[1] == {} and not ({8002, 8003} & set(mod_obj._house_bound_ranks())))
# (b) helper marks bound only on True
reg3 = LayoutDescRegistry()
mod_obj._ctx.layout_desc_registry = reg3
ok = mod_obj._house_bind_rank_layouts(9001, FakeCC(16), "m", 3, 0, "test")
check("(b) bind with no registry entry -> False, not marked", ok is False and 9001 not in mod_obj._house_bound_ranks())
reg3.register("m", 3, lay(16), DEFAULT_ATTN_WINDOW_DESC, group_layout_descs={0: lay(16)})
ok = mod_obj._house_bind_rank_layouts(9001, FakeCC(16), "m", 3, 0, "test")
check("(b) bind with registry entry -> True, marked with its rank", ok is True and mod_obj._house_bound_ranks().get(9001) == RB[0])
# re-register of a known id after a reap forgets the stale mark
mod_obj._cache_contexts = {}
_next["n"] = 16
mod_obj.register_kv_cache(9001, [], "m", 3, None, {}, [])
check("new registration of a reused id clears the stale bound mark", 9001 not in mod_obj._house_bound_ranks())

# ---- 4b. F4: the store-time check corrects a register-time bind under the wrong rank ----
reg4 = LayoutDescRegistry()
reg4.register("m", 3, lay(16), DEFAULT_ATTN_WINDOW_DESC, group_layout_descs={0: lay(16)})
mod_obj._ctx.layout_desc_registry = reg4
mod_obj.__dict__["_house_rank_bound"] = {}
mod_obj._house_bind_rank_layouts(9101, FakeCC(16), "m", 3, 1, "register")  # WRONG: worker 0 claimed worker 1
mod_obj._house_bind_rank_layouts(9102, FakeCC(17), "m", 3, 2, "register")
check("F4 setup: 9101 bound under RB[1]", mod_obj._house_bound_ranks() == {9101: RB[1], 9102: RB[2]})
mod_obj._house_store_bind(9101, FakeCC(16), "m", 3, RB[0])  # its keys say RB[0]
check("F4: mismatch rebinds under the key's rank and clears every other mark",
      mod_obj._house_bound_ranks() == {9101: RB[0]}, str(mod_obj._house_bound_ranks()))
check("F4: registry now holds RB[0] -> this instance's layout", reg4.find_rank_layouts("m", 3)[1].get(RB[0]) == {0: lay(16)})
calls = []
orig_bind = mod_obj._house_bind_rank_layouts
mod_obj._house_bind_rank_layouts = lambda *a, **k: calls.append(a) or True
mod_obj._house_store_bind(9101, FakeCC(16), "m", 3, RB[0])
check("F4: matching rank -> no rebind on later stores", calls == [])
mod_obj._house_store_bind(9102, FakeCC(17), "m", 3, RB[2])
check("F4: a cleared instance rebinds from its store key", len(calls) == 1)
del mod_obj._house_bind_rank_layouts
check("F4 cleanup restored bound method", mod_obj._house_bind_rank_layouts == orig_bind)


# ---- 5. (c) prefetch reservation with an unbound rank ----
def key(rank, gid=0, h=b"\x01" * 8):
    return ObjectKey(chunk_hash=h, model_name="m", kv_rank=rank, object_group_id=gid)


keys = [key(RB[w]) for w in range(3)]
fake = MagicMock()
fake._policy.select_l1_retentions.side_effect = lambda ks: [True] * len(ks)
req = SimpleNamespace(
    request_id=77,
    mode=PrefetchMode.LOOKUP,
    rank_group_layout_descs={RB[0]: {0: lay(16)}, RB[1]: {0: lay(17)}},  # rank 2 unbound
    require_rank_layouts=True,
    group_layout_descs={0: lay(16)},
    write_reserved_keys=[],
    write_reserved_objs={},
)
res = pc.PrefetchController._reserve_load_buffers(fake, req, keys)
ev = fake._event_bus.publish.call_args_list
reasons = [c.args[0].metadata.get("reason") for c in ev]
check("(c) unbound rank -> nothing reserved", res == set() and req.write_reserved_keys == [])
check("(c) reserve_write never called", fake._l1_manager.reserve_write.call_count == 0)
check("(c) reason is rank_layout_unbound (not l1_contended)", reasons == ["rank_layout_unbound"], str(reasons))
check("(c) event lists the unbound rank's keys (l1_oom/l1_contended convention)", ev[0].args[0].metadata["keys"] == [keys[2]])
fake2 = MagicMock()
fake2._policy.select_l1_retentions.side_effect = lambda ks: [True] * len(ks)
fake2._l1_manager.reserve_write.side_effect = lambda keys, is_temporary, layout_desc, mode: {k: (L1Error.SUCCESS, object()) for k in keys}
req.rank_group_layout_descs[RB[2]] = {0: lay(15)}
req.write_reserved_keys, req.write_reserved_objs = [], {}
res2 = pc.PrefetchController._reserve_load_buffers(fake2, req, keys)
used = {c.kwargs["keys"][0].kv_rank: c.kwargs["layout_desc"] for c in fake2._l1_manager.reserve_write.call_args_list}
check("(c) all bound -> all reserved", res2 == set(keys))
check("(c) each rank reserved with its own layout", used == {RB[0]: lay(16), RB[1]: lay(17), RB[2]: lay(15)})
fake3 = MagicMock()
fake3._policy.select_l1_retentions.side_effect = lambda ks: [True] * len(ks)
fake3._l1_manager.reserve_write.side_effect = lambda keys, is_temporary, layout_desc, mode: {k: (L1Error.SUCCESS, object()) for k in keys}
req3 = SimpleNamespace(request_id=78, mode=PrefetchMode.LOOKUP, rank_group_layout_descs={}, require_rank_layouts=False,
                       group_layout_descs={0: lay(16)}, write_reserved_keys=[], write_reserved_objs={})
res3 = pc.PrefetchController._reserve_load_buffers(fake3, req3, keys)
check("(c) homogeneous path unchanged (group layout, all reserved)", res3 == set(keys) and all(c.kwargs["layout_desc"] == lay(16) for c in fake3._l1_manager.reserve_write.call_args_list))

# ---- 5b. F5: stale strings ----
import inspect as _inspect  # noqa: E402
import lmcache.v1.mp_observability.subscribers.metrics.l2_failures as l2f  # noqa: E402
import lmcache.v1.multiprocess.engine_context as ec  # noqa: E402
check("F5: l2 counter description lists rank_layout_unbound", "not_found | rank_layout_unbound" in _inspect.getsource(l2f))
check("F5: engine_context no longer says 'bound at first store.'", "bound at first store." not in _inspect.getsource(ec)
      and "bound at REGISTER" in _inspect.getsource(ec))

# ---- 6. (e) PR #5249 lookup range ----
from lmcache.integration.vllm.vllm_multi_process_adapter import LMCacheMPSchedulerAdapter  # noqa: E402
ad = object.__new__(LMCacheMPSchedulerAdapter)
ad.lmcache_tokens_per_chunk = 256
ad._pending_lookups = {}
ad._ensure_heartbeat_started = lambda: None
captured = {}


class Stop(Exception):
    pass


def fake_create_key(token_ids, start, end, request_id, cache_salt="", **kw):
    captured["end"] = end
    raise Stop()


ad._create_key = fake_create_key
type(ad).is_healthy = property(lambda self: True)
for n, reserve, want in [(512, False, 512), (512, True, 256), (513, True, 512), (1, True, 0), (0, True, 0)]:
    try:
        ad.maybe_submit_lookup_request("r", token_ids=list(range(n)), reserve_last_token=reserve)
    except Stop:
        pass
    check(f"(e) lookup end n={n} reserve={reserve} -> {want}", captured.get("end") == want, str(captured))
from lmcache.v1.multiprocess.modules.lookup import LookupModule  # noqa: E402
ctx = MagicMock()
ctx.chunk_size = 16
ctx.event_bus.has_subscribers.return_value = False
ctx.layout_desc_registry.find.return_value = MagicMock()
ctx.token_hasher.compute_chunk_hashes.return_value = []
k = IPCCacheServerKey(model_name="m", world_size=1, num_kv_readers=1, worker_id=None, token_ids=tuple(range(32)),
                      start=0, end=16, request_id="r")
try:
    LookupModule(ctx).lookup(k, tp_size=1)
except Exception as e:  # noqa: BLE001
    print("lookup raised (ignored for the call check):", repr(e))
check("(e) server lookup hashes only up to key.end", ctx.token_hasher.compute_chunk_hashes.call_args_list[:1] == [((list(range(32)),), {"end": 16})], str(ctx.token_hasher.compute_chunk_hashes.call_args_list))

# ---- 6b. 02 lookup hunk (hand-ported to 0.5.5's num_kv_readers): the spec carries the rank layouts ----
for het_flag, rank_map in ((True, {RB[0]: {0: lay(16)}}), (False, {})):
    ctx = MagicMock()
    ctx.chunk_size = 16
    ctx.event_bus.has_subscribers.return_value = False
    ctx.layout_desc_registry.find_attn_desc.return_value = DEFAULT_ATTN_WINDOW_DESC
    ctx.layout_desc_registry.find_group_layout_descs.return_value = {0: lay(16)}
    ctx.layout_desc_registry.find_rank_layouts.return_value = (het_flag, rank_map)
    ctx.token_hasher.compute_chunk_hashes.return_value = [b"\x01" * 8]
    k = IPCCacheServerKey(model_name="m", world_size=3, num_kv_readers=1, worker_id=None, token_ids=tuple(range(16)),
                          start=0, end=16, request_id="r6b")
    try:
        LookupModule(ctx).lookup(k, tp_size=1)
        spec = ctx.storage_manager.submit_prefetch_task.call_args.args[0]
        check(f"02: lookup forwards rank layouts (heterogeneous={het_flag}) and num_kv_readers",
              spec.require_rank_layouts is het_flag and spec.rank_group_layout_descs == rank_map
              and spec.num_kv_readers == 1
              and ctx.layout_desc_registry.find_rank_layouts.call_args.args == ("m", 3),
              repr(spec))
    except Exception as e:  # noqa: BLE001
        check(f"02: lookup forwards rank layouts (heterogeneous={het_flag}) and num_kv_readers", False, repr(e))

# ---- 7. (d) PR #4709: failed retrieves record their block ids ----
from lmcache.integration.vllm.vllm_multi_process_adapter import LMCacheMPWorkerAdapter  # noqa: E402


def done_future(val):
    f = MagicMock()
    f.query.return_value = True
    f.result.return_value = val
    return f


for meth in ("get_finished", "get_finished_with_lazy_offload"):
    wa = object.__new__(LMCacheMPWorkerAdapter)
    wa.dispatcher = None
    wa._health_event = threading.Event()
    wa._health_event.set()
    wa.store_futures, wa.store_events, wa.retrieve_events = {}, {}, {}
    wa.retrieve_futures = {"bad": (done_future(False), [11, 12]), "good": (done_future(True), [13])}
    wa._dropped_retrieves = set()
    wa.error_block_ids = set()
    wa._process_finished_stores = lambda *a, **k: set()
    wa.request_telemetry = MagicMock()
    wa.lazy_offload = meth != "get_finished"
    wa._completed_store_requests = {}
    wa.model_name, wa.parallel_strategy = "m", MagicMock()
    try:
        out = getattr(wa, meth)(set()) if meth == "get_finished" else getattr(wa, meth)()
        check(f"(d) {meth}: failed retrieve blocks -> error_block_ids", wa.error_block_ids == {11, 12}, str(wa.error_block_ids))
        check(f"(d) {meth}: both retrieves reported finished", out[1] is not None and {"bad", "good"} <= set(out[1]), str(out))
    except Exception as e:  # noqa: BLE001
        check(f"(d) {meth} callable on fake adapter", False, repr(e))

# ---- 8. F1: the connector warns at scheduler start unless the policy is "recompute" ----
import lmcache.integration.vllm.lmcache_mp_connector as conn  # noqa: E402
from vllm.distributed.kv_transfer.kv_connector.v1.base import KVConnectorRole  # noqa: E402

saved_conn = (conn.validate_mamba_step_alignment, conn.validate_kv_cache_groups, conn.logger,
              conn.KVConnectorBase_V1.__init__, conn.get_group_tokens_per_block,
              conn.get_vllm_scheduler_block_size, conn.get_dcp_decorated_model_name)
conn.validate_mamba_step_alignment = lambda *a, **k: None
conn.validate_kv_cache_groups = lambda *a, **k: None
conn.KVConnectorBase_V1.__init__ = lambda self, *a, **k: None
# 0.5.5 derives these from the vLLM config before the checks; not under test here.
conn.get_group_tokens_per_block = lambda *a, **k: [16]
conn.get_vllm_scheduler_block_size = lambda *a, **k: 16
conn.get_dcp_decorated_model_name = lambda *a, **k: "m"
try:
    for policy, role, want in (("fail", KVConnectorRole.SCHEDULER, True), ("recompute", KVConnectorRole.SCHEDULER, False),
                               ("fail", KVConnectorRole.WORKER, False)):
        conn.logger = MagicMock()
        cfg = MagicMock()
        cfg.kv_transfer_config.kv_load_failure_policy = policy
        cfg.kv_transfer_config.get_from_extra_config.side_effect = Stop  # stop right after the check
        try:
            conn.LMCacheMPConnector(cfg, role)
        except Stop:
            pass
        warned = any("kv_load_failure_policy" in str(c.args[0]) for c in conn.logger.warning.call_args_list)
        check(f"F1: policy={policy} role={role.name} -> warning={want}", warned is want, str(conn.logger.warning.call_args_list))
    # (e) PR #5249 connector half: reserve the last token only in Mamba align/all
    for mode, want in (("none", False), ("align", True), ("all", True)):
        conn.logger = MagicMock()
        cfg = MagicMock()
        cfg.cache_config.mamba_cache_mode = mode
        cfg.kv_transfer_config.kv_load_failure_policy = "recompute"
        cfg.kv_transfer_config.get_from_extra_config.side_effect = Stop
        c = object.__new__(conn.LMCacheMPConnector)
        try:
            conn.LMCacheMPConnector.__init__(c, cfg, KVConnectorRole.SCHEDULER)
        except Stop:
            pass
        check(f"(e) connector mamba_cache_mode={mode} -> reserve_last_token={want}",
              getattr(c, "_reserve_last_token_for_lookup", None) is want)
    src = _inspect.getsource(conn.LMCacheMPConnector)
    check("(e) both lookup call sites pass reserve_last_token",
          src.count("reserve_last_token=self._reserve_last_token_for_lookup") == 2)
finally:
    (conn.validate_mamba_step_alignment, conn.validate_kv_cache_groups, conn.logger,
     conn.KVConnectorBase_V1.__init__, conn.get_group_tokens_per_block,
     conn.get_vllm_scheduler_block_size, conn.get_dcp_decorated_model_name) = saved_conn

print(f"SMOKE TOTAL: {PASS} passed, {FAIL} failed")
sys.exit(1 if FAIL else 0)
