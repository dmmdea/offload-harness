"""Browse-lane sidecar: drives an already-running Chromium through browser-use's jev-ultrafast loop.

The sidecar owns the browser; the harness owns every model call. Everything crosses one JSON-lines
stdio channel (see docs/systems/browse-lane.md). The pure helpers at the top of this module import
without jev_ultrafast / browser_harness; those are imported inside main() only.

stdout carries protocol lines and nothing else. Diagnostics go to stderr.
"""

from __future__ import annotations

import json
import os
import platform
import re
import sys
import time
import urllib.parse
from pathlib import Path
from urllib.parse import urlparse

# --------------------------------------------------------------------------------------------
# Deny-list
# --------------------------------------------------------------------------------------------

DENY_WORDS = (
    "publish|send|post|delete|remove|pay|buy|purchase|checkout|place order|order now|subscribe|"
    "unsubscribe|transfer|withdraw|sign out|log out|logout|deactivate|close account|confirm"
)
DENY_PATTERN = re.compile(r"\b(?:" + DENY_WORDS.replace(" ", r"\s+") + r")\b", re.IGNORECASE)

# Operations whose execution is an effect. Typing into a field is not an effect by itself.
EFFECT_KINDS = {"click", "select"}


def _norm(label) -> str:
    return " ".join(str(label or "").split()).lower()


def is_denied(label, allow_labels, unattended) -> bool:
    """True when the label matches the deny-list and is not lifted by an attended allow_labels entry."""
    text = str(label or "")
    if not DENY_PATTERN.search(text):
        return False
    if unattended:
        return True
    allowed = {_norm(a) for a in (allow_labels or [])}
    if _norm(text) in allowed:
        return False
    # jev labels look like "Option → detail": an allowed head lifts only a deny that came from the head.
    head, sep, tail = text.partition(" → ")
    if sep and _norm(head) in allowed and not DENY_PATTERN.search(tail):
        return False
    return True


def filter_actions(actions, allow_labels, unattended):
    """Drop click/select actions whose label is denied. Returns (kept, removed_labels)."""
    kept, removed = [], []
    for action in actions or []:
        if action.get("kind") in EFFECT_KINDS and is_denied(action.get("label", ""), allow_labels, unattended):
            removed.append(str(action.get("label", "")))
        else:
            kept.append(action)
    return kept, removed


# --------------------------------------------------------------------------------------------
# Host allowlist
# --------------------------------------------------------------------------------------------


def host_allowed(url, allow_hosts, allow_blank=False) -> bool:
    """http(s) only. Empty allow_hosts means any host; entries match exactly or as a parent domain."""
    url = str(url or "")
    if url == "about:blank":
        return bool(allow_blank)
    try:
        parsed = urlparse(url)
        host = (parsed.hostname or "").lower().rstrip(".")
    except ValueError:
        return False
    if parsed.scheme not in ("http", "https") or not host:
        return False
    entries = [str(h).strip().lower().rstrip(".") for h in (allow_hosts or []) if str(h).strip()]
    if not entries:
        return True
    return any(host == e or host.endswith("." + e) for e in entries)


# --------------------------------------------------------------------------------------------
# Header redaction
# --------------------------------------------------------------------------------------------

_DROP_NAMES = {"cookie", "set-cookie", "authorization", "proxy-authorization"}
_DROP_SUBSTRINGS = ("token", "auth", "session", "csrf", "key", "secret")


def redact_headers(headers) -> dict:
    out = {}
    for name, value in (headers or {}).items():
        lowered = str(name).lower()
        if lowered in _DROP_NAMES or any(s in lowered for s in _DROP_SUBSTRINGS):
            continue
        out[name] = value
    return out


# --------------------------------------------------------------------------------------------
# Browser discovery
# --------------------------------------------------------------------------------------------


def parse_devtools_active_port(text) -> tuple[int, str]:
    lines = [ln.strip() for ln in str(text).splitlines() if ln.strip()]
    if len(lines) < 2:
        raise ValueError("DevToolsActivePort needs a port line and a websocket path line")
    port = int(lines[0])
    path = lines[1]
    if not path.startswith("/"):
        raise ValueError("DevToolsActivePort websocket path must start with '/'")
    return port, path


_WINDOWS_DIRS = {
    "chrome": ("Google", "Chrome", "User Data"),
    "edge": ("Microsoft", "Edge", "User Data"),
    "brave": ("BraveSoftware", "Brave-Browser", "User Data"),
    "chromium": ("Chromium", "User Data"),
}
_DARWIN_DIRS = {
    "chrome": ("Library", "Application Support", "Google", "Chrome"),
    "edge": ("Library", "Application Support", "Microsoft Edge"),
    "brave": ("Library", "Application Support", "BraveSoftware", "Brave-Browser"),
    "chromium": ("Library", "Application Support", "Chromium"),
}
_LINUX_DIRS = {
    "chrome": (".config", "google-chrome"),
    "edge": (".config", "microsoft-edge"),
    "brave": (".config", "BraveSoftware", "Brave-Browser"),
    "chromium": (".config", "chromium"),
}


def current_system() -> str:
    return {"Windows": "windows", "Darwin": "darwin"}.get(platform.system(), "linux")


def profile_dirs(browser, system, env) -> list[Path]:
    browser = str(browser or "").lower()
    home = env.get("HOME") or env.get("USERPROFILE") or ""
    if system == "windows":
        table, base = _WINDOWS_DIRS, env.get("LOCALAPPDATA") or (str(Path(home) / "AppData" / "Local") if home else "")
    elif system == "darwin":
        table, base = _DARWIN_DIRS, home
    elif system == "linux":
        table, base = _LINUX_DIRS, home
    else:
        return []
    if browser not in table or not base:
        return []
    return [Path(base).joinpath(*table[browser])]


def cdp_env_for(cdp_url) -> dict:
    """The browser-harness variable that pins a run to one browser endpoint.

    The harness has already held browse_cdp_url to loopback http/ws with a port; this only
    maps it: a ws:// endpoint is used as-is (BU_CDP_WS), an http:// one is resolved by
    browser-harness through /json/version (BU_CDP_URL). Empty means discovery.
    """
    url = str(cdp_url or "").strip()
    if not url:
        return {}
    if url.lower().startswith("ws://"):  # schemes are case-insensitive; Go accepted WS:// too
        return {"BU_CDP_WS": url}
    return {"BU_CDP_URL": url.rstrip("/")}


CDP_ENV_KEYS = ("BU_CDP_URL", "BU_CDP_WS")


def browser_env(start, system, env) -> tuple[dict, str]:
    """Which browser this run attaches to: (env to set, error). Precedence, highest first:
    the harness's cdp_url pin, then the named browser's DevToolsActivePort, then
    browser-harness discovery ({}). The caller clears CDP_ENV_KEYS before applying, so an
    inherited BU_CDP_* can never override the pin."""
    pinned = cdp_env_for(start.get("cdp_url"))
    if pinned:
        return pinned, ""
    name = str(start.get("browser") or "")
    if name:
        ws = resolve_cdp_ws(name, system, env)
        if not ws:
            return {}, f"no DevToolsActivePort found for browser {name!r}"
        return {"BU_CDP_WS": ws}, ""
    return {}, ""


def resolve_cdp_ws(browser, system, env):
    """ws://127.0.0.1:<port><path> from the first candidate profile dir that has DevToolsActivePort."""
    for directory in profile_dirs(browser, system, env):
        try:
            text = (directory / "DevToolsActivePort").read_text(encoding="utf-8")
            port, path = parse_devtools_active_port(text)
        except (OSError, ValueError):
            continue
        return f"ws://127.0.0.1:{port}{path}"
    return None


# --------------------------------------------------------------------------------------------
# Stdio protocol
# --------------------------------------------------------------------------------------------


def log(message) -> None:
    print(f"[browse-sidecar] {message}", file=sys.stderr, flush=True)


class ProtocolError(Exception):
    def __init__(self, message, op=""):
        super().__init__(message)
        self.op = op


class Protocol:
    REPLY_TYPES = {"decide": "decision", "text": "text_result"}

    def __init__(self, instream, outstream):
        self.inp = instream
        self.out = outstream
        self.next_id = 1
        self.counts = {"decide": 0, "text": 0}

    def send(self, obj) -> None:
        # ensure_ascii: page strings can carry lone surrogates (snapshot.js slices by UTF-16
        # unit); escaped, they can never make the UTF-8 stdout raise mid-run.
        self.out.write(json.dumps(obj, ensure_ascii=True, separators=(",", ":")) + "\n")
        self.out.flush()

    def read(self):
        """Next parseable JSON object line, or None on EOF."""
        while True:
            line = self.inp.readline()
            if line == "":
                return None
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except ValueError:
                log("ignoring non-JSON line on stdin")
                continue
            if isinstance(obj, dict):
                return obj

    def request(self, type_, payload):
        reply_type = self.REPLY_TYPES[type_]
        rid = self.next_id
        self.next_id += 1
        self.send({"type": type_, "id": rid, **payload})
        self.counts[type_] += 1
        while True:
            msg = self.read()
            if msg is None:
                raise ProtocolError("harness closed the channel", op=type_)
            if msg.get("type") != reply_type or msg.get("id") != rid:
                log(f"ignoring unrelated message {msg.get('type')!r} id={msg.get('id')!r}")
                continue
            if not msg.get("ok"):
                raise ProtocolError(str(msg.get("error") or "harness reported failure"), op=type_)
            return msg


# --------------------------------------------------------------------------------------------
# Traffic capture
# --------------------------------------------------------------------------------------------

POST_DATA_LIMIT = 65536
BODY_LIMIT = 262144

# A key is sensitive when its last name segment is one of these (case-insensitive):
# "access_token", "X-Api-Key", "session_id" and "code" (an OAuth code) all match.
SENSITIVE_KEY = re.compile(
    r"(^|[_\-.])(password|passwd|pass|pwd|token|secret|apikey|api_key|key|auth|authorization|session|"
    r"sessionid|session_id|sid|cookie|csrf|xsrf|signature|sig|otp|credential|credentials|code)$",
    re.I,
)
REDACTED = "[redacted]"


def prefix_matches(url, prefix) -> bool:
    """A capture prefix matches on a path boundary, never mid-host or mid-segment:
    "https://api.example.com" must not match "https://api.example.com.evil.net/"."""
    url, prefix = str(url), str(prefix)
    if not prefix or not url.startswith(prefix):
        return False
    if len(url) == len(prefix) or prefix[-1] in "/?#&=":
        return True
    return url[len(prefix)] in "/?#"


def redact_url(url) -> str:
    """Replace the values of sensitive query parameters (tokens, keys, codes, signatures)."""
    try:
        parts = urllib.parse.urlsplit(str(url))
    except ValueError:
        return str(url)
    if not parts.query:
        return str(url)
    pairs = urllib.parse.parse_qsl(parts.query, keep_blank_values=True)
    red = [(k, REDACTED if SENSITIVE_KEY.search(k) else v) for k, v in pairs]
    return urllib.parse.urlunsplit(parts._replace(query=urllib.parse.urlencode(red, safe="[]")))


def _redact_obj(obj):
    if isinstance(obj, dict):
        return {k: (REDACTED if isinstance(k, str) and SENSITIVE_KEY.search(k) else _redact_obj(v)) for k, v in obj.items()}
    if isinstance(obj, list):
        return [_redact_obj(v) for v in obj]
    return obj


def redact_payload(text) -> str:
    """Redact sensitive keys in a JSON or form-urlencoded body; other text is kept as is."""
    text = str(text)
    stripped = text.strip()
    if stripped[:1] in ("{", "["):
        try:
            return json.dumps(_redact_obj(json.loads(stripped)), ensure_ascii=True)
        except ValueError:
            return text
    if "=" in stripped and " " not in stripped and "\n" not in stripped:
        try:
            pairs = urllib.parse.parse_qsl(stripped, keep_blank_values=True, strict_parsing=True)
        except ValueError:
            return text
        return urllib.parse.urlencode([(k, REDACTED if SENSITIVE_KEY.search(k) else v) for k, v in pairs])
    return text


class Capture:
    """Collects request/response records for URLs under the given prefixes, headers redacted."""

    def __init__(self, prefixes, path, session_id=None, get_body=None):
        self.prefixes = tuple(p for p in (prefixes or []) if p)
        self.path = path
        self.session_id = session_id
        self.get_body = get_body
        self.records: list[dict] = []
        self._by_id: dict[str, dict] = {}
        self._written = 0

    def _wanted(self, url) -> bool:
        return any(prefix_matches(url, p) for p in self.prefixes)

    def feed(self, event) -> None:
        if self.session_id is not None and event.get("session_id") != self.session_id:
            return
        method = event.get("method", "")
        params = event.get("params") or {}
        rid = params.get("requestId")
        if method == "Network.requestWillBeSent":
            req = params.get("request") or {}
            url = req.get("url", "")
            if not self._wanted(url):
                return
            record = {
                "request_id": rid,
                "method": req.get("method", ""),
                "url": redact_url(url),
                "request_headers": redact_headers(req.get("headers")),
                "finished": False,
            }
            post = req.get("postData")
            if post:
                record["post_data"] = redact_payload(str(post)[:POST_DATA_LIMIT])
            self.records.append(record)
            self._by_id[rid] = record
        elif method == "Network.responseReceived":
            record = self._by_id.get(rid)
            if record is None:
                return
            resp = params.get("response") or {}
            record["status"] = resp.get("status")
            record["mime_type"] = resp.get("mimeType", "")
            record["response_headers"] = redact_headers(resp.get("headers"))
        elif method == "Network.loadingFinished":
            record = self._by_id.get(rid)
            if record is None:
                return
            record["finished"] = True
            if self.get_body and "json" in str(record.get("mime_type", "")).lower():
                try:
                    body = self.get_body(rid)
                except Exception as exc:  # the body may be evicted after navigation
                    log(f"response body unavailable: {type(exc).__name__}")
                    body = None
                if body is not None:
                    record["body"] = redact_payload(str(body)[:BODY_LIMIT])

    def write(self) -> int:
        """Append records not yet written as JSONL; returns the total count kept."""
        pending = self.records[self._written:]
        if pending and self.path:
            with open(self.path, "a", encoding="utf-8") as fh:
                for record in pending:
                    fh.write(json.dumps(record, ensure_ascii=True) + "\n")
        self._written = len(self.records)
        return len(self.records)


# --------------------------------------------------------------------------------------------
# Run control (used by main only)
# --------------------------------------------------------------------------------------------


class HostNotAllowed(Exception):
    pass


class ActionDenied(Exception):
    pass


class BudgetExceeded(Exception):
    pass


FINAL_TEXT_LIMIT = 4000
ACTIONS_LIMIT = 60

LIVE_LABEL_JS = (
    "(n => { const e = window.__jevFast?.nodes.get(n); if (!e) return null;"
    " return [e.getAttribute('aria-label'), e.getAttribute('title'), e.value,"
    " (e.innerText || '').slice(0, 200)].filter(x => typeof x === 'string' && x.trim()); })"
)

# Where a click would navigate or submit: the enclosing link's href and, for a submit
# control, its form's action. Checked against the host allowlist BEFORE the click, so
# an off-list page is never loaded in the operator's session by a click.
CLICK_TARGETS_JS = (
    "(n => { const e = window.__jevFast?.nodes.get(n); if (!e || !e.closest) return [];"
    " const out = []; const a = e.closest('a[href]'); if (a && a.href) out.push(a.href);"
    " const submit = (e.tagName === 'BUTTON' && (e.type || 'submit') === 'submit') ||"
    " (e.tagName === 'INPUT' && (e.type === 'submit' || e.type === 'image'));"
    " if (submit && e.form) out.push(e.formAction || e.form.action);"
    " return out.filter(x => typeof x === 'string' && x); })"
)


def off_list_targets(urls, allow_hosts) -> list[str]:
    """The http(s) targets whose host is outside the allowlist (javascript:, #, mailto: are not navigations)."""
    bad = []
    for u in urls or []:
        u = str(u)
        if urlparse(u).scheme in ("http", "https") and not host_allowed(u, allow_hosts):
            bad.append(u)
    return bad


class Run:
    def __init__(self, start, proto):
        self.proto = proto
        self.goal = str(start.get("goal", ""))
        self.url = str(start.get("url", ""))
        self.max_actions = max(1, min(60, int(start.get("max_actions") or 30)))
        self.allow_labels = list(start.get("allow_labels") or [])
        self.allow_hosts = list(start.get("allow_hosts") or [])
        self.unattended = bool(start.get("unattended", True))
        self.executed = 0
        self.removed: list[str] = []
        self.observed_once = False
        self.last_page = None
        self.denied_label = ""
        self.blocked_host = ""
        self.capture: Capture | None = None
        self.net_enabled = False
        self.helpers = None  # browser_harness.helpers once imported (capture drains)

    def denied(self, label) -> bool:
        return is_denied(label, self.allow_labels, self.unattended)


def _classify(exc, agent_built) -> tuple[str, str]:
    if isinstance(exc, ProtocolError):
        return ("TEXT_UNAVAILABLE" if exc.op == "text" else "DECISION_UNAVAILABLE"), str(exc)
    text = str(exc)
    if isinstance(exc, ValueError) and text.startswith("Invalid TypeSafe response"):
        return "DECISION_UNAVAILABLE", text
    if not agent_built:
        return "BROWSER_UNAVAILABLE", f"{type(exc).__name__}: {text}"[:500]
    lowered = text.lower()
    if isinstance(exc, (ConnectionError, TimeoutError, OSError)) or any(
        s in lowered for s in ("daemon", "websocket", "cdp", "target closed", "no target", "session with given id")
    ):
        return "BROWSER_UNAVAILABLE", f"{type(exc).__name__}: {text}"[:500]
    return "RUNNER_FAILED", f"{type(exc).__name__}: {text}"[:500]


def _install_patches(run: Run, model, agent_mod, browser_mod) -> None:
    proto = run.proto

    def post_json(url, key, body):
        if str(url).endswith("/v1/systemone"):
            return proto.request("decide", {"body": body})["result"]
        raise RuntimeError("blocked: the browse sidecar makes no direct network calls")

    def field_text(context):
        started = time.perf_counter()
        text = proto.request("text", {"context": context})["text"]
        if not isinstance(text, str) or not text.strip():
            raise ValueError("Text helper returned no valid field value; nothing typed.")
        latency = round((time.perf_counter() - started) * 1000)
        return text, {"model": "harness-local", "latency_ms": latency, "usage": {}}

    # choose() resolves post_json as a model-module global; agent.py from-imports field_text.
    model.post_json = post_json
    model.field_text = field_text
    agent_mod.field_text = field_text

    Browser = browser_mod.Browser
    orig_observe, orig_act, orig_call = Browser.observe, Browser.act, Browser.call

    def observe(self, *args, **kwargs):
        page = orig_observe(self, *args, **kwargs)
        _drain(run, run.helpers)
        url = page.get("url", "")
        if not host_allowed(url, run.allow_hosts, allow_blank=not run.observed_once):
            # last_page is NOT updated: the result must not carry an off-list page's text.
            run.blocked_host = url
            raise HostNotAllowed(url)
        run.last_page = page
        run.observed_once = True
        kept, removed = filter_actions(page.get("actions", []), run.allow_labels, run.unattended)
        page["actions"] = kept
        for label in removed:
            if label not in run.removed:
                run.removed.append(label)
        return page

    def act(self, action, page, text=None):
        if run.executed >= run.max_actions:
            raise BudgetExceeded(f"max_actions {run.max_actions} reached")
        kind, label = action.get("kind"), str(action.get("label", ""))
        if kind in EFFECT_KINDS:
            if run.denied(label):
                run.denied_label = label
                raise ActionDenied(label)
            node = action.get("node")
            if type(node) is int:
                try:
                    # A <select>'s innerText is every option; judge only the option chosen
                    # (its label, checked above), never the whole list.
                    live = [] if kind == "select" else (self.evaluate(f"({LIVE_LABEL_JS})({node})") or [])
                    targets = self.evaluate(f"({CLICK_TARGETS_JS})({node})") or [] if kind == "click" else []
                except Exception as exc:
                    # The page changed under the recheck: never click blind. StalePage makes
                    # the loop observe again instead of acting.
                    raise browser_mod.StalePage(f"deny recheck could not read the live element: {type(exc).__name__}") from exc
                for candidate in live:
                    if run.denied(candidate):
                        run.denied_label = str(candidate)[:120]
                        raise ActionDenied(run.denied_label)
                bad = off_list_targets(targets, run.allow_hosts)
                if bad:
                    run.blocked_host = bad[0]
                    raise HostNotAllowed(bad[0])
        _drain(run, run.helpers)
        result = orig_act(self, action, page, text=text)
        _drain(run, run.helpers)
        run.executed += 1
        proto.send({"type": "step", "n": run.executed, "op": str(kind), "label": label, "url": page.get("url", "")})
        return result

    def call(self, method, **params):
        # Enable Network before the first navigation so the initial page's traffic is captured too.
        if run.capture is not None and not run.net_enabled and method == "Page.navigate":
            run.net_enabled = True
            run.capture.session_id = self.session
            orig_call(self, "Network.enable")
        return orig_call(self, method, **params)

    Browser.observe, Browser.act, Browser.call = observe, act, call


# Before the tab closes, the page's own XHR/fetch traffic must go quiet: an editor saves
# on a debounce after the last input (Substack's autosave leaves 2.3 s after a keystroke,
# measured 2026-09-29), and closing the tab at DONE dropped that save — a typed title was
# lost while the run reported done. Quiet = nothing in flight for SETTLE_QUIET_S; the wait
# is capped so a long-poll can never hold a run open.
SETTLE_QUIET_S = 3.5
SETTLE_MAX_S = 15.0
SETTLE_POLL_S = 0.2
SETTLE_TYPES = frozenset({"XHR", "Fetch"})


class NetworkSettle:
    """One session's XHR/fetch traffic after the run's last action (pure; clock injected)."""

    def __init__(self, session_id, started, quiet=SETTLE_QUIET_S, cap=SETTLE_MAX_S):
        self.session_id = session_id
        self.started = started
        self.quiet = quiet
        self.cap = cap
        self.pending: set = set()
        self.last_activity = started
        self.seen = 0

    def feed(self, event, now) -> None:
        if event.get("session_id") != self.session_id:
            return
        method = event.get("method", "")
        params = event.get("params") or {}
        rid = params.get("requestId")
        if method == "Network.requestWillBeSent":
            if params.get("type") in SETTLE_TYPES:
                self.pending.add(rid)
                self.seen += 1
                self.last_activity = now
        elif method in ("Network.loadingFinished", "Network.loadingFailed") and rid in self.pending:
            self.pending.discard(rid)
            self.last_activity = now

    def done(self, now) -> bool:
        if now - self.started >= self.cap:
            return True
        return not self.pending and now - self.last_activity >= self.quiet


def settle_network(run, agent, helpers, clock=time.monotonic, sleep=time.sleep):
    """Hold the tab open until its XHR/fetch traffic is quiet (see SETTLE_QUIET_S); returns
    the tracker, or None when there is no live browser session to watch. Events drained
    here still reach the capture: the daemon's buffer is shared and draining is destructive."""
    session = getattr(getattr(agent, "browser", None), "session", None)
    if not session or helpers is None:
        return None
    try:
        if run.net_enabled:
            # Stale events from before the last action are not the save we wait for.
            for event in helpers.drain_events():
                if run.capture is not None:
                    run.capture.feed(event)
        else:
            helpers.cdp("Network.enable", session_id=session)
            run.net_enabled = True
    except Exception as exc:  # the browser may be gone; closing is all that is left
        log(f"settle skipped: {type(exc).__name__}: {exc}")
        return None
    tracker = NetworkSettle(session, clock())
    while True:
        try:
            events = helpers.drain_events()
        except Exception as exc:
            log(f"settle drain failed: {type(exc).__name__}: {exc}")
            break
        now = clock()
        for event in events:
            tracker.feed(event, now)
            if run.capture is not None:
                run.capture.feed(event)
        if tracker.done(clock()):
            break
        sleep(SETTLE_POLL_S)
    log(f"settled after {clock() - tracker.started:.1f}s ({tracker.seen} request(s) watched)")
    return tracker


def _drain(run: Run, helpers) -> None:
    # Drained around every act and after every observe, not once per tick: the daemon's
    # shared event buffer holds 500 events and drops the oldest, so a busy page between
    # two drains would lose requests (and their bodies, which Chrome may evict).
    if run.capture is None or helpers is None:
        return
    try:
        for event in helpers.drain_events():
            run.capture.feed(event)
    except Exception as exc:
        log(f"capture drain failed: {type(exc).__name__}: {exc}")


def _set(result, status, cls, reason, model_done=False):
    result.update(status=status, reason=reason, model_done=model_done)
    result["class"] = cls


def main() -> int:
    real_out = sys.stdout
    sys.stdout = sys.stderr  # nothing but protocol lines may reach the real stdout
    try:
        sys.stdin.reconfigure(encoding="utf-8")
        real_out.reconfigure(encoding="utf-8", newline="\n")
    except (AttributeError, ValueError):
        pass
    proto = Protocol(sys.stdin, real_out)

    result = {
        "type": "result", "status": "error", "class": "RUNNER_FAILED", "reason": "", "model_done": False,
        "final": {"url": "", "title": "", "text": ""}, "actions": [], "decisions": 0, "text_calls": 0, "captured": 0,
    }
    start = proto.read()
    if not start or start.get("type") != "start":
        result["reason"] = "no start message on stdin"
        proto.send(result)
        return 1

    for key in ("BH_TELEMETRY", "BROWSER_HARNESS_TELEMETRY", "ANONYMIZED_TELEMETRY", "BH_TAB_MARKER"):
        os.environ[key] = "0"
    os.environ["TYPESAFE_API_KEY"] = "via-harness"  # jev's choose() reads it; the real call is replaced
    # The lane's own browser-harness daemon name: stop_lane_daemon() below only ever
    # stops this one, never a daemon another tool on the machine started.
    os.environ.setdefault("BU_NAME", LANE_DAEMON_NAME)

    run = Run(start, proto)
    agent, agent_built, helpers = None, False, None
    try:
        if not host_allowed(run.url, run.allow_hosts):
            _set(result, "error", "HOST_NOT_ALLOWED", f"start url not allowed: {run.url}")
            return _finish(proto, result, run, None)
        # browse_cdp_url (a dedicated browser instance) wins over the named browser, which
        # wins over discovery. Inherited BU_CDP_* are cleared first so nothing overrides that.
        updates, browser_error = browser_env(start, current_system(), dict(os.environ))
        if browser_error:
            _set(result, "error", "BROWSER_UNAVAILABLE", browser_error)
            return _finish(proto, result, run, None)
        for key in CDP_ENV_KEYS:
            os.environ.pop(key, None)
        os.environ.update(updates)

        # Imported only now, after the environment above is set.
        import jev_ultrafast.agent as agent_mod
        import jev_ultrafast.browser as browser_mod
        import jev_ultrafast.model as model
        from browser_harness import helpers as bh_helpers

        helpers = bh_helpers
        run.helpers = bh_helpers
        if start.get("capture_prefixes"):
            run.capture = Capture(start["capture_prefixes"], start.get("capture_path") or "")

            def get_body(request_id):
                resp = helpers.cdp("Network.getResponseBody", requestId=request_id, session_id=run.capture.session_id)
                return None if resp.get("base64Encoded") else resp.get("body")

            run.capture.get_body = get_body
        _install_patches(run, model, agent_mod, browser_mod)

        agent = agent_mod.Agent(run.url, run.goal)
        agent_built = True
        if run.capture is not None and not run.net_enabled:
            run.net_enabled = True
            run.capture.session_id = agent.browser.session
            helpers.cdp("Network.enable", session_id=agent.browser.session)
        _drain(run, helpers)

        for _state in agent.run():
            _drain(run, helpers)
        status = agent.state["status"]
        if status == "done":
            _set(result, "done", "", "", model_done=True)
        else:
            _set(result, "blocked", "", "the model or the loop reported no progress")
    except HostNotAllowed:
        _set(result, "error", "HOST_NOT_ALLOWED", f"navigated to a host outside the allowlist: {run.blocked_host}")
    except ActionDenied:
        _set(result, "denied", "DENIED", f"control refused by the deny-list: {run.denied_label}")
    except BudgetExceeded as exc:
        _set(result, "budget", "", str(exc))
    except Exception as exc:
        cls, reason = _classify(exc, agent_built)
        log(f"run failed: {cls}: {reason}")
        if "action demo budget" in reason:
            # jev-ultrafast's own hard cap (MAX_STEPS) fired before ours: same meaning.
            _set(result, "budget", "", reason)
        elif agent_built and cls == "RUNNER_FAILED" and agent.state.get("status") == "blocked":
            _set(result, "blocked", "", reason)
        else:
            _set(result, "error", cls, reason)
    finally:
        if helpers is not None:
            _drain(run, helpers)
            if agent is not None:
                settle_network(run, agent, helpers)  # let a debounced save leave before the tab closes
        if agent is not None:
            try:
                agent.close()
            except Exception as exc:
                log(f"agent close failed: {type(exc).__name__}: {exc}")
    return _finish(proto, result, run, agent)


def _finish(proto: Protocol, result: dict, run: Run, agent) -> int:
    page = (agent.state.get("page") if agent is not None else None) or run.last_page or {}
    result["final"] = {
        "url": str(page.get("url", "")),
        "title": str(page.get("title", "")),
        "text": str(page.get("text", ""))[:FINAL_TEXT_LIMIT],
    }
    history = agent.state.get("history", []) if agent is not None else []
    result["actions"] = [
        {"step": h.get("step"), "label": h.get("action"), "kind": h.get("kind"), "url": h.get("url"),
         "text": (h.get("text") or "")[:200] or None}
        for h in history[-ACTIONS_LIMIT:]
    ]
    result["decisions"] = proto.counts["decide"]
    result["text_calls"] = proto.counts["text"]
    result["removed_labels"] = run.removed[:50]
    try:
        result["captured"] = run.capture.write() if run.capture is not None else 0
    except OSError as exc:
        log(f"capture write failed: {exc}")
        result["captured"] = len(run.capture.records) if run.capture else 0
    proto.send(result)
    return 0 if result["status"] in ("done", "blocked", "denied", "budget") else 1


LANE_DAEMON_NAME = "offload-browse"


def stop_lane_daemon() -> None:
    """Stop the browser-harness daemon this run started, best effort.

    ensure_daemon() leaves a background process that outlives the sidecar; the lane
    must not leave a resident process behind. Runs AFTER the result line is sent, so
    the harness never waits on it for the answer. Only acts when browser_harness was
    actually imported this run (nothing to stop otherwise) and only on the lane's own
    daemon name.
    """
    if "browser_harness" not in sys.modules:
        return
    try:
        from browser_harness import admin

        admin.restart_daemon(name=os.environ.get("BU_NAME") or LANE_DAEMON_NAME)  # stops only, despite the name
    except Exception as exc:  # noqa: BLE001 - shutdown is best effort; the run's answer is already out
        log(f"daemon stop failed: {type(exc).__name__}: {exc}")


if __name__ == "__main__":
    rc = main()
    stop_lane_daemon()
    sys.exit(rc)
