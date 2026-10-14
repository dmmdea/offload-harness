"""Harness-owned ComfyUI idle bound: free models and caches after a quiet period (seam, no behaviour yet).

Loaded through render/comfyui-ext/extra_model_paths_idle.yaml (its `custom_nodes` key), which
render/comfy-lifecycle.mjs passes with --extra-model-paths-config on a kept or warm launch. It
registers no node: NODE_CLASS_MAPPINGS is empty on purpose, because ComfyUI (nodes.py) reports a
folder that exports neither NODE_CLASS_MAPPINGS nor comfy_entrypoint as a custom node that failed to
import. This file is the seam the tests are written against; the behaviour lands in the next commit.
"""
import logging
import threading
import time

NODE_CLASS_MAPPINGS = {}

DEFAULT_LIMIT_SEC = 300
MIN_LIMIT_SEC = 5
DEFAULT_POLL_SEC = 5
MIN_POLL_SEC = 1
MAX_POLL_SEC = 30


def _say(message):
    logging.info("[offload_idle] %s", message)


def resolve_limit(raw, say=None):
    return DEFAULT_LIMIT_SEC


def resolve_poll(raw, say=None):
    return DEFAULT_POLL_SEC


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

    def tick(self):
        return False

    def status(self):
        return {}

    def run(self):
        return None

    def start(self):
        return threading.Thread(target=self.run)

    def stop(self):
        self._stop.set()


def boot(environ=None, get_queue=None, register_route=None, autostart=True, say=None):
    return None


BOUND = None
