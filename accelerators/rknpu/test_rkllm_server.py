#!/usr/bin/env python3
"""Unit tests for rkllm_server.py against a fake runtime: no NPU, no vendor library, runnable on Windows and Linux.

Pins what llama-swap and the harness's OpenAI client depend on: prompt rendering, request validation, the fields
that are ignored, SSE framing, stop strings, finish_reason, the single-flight lock, the loopback refusal and the
runtime callback's state machine. numpy and Pillow are only needed by the image-preprocessing tests, which skip
without them.  Run: python -m unittest test_rkllm_server   (from this directory)
"""
from __future__ import annotations

import base64
import contextlib
import ctypes
import http.client
import io
import json
import os
import re
import socket
import sys
import threading
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import rkllm_server as rs  # noqa: E402

try:
    import numpy as np
    from PIL import Image

    HAVE_IMAGING = True
except ImportError:  # the preprocessing tests skip
    HAVE_IMAGING = False

PERF = rs.Perf(prefill_ms=130.0, prefill_tokens=13, generate_ms=120.0, generate_tokens=2, memory_mb=1200.0)
PNG_URI = "data:image/png;base64," + base64.b64encode(b"\x89PNG fake").decode()


class FakeRuntime:
    """Stands in for RKLLMRuntime: replays a script through on_text the way the C callback does."""

    def __init__(self, script=("Hel", "lo"), eos=True, rc=0, error="", perf=PERF, gate=None, delay=0.0):
        self.script, self.eos, self.rc, self.error, self.perf = list(script), eos, rc, error, perf
        self.gate, self.delay = gate, delay  # gate: an Event generate() waits on before it starts
        self.calls, self.stops, self.aborts, self.closed = [], 0, 0, False
        self.active = self.max_active = 0
        self._mu = threading.Lock()

    def generate(self, prompt, images, sampling, max_new, on_text):
        with self._mu:
            self.active += 1
            self.max_active = max(self.max_active, self.active)
            self.calls.append({"prompt": prompt, "images": images, "sampling": sampling, "max_new": max_new})
        try:
            if self.gate is not None:
                self.gate.wait(10)
            if self.rc != 0:
                return rs.Run(self.rc, 0, False, "", None)
            tokens = 0
            for chunk in self.script:
                if tokens >= max_new:
                    break
                tokens += 1
                if self.delay:
                    time.sleep(self.delay)
                if not on_text(chunk):
                    self.stops += 1
                    return rs.Run(0, tokens, False, "", self.perf)
            eos = self.eos and tokens < max_new
            return rs.Run(0, tokens + (1 if eos else 0), eos, self.error, self.perf)
        finally:
            with self._mu:
                self.active -= 1

    def abort(self):
        self.aborts += 1

    def close(self):
        self.closed = True


class FakeVision:
    tokens, width, height = 4, 8, 8

    def __init__(self):
        self.encoded = []

    def encode(self, data):
        self.encoded.append(data)
        return [len(data)]

    def close(self):
        pass


class ServerCase(unittest.TestCase):
    """An in-process server on an ephemeral loopback port, bound to a FakeRuntime."""

    ctx = 4096
    with_vision = False
    ready = True

    def setUp(self):
        quiet = contextlib.redirect_stderr(io.StringIO())  # the server logs one line per chat request
        quiet.__enter__()
        self.addCleanup(quiet.__exit__, None, None, None)
        self.rt = FakeRuntime()
        self.vision = FakeVision() if self.with_vision else None
        self.start()

    def start(self):
        self.app = rs.App("test-seat", self.ctx)
        self.app.runtime, self.app.vision = self.rt, self.vision
        if self.ready:
            self.app.ready.set()
        self.srv = rs.make_server(self.app, "127.0.0.1", 0)
        self.port = self.srv.server_address[1]
        threading.Thread(target=self.srv.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True).start()
        self.addCleanup(self.stop)

    def stop(self):
        self.srv.shutdown()
        self.srv.server_close()

    def request(self, method, path, body=None, raw=None):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=15)
        data = raw if raw is not None else None if body is None else json.dumps(body).encode()
        conn.request(method, path, body=data, headers={"Content-Type": "application/json"})
        resp = conn.getresponse()
        payload = resp.read()
        conn.close()
        return resp.status, resp, payload

    def chat(self, **fields):
        body = {"messages": [{"role": "user", "content": "hi"}], **fields}
        status, resp, payload = self.request("POST", "/v1/chat/completions", body)
        return status, json.loads(payload), resp

    def sse(self, **fields):
        """POST with stream=true; returns (status, content-type, decoded frames, raw text)."""
        body = {"messages": [{"role": "user", "content": "hi"}], "stream": True, **fields}
        status, resp, payload = self.request("POST", "/v1/chat/completions", body)
        text = payload.decode()
        frames = [f[len("data: "):] for f in text.split("\n\n") if f.startswith("data: ")]
        return status, resp.getheader("Content-Type"), [json.loads(f) for f in frames if f != "[DONE]"], text


class RenderPromptTest(unittest.TestCase):
    def test_system_and_user_with_thinking_off(self):
        prompt, images = rs.render_prompt([{"role": "system", "content": "  Be brief. "},
                                           {"role": "user", "content": "hi"}], False)
        self.assertEqual(prompt, "<|im_start|>system\nBe brief.<|im_end|>\n<|im_start|>user\nhi<|im_end|>\n"
                                 "<|im_start|>assistant\n<think>\n\n</think>\n\n")
        self.assertEqual(images, [])

    def test_thinking_on_ends_inside_the_think_block(self):
        prompt, _ = rs.render_prompt([{"role": "user", "content": "hi"}], True)
        self.assertTrue(prompt.endswith("<|im_start|>assistant\n<think>\n"))

    def test_earlier_assistant_turns_lose_their_reasoning(self):
        prompt, _ = rs.render_prompt([
            {"role": "user", "content": "q1"},
            {"role": "assistant", "content": "<think>\nplan\n</think>\n\nA1"},
            {"role": "user", "content": "q2"}], False)
        self.assertIn("<|im_start|>assistant\nA1<|im_end|>\n", prompt)
        self.assertNotIn("plan", prompt)

    def test_assistant_turn_after_the_last_user_turn_keeps_its_reasoning(self):
        prompt, _ = rs.render_prompt([
            {"role": "user", "content": "q"},
            {"role": "assistant", "content": "A", "reasoning_content": " plan "}], False)
        self.assertIn("<|im_start|>assistant\n<think>\nplan\n</think>\n\nA<|im_end|>\n", prompt)

    def test_developer_is_a_system_message(self):
        prompt, _ = rs.render_prompt([{"role": "developer", "content": "rules"}, {"role": "user", "content": "q"}], False)
        self.assertTrue(prompt.startswith("<|im_start|>system\nrules<|im_end|>\n"))

    def test_image_parts_keep_their_position(self):
        prompt, images = rs.render_prompt([{"role": "user", "content": [
            {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}},
            {"type": "text", "text": "What is this?"},
            {"type": "image_url", "image_url": "data:image/png;base64,BBBB"}]}], False)
        self.assertIn("<|im_start|>user\n<image>What is this?<image><|im_end|>\n", prompt)
        self.assertEqual(images, ["data:image/png;base64,AAAA", "data:image/png;base64,BBBB"])

    def test_refusals(self):
        user = {"role": "user", "content": "q"}
        for name, messages in {
            "empty": [],
            "not a list": "hi",
            "no user turn": [{"role": "system", "content": "s"}],
            "system not first": [user, {"role": "system", "content": "s"}],
            "tool role": [user, {"role": "tool", "content": "x"}],
            "tool calls": [user, {"role": "assistant", "content": "", "tool_calls": [{"id": "1"}]}],
            "image in system": [{"role": "system", "content": [{"type": "image_url", "image_url": {"url": "x"}}]}, user],
            "unknown part": [{"role": "user", "content": [{"type": "audio"}]}],
            "content not text": [{"role": "user", "content": 7}],
        }.items():
            with self.subTest(name), self.assertRaises(rs.ApiError) as cm:
                rs.render_prompt(messages, False)
            self.assertEqual(cm.exception.status, 400)


class BuildRequestTest(unittest.TestCase):
    def build(self, ctx=4096, vision=False, **fields):
        return rs.build_request({"messages": [{"role": "user", "content": "hi"}], **fields}, ctx, vision)

    def test_defaults(self):
        r = self.build()
        self.assertEqual(r.sampling, rs.Sampling(40, 0.95, 0.8, 1.0, 0.0, 0.0))
        self.assertEqual((r.max_tokens, r.stream, r.include_usage, r.thinking, r.stops, r.images),
                         (4096, False, False, False, [], []))

    def test_max_tokens_is_capped_at_the_window_and_has_aliases(self):
        self.assertEqual(self.build(max_tokens=64).max_tokens, 64)
        self.assertEqual(self.build(max_tokens=10 ** 6).max_tokens, 4096)
        self.assertEqual(self.build(max_completion_tokens=7).max_tokens, 7)
        self.assertEqual(self.build(max_tokens=-1).max_tokens, 4096)  # llama.cpp's "until done"
        self.assertEqual(self.build(max_tokens=64.0).max_tokens, 64)  # 40 serialised as 40.0 by some clients
        for bad in (0, -2, 1.5, "8", True):
            with self.subTest(bad), self.assertRaises(rs.ApiError):
                self.build(max_tokens=bad)

    def test_temperature_zero_is_greedy(self):
        s = self.build(temperature=0, top_k=50, top_p=0.5).sampling
        self.assertEqual((s.top_k, s.top_p, s.temperature), (1, 1.0, 1.0))

    def test_sampling_knobs_and_the_vllm_spelling(self):
        s = self.build(temperature=0.3, top_p=0.5, top_k=20, repetition_penalty=1.2, presence_penalty=0.5,
                       frequency_penalty=-0.5).sampling
        self.assertEqual(s, rs.Sampling(20, 0.5, 0.3, 1.2, -0.5, 0.5))
        self.assertEqual(self.build(repeat_penalty=1.3).sampling.repeat_penalty, 1.3)
        self.assertEqual(self.build(top_k=0).sampling.top_k, 40)  # 0 = off in llama.cpp; the runtime has no such value
        self.assertEqual(self.build(top_k=-5).sampling.top_k, 40)
        self.assertEqual(self.build(top_k=2 ** 32).sampling.top_k, 2 ** 31 - 1)  # the runtime's field is an int32
        self.assertEqual(self.build(top_k=2 ** 31 - 1).sampling.top_k, 2 ** 31 - 1)
        for bad in ({"temperature": -1}, {"temperature": "hot"}, {"top_p": 2}, {"top_k": 1.5}, {"presence_penalty": 9}):
            with self.subTest(bad), self.assertRaises(rs.ApiError):
                self.build(**bad)

    def test_fields_the_seat_ignores_never_fail(self):
        r = self.build(grammar="", response_format={"type": "text"}, logprobs=True, top_logprobs=3,
                       cache_prompt=True, seed=7, n=2, user="u", tool_choice="auto", tools=[],
                       chat_template_kwargs={"preserve_thinking": True}, stream_options={"include_usage": False})
        self.assertFalse(r.thinking)
        self.assertFalse(r.include_usage)

    def test_constrained_decoding_is_refused(self):
        for name, fields in (("grammar", {"grammar": "root ::= x"}),
                             ("json_object", {"response_format": {"type": "json_object"}}),
                             ("json_schema", {"response_format": {"type": "json_schema", "json_schema": {"name": "x"}}}),
                             ("structured_outputs", {"structured_outputs": {"json": {"type": "object"}}})):
            with self.subTest(name), self.assertRaises(rs.ApiError) as cm:
                self.build(**fields)
            self.assertEqual((cm.exception.status, cm.exception.body()["error"]["code"]),
                             (400, "constrained_decoding_unsupported"))

    def test_enable_thinking_must_be_a_real_true(self):
        self.assertTrue(self.build(chat_template_kwargs={"enable_thinking": True}).thinking)
        self.assertFalse(self.build(chat_template_kwargs={"enable_thinking": "true"}).thinking)
        self.assertFalse(self.build(chat_template_kwargs={"enable_thinking": False}).thinking)

    def test_tools_are_refused(self):
        with self.assertRaises(rs.ApiError) as cm:
            self.build(tools=[{"type": "function", "function": {"name": "f"}}])
        self.assertEqual(cm.exception.status, 400)

    def test_stop_forms(self):
        self.assertEqual(self.build(stop="END").stops, ["END"])
        self.assertEqual(self.build(stop=["a", "", "b"]).stops, ["a", "b"])
        with self.assertRaises(rs.ApiError):
            self.build(stop=[1])

    def test_stream_flags(self):
        r = self.build(stream=True, stream_options={"include_usage": True})
        self.assertTrue(r.stream and r.include_usage)

    def test_images(self):
        msg = [{"role": "user", "content": [{"type": "text", "text": "what?"},
                                            {"type": "image_url", "image_url": {"url": PNG_URI}}]}]
        with self.assertRaises(rs.ApiError) as cm:  # no encoder on this seat
            rs.build_request({"messages": msg}, 4096, False)
        self.assertIn("--vision-encoder", cm.exception.message)
        r = rs.build_request({"messages": msg}, 4096, True)
        self.assertEqual(r.images, [b"\x89PNG fake"])
        self.assertIn("what?<image>", r.prompt)

    def test_image_urls_that_are_not_base64_data_uris_are_refused(self):
        for url in ("https://example.com/a.png", "file:///etc/passwd", "data:text/plain;base64,QUJD",
                    "data:image/png,rawbytes", "data:image/gif;base64,QUJD", "data:image/png;base64,@@@"):
            msg = [{"role": "user", "content": [{"type": "image_url", "image_url": {"url": url}}]}]
            with self.subTest(url), self.assertRaises(rs.ApiError) as cm:
                rs.build_request({"messages": msg}, 4096, True)
            self.assertEqual(cm.exception.status, 400)

    def test_the_image_placeholder_is_reserved_only_when_images_are_attached(self):
        text = {"type": "text", "text": "say <image> back"}
        img = {"type": "image_url", "image_url": {"url": PNG_URI}}
        with self.assertRaises(rs.ApiError):  # the runtime would pair the literal with an embedding that is not there
            rs.build_request({"messages": [{"role": "user", "content": [text, img]}]}, 4096, True)
        r = rs.build_request({"messages": [{"role": "user", "content": [text]}]}, 4096, True)  # plain text otherwise
        self.assertIn("say <image> back", r.prompt)


class EmitterTest(unittest.TestCase):
    def run_all(self, emitter, chunks):
        out = []
        for c in chunks:
            out += emitter.feed(c)
            if emitter.stopped:
                break
        return out + ([] if emitter.stopped else emitter.flush())

    def joined(self, pieces, field="content"):
        return "".join(t for f, t in pieces if f == field)

    def test_stop_string_split_across_chunks(self):
        e = rs.Emitter(["END"], False)
        pieces = self.run_all(e, ["Hello E", "N", "D world"])
        self.assertEqual(self.joined(pieces), "Hello ")
        self.assertTrue(e.stopped)

    def test_a_partial_stop_string_is_held_back_and_released_at_the_end(self):
        e = rs.Emitter(["</s>"], False)
        self.assertEqual(e.feed("abc<"), [("content", "abc")])  # "<" could still become "</s>"
        self.assertEqual(e.feed("/x"), [("content", "</x")])  # it did not
        e = rs.Emitter(["</s>"], False)
        e.feed("abc</")
        self.assertEqual(e.flush(), [("content", "</")])  # the run ended: it never was a stop string

    def test_the_earliest_of_several_stops_wins(self):
        e = rs.Emitter(["B", "A"], False)
        self.assertEqual(self.joined(self.run_all(e, ["xxAyyB"])), "xx")

    def test_think_block_is_split_off(self):
        e = rs.Emitter([], True)
        pieces = self.run_all(e, ["I should ", "think", ".\n</th", "ink>", "\n\n", "The ", "answer"])
        self.assertEqual(self.joined(pieces, "reasoning_content"), "I should think.\n")
        self.assertEqual(self.joined(pieces), "The answer")  # the newlines after </think> are dropped

    def test_a_think_block_that_never_closes_is_all_reasoning(self):
        e = rs.Emitter([], True)
        pieces = self.run_all(e, ["still thi", "nking </thin"])
        self.assertEqual(self.joined(pieces, "reasoning_content"), "still thinking </thin")
        self.assertEqual(self.joined(pieces), "")

    def test_with_thinking_off_nothing_is_split(self):
        e = rs.Emitter([], False)
        self.assertEqual(self.joined(self.run_all(e, ["a</think>b"])), "a</think>b")


class HealthAndModelsTest(ServerCase):
    def test_models_list(self):
        status, _, payload = self.request("GET", "/v1/models")
        data = json.loads(payload)
        self.assertEqual(status, 200)
        self.assertEqual(data["object"], "list")
        self.assertEqual((data["data"][0]["id"], data["data"][0]["max_model_len"]), ("test-seat", 4096))

    def test_health_ok(self):
        status, _, payload = self.request("GET", "/health")
        self.assertEqual((status, json.loads(payload)), (200, {"status": "ok"}))

    def test_unknown_paths_are_404_with_an_openai_error(self):
        for method, path in (("GET", "/nope"), ("POST", "/v1/completions"), ("GET", "/v1/chat/completions")):
            status, _, payload = self.request(method, path, {} if method == "POST" else None)
            self.assertEqual(status, 404, path)
            self.assertIn("message", json.loads(payload)["error"])


class LoadingTest(ServerCase):
    ready = False

    def test_health_is_503_until_the_runtime_is_up_and_chat_waits_out(self):
        status, _, payload = self.request("GET", "/health")
        self.assertEqual((status, json.loads(payload)), (503, {"status": "loading"}))
        status, body, _ = self.chat()
        self.assertEqual(status, 503)
        self.assertEqual(body["error"]["type"], "server_error")
        self.app.ready.set()
        self.assertEqual(self.request("GET", "/health")[0], 200)
        self.assertEqual(self.chat()[0], 200)


class ChatTest(ServerCase):
    def test_answer_shape_usage_and_timings(self):
        status, body, resp = self.chat()
        self.assertEqual(status, 200)
        self.assertTrue(resp.getheader("Content-Type").startswith("application/json"))
        self.assertEqual(body["object"], "chat.completion")
        self.assertEqual(body["model"], "test-seat")
        choice = body["choices"][0]
        self.assertEqual((choice["message"], choice["finish_reason"]),
                         ({"role": "assistant", "content": "Hello"}, "stop"))
        self.assertEqual(body["usage"], {"prompt_tokens": 13, "completion_tokens": 3, "total_tokens": 16})  # 2 + stop token
        t = body["timings"]
        self.assertEqual((t["prompt_n"], t["prompt_ms"], t["predicted_n"], t["predicted_ms"]), (13, 130.0, 2, 120.0))
        self.assertAlmostEqual(t["predicted_per_second"], 16.67, places=2)
        self.assertAlmostEqual(t["prompt_per_second"], 100.0, places=2)

    def test_the_runtime_gets_the_rendered_prompt_and_sampling(self):
        self.chat(messages=[{"role": "system", "content": "s"}, {"role": "user", "content": "q"}],
                  temperature=0, max_tokens=9)
        call = self.rt.calls[0]
        self.assertEqual(call["prompt"], "<|im_start|>system\ns<|im_end|>\n<|im_start|>user\nq<|im_end|>\n"
                                         "<|im_start|>assistant\n<think>\n\n</think>\n\n")
        self.assertEqual((call["sampling"].top_k, call["max_new"], call["images"]), (1, 9, None))

    def test_finish_reason_length_when_the_cap_ends_the_run(self):
        self.rt.script, self.rt.eos = ["a", "b", "c", "d", "e"], False
        _, body, _ = self.chat(max_tokens=3)
        self.assertEqual((body["choices"][0]["message"]["content"], body["choices"][0]["finish_reason"]), ("abc", "length"))

    def test_an_eos_that_lands_exactly_on_the_cap_is_a_stop(self):
        self.rt.script, self.rt.eos = ["a", "b"], True  # 2 tokens + the EOS event = the cap of 3
        _, body, _ = self.chat(max_tokens=3)
        self.assertEqual((body["choices"][0]["message"]["content"], body["choices"][0]["finish_reason"]), ("ab", "stop"))

    def test_stop_string_cuts_the_text_and_aborts_the_run(self):
        self.rt.script = ["foo", " ST", "OP", " bar"]
        _, body, _ = self.chat(stop=["STOP"])
        self.assertEqual((body["choices"][0]["message"]["content"], body["choices"][0]["finish_reason"]), ("foo ", "stop"))
        self.assertEqual(self.rt.stops, 1)  # on_text returned False: the runtime aborts, " bar" is never produced

    def test_thinking_splits_reasoning_from_content(self):
        self.rt.script = ["plan", "</think>", "\n\n", "Answer"]
        _, body, _ = self.chat(chat_template_kwargs={"enable_thinking": True})
        self.assertEqual(body["choices"][0]["message"],
                         {"role": "assistant", "content": "Answer", "reasoning_content": "plan"})
        self.assertTrue(self.rt.calls[0]["prompt"].endswith("<|im_start|>assistant\n<think>\n"))

    def test_constrained_decoding_is_a_400_with_the_openai_error_and_never_reaches_the_runtime(self):
        for fields in ({"grammar": "root ::= x"}, {"response_format": {"type": "json_object"}}):
            for stream in (False, True):
                with self.subTest(fields=fields, stream=stream):
                    status, body, _ = self.chat(stream=stream, **fields)
                    self.assertEqual(status, 400)
                    self.assertEqual(body, {"error": {
                        "message": "constrained decoding (grammar / json_schema) is not supported by the RKLLM runtime",
                        "type": "invalid_request_error", "param": None, "code": "constrained_decoding_unsupported"}})
        self.assertEqual(self.rt.calls, [])

    def test_ignored_fields_still_answer(self):
        status, body, _ = self.chat(response_format={"type": "text"}, logprobs=True,
                                    top_logprobs=2, cache_prompt=True, seed=1, chat_template_kwargs={"x": 1})
        self.assertEqual((status, body["choices"][0]["message"]["content"]), (200, "Hello"))

    def test_bad_requests_get_an_openai_error(self):
        for name, raw, want in (("not json", b"{oops", 400), ("empty", b"", 400), ("array", b"[1]", 400),
                                ("no messages", b'{"messages": []}', 400)):
            with self.subTest(name):
                status, _, payload = self.request("POST", "/v1/chat/completions", raw=raw)
                self.assertEqual(status, want)
                self.assertEqual(json.loads(payload)["error"]["type"], "invalid_request_error")
        self.assertEqual(self.rt.calls, [])

    def test_a_refused_prompt_is_a_400_not_a_hang(self):
        self.rt.rc = -1  # what rkllm_run returned for a prompt over the window: no events at all
        status, body, _ = self.chat()
        self.assertEqual(status, 400)
        self.assertIn("4096-token window", body["error"]["message"])
        status, _, _ = self.request("POST", "/v1/chat/completions",
                                    {"stream": True, "messages": [{"role": "user", "content": "hi"}]})
        self.assertEqual(status, 400)  # streaming too: the SSE response had not started

    def test_a_run_that_ends_in_error_is_a_500(self):
        self.rt.error = "the runtime reported RKLLM_RUN_ERROR"
        status, body, _ = self.chat()
        self.assertEqual(status, 500)
        self.assertIn("RUN_ERROR", body["error"]["message"])


class StreamTest(ServerCase):
    def test_sse_framing(self):
        status, ctype, frames, text = self.sse()
        self.assertEqual((status, ctype), (200, "text/event-stream"))
        self.assertTrue(text.endswith("data: [DONE]\n\n"))
        self.assertEqual(frames[0]["choices"][0]["delta"], {"role": "assistant", "content": ""})
        self.assertEqual([f["choices"][0]["delta"] for f in frames[1:3]], [{"content": "Hel"}, {"content": "lo"}])
        last = frames[-1]
        self.assertEqual((last["choices"][0]["delta"], last["choices"][0]["finish_reason"]), ({}, "stop"))
        self.assertEqual(last["timings"]["predicted_n"], 2)
        self.assertEqual({f["id"] for f in frames}, {frames[0]["id"]})
        self.assertTrue(all(f["object"] == "chat.completion.chunk" for f in frames))
        self.assertEqual(len(frames), 4)  # role, two deltas, finish: no usage frame unless it was asked for

    def test_usage_frame_when_asked_for(self):
        _, _, frames, text = self.sse(stream_options={"include_usage": True})
        usage = frames[-1]
        self.assertEqual(usage["choices"], [])  # the shape the harness's SSE decoder reads
        self.assertEqual(usage["usage"], {"prompt_tokens": 13, "completion_tokens": 3, "total_tokens": 16})
        self.assertEqual(usage["timings"]["prompt_n"], 13)
        self.assertEqual(frames[-2]["choices"][0]["finish_reason"], "stop")
        self.assertTrue(text.endswith("data: [DONE]\n\n"))

    def test_length_finish_reason(self):
        self.rt.script, self.rt.eos = list("abcdef"), False
        _, _, frames, _ = self.sse(max_tokens=4, stream_options={"include_usage": True})
        self.assertEqual(frames[-2]["choices"][0]["finish_reason"], "length")

    def test_a_partial_stop_string_never_reaches_the_client(self):
        self.rt.script = ["ab", "<", "/", "s", ">", "zz"]
        _, _, frames, _ = self.sse(stop=["</s>"])
        text = "".join(f["choices"][0]["delta"].get("content", "") for f in frames if f["choices"])
        self.assertEqual(text, "ab")
        self.assertEqual(self.rt.stops, 1)

    def test_reasoning_streams_as_reasoning_content(self):
        self.rt.script = ["hm", "</think>", "\n\n", "ok"]
        _, _, frames, _ = self.sse(chat_template_kwargs={"enable_thinking": True})
        deltas = [f["choices"][0]["delta"] for f in frames if f["choices"]]
        self.assertIn({"reasoning_content": "hm"}, deltas)
        self.assertIn({"content": "ok"}, deltas)

    def test_a_failure_after_the_first_token_is_an_error_frame_then_done(self):
        self.rt.error = "the runtime reported RKLLM_RUN_ERROR"
        _, _, frames, text = self.sse()
        self.assertIn("RUN_ERROR", frames[-1]["error"]["message"])
        self.assertTrue(text.endswith("data: [DONE]\n\n"))


class LockAndDisconnectTest(ServerCase):
    def post_in_thread(self, results, key, **fields):
        def go():
            results[key] = self.chat(**fields)[0]

        t = threading.Thread(target=go)
        t.start()
        return t

    def wait_for(self, cond, what, sec=5.0):
        end = time.monotonic() + sec
        while time.monotonic() < end:
            if cond():
                return
            time.sleep(0.01)
        self.fail(f"timed out waiting for {what}")

    def test_one_generation_at_a_time_and_the_second_request_waits(self):
        gate = self.rt.gate = threading.Event()
        results = {}
        first = self.post_in_thread(results, "a")
        self.wait_for(lambda: len(self.rt.calls) == 1, "the first request to reach the runtime")
        second = self.post_in_thread(results, "b")
        time.sleep(0.6)  # long enough for a second request to have entered the runtime if nothing stopped it
        self.assertEqual(len(self.rt.calls), 1)
        gate.set()
        first.join(10)
        second.join(10)
        self.assertEqual(results, {"a": 200, "b": 200})
        self.assertEqual((len(self.rt.calls), self.rt.max_active), (2, 1))

    def test_a_queued_request_whose_client_hung_up_never_runs(self):
        gate = self.rt.gate = threading.Event()
        results = {}
        first = self.post_in_thread(results, "a")
        self.wait_for(lambda: len(self.rt.calls) == 1, "the first request to reach the runtime")
        body = json.dumps({"messages": [{"role": "user", "content": "hi"}]}).encode()
        s = socket.create_connection(("127.0.0.1", self.port))
        s.sendall(b"POST /v1/chat/completions HTTP/1.0\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n"
                  % len(body) + body)
        time.sleep(0.3)  # queued behind the first
        s.close()
        time.sleep(0.8)  # the queue polls for a hung-up client every 250 ms
        gate.set()
        first.join(10)
        self.assertEqual(results["a"], 200)
        time.sleep(0.3)
        self.assertEqual(len(self.rt.calls), 1)

    def test_a_client_that_leaves_mid_stream_aborts_the_run(self):
        self.rt.script, self.rt.eos, self.rt.delay = ["x"] * 400, False, 0.02
        body = json.dumps({"stream": True, "messages": [{"role": "user", "content": "hi"}]}).encode()
        s = socket.create_connection(("127.0.0.1", self.port))
        s.sendall(b"POST /v1/chat/completions HTTP/1.0\r\nContent-Length: %d\r\n\r\n" % len(body) + body)
        got = b""
        while b'"content": "x"' not in got:
            got += s.recv(4096)
        s.close()
        self.wait_for(lambda: self.rt.stops == 1, "the run to be aborted")

    def test_a_peer_that_stalls_mid_request_gets_a_408_instead_of_holding_a_thread(self):
        self.addCleanup(setattr, rs.Handler, "timeout", rs.Handler.timeout)
        rs.Handler.timeout = 0.3  # the class attribute is read when each connection is set up
        s = socket.create_connection(("127.0.0.1", self.port))
        s.sendall(b"POST /v1/chat/completions HTTP/1.0\r\nContent-Length: 100\r\n\r\n{")  # promises 100 bytes, sends 1
        s.settimeout(5)
        self.assertIn(b" 408 ", s.recv(4096).split(b"\r\n")[0])
        s.close()
        self.assertEqual(self.rt.calls, [])

    def test_a_run_cut_short_by_shutdown_is_a_503_not_a_finished_answer(self):
        app = self.app

        class Cut(FakeRuntime):
            def generate(self, prompt, images, sampling, max_new, on_text):
                on_text("Hel")
                app.closing = True  # SIGTERM lands mid-run
                return rs.Run(0, 1, False, "", PERF)  # what rkllm_abort leaves behind: rc 0 and fewer tokens than asked

        app.runtime = Cut()
        status, body, _ = self.chat()
        self.assertEqual(status, 503)
        self.assertIn("cut short", body["error"]["message"])
        app.closing = False
        _, _, frames, text = self.sse()  # streamed: the frames already sent stand, then an error frame and [DONE]
        self.assertIn("cut short", frames[-1]["error"]["message"])
        self.assertTrue(text.endswith("data: [DONE]\n\n"))

    def test_shutdown_aborts_then_destroys_and_new_requests_are_refused(self):
        order = []
        self.rt.abort = lambda: order.append("abort")
        self.rt.close = lambda: order.append("close")
        self.app.shutdown(wait_sec=1)
        self.assertEqual(order, ["abort", "close"])
        self.assertEqual(self.chat()[0], 503)


class VisionRequestTest(ServerCase):
    with_vision = True

    def image_part(self, uri=PNG_URI):
        return {"type": "image_url", "image_url": {"url": uri}}

    def test_an_image_is_encoded_and_handed_to_the_runtime_with_its_tag(self):
        status, _, _ = self.chat(messages=[{"role": "user", "content": [self.image_part(),
                                                                            {"type": "text", "text": "What is it?"}]}])
        self.assertEqual(status, 200)
        call = self.rt.calls[0]
        self.assertIn("<|im_start|>user\n<image>What is it?<|im_end|>\n", call["prompt"])
        batch = call["images"]
        self.assertEqual((batch.tokens, batch.width, batch.height, len(batch.embeds)), (4, 8, 8, 1))
        self.assertEqual(self.vision.encoded, [b"\x89PNG fake"])

    def test_two_images_make_two_embeddings_in_order(self):
        other = "data:image/jpeg;base64," + base64.b64encode(b"jpeg!").decode()
        self.chat(messages=[{"role": "user", "content": [self.image_part(), self.image_part(other)]}])
        self.assertEqual(self.vision.encoded, [b"\x89PNG fake", b"jpeg!"])
        self.assertEqual(len(self.rt.calls[0]["images"].embeds), 2)

    def test_a_text_only_request_skips_the_encoder(self):
        self.chat()
        self.assertEqual((self.vision.encoded, self.rt.calls[0]["images"]), ([], None))

    def test_an_undecodable_image_is_a_400_and_the_lock_is_released(self):
        def boom(data):
            raise rs.ApiError(400, "could not decode the image")

        self.vision.encode = boom
        status, _, _ = self.chat(messages=[{"role": "user", "content": [self.image_part()]}])
        self.assertEqual(status, 400)
        self.assertEqual(self.chat()[0], 200)  # the next request is not stuck behind the failed one


class NoVisionSeatTest(ServerCase):
    def test_images_are_refused(self):
        status, body, _ = self.chat(messages=[{"role": "user", "content": [
            {"type": "image_url", "image_url": {"url": PNG_URI}}]}])
        self.assertEqual(status, 400)
        self.assertIn("--vision-encoder", body["error"]["message"])


class RuntimeCallbackTest(unittest.TestCase):
    """RKLLMRuntime._on_result without the vendor library: event -> Run bookkeeping, aborts, exceptions."""

    def runtime(self, on_text):
        rt = object.__new__(rs.RKLLMRuntime)
        self.aborted = []
        rt._lib = type("Lib", (), {"rkllm_abort": lambda _s, h: self.aborted.append(h) or 0})()
        rt._handle = "H"
        rt._state = rs._RunState(on_text)
        return rt

    def event(self, rt, state, text=None, perf=None):
        r = rs.RKLLMResult()
        r.text = text
        if perf:
            (r.perf.prefill_time_ms, r.perf.prefill_tokens, r.perf.generate_time_ms, r.perf.generate_tokens,
             r.perf.memory_usage_mb) = perf
        return rt._on_result(ctypes.pointer(r), None, state)

    def test_events_become_a_run(self):
        seen = []
        rt = self.runtime(lambda t: seen.append(t) or True)
        for state, text in ((rs.RKLLM_RUN_NORMAL, b"Hi "), (rs.RKLLM_RUN_WAITING, None),
                            (rs.RKLLM_RUN_NORMAL, "\N{SMILING FACE WITH SMILING EYES}".encode()),
                            (rs.RKLLM_RUN_NORMAL, None)):  # the EOS event: a special token, no text
            self.assertEqual(self.event(rt, state, text), 0)
        self.event(rt, rs.RKLLM_RUN_FINISH, perf=(1262.11, 13, 1702.84, 11, 1186.2))
        st = rt._state
        self.assertEqual(seen, ["Hi ", "\N{SMILING FACE WITH SMILING EYES}"])  # empty text is not forwarded
        self.assertEqual((st.tokens, st.eos, st.error, st.stopped), (4, True, "", False))
        self.assertEqual(st.perf, rs.Perf(1262.11, 13, 1702.84, 11, 1186.2))

    def test_a_stop_request_aborts_once_and_later_events_are_dropped(self):
        seen = []
        rt = self.runtime(lambda t: seen.append(t) or len(seen) < 2)
        self.event(rt, rs.RKLLM_RUN_NORMAL, b"a")
        self.event(rt, rs.RKLLM_RUN_NORMAL, b"b")  # on_text says stop here
        self.event(rt, rs.RKLLM_RUN_NORMAL, b"c")
        self.event(rt, rs.RKLLM_RUN_FINISH, perf=(1, 1, 1, 1, 1))
        st = rt._state
        self.assertEqual((seen, st.tokens, st.stopped, self.aborted), (["a", "b"], 2, True, ["H"]))
        self.assertIsNotNone(st.perf)  # the FINISH event after an abort still carries the stats

    def test_an_exception_in_on_text_ends_the_run_instead_of_crossing_the_c_frame(self):
        def boom(_t):
            raise ValueError("bad")

        rt = self.runtime(boom)
        self.assertEqual(self.event(rt, rs.RKLLM_RUN_NORMAL, b"a"), 0)
        st = rt._state
        self.assertEqual((st.stopped, self.aborted), (True, ["H"]))
        self.assertIn("ValueError: bad", st.error)

    def test_run_error_is_recorded(self):
        rt = self.runtime(lambda t: True)
        self.event(rt, rs.RKLLM_RUN_ERROR)
        self.assertIn("RKLLM_RUN_ERROR", rt._state.error)

    def test_a_late_event_with_no_run_in_flight_is_ignored(self):
        rt = self.runtime(lambda t: True)
        rt._state = None
        self.assertEqual(self.event(rt, rs.RKLLM_RUN_NORMAL, b"x"), 0)


class CliTest(unittest.TestCase):
    def run_main(self, *argv):
        err = io.StringIO()
        with contextlib.redirect_stderr(err), mock.patch.object(rs, "_pin_to_cpus"):  # never pin the test runner
            try:
                code = rs.main(list(argv))
            except SystemExit as e:
                code = e.code
        return code, err.getvalue()

    def test_the_server_pins_itself_to_the_seat_mask_before_it_starts_threads(self):
        calls = []
        with mock.patch.object(os, "sched_setaffinity", lambda pid, cpus: calls.append((pid, cpus)), create=True):
            rs._pin_to_cpus(0xF0)
            rs._pin_to_cpus(0x0F)
        self.assertEqual(calls, [(0, {4, 5, 6, 7}), (0, {0, 1, 2, 3})])

    def test_a_mask_naming_cpus_the_machine_lacks_warns_instead_of_dying(self):
        def refuse(_pid, _cpus):
            raise OSError(22, "Invalid argument")

        err = io.StringIO()
        with mock.patch.object(os, "sched_setaffinity", refuse, create=True), contextlib.redirect_stderr(err):
            rs._pin_to_cpus(0x100)
        self.assertIn("could not pin", err.getvalue())

    def test_main_pins_to_the_parsed_mask(self):
        pinned = []
        err = io.StringIO()
        with contextlib.redirect_stderr(err), mock.patch.object(rs, "_pin_to_cpus", pinned.append):
            with socket.socket() as taken:  # a taken port ends main() right after the pin, before any model loads
                taken.bind(("127.0.0.1", 0))
                taken.listen(1)
                rs.main(["--model", __file__, "--port", str(taken.getsockname()[1]), "--cpu-mask", "0xf0"])
        self.assertEqual(pinned, [0xF0])

    def test_a_non_loopback_host_is_refused_before_anything_loads(self):
        for host in ("0.0.0.0", "::", "example.com", "fe80::1"):
            with self.subTest(host):
                code, err = self.run_main("--model", __file__, "--port", "1", "--host", host)
                self.assertEqual(code, 2)
                self.assertIn("non-loopback", err)

    def test_loopback_forms(self):
        for host in ("127.0.0.1", "127.0.0.2", "::1", "localhost"):
            self.assertTrue(rs._is_loopback(host), host)
        for host in ("0.0.0.0", "", "::", "fe80::1", "not a host"):
            self.assertFalse(rs._is_loopback(host), host)

    def test_a_port_that_is_taken_exits_2(self):
        with socket.socket() as taken:
            taken.bind(("127.0.0.1", 0))
            taken.listen(1)
            code, err = self.run_main("--model", __file__, "--port", str(taken.getsockname()[1]))
        self.assertEqual(code, 2)
        self.assertIn("cannot listen", err)

    def test_argument_errors_exit_2(self):
        self.assertEqual(self.run_main("--port", "1")[0], 2)  # no --model
        self.assertEqual(self.run_main("--model", __file__, "--port", "1", "--cpu-mask", "zz")[0], 2)
        self.assertEqual(self.run_main("--model", __file__, "--port", "1", "--cpu-mask", "0")[0], 2)
        code, err = self.run_main("--model", os.path.join(HERE, "absent.rkllm"), "--port", "1")
        self.assertEqual(code, 2)
        self.assertIn("no such file", err)


@unittest.skipUnless(HAVE_IMAGING, "needs numpy and Pillow")
class PreprocessTest(unittest.TestCase):
    @staticmethod
    def encode(img, fmt="PNG"):
        buf = io.BytesIO()
        img.save(buf, fmt)
        return buf.getvalue()

    @staticmethod
    def reference(src, out_h, out_w):
        """cv::resize INTER_LINEAR of the gray-padded square, one output pixel at a time in plain Python."""
        h, w = len(src), len(src[0])
        side = max(h, w)
        oy, ox = (side - h) // 2, (side - w) // 2

        def px(y, x, c):
            yy, xx = y - oy, x - ox
            return src[yy][xx][c] if 0 <= yy < h and 0 <= xx < w else 128

        def taps(d, n_out):
            f = min(max((d + 0.5) * side / n_out - 0.5, 0.0), side - 1)
            i = int(f)
            return i, min(i + 1, side - 1), f - i

        out = []
        for dy in range(out_h):
            y0, y1, fy = taps(dy, out_h)
            row = []
            for dx in range(out_w):
                x0, x1, fx = taps(dx, out_w)
                row.append([int(((px(y0, x0, c) * (1 - fx) + px(y0, x1, c) * fx) * (1 - fy)
                                 + (px(y1, x0, c) * (1 - fx) + px(y1, x1, c) * fx) * fy) + 0.5) for c in range(3)])
            out.append(row)
        return out

    def test_resize_matches_a_plain_python_reference(self):
        rng = np.random.default_rng(7)
        for shape, out in (((3, 5), (4, 4)), ((7, 2), (5, 6)), ((9, 9), (4, 4)), ((2, 2), (5, 5))):
            src = rng.integers(0, 256, size=(*shape, 3), dtype=np.uint8)
            got = rs._resize_square(src, *out)
            want = np.array(self.reference(src.tolist(), *out), dtype=np.int64)
            self.assertLessEqual(int(np.abs(got.astype(np.int64) - want).max()), 1, (shape, out))

    def test_padding_is_mid_gray_and_the_picture_stays_centred(self):
        img = Image.new("RGB", (20, 10), (255, 0, 0))
        out = rs.preprocess_image(self.encode(img), 8, 8)
        self.assertEqual((out.shape, out.dtype), ((8, 8, 3), np.uint8))
        self.assertEqual(out[0, 4].tolist(), [128, 128, 128])  # top band: padding
        self.assertEqual(out[7, 4].tolist(), [128, 128, 128])
        self.assertEqual(out[4, 4].tolist(), [255, 0, 0])  # middle band: the picture

    def test_jpeg_and_exif_orientation(self):
        img = Image.new("RGB", (16, 8), (0, 0, 255))
        exif = Image.Exif()
        exif[0x0112] = 6  # rotate 90 clockwise on display: a wide picture becomes tall, so the padding is left/right
        buf = io.BytesIO()
        img.save(buf, "JPEG", exif=exif)
        out = rs.preprocess_image(buf.getvalue(), 8, 8)
        self.assertEqual(out[4, 0].tolist(), [128, 128, 128])
        self.assertLess(int(out[4, 4, 0]), 30)  # blue survived the JPEG round trip

    def test_only_png_jpeg_and_webp_decode(self):
        gif = self.encode(Image.new("RGB", (4, 4)), "GIF")
        for name, data in (("gif", gif), ("garbage", b"not an image")):
            with self.subTest(name), self.assertRaises(rs.ApiError) as cm:
                rs.preprocess_image(data, 8, 8)
            self.assertEqual(cm.exception.status, 400)

    def test_oversized_pictures_are_refused_before_they_are_decoded(self):
        data = self.encode(Image.new("RGB", (30, 30)))
        old, rs.MAX_IMAGE_PIXELS = rs.MAX_IMAGE_PIXELS, 100
        try:
            with self.assertRaises(rs.ApiError) as cm:
                rs.preprocess_image(data, 8, 8)
        finally:
            rs.MAX_IMAGE_PIXELS = old
        self.assertIn("megapixels", cm.exception.message)


class LayoutTest(unittest.TestCase):
    @unittest.skipUnless(ctypes.sizeof(ctypes.c_void_p) == 8, "the golden numbers are for a 64-bit ABI")
    def test_ctypes_structs_match_the_vendor_headers(self):
        """rkllm_layout_aarch64.txt is what rkllm_layout_check.cpp prints from rkllm.h / rknn_api.h (g++ on the board)."""
        with open(os.path.join(HERE, "rkllm_layout_aarch64.txt"), encoding="utf-8") as fh:
            golden = fh.read().splitlines()
        self.assertEqual(rs.layout_report(), golden)  # a shifted field shows up as the one differing line

    def test_the_cpp_checker_lists_the_same_members_in_the_same_order(self):
        """So the golden file cannot go stale against LAYOUT without a compiler in sight."""
        with open(os.path.join(HERE, "rkllm_layout_check.cpp"), encoding="utf-8") as fh:
            calls = re.findall(r"^\s+(SIZE|OFF)\((.*?)\);$", fh.read(), re.MULTILINE)
        want = []
        for name, (_cls, members) in rs.LAYOUT.items():
            want.append(("SIZE", name))
            want += [("OFF", f"{name}, {m}") for m in members]
        self.assertEqual(calls, want)


if __name__ == "__main__":
    unittest.main()
