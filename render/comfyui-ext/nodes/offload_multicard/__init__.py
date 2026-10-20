"""Harness-owned ComfyUI nodes for the card-only (multicard) Wan 2.2, LTX-2.5, image and edit placements.

ComfyUI loads this folder through render/comfyui-ext/extra_model_paths.yaml (its `custom_nodes` key),
which render/wan-multicard.mjs passes with --extra-model-paths-config. It is never copied into the
shared install. Four parts:

1. A READ-ONLY safetensors reader, active only when the harness sets OFFLOAD_RO_LOAD=1 in the
   ComfyUI process. Stock ComfyUI (dynamic VRAM off, which --gpu-only implies) opens a safetensors file
   through safe_open, which maps it copy-on-write; Windows charges that mapping to system commit in
   full at map time (measured: +13.3 GiB for one 13.3 GiB Wan expert). The same file mapped read-only
   charges nothing, and its pages are clean file pages the OS can drop. The reader returns tensors
   backed by that read-only map and is otherwise a drop-in for comfy.utils.load_torch_file on a CPU load
   of a .safetensors file; every other load goes to the original function. If the reader cannot be
   installed this module raises, so ComfyUI reports the pack as failed and the runner's node check
   (verifyMulticardPack in render/comfy-video.mjs, asked under every decode) stops the run before any
   GPU work (a placement that would silently charge 15 GiB per expert).
   ASSUMPTION, not a guarantee: nothing writes into a loaded weight in place on this path. The map is
   read-only, so such a write would not corrupt the file: it would crash ComfyUI with a Windows access
   violation. None exists on the Wan load path read at ComfyUI 0.38.0 (under --gpu-only the weights are
   copied to the card, not assigned in place) and the runs made so far wrote none.
   With the reader, and under the same OFFLOAD_RO_LOAD=1, a diffusion model whose every tensor is a plain
   float (bf16, fp16, fp32) and that names no operations of its own is built with ComfyUI's mixed-precision
   layers and an empty quantization config. Stock ComfyUI builds such a model with real weight tensors and,
   under --gpu-only, moves it whole to the offload card (cuda:0) before DisTorch2 splits it
   (comfy.sd.load_diffusion_model_state_dict: model.to(offload_device)), so a DiT bigger than one card runs
   out of memory there (krea2 bf16, 24.48 GiB, on a 15.9 GiB card, 2026-10-20). The mixed-precision layers
   create no weight until the load, and then take the file's tensor as it is (comfy/ops.py,
   _load_quantized_module, a layer with no quantization entry), so the model is built empty, filled from the
   read-only map without a copy, and its blocks reach the cards only through DisTorch2's allocation. That is
   the path every unquantized layer of an fp8mixed checkpoint already takes (the 2511 edit). A GGUF or
   quantized checkpoint (its loader passes its own operations, or the file carries a quantization marker)
   keeps ComfyUI's own path. Here the loaded weights ARE the read-only map until DisTorch2 copies them to a
   card, so the assumption above covers this path too.

2. OffloadFreeForDecode, a pass-through LATENT node that sits between the last sampler and a plain
   VAEDecode. With --gpu-only the offload device IS the card, so ComfyUI's own "unload" frees
   nothing; the experts leave the cards when the last Python reference to their model patchers goes,
   which is when the sampler that used them completes (--cache-none drops its inputs). This node makes
   that certain and visible: it saves the latent first (optional), collects garbage, returns every
   card's cached blocks to the driver, reports each card's memory, and refuses to continue when the
   decode card cannot hold a plain decode, because ComfyUI would then retry the decode tiled with its
   own temporal chunking and the clip would carry the seam the plain decode exists to avoid.

3. OffloadCLIPLoaderDisTorch2, the DisTorch2 text-encoder loader for an encoder that is bigger than one card
   (the 16.33 GiB bf16 Qwen3-VL-8B of qwen-image-2.1). ComfyUI-MultiGPU's own CLIPLoaderDisTorch2MultiGPU
   cannot load it card-only: under --gpu-only ComfyUI builds the whole encoder on the text-encoder card
   (comfy.sd.CLIP.__init__ asks text_encoder_initial_device, which answers the load device when the
   load and offload devices are the same card) and only then does the DisTorch2 allocation apply, so the
   build runs out of memory on a 15.9 GiB card. This node runs the stock loader with that one answer
   changed to the CPU (so the encoder is built and filled off the cards, from the read-only map), then
   places it at once through DisTorch2's own load path, block by block, onto the cards its allocation
   string names, and checks that no weight is left in host RAM.

4. OffloadFreeAfterEncode, a pass-through of the positive and negative CONDITIONING that takes the text
   encoder off every card (the LTX-2.5 card-only graph: the 14.3 GiB Gemma encoder and the 20 GiB int8 DiT do
   not fit the two cards together, and the DiT is placed at the first sampler). Under --gpu-only ComfyUI's
   own unload frees nothing (its offload device IS the card), so the node does it itself: it drops the
   encoder from comfy.model_management.current_loaded_models without going through model_unload (which would
   move the weights to the offload device, the card), replaces every parameter and buffer that sits on a
   card with an empty CPU tensor, returns the cached blocks to the driver, reports each card's memory before
   and after, and raises OFFLOAD-TE-STILL-RESIDENT if any of the encoder's tensors is still on a CUDA device.
   The CLIP is spent after it. The harness loads this pack with --cache-none, so the next prompt builds and
   places the encoder again from the read-only map.
"""
import gc
import json
import logging
import mmap
import os
import struct
import time
import warnings

GIB = 1024 ** 3
# A plain Wan VAE decode of the film latent measured +10.0 GiB against ComfyUI's own estimate of 12.0
# GiB (83 %); the node refuses only below this share of the estimate, never on the estimate itself.
DECODE_SHARE_OF_ESTIMATE = 0.85
# Used when the VAE gives no estimate (a graph that wires no VAE in).
DECODE_FALLBACK_GIB = 10.2

def _say(message):
    # ComfyUI's console goes to the harness's log file; one prefix makes every line findable.
    logging.info("[offload_multicard] %s", message)


# ---------------------------------------------------------------------------------------------
# 1. the read-only safetensors reader
# ---------------------------------------------------------------------------------------------

def _install_readonly_loader():
    import torch
    import comfy.utils as cu
    import comfy.storage

    original = cu.load_torch_file

    def read_only(path):
        shown = os.path.basename(str(path))
        # the map keeps its own duplicate of the file handle (CPython's mmap does on Windows and POSIX alike), so the file object is closed here
        with open(path, "rb") as handle:
            mapped = mmap.mmap(handle.fileno(), 0, access=mmap.ACCESS_READ)
        view = memoryview(mapped)
        try:
            (header_len,) = struct.unpack("<Q", view[:8])
            header = json.loads(bytes(view[8:8 + header_len]).decode("utf-8"))
            base = 8 + header_len
            metadata = header.pop("__metadata__", {})
            data_size = len(mapped) - base
            plan = []
            for name, info in header.items():
                start, end = info["data_offsets"]
                dtype = cu._TYPES[info["dtype"]]
                if start != end:
                    # upstream's reader checks the header against the file; this one says the same in words, because torch's own error for
                    # a short buffer ("shape '[3]' is invalid for input of size 1") names neither the file nor the tensor
                    if start < 0 or end < start or end > data_size:
                        raise ValueError(f"{shown}: tensor {name!r} needs bytes {start}:{end} of the data section but the file holds only "
                                         f"{data_size} bytes after its header (truncated file?)")
                    count = 1
                    for dim in info["shape"]:
                        count *= dim
                    want = count * torch.empty(0, dtype=dtype).element_size()
                    if end - start != want:
                        raise ValueError(f"{shown}: tensor {name!r} has shape {info['shape']} ({want} bytes as {info['dtype']}) "
                                         f"but its data_offsets cover {end - start} bytes: not a valid safetensors file")
                plan.append((name, info, start, end, dtype))
        except BaseException:
            # nothing is cut from the map yet, so it can be closed here instead of waiting for the collector
            view.release()
            mapped.close()
            raise
        state = {}
        for name, info, start, end, dtype in plan:
            if start == end:
                state[name] = torch.empty(info["shape"], dtype=dtype)
                continue
            with warnings.catch_warnings():
                # the map is read-only on purpose; nothing writes into a loaded weight in place
                warnings.filterwarnings("ignore", message="The given buffer is not writable")
                tensor = torch.frombuffer(view[base + start: base + end], dtype=dtype).view(info["shape"])
            # the mapping lives exactly as long as any tensor cut from it
            setattr(tensor.untyped_storage(), "_offload_ro_refs", (mapped, view))
            state[name] = tensor
        _say(f"read-only map of {shown}: {len(state)} tensors, {len(mapped) / GIB:.2f} GiB")
        return state, metadata

    def load_torch_file(ckpt, safe_load=False, device=None, return_metadata=False):
        on_cpu = device is None or getattr(device, "type", "cpu") == "cpu"
        if on_cpu and str(ckpt).lower().endswith((".safetensors", ".sft")):
            state, metadata = read_only(ckpt)
            comfy.storage.annotate_state_dict(state, ckpt)
            return (state, metadata) if return_metadata else state
        return original(ckpt, safe_load=safe_load, device=device, return_metadata=return_metadata)

    cu.load_torch_file = load_torch_file
    _say("read-only safetensors reader installed (a safetensors file is mapped read-only, not copy-on-write)")


def _install_lazy_diffusion_build():
    import torch
    import comfy.ops
    import comfy.sd

    original = comfy.sd.load_diffusion_model_state_dict
    plain = (torch.bfloat16, torch.float16, torch.float32)

    def weight_dtype(sd, metadata):
        # the dtype holding most of the weight bytes, or None when a tensor is not a plain float or the file carries a quantization marker
        if metadata and "_quantization_metadata" in metadata:
            return None
        sizes = {}
        for name, tensor in sd.items():
            if name.endswith((".comfy_quant", "scaled_fp8")) or tensor.dtype not in plain:
                return None
            sizes[tensor.dtype] = sizes.get(tensor.dtype, 0) + tensor.numel() * tensor.element_size()
        return max(sizes, key=sizes.get) if sizes else None

    def load_diffusion_model_state_dict(sd, model_options={}, metadata=None, disable_dynamic=False):
        if model_options.get("custom_operations") is None:
            dtype = weight_dtype(sd, metadata)
            if dtype is not None:
                model_options = dict(model_options, custom_operations=comfy.ops.mixed_precision_ops({}, compute_dtype=dtype))
                _say(f"diffusion model built empty and filled from the map ({len(sd)} tensors, {dtype}), not built on the offload card")
        return original(sd, model_options=model_options, metadata=metadata, disable_dynamic=disable_dynamic)

    comfy.sd.load_diffusion_model_state_dict = load_diffusion_model_state_dict
    _say("lazy diffusion-model build installed (an unquantized DiT is built without weights and takes the read-only map's tensors)")


if os.environ.get("OFFLOAD_RO_LOAD") == "1":
    _install_readonly_loader()
    _install_lazy_diffusion_build()


# ---------------------------------------------------------------------------------------------
# 2. free the cards between the last sampler and a plain decode
# ---------------------------------------------------------------------------------------------

def _cards():
    import torch
    return list(range(torch.cuda.device_count())) if torch.cuda.is_available() else []


def _card_line(index):
    import torch
    free, total = torch.cuda.mem_get_info(index)
    return {
        "card": f"cuda:{index}",
        "free_gib": round(free / GIB, 2),
        "total_gib": round(total / GIB, 2),
        "torch_allocated_gib": round(torch.cuda.memory_allocated(index) / GIB, 2),
        "torch_reserved_gib": round(torch.cuda.memory_reserved(index) / GIB, 2),
    }


def _release_cards():
    import torch
    started = time.time()
    gc.collect()
    try:
        import comfy.model_management as mm
        mm.cleanup_models()
    except Exception as err:  # a missing helper must not stop the release itself
        _say(f"cleanup_models skipped: {err}")
    gc.collect()
    for index in _cards():
        with torch.cuda.device(index):
            torch.cuda.synchronize()
            torch.cuda.empty_cache()
            torch.cuda.ipc_collect()
    return time.time() - started


def _decode_need_gib(samples, vae):
    estimate = getattr(vae, "memory_used_decode", None) if vae is not None else None
    dtype = getattr(vae, "vae_dtype", None) if vae is not None else None
    if callable(estimate) and dtype is not None:
        try:
            return DECODE_SHARE_OF_ESTIMATE * float(estimate(samples["samples"].shape, dtype)) / GIB
        except Exception as err:
            _say(f"the VAE gave no decode estimate ({err}); using {DECODE_FALLBACK_GIB} GiB")
    return DECODE_FALLBACK_GIB


class OffloadFreeForDecode:
    """Pass-through LATENT. Saves the latent when `save_prefix` is set, frees every card, checks the decode card."""

    @classmethod
    def INPUT_TYPES(cls):
        return {
            "required": {"samples": ("LATENT",)},
            "optional": {"vae": ("VAE",), "save_prefix": ("STRING", {"default": ""})},
        }

    RETURN_TYPES = ("LATENT",)
    FUNCTION = "free"
    CATEGORY = "offload"

    def free(self, samples, vae=None, save_prefix=""):
        ui = {}
        saved = ""
        if save_prefix:
            import nodes
            ui = nodes.SaveLatent().save(samples, save_prefix).get("ui", {})
            files = ui.get("latents") or []
            if files:
                first = files[0]
                saved = os.path.join(first.get("subfolder", ""), first["filename"])
                _say(f"latent saved to the output directory as {saved}")
        if not _cards():
            _say("no CUDA card is visible; nothing to free")
            return {"ui": ui, "result": (samples,)}

        before = [_card_line(i) for i in _cards()]
        seconds = _release_cards()
        after = [_card_line(i) for i in _cards()]
        _say(f"cards before the free: {json.dumps(before)}")
        _say(f"cards after the free ({seconds:.1f} s): {json.dumps(after)}")

        device = getattr(vae, "device", None)
        index = getattr(device, "index", None)
        if index is None:
            index = _cards()[0]
        need = _decode_need_gib(samples, vae)
        free = next(c["free_gib"] for c in after if c["card"] == f"cuda:{index}")
        if free < need:
            where = f" The sampled latent is saved as {saved} in ComfyUI's output directory." if saved else ""
            raise RuntimeError(
                f"OFFLOAD-DECODE-NOFIT: cuda:{index} has {free:.2f} GiB free after the experts were released, "
                f"and a plain decode of this latent needs about {need:.2f} GiB. ComfyUI would retry the decode tiled "
                f"with its own temporal chunking, which can seam the clip, so the run stops here.{where} cards: {json.dumps(after)}"
            )
        _say(f"cuda:{index} has {free:.2f} GiB free for the plain decode (needs about {need:.2f} GiB)")
        return {"ui": ui, "result": (samples,)}


# ---------------------------------------------------------------------------------------------
# 3. a text encoder bigger than one card, built off the cards and placed block by block
# ---------------------------------------------------------------------------------------------

# Parameters and buffers that may stay on the CPU after the placement (a few small tensors DisTorch2's load list does not move); more than this
# is a weight left in host RAM and the node refuses.
HOST_WEIGHT_LIMIT_BYTES = 64 * 1024 ** 2


def _stock_clip_loader():
    # looked up when asked, never at import: ComfyUI loads the packs in no fixed order
    import nodes
    stock = nodes.NODE_CLASS_MAPPINGS.get("CLIPLoaderDisTorch2MultiGPU")
    if stock is None:
        raise RuntimeError("OFFLOAD-TE-NOPACK: ComfyUI has no CLIPLoaderDisTorch2MultiGPU (the ComfyUI-MultiGPU pack), which this node wraps")
    return stock


def _weight_bytes_by_device(module):
    totals = {}
    for tensor in list(module.parameters()) + list(module.buffers()):
        key = str(tensor.device)
        totals[key] = totals.get(key, 0) + tensor.numel() * tensor.element_size()
    return totals


def _gib_line(totals):
    return ", ".join(f"{name} {size / GIB:.2f} GiB" for name, size in sorted(totals.items())) or "no weights"


class OffloadCLIPLoaderDisTorch2:
    """CLIP loader. Builds the text encoder off the cards, then places its blocks on the cards `expert_mode_allocations` names (DisTorch2)."""

    @classmethod
    def INPUT_TYPES(cls):
        # the stock loader's own inputs (its `type` list follows the installed ComfyUI), without the two the allocation string makes moot
        stock = _stock_clip_loader().INPUT_TYPES()
        optional = {k: v for k, v in stock.get("optional", {}).items() if k not in ("virtual_vram_gb", "donor_device")}
        return {"required": dict(stock["required"]), "optional": optional}

    RETURN_TYPES = ("CLIP",)
    FUNCTION = "load"
    CATEGORY = "offload"

    def load(self, clip_name, type, device="cuda:0", expert_mode_allocations="", eject_models=False):
        import torch
        import comfy.model_management as mm

        if not str(expert_mode_allocations).strip():
            raise ValueError("OFFLOAD-TE-NOSPLIT: expert_mode_allocations is empty, so this would build the whole encoder on one card, which is what this node exists to avoid")
        stock = _stock_clip_loader()()
        original = mm.text_encoder_initial_device

        def off_card(load_device, offload_device, model_size=0):
            _say(f"text encoder {clip_name}: {model_size / GIB:.2f} GiB built off the cards (load device {load_device}), not on {load_device} as --gpu-only would")
            return torch.device("cpu")

        mm.text_encoder_initial_device = off_card
        try:
            out = stock.override(clip_name=clip_name, type=type, device=device, virtual_vram_gb=0.0, donor_device="cpu",
                                 expert_mode_allocations=expert_mode_allocations, eject_models=eject_models)
        finally:
            mm.text_encoder_initial_device = original

        clip = out[0]
        model = clip.patcher.model
        built = _weight_bytes_by_device(model)
        _say(f"text encoder {clip_name} built: {_gib_line(built)}")
        started = time.time()
        # the load ComfyUI makes itself when the build was on the load device (comfy.sd.CLIP.__init__), now through DisTorch2's allocation
        mm.load_models_gpu([clip.patcher], force_full_load=True)
        gc.collect()
        placed = _weight_bytes_by_device(model)
        _say(f"text encoder {clip_name} placed in {time.time() - started:.1f} s by {expert_mode_allocations!r} (compute card {device}): {_gib_line(placed)}")
        host = sum(size for name, size in placed.items() if name.startswith("cpu"))
        if host > HOST_WEIGHT_LIMIT_BYTES:
            raise RuntimeError(
                f"OFFLOAD-TE-HOSTWEIGHT: {host / GIB:.2f} GiB of the text encoder {clip_name} is still in host RAM after the placement "
                f"({_gib_line(placed)}); a card-only run keeps no weight in host RAM, so it stops here")
        return out


# ---------------------------------------------------------------------------------------------
# 4. take the text encoder off the cards once the prompt is encoded
# ---------------------------------------------------------------------------------------------

def _drop_from_loaded_models(model):
    # ComfyUI's own unload would move the weights to the offload device, which under --gpu-only is the card itself (and, for a DisTorch2 placement,
    # would first try to put the whole encoder on one card). So the entry is removed from the list without going through model_unload; a patcher
    # clone (the same module under another patcher) is matched by its module.
    import comfy.model_management as mm
    dropped = 0
    for index in reversed(range(len(mm.current_loaded_models))):
        loaded = mm.current_loaded_models[index]
        patcher = loaded.model
        if patcher is not None and getattr(patcher, "model", None) is model:
            mm.current_loaded_models.pop(index)
            finalizer = getattr(loaded, "model_finalizer", None)
            if finalizer is not None:
                finalizer.detach()
            loaded.model_finalizer = None
            loaded.real_model = None
            dropped += 1
    return dropped


def _free_card_weights(model):
    # Every parameter and buffer that sits on a card is replaced by an empty CPU tensor, so the card's allocation goes when the last reference to the
    # weight does. The encoder is spent after this: nothing may encode with it again.
    import torch
    freed = {}
    for module in model.modules():
        for table, empty in ((module._parameters, lambda: torch.nn.Parameter(torch.empty(0), requires_grad=False)), (module._buffers, lambda: torch.empty(0))):
            for name, tensor in list(table.items()):
                if tensor is not None and tensor.device.type == "cuda":
                    key = str(tensor.device)
                    freed[key] = freed.get(key, 0) + tensor.numel() * tensor.element_size()
                    table[name] = empty()
    return freed


class OffloadFreeAfterEncode:
    """Pass-through CONDITIONING pair. Takes the text encoder (`clip`) off every card; wire the prompt's conditioning through it so it runs before the sampler."""

    @classmethod
    def INPUT_TYPES(cls):
        return {"required": {"positive": ("CONDITIONING",), "negative": ("CONDITIONING",), "clip": ("CLIP",)}}

    RETURN_TYPES = ("CONDITIONING", "CONDITIONING")
    RETURN_NAMES = ("positive", "negative")
    FUNCTION = "release"
    CATEGORY = "offload"

    def release(self, positive, negative, clip):
        patcher = getattr(clip, "patcher", None)
        model = getattr(patcher, "model", None)
        if model is None:
            raise ValueError("OFFLOAD-TE-NOMODEL: the CLIP input carries no text-encoder model (clip.patcher.model), so there is nothing to release")
        before = [_card_line(i) for i in _cards()]
        held = {name: size for name, size in _weight_bytes_by_device(model).items() if name.startswith("cuda")}
        dropped = _drop_from_loaded_models(model)
        backup = getattr(patcher, "backup", None)
        if isinstance(backup, dict):
            backup.clear()
        freed = _free_card_weights(model)
        seconds = _release_cards() if _cards() else 0.0
        after = [_card_line(i) for i in _cards()]
        _say(f"text encoder released: {_gib_line(held)} on the cards, {_gib_line(freed)} freed, {dropped} loaded-model entr{'y' if dropped == 1 else 'ies'} dropped ({seconds:.1f} s)")
        _say(f"cards before the release: {json.dumps(before)}")
        _say(f"cards after the release: {json.dumps(after)}")
        left = {name: size for name, size in _weight_bytes_by_device(model).items() if name.startswith("cuda") and size > 0}
        if left:
            raise RuntimeError(
                f"OFFLOAD-TE-STILL-RESIDENT: {_gib_line(left)} of the text encoder is still on a card after the release, and the DiT placed at the first "
                f"sampler needs the room; the run stops here. cards: {json.dumps(after)}")
        return (positive, negative)


NODE_CLASS_MAPPINGS = {"OffloadFreeForDecode": OffloadFreeForDecode, "OffloadCLIPLoaderDisTorch2": OffloadCLIPLoaderDisTorch2, "OffloadFreeAfterEncode": OffloadFreeAfterEncode}
NODE_DISPLAY_NAME_MAPPINGS = {
    "OffloadFreeForDecode": "Free the cards for a plain decode (harness)",
    "OffloadCLIPLoaderDisTorch2": "Load CLIP, built off the cards and split by DisTorch2 (harness)",
    "OffloadFreeAfterEncode": "Take the text encoder off the cards after the prompt is encoded (harness)",
}
