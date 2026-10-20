"""Tests for nodes/offload_multicard (run by render/comfyui-ext.test.mjs, or: python -B test_offload_multicard.py).

No ComfyUI and no GPU: `comfy.*` are stubs, torch is the real one, and the card queries are replaced. Needs torch and
safetensors; the node test skips this file when they are missing.
"""
import importlib.util
import json
import os
import sys
import tempfile
import types
import unittest

os.environ.setdefault("CUDA_VISIBLE_DEVICES", "-1")  # no card is ever touched by this file

import torch
from safetensors.torch import save_file

PACK = os.path.join(os.path.dirname(os.path.abspath(__file__)), "nodes", "offload_multicard", "__init__.py")
GIB = 1024 ** 3


def stub_comfy():
    """Install stand-ins for the comfy modules the pack imports; return (utils, original_loader)."""
    utils = types.ModuleType("comfy.utils")
    utils._TYPES = {"F32": torch.float32, "F16": torch.float16, "BF16": torch.bfloat16, "I64": torch.int64,
                    "F8_E4M3": torch.float8_e4m3fn, "BOOL": torch.bool}
    calls = []

    def original(ckpt, safe_load=False, device=None, return_metadata=False):
        calls.append((ckpt, device))
        return ("ORIGINAL", None) if return_metadata else "ORIGINAL"

    utils.load_torch_file = original
    utils.calls = calls
    storage = types.ModuleType("comfy.storage")
    annotated = []
    storage.annotate_state_dict = lambda sd, path: annotated.append((sorted(sd), path))
    storage.annotated = annotated
    mm = types.ModuleType("comfy.model_management")
    mm.cleanup_models = lambda: None
    sd = types.ModuleType("comfy.sd")
    sd.load_diffusion_model_state_dict = lambda sd, model_options={}, metadata=None, disable_dynamic=False: model_options
    ops = types.ModuleType("comfy.ops")
    ops.mixed_precision_ops = lambda quant_config={}, compute_dtype=torch.bfloat16: ("mixed", compute_dtype)
    pkg = types.ModuleType("comfy")
    pkg.utils, pkg.storage, pkg.model_management, pkg.sd, pkg.ops = utils, storage, mm, sd, ops
    sys.modules.update({"comfy": pkg, "comfy.utils": utils, "comfy.storage": storage, "comfy.model_management": mm,
                        "comfy.sd": sd, "comfy.ops": ops})
    return utils, original


def load_pack(name="offload_multicard_under_test"):
    spec = importlib.util.spec_from_file_location(name, PACK)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ReadOnlyReader(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.path = os.path.join(self.dir.name, "w.safetensors")
        self.tensors = {
            "a.weight": torch.arange(12, dtype=torch.float32).reshape(3, 4),
            "b.bias": torch.tensor([1.5, -2.0, 3.25], dtype=torch.bfloat16),
            "c.fp8": torch.tensor([[0.5, 1.0], [2.0, -4.0]]).to(torch.float8_e4m3fn),
            "d.ids": torch.tensor([1, 2, 3], dtype=torch.int64),
            "e.empty": torch.empty(0, dtype=torch.float32),
            "f.scalar": torch.tensor(7.0),
        }
        save_file(self.tensors, self.path, metadata={"format": "pt", "note": "x"})

    def test_without_the_switch_nothing_is_patched(self):
        os.environ.pop("OFFLOAD_RO_LOAD", None)
        utils, original = stub_comfy()
        load_pack()
        self.assertIs(utils.load_torch_file, original)

    def test_the_switch_installs_a_reader_that_returns_the_same_tensors(self):
        os.environ["OFFLOAD_RO_LOAD"] = "1"
        self.addCleanup(os.environ.pop, "OFFLOAD_RO_LOAD", None)
        utils, original = stub_comfy()
        load_pack()
        self.assertIsNot(utils.load_torch_file, original)
        state, metadata = utils.load_torch_file(self.path, return_metadata=True)
        self.assertEqual(metadata, {"format": "pt", "note": "x"})
        self.assertEqual(sorted(state), sorted(self.tensors))
        for name, want in self.tensors.items():
            got = state[name]
            self.assertEqual(got.dtype, want.dtype, name)
            self.assertEqual(tuple(got.shape), tuple(want.shape), name)
            self.assertTrue(torch.equal(got.to(torch.float32) if got.is_floating_point() else got,
                                        want.to(torch.float32) if want.is_floating_point() else want), name)
        self.assertEqual(sys.modules["comfy.storage"].annotated[0][1], self.path, "annotated like the stock loader does")
        # without return_metadata it is the bare state dict, as the stock function returns
        self.assertEqual(sorted(utils.load_torch_file(self.path)), sorted(self.tensors))

    def test_the_tensors_are_backed_by_a_read_only_map_that_lives_as_long_as_they_do(self):
        os.environ["OFFLOAD_RO_LOAD"] = "1"
        self.addCleanup(os.environ.pop, "OFFLOAD_RO_LOAD", None)
        utils, _ = stub_comfy()
        load_pack()
        state = utils.load_torch_file(self.path)
        refs = getattr(state["a.weight"].untyped_storage(), "_offload_ro_refs")
        mapped, view = refs
        self.assertFalse(view.readonly is False, "the map is read-only")
        with self.assertRaises((TypeError, ValueError)):
            mapped[0:1] = b"x"  # a write through the map is refused
        self.assertEqual(state["a.weight"].sum().item(), sum(range(12)))

    def test_other_loads_go_to_the_original(self):
        os.environ["OFFLOAD_RO_LOAD"] = "1"
        self.addCleanup(os.environ.pop, "OFFLOAD_RO_LOAD", None)
        utils, _ = stub_comfy()
        load_pack()
        self.assertEqual(utils.load_torch_file("model.ckpt"), "ORIGINAL", "not a safetensors file")
        self.assertEqual(utils.load_torch_file(self.path, device=torch.device("cuda", 0)), "ORIGINAL", "a load straight to a card")
        self.assertEqual([c[0] for c in utils.calls], ["model.ckpt", self.path])
        self.assertIsInstance(utils.load_torch_file(self.path, device=torch.device("cpu")), dict, "an explicit CPU load is ours")

    def _reader(self):
        os.environ["OFFLOAD_RO_LOAD"] = "1"
        self.addCleanup(os.environ.pop, "OFFLOAD_RO_LOAD", None)
        utils, _ = stub_comfy()
        load_pack()
        return utils

    def test_a_truncated_file_is_a_clear_error_not_a_torch_shape_error(self):
        utils = self._reader()
        with open(self.path, "rb") as source:
            raw = source.read()
        cut = os.path.join(self.dir.name, "cut.safetensors")
        with open(cut, "wb") as fh:
            fh.write(raw[:-8])
        with self.assertRaises(ValueError) as ctx:
            utils.load_torch_file(cut, return_metadata=True)
        message = str(ctx.exception)
        self.assertIn("cut.safetensors", message)
        self.assertIn("truncated", message)
        self.assertIn("data section", message)

    def test_a_header_whose_byte_range_disagrees_with_its_shape_is_a_clear_error(self):
        utils = self._reader()
        header = json.dumps({"w": {"dtype": "F32", "shape": [4], "data_offsets": [0, 8]}}).encode()
        bad = os.path.join(self.dir.name, "bad.safetensors")
        with open(bad, "wb") as fh:
            fh.write(len(header).to_bytes(8, "little") + header + bytes(8))
        with self.assertRaises(ValueError) as ctx:
            utils.load_torch_file(bad)
        message = str(ctx.exception)
        self.assertIn("bad.safetensors", message)
        self.assertIn("'w'", message)
        self.assertIn("16 bytes", message)
        self.assertIn("8 bytes", message)

    def test_a_pack_that_cannot_install_its_reader_fails_to_import(self):
        os.environ["OFFLOAD_RO_LOAD"] = "1"
        self.addCleanup(os.environ.pop, "OFFLOAD_RO_LOAD", None)
        stub_comfy()
        del sys.modules["comfy.storage"]  # the seam this reader needs is gone
        sys.modules["comfy"].storage = None
        with self.assertRaises(Exception):
            load_pack()


class FreeNode(unittest.TestCase):
    def setUp(self):
        os.environ.pop("OFFLOAD_RO_LOAD", None)
        stub_comfy()
        self.pack = load_pack()
        self.samples = {"samples": torch.zeros(1, 16, 21, 160, 90)}

    def fake_cards(self, free_gib):
        cards = [{"card": f"cuda:{i}", "free_gib": g, "total_gib": 15.9, "torch_allocated_gib": 0.0, "torch_reserved_gib": 0.0} for i, g in enumerate(free_gib)]
        self.pack._cards = lambda: list(range(len(cards)))
        self.pack._card_line = lambda i: cards[i]
        self.released = []
        self.pack._release_cards = lambda: self.released.append(1) or 0.5

    def vae(self, device_index, estimate_gib=12.0):
        vae = types.SimpleNamespace(device=torch.device("cuda", device_index), vae_dtype=torch.bfloat16)
        vae.memory_used_decode = lambda shape, dtype: estimate_gib * GIB
        return vae

    def test_the_node_is_registered_and_passes_the_latent_through(self):
        self.assertIn("OffloadFreeForDecode", self.pack.NODE_CLASS_MAPPINGS)
        node = self.pack.NODE_CLASS_MAPPINGS["OffloadFreeForDecode"]
        self.assertEqual(node.RETURN_TYPES, ("LATENT",))
        inputs = node.INPUT_TYPES()
        self.assertEqual(list(inputs["required"]), ["samples"])
        self.assertEqual(sorted(inputs["optional"]), ["check", "save_prefix", "vae"])
        out = node().free(self.samples)  # no card visible in this process
        self.assertIs(out["result"][0], self.samples)

    def test_enough_free_memory_after_the_release_lets_the_decode_run(self):
        self.fake_cards([14.9, 14.0])
        out = self.pack.OffloadFreeForDecode().free(self.samples, vae=self.vae(0))
        self.assertIs(out["result"][0], self.samples)
        self.assertEqual(self.released, [1])

    def test_a_decode_card_that_still_holds_the_experts_stops_the_run_with_the_numbers(self):
        self.fake_cards([4.6, 12.0])  # the donor blocks (10.3 GiB) are still on cuda:0
        with self.assertRaises(RuntimeError) as ctx:
            self.pack.OffloadFreeForDecode().free(self.samples, vae=self.vae(0))
        message = str(ctx.exception)
        self.assertIn("OFFLOAD-DECODE-NOFIT", message)
        self.assertIn("cuda:0 has 4.60 GiB free", message)
        self.assertIn("needs about 10.20 GiB", message, "85 % of ComfyUI's 12 GiB estimate")
        self.assertIn("temporal chunking", message)

    def test_the_card_checked_is_the_vaes(self):
        self.fake_cards([4.0, 14.9])
        self.pack.OffloadFreeForDecode().free(self.samples, vae=self.vae(1))  # the VAE is on cuda:1, which has room
        with self.assertRaises(RuntimeError):
            self.pack.OffloadFreeForDecode().free(self.samples, vae=self.vae(0))

    def test_without_a_vae_the_fallback_need_applies_to_the_first_card(self):
        self.fake_cards([9.0])
        with self.assertRaises(RuntimeError) as ctx:
            self.pack.OffloadFreeForDecode().free(self.samples)
        self.assertIn("needs about 10.20 GiB", str(ctx.exception))

    def test_the_latent_is_saved_first_and_named_in_the_failure(self):
        saved = []

        class SaveLatent:
            def save(self, samples, prefix):
                saved.append(prefix)
                return {"ui": {"latents": [{"filename": "render_00001_.latent", "subfolder": "latents", "type": "output"}]}}

        sys.modules["nodes"] = types.SimpleNamespace(SaveLatent=SaveLatent)
        self.addCleanup(sys.modules.pop, "nodes", None)
        self.fake_cards([14.9, 14.0])
        out = self.pack.OffloadFreeForDecode().free(self.samples, vae=self.vae(0), save_prefix="latents/render_wan")
        self.assertEqual(saved, ["latents/render_wan"])
        self.assertEqual(out["ui"]["latents"][0]["filename"], "render_00001_.latent", "the UI result reaches ComfyUI's history")
        self.fake_cards([2.0, 14.0])
        with self.assertRaises(RuntimeError) as ctx:
            self.pack.OffloadFreeForDecode().free(self.samples, vae=self.vae(0), save_prefix="latents/render_wan")
        self.assertIn(os.path.join("latents", "render_00001_.latent"), str(ctx.exception))

    def test_a_vae_without_an_estimate_uses_the_fallback(self):
        self.fake_cards([10.0])
        vae = types.SimpleNamespace(device=torch.device("cuda", 0))
        with self.assertRaises(RuntimeError):
            self.pack.OffloadFreeForDecode().free(self.samples, vae=vae)
        self.fake_cards([10.5])
        self.pack.OffloadFreeForDecode().free(self.samples, vae=vae)


if __name__ == "__main__":
    unittest.main()
