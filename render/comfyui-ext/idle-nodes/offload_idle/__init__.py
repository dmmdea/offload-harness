"""Harness-owned ComfyUI idle bound: free the models and the executor cache after a quiet period.

The house rule is that no idle model stays loaded: every engine unloads after 5 minutes. llama-swap does it with its own ttl;
ComfyUI has no such timer (cli_args.py has no idle or timeout argument), so a ComfyUI that nothing frees holds what it keeps.
The harness frees it at the end of every job (render/gpu-lock.mjs POSTs /free), and the holder of the GPU lease stops a kept
instance when the lease ends. This pack is the engine-side timer for what is left, the llama-swap ttl analogue: ComfyUI owns
the clock over its own models and caches and the harness runs no watcher.

WHAT IS LEFT, AND WHAT IS NOT. The harness launches ComfyUI with --disable-smart-memory, and with that flag ComfyUI itself unloads
every model when a prompt ends, whether it succeeded or failed (execution.py, after the node loop: `if DISABLE_SMART_MEMORY:
unload_all_models()`; read in ComfyUI 0.38.0). A kept single-prompt instance (--cache-none) is therefore already bare between
jobs, with or without the runner's /free, and for it this pack finds nothing to release: it does not fire and says nothing.
What it bounds is what survives that: (a) a warm (--batch) session's executor cache, which keeps the loader outputs, the weights
the unload moved to host RAM, until a free resets the executor; (b) a hand-started instance launched without
--disable-smart-memory, which keeps its models loaded in VRAM after a prompt. Those are the two things the gate below tests (a
loaded model, or a cache that is on). This is read from ComfyUI's code, not yet measured on a card (docs/systems/media-generation.md
has the live check).

ComfyUI loads this folder through render/comfyui-ext/extra_model_paths_idle.yaml (its `custom_nodes` key). render/comfy-lifecycle.mjs
passes that file, and the pack name in the custom-node whitelist, on every kept (--keep-comfy) and warm (--batch) launch; a
hand-started ComfyUI opts in by adding the same flag (docs/OPERATOR-GUIDE.md). It is never copied into the shared install. It
registers no node, but NODE_CLASS_MAPPINGS must exist: nodes.py reports a folder that exports neither NODE_CLASS_MAPPINGS nor
comfy_entrypoint as a custom node that failed to import.

WHAT IT DOES. One daemon thread polls every OFFLOAD_IDLE_POLL_SEC (5 s). A poll is BUSY when the queue holds a prompt (queued or
running: get_tasks_remaining) or when task_counter moved since the last poll (a prompt that began and ended between two polls).
Both are read under the queue's mutex. After OFFLOAD_IDLE_FREE_SEC (300 s) of polls that were not busy, if the engine holds
anything (a loaded model, or an executor cache that is on) the pack asks for ONE free per busy period.

THREAD SAFETY. The timer thread never touches a model. It sets the two queue flags unload_models and free_memory, exactly what
POST /free does (server.py post_free), and ComfyUI's own worker thread drains them (main.py prompt_worker: unload_all_models,
then the executor's reset, then a gc pass). The empty-queue check and the two set_flag calls happen in one hold of q.mutex (an
RLock; set_flag takes it too), so a prompt cannot be queued between them. A prompt that lands in the microsecond after is run
first and the flags are drained after it: the cost is one avoidable reload, never a corrupted run.

THE PROCESS BOUND (OFFLOAD_IDLE_EXIT_SEC, 2026-10-24, ADR 0097). The harness keeps the ComfyUI it launches between jobs instead of
killing it after each one: a relaunch cost 31-89 s of a 66-135 s render on an 8 GB laptop card, while the bare process holds about
240 MiB of the card once its models are gone. The process is then bounded the way llama-swap bounds a seat: after
OFFLOAD_IDLE_EXIT_SEC seconds in which no prompt was queued or running AND no runner held the instance, it exits. The clock runs
from the moment the pack loads, so an instance whose runner died before submitting anything still goes.

A runner HOLDS the instance for the whole of its job with GET /offload/hold, a response that stays open and carries one byte every
HOLD_BEAT_SEC. That answers the objection that kept this exit out before: a runner mid-job in a long step that is not ComfyUI's
(ffmpeg, a QA pass, an LLM call) holds the instance, so it does not look abandoned. A hold ends when the runner closes it or its
process dies: the next heartbeat write fails or the transport reports closing (ComfyUI runs aiohttp without handler cancellation,
so the handler is never cancelled on a disconnect and must find out by writing). The hold blocks the exit only; the model free
above runs on its own clock as before. Before it exits the pack waits EXIT_GRACE_SEC and checks again, under the queue's mutex,
that nothing was queued and nobody took a hold; then it says why and leaves with os._exit(0) (nothing is running, so there is
nothing to wind down, and an interpreter shutdown from a daemon thread can hang on ComfyUI's own threads).

Unset or 0 means no process bound: a hand-started instance that loads the pack for its model free is never stopped by it. Only
the harness's own launch sets the variable (render/comfy-lifecycle.mjs ensureComfy).

Nothing it does can fail a render: it never raises, and a failure is logged once as NOT ACTIVE.

GET /offload/idle answers {limit_sec, idle_sec, armed, freed, last_free_unix, error, exit_limit_sec, exit_idle_sec, holds}. It is
read-only, and it is how the launcher checks that the pack is alive (a pack that loaded but cannot find the queue says so in
`error`). The bounds do not depend on it.
"""
import asyncio
import logging
import os
import sys
import threading
import time

NODE_CLASS_MAPPINGS = {}

DEFAULT_LIMIT_SEC = 300
MIN_LIMIT_SEC = 5
DEFAULT_POLL_SEC = 5
MIN_POLL_SEC = 1
MAX_POLL_SEC = 30
ROUTE = "/offload/idle"
HOLD_ROUTE = "/offload/hold"
HOLD_BEAT_SEC = 2
EXIT_GRACE_SEC = 2


def _say(message):
    # ComfyUI's console goes to the instance's own log file for a kept instance; one prefix makes every line findable.
    logging.info("[offload_idle] %s", message)


def _whole_seconds(raw):
    """The int in `raw` when it is a plain run of ASCII digits (after trimming), else None."""
    text = "" if raw is None else str(raw).strip()
    return int(text) if text.isascii() and text.isdigit() else None


def resolve_limit(raw, say=None):
    """OFFLOAD_IDLE_FREE_SEC: unset or empty = 300, 0 = off, a whole number of seconds >= 5, else 300 and one line."""
    if raw is None or str(raw).strip() == "":
        return DEFAULT_LIMIT_SEC
    value = _whole_seconds(raw)
    if value is not None and (value == 0 or value >= MIN_LIMIT_SEC):
        return value
    (say or _say)("ignoring OFFLOAD_IDLE_FREE_SEC=%r (0 turns the bound off, otherwise a whole number of seconds, %d or more); using %d"
                  % (raw, MIN_LIMIT_SEC, DEFAULT_LIMIT_SEC))
    return DEFAULT_LIMIT_SEC  # a value it cannot use


def resolve_exit(raw, say=None):
    """OFFLOAD_IDLE_EXIT_SEC: unset, empty or 0 = no process bound, a whole number of seconds >= 5 = that, else none and one line."""
    if raw is None or str(raw).strip() == "":
        return 0
    value = _whole_seconds(raw)
    if value is not None and (value == 0 or value >= MIN_LIMIT_SEC):
        return value
    (say or _say)("ignoring OFFLOAD_IDLE_EXIT_SEC=%r (0 turns the process bound off, otherwise a whole number of seconds, %d or "
                  "more); the process is not bounded" % (raw, MIN_LIMIT_SEC))
    return 0  # never exit on a value nobody can read


def _exit_now(code):
    """Leave the process: flush what was said first, then os._exit (see THE PROCESS BOUND above)."""
    for handler in logging.getLogger().handlers:
        try:
            handler.flush()
        except Exception:
            pass
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.flush()
        except Exception:
            pass
    os._exit(code)


def resolve_poll(raw, say=None):
    """OFFLOAD_IDLE_POLL_SEC, the test seam: whole seconds clamped to 1..30, else 5."""
    value = _whole_seconds(raw)
    if value is not None:
        return max(MIN_POLL_SEC, min(MAX_POLL_SEC, value))
    if raw is not None and str(raw).strip() != "":
        (say or _say)("ignoring OFFLOAD_IDLE_POLL_SEC=%r (a whole number of seconds, %d to %d); using %d"
                      % (raw, MIN_POLL_SEC, MAX_POLL_SEC, DEFAULT_POLL_SEC))
    return DEFAULT_POLL_SEC


def models_loaded():
    """Does ComfyUI hold a loaded model? Read by name so that no comfy module is imported here."""
    module = sys.modules.get("comfy.model_management")
    if module is None:
        raise LookupError("comfy.model_management is not imported")
    return bool(module.current_loaded_models)


def cache_on():
    """Is the executor cache on (a warm session)? Only --cache-none turns it off."""
    module = sys.modules.get("comfy.cli_args")
    if module is None:
        raise LookupError("comfy.cli_args is not imported")
    return not bool(getattr(module.args, "cache_none", False))


def server_queue():
    """ComfyUI's prompt queue, resolved on every tick: PromptServer.instance exists before custom nodes import (server.py __init__)."""
    module = sys.modules.get("server")
    prompt_server = getattr(module, "PromptServer", None)
    instance = getattr(prompt_server, "instance", None)
    return getattr(instance, "prompt_queue", None)


class IdleBound:
    def __init__(self, get_queue, loaded_fn, cache_on_fn, limit, poll=DEFAULT_POLL_SEC, clock=time.monotonic,
                 wait=None, say=None, exit_limit=0, exit_fn=None, grace=EXIT_GRACE_SEC):
        self.get_queue = get_queue
        self.loaded_fn = loaded_fn
        self.cache_on_fn = cache_on_fn
        self.limit = limit
        self.poll = poll
        self.clock = clock
        self.say = say or _say
        self._stop = threading.Event()
        self.wait = wait or self._stop.wait
        self.thread = None
        self.armed = False
        self.last_busy = None
        self.last_counter = 0   # a fresh PromptQueue starts at 0, so a first poll that sees more has missed a prompt
        self.freed = 0
        self.last_free_unix = None
        self.error = None
        self._reported = None
        self._said = set()
        # The process bound: 0 = none. Its clock starts now, not at the first prompt, so an instance nobody ever used still goes.
        self.exit_limit = exit_limit
        self.exit_fn = exit_fn or _exit_now
        self.grace = grace
        self.last_activity = clock()
        self.holds = 0
        self._holds_lock = threading.Lock()

    def hold_open(self):
        with self._holds_lock:
            self.holds += 1
        self.last_activity = self.clock()

    def hold_close(self):
        with self._holds_lock:
            self.holds = max(0, self.holds - 1)
        self.last_activity = self.clock()  # the clock restarts when the last runner lets go

    def _once(self, message):
        if message not in self._said:
            self._said.add(message)
            self.say(message)

    def _note_error(self, text):
        self.error = text
        if text is None:
            self._reported = None
        elif text != self._reported:
            self._reported = text
            self.say("NOT ACTIVE: %s (will retry)" % text)

    def _holds_something(self):
        try:
            if self.loaded_fn():
                return True
        except Exception as err:
            self._once("cannot read the loaded models (%s); freeing on idle without checking" % err)
            return True  # not knowing what is loaded is no reason to hold it
        try:
            return bool(self.cache_on_fn())
        except Exception as err:
            self._once("cannot read the cache setting (%s); freeing on idle without checking" % err)
            return True

    def _tick(self):
        queue = self.get_queue()
        if queue is None:
            raise LookupError("no prompt queue yet (PromptServer.instance.prompt_queue)")
        now = self.clock()
        with queue.mutex:  # read: the queue count and the counter as one snapshot
            remaining = queue.get_tasks_remaining()
            counter = queue.task_counter
        busy = remaining > 0 or counter != self.last_counter
        self.last_counter = counter
        if busy or self.holds > 0:
            self.last_activity = now
        if busy:
            self.last_busy = now
            self.armed = True
            return False
        if self.exit_limit and now - self.last_activity >= self.exit_limit:
            self._leave(queue, counter)  # it returns only when the instance was woken during the grace
            return False
        if not self.limit or not self.armed or now - self.last_busy < self.limit:
            return False
        if not self._holds_something():
            return False
        with queue.mutex:  # fire: re-check and set both flags as one step
            if queue.get_tasks_remaining() == 0 and queue.task_counter == counter:
                queue.set_flag("unload_models", True)
                queue.set_flag("free_memory", True)
            else:
                return False
        self.armed = False  # one free per busy period
        self.freed += 1
        self.last_free_unix = time.time()
        self.say("idle bound reached with an empty queue: asked the worker to unload models and free memory")
        return True

    def _leave(self, queue, counter):
        """The process bound: wait the grace, check again under the queue's mutex that nothing was queued and nobody took a hold,
        then exit. A prompt or a hold that arrived in the grace restarts the clock instead."""
        self.wait(self.grace)
        if self._stop.is_set():
            return
        with queue.mutex:
            if queue.get_tasks_remaining() != 0 or queue.task_counter != counter or self.holds > 0:
                self.last_activity = self.clock()
                return
            self.say("process bound reached: %d s with no prompt queued or running and no runner holding the instance; exiting"
                     % int(self.clock() - self.last_activity))
            self.exit_fn(0)

    def tick(self):
        """One poll. True when it asked the worker to free. It never raises: a failure is said once and the next poll retries."""
        try:
            fired = self._tick()
        except Exception as err:
            self._note_error("%s: %s" % (type(err).__name__, err))
            return False
        self._note_error(None)
        return fired

    def status(self):
        idle = None if self.last_busy is None else max(0.0, self.clock() - self.last_busy)
        return {"limit_sec": self.limit, "idle_sec": idle, "armed": self.armed, "freed": self.freed,
                "last_free_unix": self.last_free_unix, "error": self.error, "exit_limit_sec": self.exit_limit,
                "exit_idle_sec": max(0.0, self.clock() - self.last_activity), "holds": self.holds}

    def run(self):
        while not self._stop.is_set():
            self.tick()
            self.wait(self.poll)

    def start(self):
        self.thread = threading.Thread(target=self.run, name="offload_idle", daemon=True)
        self.thread.start()
        return self.thread

    def stop(self):
        self._stop.set()


def register_status_route(bound):
    """GET /offload/idle and GET /offload/hold on ComfyUI's route table. Custom nodes import before add_routes (main.py), so both
    are served."""
    module = sys.modules.get("server")
    instance = getattr(getattr(module, "PromptServer", None), "instance", None)
    routes = getattr(instance, "routes", None)
    if routes is None:
        raise LookupError("no PromptServer route table")
    from aiohttp import web

    @routes.get(ROUTE)
    async def idle_status(request):
        return web.json_response(bound.status())

    @routes.get(HOLD_ROUTE)
    async def hold(request):
        # Open for as long as the runner keeps it: one byte every HOLD_BEAT_SEC, and the hold ends at the first write that fails
        # or the first look that finds the transport closing (the handler is not cancelled on a disconnect; see THE PROCESS BOUND).
        response = web.StreamResponse(headers={"Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store"})
        await response.prepare(request)
        bound.hold_open()
        try:
            while True:
                transport = request.transport
                if transport is None or transport.is_closing():
                    break
                try:
                    await response.write(b".")
                except Exception:
                    break
                await asyncio.sleep(HOLD_BEAT_SEC)
        finally:
            bound.hold_close()
        return response


def boot(environ=None, get_queue=None, register_route=None, autostart=True, say=None):
    """Read the settings and start the bound; None when it is switched off. Every argument is a test seam."""
    environ = os.environ if environ is None else environ
    say = say or _say
    limit = resolve_limit(environ.get("OFFLOAD_IDLE_FREE_SEC"), say)
    exit_limit = resolve_exit(environ.get("OFFLOAD_IDLE_EXIT_SEC"), say)
    if limit == 0 and exit_limit == 0:
        say("idle bound disabled (OFFLOAD_IDLE_FREE_SEC=0)")
        return None
    poll = resolve_poll(environ.get("OFFLOAD_IDLE_POLL_SEC"), say)
    bound = IdleBound(get_queue or server_queue, models_loaded, cache_on, limit, poll=poll, say=say, exit_limit=exit_limit)
    try:
        (register_route or register_status_route)(bound)
    except Exception as err:  # the route is optional
        say("GET %s not registered (%s); the bound itself does not depend on it" % (ROUTE, err))
    say("idle bound %d s" % limit if limit else "model free disabled (OFFLOAD_IDLE_FREE_SEC=0)")
    if exit_limit:
        say("process bound %d s: the process exits after that long with no prompt and no runner holding it" % exit_limit)
    if autostart:
        bound.start()
    return bound


BOUND = boot()
