"""Harness-owned ComfyUI idle bound: free the models and the executor cache after a quiet period.

The house rule is that no idle model stays loaded: every engine unloads after 5 minutes. llama-swap does it with its own ttl;
ComfyUI has no such timer (cli_args.py has no idle or timeout argument), so a ComfyUI that nothing frees holds what it loaded.
The harness frees it at the end of every job (render/gpu-lock.mjs POSTs /free), and the holder of the GPU lease stops a kept
instance when the lease ends, but a runner killed before its cleanup, a warm session that stalls, or a hand-started instance
leaves models resident. This pack is the engine-side timer for that gap, the llama-swap ttl analogue: ComfyUI owns the clock
over its own models and the harness runs no watcher.

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

WHAT IT DOES NOT DO. It frees models and caches; it does not stop the process. The Python process and its CUDA context stay until
the lease holder stops the instance at release (internal/comfyinst), and an instance whose holder died stays until an operator
stops it. A self-exit is deliberately not built: from inside ComfyUI a runner that is mid-job in a long non-ComfyUI step (ffmpeg,
a QA pass, an LLM call) looks the same as an abandoned instance, and a /free is always safe where an exit would break the next
prompt with ECONNREFUSED. Nothing it does can fail a render: it never raises, and a failure is logged once as NOT ACTIVE.

GET /offload/idle answers {limit_sec, idle_sec, armed, freed, last_free_unix, error}. It is read-only, and it is how the launcher
checks that the pack is alive (a pack that loaded but cannot find the queue says so in `error`). The bound does not depend on it.
"""
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
                 wait=None, say=None):
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
        if busy:
            self.last_busy = now
            self.armed = True
            return False
        if not self.armed or now - self.last_busy < self.limit:
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
                "last_free_unix": self.last_free_unix, "error": self.error}

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
    """GET /offload/idle on ComfyUI's route table. Custom nodes import before add_routes (main.py), so the route is served."""
    module = sys.modules.get("server")
    instance = getattr(getattr(module, "PromptServer", None), "instance", None)
    routes = getattr(instance, "routes", None)
    if routes is None:
        raise LookupError("no PromptServer route table")
    from aiohttp import web

    @routes.get(ROUTE)
    async def idle_status(request):
        return web.json_response(bound.status())


def boot(environ=None, get_queue=None, register_route=None, autostart=True, say=None):
    """Read the settings and start the bound; None when it is switched off. Every argument is a test seam."""
    environ = os.environ if environ is None else environ
    say = say or _say
    limit = resolve_limit(environ.get("OFFLOAD_IDLE_FREE_SEC"), say)
    if limit == 0:
        say("idle bound disabled (OFFLOAD_IDLE_FREE_SEC=0)")
        return None
    poll = resolve_poll(environ.get("OFFLOAD_IDLE_POLL_SEC"), say)
    bound = IdleBound(get_queue or server_queue, models_loaded, cache_on, limit, poll=poll, say=say)
    try:
        (register_route or register_status_route)(bound)
    except Exception as err:  # the route is optional
        say("GET %s not registered (%s); the bound itself does not depend on it" % (ROUTE, err))
    say("idle bound %d s" % limit)
    if autostart:
        bound.start()
    return bound


BOUND = boot()
