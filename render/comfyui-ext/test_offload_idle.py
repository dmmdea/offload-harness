"""Tests for idle-nodes/offload_idle (run by render/comfyui-ext.test.mjs, or: python -I -B test_offload_idle.py).

Standard library only: no ComfyUI, no torch, no GPU and no real sleep. The pack is loaded by path (a `-I` interpreter has no
script directory on sys.path), `comfy.*` and `server` are stand-ins, the clock and the wait are injected, and every tick is
driven by hand.

FakeQueue copies the surface of ComfyUI 0.38.0's execution.PromptQueue that the pack touches, with its signatures:
  execution.py:1289  self.mutex = threading.RLock()          (a real RLock, so "under the mutex" can be asked)
  execution.py:1291  self.task_counter = 0                   (an int, bumped by get() under the mutex, never reset)
  execution.py:1377  get_tasks_remaining(self)               (queued + running, under the mutex)
  execution.py:1427  set_flag(self, name, data)              (takes the mutex, then notifies not_empty)
and main.py's prompt_worker drains those flags on ITS thread, which is why the pack only ever sets them.
"""
import asyncio
import importlib.util
import os
import sys
import threading
import types
import unittest

sys.dont_write_bytecode = True

PACK = os.path.join(os.path.dirname(os.path.abspath(__file__)), "idle-nodes", "offload_idle", "__init__.py")
LIMIT = 300


class FakeQueue:
    def __init__(self):
        self.mutex = threading.RLock()
        self._counter = 0
        self.queue = []
        self.currently_running = {}
        self.flags = {}
        self.set_calls = []
        self.owned_during_set = []
        self.watching = False   # True while the pack's tick runs: only its reads are recorded
        self.reads = []

    @property
    def task_counter(self):
        if self.watching:
            self.reads.append(("counter", self.mutex._is_owned()))
        return self._counter

    @task_counter.setter
    def task_counter(self, value):
        self._counter = value

    def get_tasks_remaining(self):
        if self.watching:
            self.reads.append(("remaining", self.mutex._is_owned()))  # asked before this method takes the mutex itself
        with self.mutex:
            return len(self.queue) + len(self.currently_running)

    def set_flag(self, name, data):
        # asked BEFORE this method takes the mutex: is the CALLER holding it?
        self.owned_during_set.append(self.mutex._is_owned())
        with self.mutex:
            self.set_calls.append((name, data))
            self.flags[name] = data

    # --- test drivers -------------------------------------------------------------------
    def prompt_ran(self):
        """A prompt began and finished (between two polls): the counter moved, nothing is left."""
        self.task_counter += 1

    def prompt_started(self):
        self.task_counter += 1
        self.currently_running[self.task_counter - 1] = ("item",)

    def prompt_finished(self):
        self.currently_running.clear()


class RacyQueue(FakeQueue):
    """A prompt lands between the pack's read and its write: scripted answers for get_tasks_remaining.

    `script` is a list of (answer, side_effect) consumed one per call, the side effect running before the answer
    is returned; None (or an empty list) = behave normally.
    """
    def __init__(self):
        super().__init__()
        self.script = None

    def get_tasks_remaining(self):
        if self.script:
            answer, side_effect = self.script.pop(0)
            if side_effect:
                side_effect()
            return answer
        return super().get_tasks_remaining()


class Clock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now


class Rig:
    """One IdleBound over one fake queue, a hand-set clock, and switches for what the engine holds."""
    def __init__(self, pack, queue=None, limit=LIMIT, **kw):
        self.q = queue or FakeQueue()
        self.clock = Clock()
        self.logs = []
        self.loaded = True        # comfy.model_management.current_loaded_models is non-empty
        self.cache_on = False     # comfy.cli_args.args.cache_none is False
        self.bound = pack.IdleBound(lambda: self.q, lambda: self.loaded, lambda: self.cache_on, limit,
                                    clock=self.clock, say=self.logs.append, **kw)

    def tick_at(self, now):
        self.clock.now = now
        self.q.watching = True
        try:
            return self.bound.tick()
        finally:
            self.q.watching = False


def load_pack(name="offload_idle_under_test", environ=None):
    """Execute the pack as a fresh module. OFFLOAD_IDLE_FREE_SEC=0 (the default here) keeps its import from starting a thread."""
    environ = {"OFFLOAD_IDLE_FREE_SEC": "0"} if environ is None else environ
    saved = {k: os.environ.get(k) for k in ("OFFLOAD_IDLE_FREE_SEC", "OFFLOAD_IDLE_POLL_SEC")}
    for k in saved:
        os.environ.pop(k, None)
    os.environ.update(environ)
    try:
        spec = importlib.util.spec_from_file_location(name, PACK)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module
    finally:
        for k, v in saved.items():
            os.environ.pop(k, None)
            if v is not None:
                os.environ[k] = v


class PackTest(unittest.TestCase):
    def setUp(self):
        self.pack = load_pack()

    def rig(self, **kw):
        return Rig(self.pack, **kw)


class WhenItFrees(PackTest):
    def test_idle_below_the_limit_frees_nothing(self):
        t = self.rig()
        t.q.prompt_ran()
        self.assertFalse(t.tick_at(0))
        self.assertFalse(t.tick_at(299))
        self.assertEqual(t.q.flags, {})
        self.assertEqual(t.q.set_calls, [])

    def test_idle_at_the_limit_sets_both_flags_in_one_hold_of_the_mutex(self):
        t = self.rig()
        t.q.prompt_ran()
        t.tick_at(0)
        self.assertTrue(t.tick_at(LIMIT), "300 s idle must free")
        self.assertEqual(t.q.flags, {"unload_models": True, "free_memory": True})
        self.assertEqual(len(t.q.set_calls), 2)
        # prompt_worker drains the flags together; two separate holds could be drained one at a time
        self.assertEqual(t.q.owned_during_set, [True, True], "the caller must hold q.mutex across both set_flag calls")
        self.assertEqual(len([m for m in t.logs if "idle bound reached" in m]), 1, t.logs)

    def test_the_queue_is_read_under_its_mutex_both_for_the_snapshot_and_for_the_recheck(self):
        t = self.rig()
        t.q.prompt_ran()
        t.tick_at(0)
        self.assertTrue(t.tick_at(LIMIT))    # the second tick reads a snapshot, then re-checks before it writes
        kinds = [kind for kind, _ in t.q.reads]
        self.assertEqual(kinds.count("remaining"), 3, t.q.reads)   # tick 0: one; tick 300: snapshot and recheck
        self.assertEqual(kinds.count("counter"), 3, t.q.reads)
        self.assertEqual([owned for _, owned in t.q.reads], [True] * 6, "every read of the queue is inside a hold of q.mutex")

    def test_it_frees_once_per_busy_period(self):
        t = self.rig()
        t.q.prompt_ran()
        t.tick_at(0)
        fired = [t.tick_at(now) for now in (300, 305, 400, 99999)]
        self.assertEqual(fired, [True, False, False, False])
        self.assertEqual(len(t.q.set_calls), 2)

    def test_new_activity_arms_it_again(self):
        t = self.rig()
        t.q.prompt_ran()
        t.tick_at(0)
        self.assertTrue(t.tick_at(300))
        t.q.prompt_ran()
        self.assertFalse(t.tick_at(500))
        self.assertFalse(t.tick_at(799))
        self.assertTrue(t.tick_at(800))
        self.assertEqual(len(t.q.set_calls), 4)

    def test_a_running_prompt_is_never_idle(self):
        t = self.rig()
        t.q.prompt_started()
        for now in range(0, 601, 50):  # a 550 s prompt, polled every 50 s of clock
            self.assertFalse(t.tick_at(now), f"fired at {now} s with a prompt running")
        self.assertEqual(t.q.flags, {})
        t.q.prompt_finished()
        self.assertFalse(t.tick_at(899), "the idle clock runs from the last poll that saw the prompt, not from when it began")
        self.assertTrue(t.tick_at(900))

    def test_a_prompt_that_began_and_ended_between_two_polls_counts(self):
        t = self.rig()
        t.tick_at(0)               # nothing has ever run
        t.q.prompt_ran()           # begins and ends before the next poll: nothing running, the counter moved
        self.assertFalse(t.tick_at(100))
        self.assertFalse(t.tick_at(399), "299 s since that prompt")
        self.assertTrue(t.tick_at(400))

    def test_nothing_ever_ran_nothing_is_freed(self):
        t = self.rig()
        for now in (0, 300, 10000):
            self.assertFalse(t.tick_at(now))
        self.assertEqual(t.q.flags, {})
        self.assertEqual(t.logs, [], "an instance that never ran a prompt is not an error either")

    def test_it_frees_only_what_there_is_to_free(self):
        # models loaded, or the executor cache on (a warm session); neither = nothing to free
        t = self.rig()
        t.loaded, t.cache_on = False, False
        t.q.prompt_ran()
        t.tick_at(0)
        self.assertFalse(t.tick_at(300), "no model loaded and --cache-none: nothing to free")
        self.assertEqual(t.q.flags, {})
        t.loaded = True
        self.assertTrue(t.tick_at(301), "the moment something is held while idle it is freed")

        t = self.rig()
        t.loaded, t.cache_on = False, True
        t.q.prompt_ran()
        t.tick_at(0)
        self.assertTrue(t.tick_at(300), "a warm session's cached outputs hold memory with no model loaded")

        t = self.rig()
        t.loaded, t.cache_on = True, False
        t.q.prompt_ran()
        t.tick_at(0)
        self.assertTrue(t.tick_at(300), "a loaded model is freed under --cache-none")

    def test_if_the_engine_cannot_be_asked_what_it_holds_it_frees_anyway_and_says_so(self):
        t = self.rig()

        def broken():
            raise AttributeError("current_loaded_models")
        t.bound.loaded_fn = broken
        t.q.prompt_ran()
        t.tick_at(0)
        self.assertTrue(t.tick_at(300), "a /free is always safe: not knowing is no reason to hold memory")
        self.assertEqual(len([m for m in t.logs if "current_loaded_models" in m]), 1)

    def test_a_prompt_that_lands_between_the_check_and_the_write_is_not_raced(self):
        q = RacyQueue()
        t = self.rig(queue=q)
        q.prompt_ran()
        t.tick_at(0)
        q.script = [(0, None), (1, None)]   # the first read says idle; by the second a prompt is queued
        self.assertFalse(t.tick_at(300))
        self.assertEqual(q.flags, {}, "the flags are set only if the queue is still empty under the same hold")
        q.script = None
        self.assertTrue(t.tick_at(301), "the free was only postponed")

    def test_a_prompt_that_ran_between_the_check_and_the_write_is_not_raced_either(self):
        q = RacyQueue()
        t = self.rig(queue=q)
        q.prompt_ran()
        t.tick_at(0)
        q.script = [(0, None), (0, q.prompt_ran)]   # nothing queued at the recheck, but one began and ended since the read
        self.assertFalse(t.tick_at(300))
        self.assertEqual(q.flags, {})


class WhenItCannotRun(PackTest):
    def test_an_exception_in_a_tick_is_logged_once_and_the_loop_goes_on(self):
        q = FakeQueue()
        t = self.rig(queue=q)
        real = q.get_tasks_remaining
        failures = [RuntimeError("boom"), RuntimeError("boom")]

        def flaky():
            if failures:
                raise failures.pop(0)
            return real()
        q.get_tasks_remaining = flaky
        self.assertFalse(t.tick_at(0))
        self.assertFalse(t.tick_at(5))
        self.assertEqual(len([m for m in t.logs if "NOT ACTIVE" in m and "boom" in m]), 1, t.logs)
        self.assertIn("boom", t.bound.status()["error"])
        q.prompt_ran()
        t.tick_at(10)
        self.assertTrue(t.bound.armed, "the next tick works")
        self.assertIsNone(t.bound.status()["error"])

    def test_no_prompt_queue_yet_is_said_once_and_retried(self):
        t = self.rig()
        queues = [None, None]
        t.bound.get_queue = lambda: queues.pop(0) if queues else t.q
        self.assertFalse(t.tick_at(0))
        self.assertFalse(t.tick_at(5))
        self.assertEqual(len([m for m in t.logs if "NOT ACTIVE" in m]), 1, t.logs)
        t.q.prompt_ran()
        t.tick_at(10)
        self.assertTrue(t.bound.armed)

    def test_the_loop_ticks_then_waits_the_poll_interval(self):
        t = self.rig(poll=7)
        ticks, waits = [], []
        real_tick = t.bound.tick

        def counting():
            ticks.append(1)
            return real_tick()

        def waiting(seconds):
            waits.append(seconds)
            if len(waits) >= 3:
                t.bound.stop()
        t.bound.tick, t.bound.wait = counting, waiting
        t.bound.run()
        self.assertEqual(waits, [7, 7, 7])
        self.assertEqual(len(ticks), 3)

    def test_the_thread_is_a_daemon_so_comfyui_can_exit(self):
        t = self.rig()
        thread = t.bound.start()
        try:
            self.assertTrue(thread.daemon)
            self.assertTrue(thread.is_alive())
        finally:
            t.bound.stop()
            thread.join(5)
        self.assertFalse(thread.is_alive())


class Settings(PackTest):
    def test_the_limit(self):
        said = []
        for raw, want in ((None, 300), ("", 300), ("0", 0), ("300", 300), (" 120 ", 120), ("5", 5), ("86400", 86400)):
            self.assertEqual(self.pack.resolve_limit(raw, said.append), want, repr(raw))
        self.assertEqual(said, [], "valid values and the default are not said")

    def test_a_bad_limit_is_300_and_said_once(self):
        for raw in ("abc", "2", "4", "-3", "1e3", "3.5", "5 s"):
            said = []
            self.assertEqual(self.pack.resolve_limit(raw, said.append), 300, repr(raw))
            self.assertEqual(len(said), 1, f"{raw!r}: {said}")
            self.assertIn("OFFLOAD_IDLE_FREE_SEC", said[0])
            self.assertIn(raw, said[0])

    def test_the_poll_interval(self):
        for raw, want in ((None, 5), ("", 5), ("1", 1), ("7", 7), ("30", 30), ("0", 1), ("99", 30), ("abc", 5), ("-4", 5)):
            self.assertEqual(self.pack.resolve_poll(raw), want, repr(raw))


class Boot(PackTest):
    def boot(self, environ, **kw):
        said = []
        routes = []
        bound = self.pack.boot(environ=environ, get_queue=lambda: None, register_route=routes.append, autostart=False,
                               say=said.append, **kw)
        return bound, said, routes

    def test_the_default_is_300_s_and_the_bound_is_announced(self):
        bound, said, routes = self.boot({})
        self.assertIsNotNone(bound)
        self.assertEqual(bound.limit, 300)
        self.assertEqual(bound.poll, 5)
        self.assertEqual(said.count("idle bound 300 s"), 1, said)
        self.assertEqual(routes, [bound], "the status route is registered for the bound")

    def test_zero_disables_it_no_thread_no_route(self):
        bound, said, routes = self.boot({"OFFLOAD_IDLE_FREE_SEC": "0"})
        self.assertIsNone(bound)
        self.assertEqual(routes, [])
        self.assertEqual(len([m for m in said if "disabled" in m]), 1, said)

    def test_a_bad_value_is_300_with_one_line_saying_what_was_ignored(self):
        for raw in ("abc", "2"):
            bound, said, _ = self.boot({"OFFLOAD_IDLE_FREE_SEC": raw})
            self.assertIsNotNone(bound, raw)
            self.assertEqual(bound.limit, 300, raw)
            self.assertEqual(len([m for m in said if raw in m]), 1, said)

    def test_the_values_are_read_from_the_environment_it_is_given(self):
        bound, _, _ = self.boot({"OFFLOAD_IDLE_FREE_SEC": "120", "OFFLOAD_IDLE_POLL_SEC": "2"})
        self.assertIsNotNone(bound)
        self.assertEqual((bound.limit, bound.poll), (120, 2))

    def test_autostart_starts_one_daemon_thread(self):
        bound = self.pack.boot(environ={"OFFLOAD_IDLE_FREE_SEC": "60"}, get_queue=lambda: None, register_route=lambda b: None,
                               say=lambda m: None)
        self.assertIsNotNone(bound)
        try:
            thread = bound.thread
            self.assertTrue(thread.is_alive())
            self.assertTrue(thread.daemon)
        finally:
            bound.stop()
            bound.thread.join(5)


class Route(PackTest):
    def setUp(self):
        super().setUp()
        self.q = FakeQueue()
        self.registered = {}

        class Routes:
            def get(inner, path):
                def decorate(handler):
                    self.registered[path] = handler
                    return handler
                return decorate
        server = types.ModuleType("server")
        server.PromptServer = types.SimpleNamespace(instance=types.SimpleNamespace(routes=Routes(), prompt_queue=self.q))
        aiohttp = types.ModuleType("aiohttp")
        aiohttp.web = types.SimpleNamespace(json_response=lambda data: data)
        saved = {k: sys.modules.get(k) for k in ("server", "aiohttp")}
        sys.modules.update({"server": server, "aiohttp": aiohttp})

        def restore():
            for k, v in saved.items():
                if v is None:
                    sys.modules.pop(k, None)
                else:
                    sys.modules[k] = v
        self.addCleanup(restore)

    def test_boot_finds_the_queue_and_registers_the_status_route_on_the_prompt_server(self):
        said = []
        bound = self.pack.boot(environ={}, autostart=False, say=said.append)
        self.assertIsNotNone(bound)
        self.assertIs(bound.get_queue(), self.q, "the queue is PromptServer.instance.prompt_queue, resolved on each tick")
        self.assertIn("/offload/idle", self.registered)

        payload = asyncio.run(self.registered["/offload/idle"](None))
        self.assertEqual(sorted(payload), ["armed", "error", "exit_idle_sec", "exit_limit_sec", "freed", "holds", "idle_sec",
                                           "last_free_unix", "limit_sec"])
        self.assertEqual((payload["exit_limit_sec"], payload["holds"]), (0, 0))
        self.assertEqual((payload["limit_sec"], payload["armed"], payload["freed"]), (300, False, 0))
        self.assertIsNone(payload["idle_sec"])
        self.assertIsNone(payload["last_free_unix"])

    def test_the_status_follows_a_free(self):
        bound = self.pack.boot(environ={"OFFLOAD_IDLE_FREE_SEC": "60"}, autostart=False, say=lambda m: None)
        self.assertIsNotNone(bound)
        clock = Clock()
        bound.clock = clock
        bound.loaded_fn = lambda: True
        self.q.prompt_ran()
        bound.tick()
        clock.now = 30
        before = asyncio.run(self.registered["/offload/idle"](None))
        self.assertEqual((before["armed"], before["freed"], before["idle_sec"]), (True, 0, 30))
        clock.now = 60
        self.assertTrue(bound.tick())
        after = asyncio.run(self.registered["/offload/idle"](None))
        self.assertEqual((after["armed"], after["freed"]), (False, 1))
        self.assertIsInstance(after["last_free_unix"], float)


class AsAComfyUIPack(unittest.TestCase):
    def test_it_registers_no_node_and_still_counts_as_a_pack(self):
        # nodes.py loads a folder only if it exports NODE_CLASS_MAPPINGS (or comfy_entrypoint); without it the import is reported as failed
        pack = load_pack()
        self.assertEqual(pack.NODE_CLASS_MAPPINGS, {})

    def test_importing_it_pulls_in_no_torch_and_no_comfyui(self):
        asked = []

        class Recorder:
            @staticmethod
            def find_spec(fullname, path=None, target=None):
                asked.append(fullname)
                return None
        sys.meta_path.insert(0, Recorder)
        try:
            load_pack("offload_idle_imports")
        except ImportError:
            pass   # a pack that imports torch fails here where torch is absent; what it ASKED for is the point
        finally:
            sys.meta_path.remove(Recorder)
        heavy = [n for n in asked if n.split(".")[0] in ("torch", "comfy", "server", "aiohttp", "numpy")]
        self.assertEqual(heavy, [], "module scope is standard library only")

    def test_importing_it_outside_comfyui_does_not_raise_and_starts_the_bound(self):
        for name in ("server", "comfy", "comfy.model_management", "comfy.cli_args", "aiohttp"):
            self.assertNotIn(name, sys.modules)
        pack = load_pack("offload_idle_standalone", environ={})
        try:
            self.assertIsNotNone(pack.BOUND)
            self.assertEqual(pack.BOUND.limit, 300)
            self.assertTrue(pack.BOUND.thread.daemon)
            self.assertIsNone(pack.BOUND.get_queue(), "no PromptServer: no queue, said and retried by the tick")
        finally:
            if pack.BOUND is not None:
                pack.BOUND.stop()
                pack.BOUND.thread.join(5)


class TheStandIns(unittest.TestCase):
    def test_default_probes_read_comfyui_modules_by_name(self):
        pack = load_pack()
        mm = types.ModuleType("comfy.model_management")
        mm.current_loaded_models = []
        cli = types.ModuleType("comfy.cli_args")
        cli.args = types.SimpleNamespace(cache_none=True)
        comfy = types.ModuleType("comfy")
        saved = {k: sys.modules.get(k) for k in ("comfy", "comfy.model_management", "comfy.cli_args")}
        sys.modules.update({"comfy": comfy, "comfy.model_management": mm, "comfy.cli_args": cli})
        try:
            self.assertFalse(pack.models_loaded())
            self.assertFalse(pack.cache_on())
            mm.current_loaded_models.append(object())
            cli.args.cache_none = False
            self.assertTrue(pack.models_loaded())
            self.assertTrue(pack.cache_on())
        finally:
            for k, v in saved.items():
                if v is None:
                    sys.modules.pop(k, None)
                else:
                    sys.modules[k] = v


if __name__ == "__main__":
    unittest.main()
