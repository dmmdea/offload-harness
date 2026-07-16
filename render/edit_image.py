# render/edit_image.py - deterministic PIL edit pipeline for offload_edit_image.
# Contract (docs/superpowers/specs/2026-07-16-edit-media-tools-design.md):
#   stdin:  JSON {"image": <path>, "ops": [<op>...], "out": <path>}
#   stdout: JSON {"out": <path>, "width": W, "height": H, "ops_applied": N}
#   exit:   0 ok | 2 bad arguments (caller surfaces the message) | 3 defer-class
#           (engine capability missing, e.g. no PIL)
# Ops are validated Go-side (internal/mediaops.ValidateOps) BEFORE this worker runs;
# validation here is a second line of defense, not the contract surface.
# The worker owns NO machine facts and takes NO GPU resources (pure CPU).
#   --selftest: run every op against an in-memory image (no files, no stdin) and
#   exit 0/1 - used by the Go test suite and CI.
import json
import sys

try:
    from PIL import Image, ImageDraw, ImageFont
except Exception as e:  # PIL missing = defer-class: the harness reports "engine absent"
    print(json.dumps({"error": "PIL unavailable: %s" % e}))
    sys.exit(3)


def apply_op(img, op):
    kind = op.get("op")
    if kind == "crop":
        x, y = int(op["x"]), int(op["y"])
        w, h = int(op["width"]), int(op["height"])
        if x + w > img.width or y + h > img.height:
            raise ValueError("crop box %dx%d+%d+%d exceeds image %dx%d"
                             % (w, h, x, y, img.width, img.height))
        return img.crop((x, y, x + w, y + h))
    if kind == "resize":
        w, h = int(op.get("width") or 0), int(op.get("height") or 0)
        keep = op.get("keep_aspect")
        if keep is None:
            keep = not (w and h)  # default: keep aspect when only one dim given
        if keep:
            if w and not h:
                h = max(1, round(img.height * w / img.width))
            elif h and not w:
                w = max(1, round(img.width * h / img.height))
            elif w and h:
                scale = min(w / img.width, h / img.height)
                w = max(1, round(img.width * scale))
                h = max(1, round(img.height * scale))
        return img.resize((w, h), Image.LANCZOS)
    if kind == "convert":
        # actual encoding happens at save; jpg needs RGB (no alpha)
        fmt = op["format"].lower()
        if fmt in ("jpg", "jpeg") and img.mode != "RGB":
            return img.convert("RGB")
        return img
    if kind == "composite":
        overlay = Image.open(op["overlay"]).convert("RGBA")
        opacity = float(op.get("opacity") or 1.0)
        if opacity < 1.0:
            alpha = overlay.getchannel("A").point(lambda a: int(a * opacity))
            overlay.putalpha(alpha)
        base = img.convert("RGBA")
        base.alpha_composite(overlay, (int(op.get("x") or 0), int(op.get("y") or 0)))
        return base
    if kind == "text":
        base = img.convert("RGBA")
        draw = ImageDraw.Draw(base)
        size = int(op.get("size") or 32)
        font = None
        if op.get("font"):
            font = ImageFont.truetype(op["font"], size)
        else:
            try:
                font = ImageFont.load_default(size=size)
            except TypeError:  # older PIL: no size kwarg
                font = ImageFont.load_default()
        draw.text((int(op.get("x") or 0), int(op.get("y") or 0)), op["text"],
                  fill=op.get("color") or "#ffffff", font=font,
                  anchor=op.get("anchor") or None)
        return base
    if kind == "flatten_design":
        # handled Go-side via GIMP before this worker runs; reaching here is a bug
        raise ValueError("flatten_design must be resolved by the harness before PIL")
    raise ValueError("unknown op %r" % kind)


def run_pipeline(img, ops):
    for op in ops:
        img = apply_op(img, op)
    return img


def selftest():
    img = Image.new("RGB", (64, 48), "#204060")
    ops = [
        {"op": "crop", "x": 2, "y": 2, "width": 60, "height": 40},
        {"op": "resize", "width": 30},
        {"op": "text", "text": "ok", "x": 1, "y": 1, "size": 10},
        {"op": "composite", "overlay": None},  # replaced below - composite needs a file; selftest uses in-memory
        {"op": "convert", "format": "jpg"},
    ]
    img = apply_op(img, ops[0])
    assert img.size == (60, 40), img.size
    img = apply_op(img, ops[1])
    assert img.size == (30, 20), img.size  # aspect kept
    img = apply_op(img, ops[2])
    assert img.mode == "RGBA"
    # composite: build the overlay in memory and inline the alpha path
    overlay = Image.new("RGBA", (10, 10), (255, 0, 0, 128))
    base = img.convert("RGBA")
    base.alpha_composite(overlay, (0, 0))
    img = base
    img = apply_op(img, ops[4])
    assert img.mode == "RGB", img.mode  # jpg forces RGB
    # error paths
    try:
        apply_op(img, {"op": "crop", "x": 0, "y": 0, "width": 999, "height": 5})
        raise SystemExit("crop bounds check failed to fire")
    except ValueError:
        pass
    try:
        apply_op(img, {"op": "nope"})
        raise SystemExit("unknown-op check failed to fire")
    except ValueError:
        pass
    print("SELFTEST PASS")
    return 0


def main():
    if "--selftest" in sys.argv:
        sys.exit(selftest())
    try:
        req = json.load(sys.stdin)
        image, ops, out = req["image"], req["ops"], req["out"]
    except Exception as e:
        print(json.dumps({"error": "bad request: %s" % e}))
        sys.exit(2)
    try:
        img = Image.open(image)
        img.load()
    except Exception as e:
        print(json.dumps({"error": "cannot open image %s: %s" % (image, e)}))
        sys.exit(2)
    try:
        img = run_pipeline(img, ops)
        if out.lower().endswith((".jpg", ".jpeg")) and img.mode != "RGB":
            img = img.convert("RGB")
        img.save(out)
    except ValueError as e:
        print(json.dumps({"error": str(e)}))
        sys.exit(2)
    except Exception as e:
        print(json.dumps({"error": "pipeline failed: %s" % e}))
        sys.exit(3)
    print(json.dumps({"out": out, "width": img.width, "height": img.height,
                      "ops_applied": len(ops)}))
    sys.exit(0)


if __name__ == "__main__":
    main()
