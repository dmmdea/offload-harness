"""Unit tests for the pure helpers in runner.py. No network, no browser, no jev dependencies."""

import io
import json
import os
import tempfile
import unittest
from pathlib import Path

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
