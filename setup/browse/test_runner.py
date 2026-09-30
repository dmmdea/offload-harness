"""Unit tests for the pure helpers in runner.py. No network, no browser, no jev dependencies."""

import io
import json
import os
import shutil
import subprocess
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock

import runner


class DenyListTests(unittest.TestCase):
    def test_denied_words(self):
        for label in ("Publish", "Send test email", "Delete draft", "PLACE   ORDER", "Log out", "Confirm"):
            self.assertTrue(runner.DENY_PATTERN.search(label), label)

    def test_word_boundary(self):
        for label in ("Published posts", "Postal code", "Sender name", "Payment history", "Confirmation email"):
            self.assertFalse(runner.DENY_PATTERN.search(label), label)

    def test_allow_labels_only_attended(self):
        allow = ["Send test email"]
        self.assertFalse(runner.is_denied("Send test email", allow, unattended=False))
        self.assertFalse(runner.is_denied("  send TEST email ", allow, unattended=False))
        self.assertTrue(runner.is_denied("Send test email", allow, unattended=True))
        self.assertTrue(runner.is_denied("Send newsletter", allow, unattended=False))

    def test_allow_head_before_arrow(self):
        self.assertFalse(runner.is_denied("Publish → dialog", ["Publish"], unattended=False))
        self.assertTrue(runner.is_denied("Next → Send", ["Next"], unattended=False))

    def test_filter_actions(self):
        actions = [
            {"id": "a1", "kind": "click", "label": "Publish"},
            {"id": "a2", "kind": "click", "label": "Save draft"},
            {"id": "a3", "kind": "fill", "label": "Send to"},
            {"id": "a4", "kind": "select", "label": "Delete all"},
            {"id": "scroll_down", "kind": "scroll", "label": "Scroll down"},
            {"id": "wait", "kind": "wait", "label": "Wait for the page to update"},
        ]
        kept, removed = runner.filter_actions(actions, [], True)
        self.assertEqual([a["id"] for a in kept], ["a2", "a3", "scroll_down", "wait"])
        self.assertEqual(removed, ["Publish", "Delete all"])
        kept, removed = runner.filter_actions(actions, ["publish"], False)
        self.assertIn("a1", [a["id"] for a in kept])
        self.assertEqual(removed, ["Delete all"])


class HostTests(unittest.TestCase):
    def test_empty_allowlist_any_http(self):
        self.assertTrue(runner.host_allowed("https://example.org/x", []))
        self.assertTrue(runner.host_allowed("http://example.org", None))

    def test_subdomain_and_exact(self):
        allow = ["substack.com"]
        self.assertTrue(runner.host_allowed("https://readypep.substack.com/p/1", allow))
        self.assertTrue(runner.host_allowed("https://SUBSTACK.com/", allow))
        self.assertFalse(runner.host_allowed("https://evilsubstack.com/", allow))
        self.assertFalse(runner.host_allowed("https://substack.com.evil.test/", allow))
        self.assertFalse(runner.host_allowed("https://example.org/", allow))

    def test_schemes(self):
        for url in ("file:///etc/passwd", "chrome://settings", "javascript:alert(1)", "data:text/html,x", "", "https://"):
            self.assertFalse(runner.host_allowed(url, []), url)

    def test_blank_only_when_flagged(self):
        self.assertFalse(runner.host_allowed("about:blank", []))
        self.assertTrue(runner.host_allowed("about:blank", [], allow_blank=True))


class RedactionTests(unittest.TestCase):
    def test_redact(self):
        headers = {
            "Cookie": "a=b", "Set-Cookie": "c=d", "Authorization": "Bearer x", "Proxy-Authorization": "y",
            "X-CSRF-Token": "t", "X-Api-Key": "k", "X-Session-Id": "s", "X-Client-Secret": "z",
            "Content-Type": "application/json", "Accept": "*/*", "User-Agent": "ua",
        }
        self.assertEqual(
            runner.redact_headers(headers),
            {"Content-Type": "application/json", "Accept": "*/*", "User-Agent": "ua"},
        )


class DiscoveryTests(unittest.TestCase):
    def test_parse(self):
        self.assertEqual(runner.parse_devtools_active_port("9222\n/devtools/browser/abc-123\n"), (9222, "/devtools/browser/abc-123"))
        self.assertEqual(runner.parse_devtools_active_port("9222\r\n/devtools/browser/x\r\n"), (9222, "/devtools/browser/x"))
        for bad in ("", "9222", "abc\n/x", "9222\nnopath"):
            with self.assertRaises(ValueError):
                runner.parse_devtools_active_port(bad)

    def test_profile_dirs(self):
        env = {"LOCALAPPDATA": "LA", "HOME": "H"}
        self.assertEqual(runner.profile_dirs("chrome", "windows", env), [Path("LA/Google/Chrome/User Data")])
        self.assertEqual(runner.profile_dirs("edge", "windows", env), [Path("LA/Microsoft/Edge/User Data")])
        self.assertEqual(runner.profile_dirs("brave", "windows", env), [Path("LA/BraveSoftware/Brave-Browser/User Data")])
        self.assertEqual(runner.profile_dirs("chromium", "windows", env), [Path("LA/Chromium/User Data")])
        self.assertEqual(runner.profile_dirs("chrome", "darwin", env), [Path("H/Library/Application Support/Google/Chrome")])
        self.assertEqual(runner.profile_dirs("edge", "darwin", env), [Path("H/Library/Application Support/Microsoft Edge")])
        self.assertEqual(runner.profile_dirs("chrome", "linux", env), [Path("H/.config/google-chrome")])
        self.assertEqual(runner.profile_dirs("edge", "linux", env), [Path("H/.config/microsoft-edge")])
        self.assertEqual(runner.profile_dirs("brave", "linux", env), [Path("H/.config/BraveSoftware/Brave-Browser")])
        self.assertEqual(runner.profile_dirs("chromium", "linux", env), [Path("H/.config/chromium")])
        self.assertEqual(runner.profile_dirs("firefox", "linux", env), [])
        self.assertEqual(runner.profile_dirs("chrome", "windows", {}), [])

    def test_resolve(self):
        with tempfile.TemporaryDirectory() as tmp:
            env = {"LOCALAPPDATA": tmp}
            self.assertIsNone(runner.resolve_cdp_ws("chrome", "windows", env))
            data_dir = Path(tmp) / "Google" / "Chrome" / "User Data"
            data_dir.mkdir(parents=True)
            self.assertIsNone(runner.resolve_cdp_ws("chrome", "windows", env))
            (data_dir / "DevToolsActivePort").write_text("9333\n/devtools/browser/guid\n", encoding="utf-8")
            self.assertEqual(runner.resolve_cdp_ws("chrome", "windows", env), "ws://127.0.0.1:9333/devtools/browser/guid")
            (data_dir / "DevToolsActivePort").write_text("garbage", encoding="utf-8")
            self.assertIsNone(runner.resolve_cdp_ws("chrome", "windows", env))


class ProtocolTests(unittest.TestCase):
    def make(self, lines):
        out = io.StringIO()
        return runner.Protocol(io.StringIO("".join(json.dumps(x) + "\n" if not isinstance(x, str) else x for x in lines)), out), out

    def test_decide_pairing_with_interleaved_line(self):
        proto, out = self.make([
            {"type": "noise", "id": 1},
            "not json\n",
            {"type": "decision", "id": 99, "ok": True, "result": {"stale": True}},
            {"type": "decision", "id": 1, "ok": True, "result": {"answers": {}}},
        ])
        reply = proto.request("decide", {"body": {"q": 1}})
        self.assertEqual(reply["result"], {"answers": {}})
        sent = json.loads(out.getvalue().splitlines()[0])
        self.assertEqual(sent, {"type": "decide", "id": 1, "body": {"q": 1}})
        self.assertEqual(proto.counts["decide"], 1)

    def test_ids_increment_and_text(self):
        proto, out = self.make([
            {"type": "decision", "id": 1, "ok": True, "result": {}},
            {"type": "text_result", "id": 2, "ok": True, "text": "hello"},
        ])
        proto.request("decide", {"body": {}})
        self.assertEqual(proto.request("text", {"context": {"a": 1}})["text"], "hello")
        second = json.loads(out.getvalue().splitlines()[1])
        self.assertEqual((second["type"], second["id"]), ("text", 2))

    def test_error_reply(self):
        proto, _ = self.make([{"type": "decision", "id": 1, "ok": False, "error": "endpoint 500"}])
        with self.assertRaises(runner.ProtocolError) as ctx:
            proto.request("decide", {"body": {}})
        self.assertIn("endpoint 500", str(ctx.exception))
        self.assertEqual(ctx.exception.op, "decide")

    def test_eof(self):
        proto, _ = self.make([{"type": "other", "id": 1}])
        with self.assertRaises(runner.ProtocolError) as ctx:
            proto.request("text", {"context": {}})
        self.assertEqual(ctx.exception.op, "text")

    def test_wrong_reply_type_ignored(self):
        proto, _ = self.make([{"type": "text_result", "id": 1, "ok": True, "text": "x"}])
        with self.assertRaises(runner.ProtocolError):
            proto.request("decide", {"body": {}})


def ev(method, session="S1", **params):
    return {"method": method, "params": params, "session_id": session}


class CaptureTests(unittest.TestCase):
    def test_filter_redact_and_json_bodies(self):
        fetched = []

        def get_body(rid):
            fetched.append(rid)
            return "x" * 300000

        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "cap.jsonl")
            cap = runner.Capture(["https://api.example.org/"], path, session_id="S1", get_body=get_body)
            events = [
                ev("Network.requestWillBeSent", requestId="1", request={
                    "url": "https://api.example.org/v1/posts", "method": "POST",
                    "headers": {"Cookie": "a", "Authorization": "Bearer q", "Content-Type": "application/json"},
                    "postData": "p" * 70000}),
                ev("Network.responseReceived", requestId="1", response={
                    "status": 201, "mimeType": "application/json", "headers": {"Set-Cookie": "z", "Vary": "Origin"}}),
                ev("Network.loadingFinished", requestId="1"),
                ev("Network.requestWillBeSent", requestId="2", request={
                    "url": "https://api.example.org/logo.png", "method": "GET", "headers": {}}),
                ev("Network.responseReceived", requestId="2", response={"status": 200, "mimeType": "image/png", "headers": {}}),
                ev("Network.loadingFinished", requestId="2"),
                ev("Network.requestWillBeSent", requestId="3", request={
                    "url": "https://other.example.org/x", "method": "GET", "headers": {}}),
                ev("Network.requestWillBeSent", session="OTHER", requestId="4", request={
                    "url": "https://api.example.org/y", "method": "GET", "headers": {}}),
            ]
            for e in events:
                cap.feed(e)
            self.assertEqual(cap.write(), 2)
            self.assertEqual(cap.write(), 2)  # idempotent: nothing appended twice
            rows = [json.loads(line) for line in Path(path).read_text(encoding="utf-8").splitlines()]
        self.assertEqual(len(rows), 2)
        first, second = rows
        self.assertEqual(first["request_headers"], {"Content-Type": "application/json"})
        self.assertEqual(first["response_headers"], {"Vary": "Origin"})
        self.assertEqual(first["status"], 201)
        self.assertEqual(len(first["post_data"]), 65536)
        self.assertEqual(len(first["body"]), 262144)
        self.assertTrue(first["finished"])
        self.assertNotIn("body", second)
        self.assertEqual(fetched, ["1"])

    def test_body_error_is_tolerated(self):
        def boom(rid):
            raise RuntimeError("evicted")

        cap = runner.Capture(["https://a.example/"], "", get_body=boom)
        cap.feed(ev("Network.requestWillBeSent", requestId="1", request={"url": "https://a.example/x", "method": "GET", "headers": {}}))
        cap.feed(ev("Network.responseReceived", requestId="1", response={"status": 200, "mimeType": "application/json", "headers": {}}))
        cap.feed(ev("Network.loadingFinished", requestId="1"))
        self.assertNotIn("body", cap.records[0])
        self.assertEqual(cap.write(), 1)


class CdpPinTests(unittest.TestCase):
    def test_cdp_env_for_maps_http_and_ws(self):
        self.assertEqual(runner.cdp_env_for(""), {})
        self.assertEqual(runner.cdp_env_for(None), {})
        self.assertEqual(runner.cdp_env_for("http://127.0.0.1:9333/"), {"BU_CDP_URL": "http://127.0.0.1:9333"})
        self.assertEqual(runner.cdp_env_for("ws://127.0.0.1:9333/devtools/browser/x"),
                         {"BU_CDP_WS": "ws://127.0.0.1:9333/devtools/browser/x"})
        self.assertEqual(runner.cdp_env_for("WS://127.0.0.1:9333/devtools/browser/x"),
                         {"BU_CDP_WS": "WS://127.0.0.1:9333/devtools/browser/x"})
        self.assertEqual(runner.cdp_env_for("HTTP://127.0.0.1:9333"), {"BU_CDP_URL": "HTTP://127.0.0.1:9333"})

    def test_pin_wins_over_named_browser(self):
        # The named browser has no DevToolsActivePort here; consulting it would be an error.
        with tempfile.TemporaryDirectory() as tmp:
            env = {"LOCALAPPDATA": tmp}
            updates, err = runner.browser_env({"cdp_url": "http://127.0.0.1:9333", "browser": "brave"}, "windows", env)
            self.assertEqual((updates, err), ({"BU_CDP_URL": "http://127.0.0.1:9333"}, ""))
            updates, err = runner.browser_env({"browser": "brave"}, "windows", env)
            self.assertEqual(updates, {})
            self.assertIn("DevToolsActivePort", err)
            self.assertEqual(runner.browser_env({}, "windows", env), ({}, ""))


class SettleTests(unittest.TestCase):
    """The tab must not close while the page is still saving: Substack's editor autosaves
    2.3 s after the last keystroke, and a run that closed its tab at DONE lost the title."""

    def ev(self, method, rid, session="s1", type_="XHR"):
        params = {"requestId": rid}
        if method == "Network.requestWillBeSent":
            params["type"] = type_
        return {"method": method, "params": params, "session_id": session}

    def test_quiet_page_settles_after_the_quiet_window(self):
        st = runner.NetworkSettle("s1", started=0.0, quiet=3.5, cap=15)
        self.assertFalse(st.done(3.0))
        self.assertTrue(st.done(3.5))

    def test_an_autosave_holds_the_tab_open_until_it_finishes_and_goes_quiet(self):
        st = runner.NetworkSettle("s1", started=0.0, quiet=3.5, cap=15)
        st.feed(self.ev("Network.requestWillBeSent", "put-1"), now=2.3)   # the debounced autosave
        self.assertFalse(st.done(4.0), "a request in flight must hold the tab open")
        st.feed(self.ev("Network.loadingFinished", "put-1"), now=4.5)
        self.assertFalse(st.done(7.0))
        self.assertTrue(st.done(8.0))
        self.assertEqual(st.seen, 1)

    def test_the_cap_ends_a_page_that_never_goes_quiet(self):
        st = runner.NetworkSettle("s1", started=0.0, quiet=3.5, cap=15)
        st.feed(self.ev("Network.requestWillBeSent", "long-poll"), now=1.0)
        self.assertFalse(st.done(14.9))
        self.assertTrue(st.done(15.0))

    def test_other_tabs_and_non_xhr_traffic_are_ignored(self):
        st = runner.NetworkSettle("s1", started=0.0, quiet=3.5, cap=15)
        st.feed(self.ev("Network.requestWillBeSent", "x", session="other"), now=1.0)
        st.feed(self.ev("Network.requestWillBeSent", "img", type_="Image"), now=1.0)
        st.feed(self.ev("Network.loadingFinished", "unknown"), now=1.0)
        self.assertEqual(st.seen, 0)
        self.assertTrue(st.done(3.5))

    def test_settle_network_drains_feeds_capture_and_returns(self):
        clock = {"t": 0.0}
        batches = [[self.ev("Network.requestWillBeSent", "put-1")], [self.ev("Network.loadingFinished", "put-1")]]
        calls = []

        class Helpers:
            def drain_events(self):
                return batches.pop(0) if batches else []

            def cdp(self, method, session_id=None, **params):
                calls.append(method)
                return {}

        class Capture:
            def __init__(self):
                self.fed = 0
                self.session_id = None

            def feed(self, event):
                self.fed += 1

        run = types.SimpleNamespace(net_enabled=False, capture=Capture(), session_id=None)
        agent = types.SimpleNamespace(browser=types.SimpleNamespace(session="s1"))

        def sleep(seconds):
            clock["t"] += seconds

        st = runner.settle_network(run, agent, Helpers(), clock=lambda: clock["t"], sleep=sleep)
        self.assertIn("Network.enable", calls, "a run that never enabled Network must enable it to watch the save")
        self.assertIsNotNone(st)
        self.assertEqual(st.seen, 1)
        self.assertGreaterEqual(run.capture.fed, 1, "settle drains the shared buffer, so capture must still see the events")
        self.assertLess(clock["t"], 15)

    def test_settle_network_without_a_browser_is_a_no_op(self):
        run = types.SimpleNamespace(net_enabled=False, capture=None, session_id=None)
        self.assertIsNone(runner.settle_network(run, None, None))


class BackgroundTabAnimationTests(unittest.TestCase):
    """The lane's tab is hidden, so CSS animations never advance there and a fading-in dialog keeps
    opacity 0, which jev's snapshot drops. Every page read must be preceded by a finish; act must not
    (it would disturb jev's pre-click freshness check), and the observe that follows an action first lets
    the page settle (SettleAfterInputTests pins the settle itself). Fakes stand in for jev's Browser and its
    module-level browser_operation/cdp: no browser, no jev."""

    PAGE = {"url": "https://example.org/", "actions": [{"id": "e1", "kind": "click", "label": "Open menu", "node": 7}]}

    def make(self, finished=2, on_finish=None, stale_reads=0, capture=None, settle="record"):
        """Patch a fresh fake Browser through the real _install_patches; returns (browser, order, run, sessions).

        The settle after an input would really sleep, so runner.settle_after_input is replaced for the test:
        settle="record" notes "settle" in `order` and sends one probe call; settle="real" notes "settle" and
        runs the real function on a fake clock, so its reads and finishes show up in `order`."""
        order = []
        sessions = []
        stale = [True] * stale_reads

        class FakeStalePage(ValueError):
            pass

        def fake_cdp(method, session_id=None, **params):
            sessions.append(session_id)
            if "getAnimations" in params.get("expression", ""):
                order.append("finish")
                if on_finish is not None:
                    return on_finish()
                return {"result": {"type": "number", "value": finished}}
            if "__laneSettle" in params.get("expression", ""):
                order.append("mutations")
                return {"result": {"value": [1, 0]}}
            return {"result": {"value": []}}

        real_settle = runner.settle_after_input

        def record_settle(send):
            order.append("settle")
            send("Runtime.evaluate", expression="probe", returnByValue=True)

        def settle_on_a_fake_clock(send):
            order.append("settle")
            now = [0.0]

            def sleep(seconds):
                now[0] += seconds

            real_settle(send, clock=lambda: now[0], sleep=sleep)

        def fake_operation(request):  # jev's module-level browser_operation
            order.append(f"read:{request['operation']}")
            if request["operation"] == "act":
                return {"executed": request["action"]["id"]}
            if stale:
                stale.pop()
                raise FakeStalePage("Document is navigating")
            return {**BackgroundTabAnimationTests.PAGE,
                    "actions": [dict(a) for a in BackgroundTabAnimationTests.PAGE["actions"]]}

        class FakeBrowser:
            session = "s1"
            after_input = None

            def call(self, method, **params):
                order.append(f"call:{method}")
                return fake_cdp(method, session_id=self.session, **params)

            def evaluate(self, expression):  # like jev: a Runtime.evaluate through self.call
                order.append("evaluate")
                return self.call("Runtime.evaluate", expression=expression, returnByValue=True)["result"]["value"]

            def observe(self, screenshot=True):  # like jev: the post-action wait, then up to 10 reads
                order.append("observe")
                if self.after_input:
                    self.after_input = None
                    order.append("wait")
                for attempt in range(10):
                    try:
                        # resolved on the module at call time, exactly as jev's Browser.observe does
                        return browser_mod.browser_operation(
                            {"operation": "observe", "session": self.session, "screenshot": screenshot})
                    except FakeStalePage:
                        if attempt == 9:
                            raise
                raise FakeStalePage("Page did not settle")

            def act(self, action, page, text=None):
                order.append("act")
                result = browser_mod.browser_operation(
                    {"operation": "act", "session": self.session, "action": action, "text": text})
                self.after_input = action if action["kind"] != "wait" else None
                return result

        browser_mod = types.SimpleNamespace(
            Browser=FakeBrowser, StalePage=FakeStalePage, browser_operation=fake_operation, cdp=fake_cdp)
        model = types.SimpleNamespace()
        agent_mod = types.SimpleNamespace()
        run = runner.Run({"url": "https://example.org/", "unattended": True}, types.SimpleNamespace(send=lambda obj: None))
        run.capture = capture
        runner._install_patches(run, model, agent_mod, browser_mod)
        patcher = mock.patch.object(runner, "settle_after_input",
                                    side_effect=record_settle if settle == "record" else settle_on_a_fake_clock)
        patcher.start()
        self.addCleanup(patcher.stop)
        return FakeBrowser(), order, run, sessions

    def test_observe_finishes_animations_before_it_reads_the_page(self):
        browser, order, _, sessions = self.make()
        page = browser.observe(screenshot=False)
        self.assertEqual(order, ["observe", "finish", "read:observe"], "the finish must run right before the page is read")
        self.assertEqual(sessions, ["s1"], "the finish must run in the observed session")
        self.assertEqual([a["id"] for a in page["actions"]], ["e1"])

    def test_every_observe_finishes_again(self):
        # An action opens the next dialog, and the observe after it must see that one too.
        browser, order, _, _ = self.make()
        browser.observe()
        browser.act(self.PAGE["actions"][0], self.PAGE)
        browser.observe()
        self.assertEqual(order.count("finish"), 2)
        self.assertEqual([o for o in order if o in ("finish", "read:observe", "read:act")],
                         ["finish", "read:observe", "read:act", "finish", "read:observe"])

    def test_the_finish_runs_after_the_post_action_wait(self):
        # jev waits 50 ms (200 ms for a combobox) after an input before it reads the page; an
        # autocomplete option the page mounts in that window still has a pending fade-in. The settle
        # comes first, before jev's wait: the page gets its time, then jev's own wait and the read.
        browser, order, _, _ = self.make()
        browser.act(self.PAGE["actions"][0], self.PAGE)
        del order[:]
        browser.observe()
        self.assertEqual(order, ["settle", "observe", "wait", "finish", "read:observe"])
    def test_the_settle_runs_in_the_observed_session_and_the_page_still_comes_back(self):
        browser, order, _, sessions = self.make()
        browser.act(self.PAGE["actions"][0], self.PAGE)
        del order[:], sessions[:]
        page = browser.observe()
        self.assertEqual(sessions, ["s1", "s1"], "the settle (probe) and the finish both run in the observed session")
        self.assertEqual(page["url"], "https://example.org/")
        self.assertEqual([a["id"] for a in page["actions"]], ["e1"])

    def test_the_settle_runs_once_per_observe_after_an_action(self):
        browser, order, _, _ = self.make()
        browser.observe()
        self.assertEqual(order.count("settle"), 0, "nothing has run before the first observe")
        browser.act(self.PAGE["actions"][0], self.PAGE)
        browser.observe()
        browser.observe()
        self.assertEqual(order.count("settle"), 1, "after_input is consumed by the observe that follows the action")
        browser.act(self.PAGE["actions"][0], self.PAGE)
        browser.observe()
        self.assertEqual(order.count("settle"), 2)

    def test_a_wait_action_and_a_stale_retry_do_not_settle(self):
        # jev leaves after_input unset after a wait, and the retries of one observe are not new inputs.
        browser, order, _, _ = self.make()
        browser.act({"id": "wait", "kind": "wait", "label": "Wait for the page to update"}, self.PAGE)
        browser.observe()
        self.assertNotIn("settle", order)
        browser2, order2, _, _ = self.make(stale_reads=2)
        browser2.act(self.PAGE["actions"][0], self.PAGE)
        browser2.observe()
        self.assertEqual(order2.count("settle"), 1)
        self.assertEqual(order2.count("finish"), 3, "every retry still finishes")

    def test_the_real_settle_polls_in_the_observed_session_before_jevs_wait(self):
        browser, order, run, sessions = self.make(finished=0, settle="real")
        browser.act(self.PAGE["actions"][0], self.PAGE)
        del order[:], sessions[:]
        page = browser.observe()
        self.assertEqual(order, ["settle", "mutations"] + ["finish", "mutations"] * 3
                         + ["observe", "wait", "finish", "read:observe"])
        self.assertEqual(set(sessions), {"s1"})
        self.assertEqual(page["url"], "https://example.org/")
        self.assertTrue(run.observed_once)


    def test_a_stale_read_is_retried_with_a_fresh_finish(self):
        # A click that navigates: the retries re-read the new document, whose fade-ins are pending.
        browser, order, _, _ = self.make(stale_reads=2)
        page = browser.observe()
        self.assertEqual(order, ["observe"] + ["finish", "read:observe"] * 3)
        self.assertEqual(page["url"], "https://example.org/")

    def test_act_does_not_finish_animations(self):
        # fresh() inside the original act compares the observed page and the target's guard with
        # the live page; a finish between them can make that comparison fail (guard() is null at
        # opacity 0 and includes the scope's innerText, which honours visibility:hidden).
        browser, order, _, _ = self.make()
        browser.act({"id": "e1", "kind": "click", "label": "Open menu", "node": 7}, self.PAGE)
        browser.act({"id": "e2", "kind": "fill", "label": "Title", "node": 8}, self.PAGE, text="x")
        browser.act({"id": "wait", "kind": "wait", "label": "Wait for the page to update"}, self.PAGE)
        self.assertEqual(order.count("act"), 3)
        self.assertEqual(order.count("read:act"), 3, "act still reaches jev's browser_operation")
        self.assertNotIn("finish", order)
        self.assertNotIn("settle", order, "the settle belongs to the observe that follows an action")

    def test_the_finish_does_not_touch_the_runs_bookkeeping(self):
        capture = types.SimpleNamespace(session_id=None)
        browser, order, run, _ = self.make(capture=capture)
        browser.observe()
        self.assertEqual(run.executed, 0)
        self.assertFalse(run.net_enabled)
        self.assertNotIn("call:Network.enable", order)
        self.assertTrue(run.observed_once)
        # Prove the Network.enable wrapper is live, so the assertions above can fail: the first
        # Page.navigate enables the Network domain, and nothing else does.
        browser.call("Page.navigate", url="https://example.org/")
        self.assertTrue(run.net_enabled)
        self.assertEqual(capture.session_id, "s1")
        self.assertEqual(order.count("call:Network.enable"), 1)
        browser.observe()
        self.assertEqual(order.count("call:Network.enable"), 1)

    def test_a_failing_finish_is_swallowed_and_observe_still_returns_the_page(self):
        def stale():
            raise ValueError("Document changed during evaluation")

        def timed_out():
            raise RuntimeError("Runtime.evaluate timed out")

        failures = {
            "an exception": stale,
            "an IPC timeout": timed_out,
            "exceptionDetails": lambda: {"exceptionDetails": {"text": "boom"}, "result": {}},
            "no response": lambda: None,
            "a non-numeric value": lambda: {"result": {"value": "x"}},
        }
        for name, on_finish in failures.items():
            with self.subTest(name):
                browser, order, run, _ = self.make(on_finish=on_finish)
                page = browser.observe()
                self.assertEqual(order, ["observe", "finish", "read:observe"])
                self.assertEqual(page["url"], "https://example.org/")
                self.assertTrue(run.observed_once)

    def test_finish_animations_reports_the_count_and_never_raises(self):
        seen = []

        def send(method, **params):
            seen.append((method, params))
            return {"result": {"type": "number", "value": 3}}

        self.assertEqual(runner.finish_animations(send), 3)
        self.assertEqual(seen, [("Runtime.evaluate", {"expression": runner.FINISH_ANIMATIONS_JS, "returnByValue": True})])
        self.assertEqual(runner.finish_animations(lambda m, **p: {"result": {"value": 0}}), 0)
        self.assertEqual(runner.finish_animations(lambda m, **p: {"result": {"value": True}}), 0)
        self.assertEqual(runner.finish_animations(lambda m, **p: {"result": {"value": float("nan")}}), 0)

        def boom(method, **params):
            raise OSError("daemon gone")

        self.assertEqual(runner.finish_animations(boom), 0)

    def test_the_script_never_resets_an_animation(self):
        js = runner.FINISH_ANIMATIONS_JS
        self.assertIn("a.finish()", js)
        self.assertNotIn("cancel()", js)  # an end state, never a reset to the start state
        self.assertEqual(js.count("(() =>"), 1)

    # Runs the real script under node against fake animations: the structure of the loop (one try
    # per animation, which animations are skipped) cannot be pinned by reading the source text.
    NODE_HARNESS = r"""
const script = __SCRIPT__;
globalThis.innerWidth = 1120;
globalThis.innerHeight = 780;
const attempted = [], finished = [];
const target = (visible, [x, y, w, h]) => ({
  isConnected: true,
  checkVisibility: () => visible,
  getBoundingClientRect: () => ({left: x, top: y, right: x + w, bottom: y + h, width: w, height: h}),
});
const onScreen = [100, 100, 200, 40];
const anim = (name, o = {}) => {
  const {state = 'running', end = 300, tgt = target(false, onScreen), throws = false,
         noEffect = false, timingThrows = false} = o;
  const a = {
    playState: state,
    effect: noEffect ? null : {
      target: tgt,
      getComputedTiming() { if (timingThrows) throw new Error('timeline'); return {endTime: end}; },
    },
    finish() { attempted.push(name); if (throws) throw new Error('finish'); a.playState = 'finished'; finished.push(name); },
  };
  return a;
};
const list = [
  anim('throws', {throws: true}),                                    // finish() throws; the rest must still run
  anim('fade-in'),                                                   // hidden by opacity
  anim('slide-in', {tgt: target(true, [1120, 100, 300, 600])}),      // opaque, but starts beside the viewport
  anim('collapsed', {tgt: target(true, [100, 100, 200, 0])}),        // opaque, but no height yet
  anim('paused', {state: 'paused'}),
  anim('detached', {tgt: null}),
  anim('toast-exit', {tgt: target(true, onScreen)}),                 // already visible: left alone
  anim('spinner', {end: Infinity, throws: true}),                    // finish() would throw
  anim('done', {state: 'finished'}),
  anim('no-effect', {noEffect: true}),
  anim('scroll-timeline', {timingThrows: true}),
];
globalThis.document = {getAnimations: () => list};
const n = eval(script);
globalThis.document = {};
const missingApi = eval(script);
console.log(JSON.stringify({n, finished, attempted, missingApi}));
"""

    @unittest.skipUnless(shutil.which("node"), "node is not on PATH")
    def test_the_script_finishes_only_what_the_snapshot_cannot_see(self):
        program = self.NODE_HARNESS.replace("__SCRIPT__", json.dumps(runner.FINISH_ANIMATIONS_JS))
        done = subprocess.run([shutil.which("node"), "-"], input=program, capture_output=True,
                              text=True, encoding="utf-8", timeout=60)
        self.assertEqual(done.returncode, 0, done.stderr)
        out = json.loads(done.stdout)
        self.assertEqual(out["finished"], ["fade-in", "slide-in", "collapsed", "paused", "detached"])
        self.assertEqual(out["n"], 5, "the count is the animations that finished, not the ones that threw")
        self.assertEqual(out["attempted"], ["throws", "fade-in", "slide-in", "collapsed", "paused", "detached"],
                         "a throwing finish() is skipped on its own; a visible, infinite, finished, effect-less "
                         "or throwing-timing animation is never finished")
        self.assertEqual(out["missingApi"], 0)


class Runaway(BaseException):
    """Not an Exception, so the settle's own catch-all cannot swallow it: a loop with no stop must fail a test."""


class SettleAfterInputTests(unittest.TestCase):
    """settle_after_input waits for the page to go quiet after an input: a dialog opened from a menu item
    mounts on the page's own timer, well after jev's ~50 ms wait. Pure Python: a scripted page, a fake
    clock that only sleep advances, and a runaway guard so a broken stop rule fails instead of hanging."""

    MIN, QUIET, POLL, CAP = (runner.INPUT_SETTLE_MIN_S, runner.INPUT_SETTLE_QUIET_POLLS,
                             runner.INPUT_SETTLE_POLL_S, runner.INPUT_SETTLE_CAP_S)

    def setUp(self):
        patcher = mock.patch.object(runner, "log")  # the settle and the finish log to stderr
        self.log = patcher.start()
        self.addCleanup(patcher.stop)

    def settled_lines(self):
        return [c.args[0] for c in self.log.call_args_list if c.args[0].startswith("settled ")]

    @staticmethod
    def pick(script, i, default):
        """script is a list (the last value repeats), a function of the index, or None (the default)."""
        if script is None:
            return default
        if callable(script):
            return script(i)
        return script[min(i, len(script) - 1)] if script else default

    def drive(self, reads=None, finished=None, send=None):
        """Run the real settle against a scripted page. reads[i] is the i-th counter read (index 0 is the
        install read), finished[i] the i-th finish count. Returns a namespace: polls made, elapsed fake
        time, the calls in order, the number of finishes and the return value."""
        t = [0.0]
        calls, sleeps = [], [0]
        counters = {"read": 0, "finish": 0}

        def sleep(seconds):
            calls.append("sleep")
            sleeps[0] += 1
            if sleeps[0] > 100:  # the cap is 15 polls: a hundred means nothing stopped the loop
                raise Runaway("the settle never stopped")
            t[0] += seconds

        def fake_send(method, **params):
            expression = params.get("expression")
            if expression == runner.FINISH_ANIMATIONS_JS:
                i, counters["finish"] = counters["finish"], counters["finish"] + 1
                calls.append("finish")
                return {"result": {"value": self.pick(finished, i, 0)}}
            self.assertEqual(expression, runner.MUTATION_COUNTER_JS)
            i, counters["read"] = counters["read"], counters["read"] + 1
            calls.append("read")
            return {"result": {"value": self.pick(reads, i, [1, 0])}}

        result = runner.settle_after_input(send or fake_send, clock=lambda: t[0], sleep=sleep)
        return types.SimpleNamespace(result=result, polls=sleeps[0], elapsed=t[0], calls=calls,
                                     finishes=counters["finish"])

    def test_a_quiet_page_stops_after_the_minimum_not_before(self):
        # Two quiet polls are reached at 0.2 s, but the minimum is 0.3 s: the settle must still be running.
        out = self.drive(reads=[[1, 0]])
        self.assertIsNone(out.result)
        self.assertEqual(out.polls, 3)
        self.assertAlmostEqual(out.elapsed, self.MIN, places=6)
        self.assertGreaterEqual(out.elapsed, self.MIN - 1e-9)

    def test_it_stops_after_consecutive_quiet_polls_once_the_page_stopped_changing(self):
        # Mutations land in polls 1 and 3. The quiet poll between them must not end the settle, and neither
        # must the one after poll 3: two quiet polls in a row do, at poll 5.
        out = self.drive(reads=[[1, 0], [1, 4], [1, 4], [1, 9], [1, 9], [1, 9]])
        self.assertEqual(out.polls, 5)
        self.assertAlmostEqual(out.elapsed, 5 * self.POLL, places=6)

    def test_a_burst_after_a_quiet_start_holds_the_settle_until_it_is_quiet(self):
        # The dialog mounts in poll 3 (0.3 s): the page was quiet for two polls, yet the settle goes on.
        out = self.drive(reads=[[1, 0], [1, 0], [1, 0], [1, 12]])
        self.assertEqual(out.polls, 5)

    def test_an_animation_finished_in_a_poll_is_activity(self):
        # No DOM mutation at all, but one animation finished in poll 2: that poll is not quiet.
        out = self.drive(reads=[[1, 0]], finished=[0, 1, 0])
        self.assertEqual(out.polls, 4)

    def test_every_poll_finishes_animations_and_reads_the_counter_after_sleeping(self):
        out = self.drive(reads=[[1, 0]])
        self.assertEqual(out.calls, ["read"] + ["sleep", "finish", "read"] * out.polls)
        self.assertEqual(out.finishes, out.polls)

    def test_a_navigation_is_a_change_even_when_the_count_matches(self):
        # A new document has no observer: its id differs and its count restarts (here at the same value).
        out = self.drive(reads=[[1, 0], [1, 0], [2, 0]])
        self.assertEqual(out.polls, 4, "the id change in poll 2 restarts the quiet window")
        out = self.drive(reads=[[1, 40], [1, 40], [2, 3]])  # and a count that went down
        self.assertEqual(out.polls, 4)

    def test_it_never_runs_past_the_cap_on_a_page_that_keeps_changing(self):
        pages = {
            "a DOM that keeps mutating": {"reads": lambda i: [1, i]},
            "an animation starting every poll": {"finished": lambda i: 1},
        }
        for name, page in pages.items():
            with self.subTest(name):
                out = self.drive(**page)
                self.assertLessEqual(out.polls, round(self.CAP / self.POLL) + 1)
                self.assertGreaterEqual(out.elapsed, self.CAP - 1e-6)
                self.assertLessEqual(out.elapsed, self.CAP + self.POLL + 1e-6, "the cap, plus at most the poll in flight")

    def test_an_unreadable_page_counts_as_a_change_and_is_bounded(self):
        def raises(exc):
            def fail():
                raise exc
            return fail

        failures = {
            "an exception": raises(RuntimeError("Cannot find context with specified id")),
            "an IPC timeout": raises(TimeoutError("Runtime.evaluate timed out")),
            "exceptionDetails": lambda: {"exceptionDetails": {"text": "boom"}, "result": {}},
            "no response": lambda: None,
            "a bare number": lambda: {"result": {"value": 7}},
            "a bool pair": lambda: {"result": {"value": [True, False]}},
            "a short list": lambda: {"result": {"value": [1]}},
        }
        for name, fail in failures.items():
            with self.subTest(name):
                def send(method, fail=fail, **params):
                    if params.get("expression") == runner.FINISH_ANIMATIONS_JS:
                        return {"result": {"value": 0}}
                    return fail()

                out = self.drive(send=send)
                self.assertIsNone(out.result)
                self.assertGreaterEqual(out.elapsed, self.CAP - 1e-6, "an unreadable page is never quiet")
                self.assertLessEqual(out.elapsed, self.CAP + self.POLL + 1e-6)

    def test_it_never_raises(self):
        def dead(method, **params):
            raise OSError("daemon gone")

        self.assertIsNone(self.drive(send=dead).result)

        def sleep_boom(seconds):
            raise RuntimeError("sleep failed")

        def clock_boom():
            raise RuntimeError("clock failed")

        def quiet(method, **params):
            return {"result": {"value": 0 if params["expression"] == runner.FINISH_ANIMATIONS_JS else [1, 0]}}

        self.log.reset_mock()
        self.assertIsNone(runner.settle_after_input(quiet, clock=lambda: 0.0, sleep=sleep_boom))
        self.assertIsNone(runner.settle_after_input(quiet, clock=clock_boom, sleep=lambda s: None))
        skipped = [c.args[0] for c in self.log.call_args_list if c.args[0].startswith("settle after input skipped")]
        self.assertEqual(len(skipped), 2, "a skipped settle says so on stderr")

    def test_it_logs_only_when_the_page_was_active(self):
        self.drive(reads=[[1, 0]])
        self.assertEqual(self.settled_lines(), [], "a page that was quiet from the start is not worth a line")
        self.drive(reads=[[1, 0], [1, 5]], finished=[0, 2, 0])
        lines = self.settled_lines()
        self.assertEqual(len(lines), 1)
        self.assertIn("2 animation(s) finished", lines[0])

    def test_the_constants_are_ordered_and_leave_the_network_settle_alone(self):
        self.assertLess(self.POLL, self.MIN)
        self.assertLess(self.MIN, self.CAP)
        self.assertGreaterEqual(self.QUIET, 2, "one quiet poll is not enough: a page is quiet between bursts")
        self.assertLess(self.CAP, 3.0, "the settle runs after every action, so it must stay cheap")
        self.assertEqual(runner.SETTLE_POLL_S, 0.2, "the network settle's own poll interval is untouched")

    def test_read_mutations_parses_the_observer_id_and_count(self):
        def ok(value):
            return lambda method, **params: {"result": {"type": "object", "value": value}}

        self.assertEqual(runner.read_mutations(ok([0.42, 7])), (0.42, 7))
        self.assertEqual(runner.read_mutations(ok([3, 0])), (3, 0))
        for bad in (None, 5, "1:2", [1], [1, 2, 3], ["a", 1], [True, 1], [None, 1], {"id": 1}):
            self.assertIsNone(runner.read_mutations(ok(bad)), repr(bad))

    NODE_HARNESS = r"""
const script = __SCRIPT__;
globalThis.window = globalThis;
const observers = [];
globalThis.MutationObserver = class {
  constructor(cb) { this.cb = cb; observers.push(this); }
  observe(target, options) { this.target = target; this.options = options; }
};
globalThis.document = {documentElement: {name: 'html'}};
const first = eval(script);
const second = eval(script);
const installedAfterTwoReads = observers.length;
observers[0].cb([{}, {}, {}]);          // three mutation records arrive
const third = eval(script);
delete globalThis.__laneSettle;         // a navigation: the new document has no state
const fourth = eval(script);
delete globalThis.__laneSettle;         // a document that has no root element yet
globalThis.document = {documentElement: null};
const early = eval(script);
globalThis.document = {documentElement: {name: 'html'}};
const later = eval(script);
delete globalThis.__laneSettle;         // a page without MutationObserver
delete globalThis.MutationObserver;
const missing = eval(script);
console.log(JSON.stringify({first, second, third, fourth, early, later, missing, installedAfterTwoReads,
  observers: observers.length, options: observers[0].options, target: observers[0].target}));
"""

    @unittest.skipUnless(shutil.which("node"), "node is not on PATH")
    def test_the_counter_script_counts_mutations_and_survives_navigations(self):
        program = self.NODE_HARNESS.replace("__SCRIPT__", json.dumps(runner.MUTATION_COUNTER_JS))
        done = subprocess.run([shutil.which("node"), "-"], input=program, capture_output=True,
                              text=True, encoding="utf-8", timeout=60)
        self.assertEqual(done.returncode, 0, done.stderr)
        out = json.loads(done.stdout)
        self.assertEqual(out["first"][1], 0)
        self.assertEqual(out["second"], out["first"], "the same document reads the same id and count")
        self.assertEqual(out["installedAfterTwoReads"], 1, "one observer per document, however often it is read")
        self.assertEqual(out["third"], [out["first"][0], 3], "the count is the mutation records seen")
        self.assertNotEqual(out["fourth"][0], out["first"][0], "a new document gets a new id")
        self.assertEqual(out["fourth"][1], 0)
        self.assertEqual(out["early"][1], 0, "no root element yet: nothing observed, nothing thrown")
        self.assertEqual(out["later"][0], out["early"][0], "the same document installs when its root exists")
        self.assertEqual(out["observers"], 3)
        self.assertEqual(out["missing"][1], 0, "no MutationObserver: a constant count, nothing thrown")
        self.assertEqual(out["options"], {"subtree": True, "childList": True, "attributes": True, "characterData": True})
        self.assertEqual(out["target"], {"name": "html"})


class ReviewHardeningTests(unittest.TestCase):
    """Review findings 2026-09-28: capture boundaries and redaction, click targets, output encoding."""

    def test_prefix_matches_on_a_boundary_only(self):
        self.assertTrue(runner.prefix_matches("https://api.example.com/v1/x", "https://api.example.com"))
        self.assertTrue(runner.prefix_matches("https://api.example.com", "https://api.example.com"))
        self.assertTrue(runner.prefix_matches("https://api.example.com/v1/x", "https://api.example.com/v1/"))
        self.assertFalse(runner.prefix_matches("https://api.example.com.evil.net/x", "https://api.example.com"))
        self.assertFalse(runner.prefix_matches("https://api.example.com/v10", "https://api.example.com/v1"))

    def test_redact_url_hides_sensitive_query_values(self):
        out = runner.redact_url("https://x.example/cb?code=abc123&state=ok&access_token=tok&page=2")
        self.assertNotIn("abc123", out)
        self.assertNotIn("tok", out.replace("access_token", ""))
        self.assertIn("state=ok", out)
        self.assertIn("page=2", out)

    def test_redact_payload_json_and_form(self):
        body = json.dumps({"user": {"email": "a@b.example", "password": "hunter2", "api_key": "k"}, "items": [{"session_id": "s"}]})
        out = runner.redact_payload(body)
        self.assertNotIn("hunter2", out)
        self.assertNotIn('"k"', out)
        self.assertNotIn('"s"', out)
        self.assertIn("a@b.example", out)
        form = runner.redact_payload("username=me&password=hunter2&remember=1")
        self.assertNotIn("hunter2", form)
        self.assertIn("remember=1", form)
        self.assertEqual(runner.redact_payload("plain text body"), "plain text body")

    def test_capture_redacts_url_post_and_body(self):
        cap = runner.Capture(["https://api.example.com/"], "", get_body=lambda rid: '{"token":"t","ok":true}')
        ev = lambda method, **p: {"method": method, "params": p, "session_id": None}
        cap.feed(ev("Network.requestWillBeSent", requestId="1",
                    request={"url": "https://api.example.com/login?sig=zzz", "method": "POST", "headers": {},
                             "postData": '{"password":"hunter2"}'}))
        cap.feed(ev("Network.responseReceived", requestId="1", response={"status": 200, "mimeType": "application/json", "headers": {}}))
        cap.feed(ev("Network.loadingFinished", requestId="1"))
        rec = json.dumps(cap.records[0])
        for secret in ("zzz", "hunter2", '"t"'):
            self.assertNotIn(secret, rec)

    def test_off_list_targets_ignores_non_navigations(self):
        hosts = ["example.com"]
        self.assertEqual(runner.off_list_targets(["javascript:void(0)", "#top", "mailto:x@y.example"], hosts), [])
        self.assertEqual(runner.off_list_targets(["https://app.example.com/a"], hosts), [])
        self.assertEqual(runner.off_list_targets(["https://evil.test/oauth"], hosts), ["https://evil.test/oauth"])

    def test_protocol_survives_a_lone_surrogate(self):
        out = io.StringIO()
        runner.Protocol(io.StringIO(""), out).send({"type": "step", "label": "half \ud83d pair"})
        line = out.getvalue()
        line.encode("utf-8")  # must not raise: the surrogate is escaped
        self.assertIn("\\ud83d", line)


if __name__ == "__main__":
    unittest.main()
