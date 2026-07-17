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


def _levels_lut(black=0, white=255, gamma=1.0):
    # float LUT for a levels adjustment; quantized ONCE by _compose8 (banding discipline)
    span = max(1, white - black)
    lut = []
    for i in range(256):
        v = min(1.0, max(0.0, (i - black) / span)) ** (1.0 / gamma)
        lut.append(v * 255.0)
    return lut


def _curve_lut(points):
    # piecewise-linear through sorted [in,out] control points, endpoints clamped
    pts = sorted((float(a), float(b)) for a, b in points)
    if pts[0][0] > 0:
        pts.insert(0, (0.0, pts[0][1]))
    if pts[-1][0] < 255:
        pts.append((255.0, pts[-1][1]))
    lut, seg = [], 0
    for i in range(256):
        while seg < len(pts) - 2 and i > pts[seg + 1][0]:
            seg += 1
        (x0, y0), (x1, y1) = pts[seg], pts[seg + 1]
        t = 0.0 if x1 == x0 else (i - x0) / (x1 - x0)
        lut.append(y0 + t * (y1 - y0))
    return lut


def _compose8(*float_luts):
    # compose float LUTs then quantize ONCE - chaining .point() calls compounds
    # 8-bit rounding into visible banding (compose-then-quantize discipline)
    out = []
    for i in range(256):
        v = float(i)
        for lut in float_luts:
            j = min(255, max(0, int(v)))
            frac = v - j
            hi = lut[min(255, j + 1)]
            v = lut[j] * (1 - frac) + hi * frac  # linear interp keeps float precision
        out.append(min(255, max(0, int(round(v)))))
    return out


def _load_cube(path):
    # vendored .cube parser: Pillow ships Color3DLUT (>=5.2) but NO .cube loader.
    # Values must be 0-1 floats; 1D LUTs and non-standard domains are rejected.
    size, table = None, []
    with open(path, "r") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or line.startswith("TITLE"):
                continue
            if line.startswith("LUT_1D_SIZE"):
                raise ValueError("1D .cube LUTs are not supported (need LUT_3D_SIZE)")
            if line.startswith("DOMAIN_MIN") or line.startswith("DOMAIN_MAX"):
                want = (0, 0, 0) if line.startswith("DOMAIN_MIN") else (1, 1, 1)
                if any(abs(float(v) - d) > 1e-6 for v, d in zip(line.split()[1:4], want)):
                    raise ValueError("non-standard DOMAIN_MIN/MAX .cube not supported")
                continue
            if line.startswith("LUT_3D_SIZE"):
                size = int(line.split()[-1])
                continue
            if line[0].isdigit() or line[0] in "-.":
                r, g, b = map(float, line.split()[:3])
                table.extend([r, g, b])
    if not size or len(table) != size ** 3 * 3:
        raise ValueError("malformed .cube: size=%r entries=%d" % (size, len(table) // 3))
    return size, table


def _find_coeffs(pa, pb):
    # perspective coefficients mapping dest quad pa (UL,UR,LR,LL on the target) to
    # overlay corners pb. Pure-python partial-pivot Gauss solver (no numpy).
    A, B = [], []
    for (x, y), (X, Y) in zip(pa, pb):
        A.append([x, y, 1, 0, 0, 0, -X * x, -X * y])
        B.append(X)
        A.append([0, 0, 0, x, y, 1, -Y * x, -Y * y])
        B.append(Y)
    for col in range(8):  # gaussian elimination, partial pivoting
        piv = max(range(col, 8), key=lambda r: abs(A[r][col]))
        if abs(A[piv][col]) < 1e-12:
            raise ValueError("degenerate quad (collinear corners)")
        A[col], A[piv] = A[piv], A[col]
        B[col], B[piv] = B[piv], B[col]
        for r in range(col + 1, 8):
            f = A[r][col] / A[col][col]
            for c in range(col, 8):
                A[r][c] -= f * A[col][c]
            B[r] -= f * B[col]
    x = [0.0] * 8
    for i in range(7, -1, -1):
        x[i] = (B[i] - sum(A[i][j] * x[j] for j in range(i + 1, 8))) / A[i][i]
    return x


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
    if kind == "mask_boxes":
        # Build a white-on-black inpaint mask AT THE IMAGE'S SIZE. Replaces the
        # working image: chain as the only op (or last) and save to the mask path.
        # Same contract as offload_inpaint_image's mask: white = repaint.
        try:
            from PIL import ImageFilter
        except Exception as e:
            raise ValueError("mask_boxes needs PIL ImageFilter: %s" % e)
        boxes = op.get("boxes") or []
        if not boxes:
            raise ValueError("mask_boxes requires a non-empty boxes array")
        pad = int(op.get("pad") or 0)
        mask = Image.new("L", (img.width, img.height), 0)
        draw = ImageDraw.Draw(mask)
        for b in boxes:
            x, y = int(b["x"]) - pad, int(b["y"]) - pad
            w, h = int(b["width"]) + 2 * pad, int(b["height"]) + 2 * pad
            draw.rectangle((max(0, x), max(0, y),
                            min(img.width, x + w), min(img.height, y + h)), fill=255)
        feather = int(op.get("feather") or 0)
        if feather > 0:
            mask = mask.filter(ImageFilter.GaussianBlur(feather))
        if op.get("invert"):
            mask = mask.point(lambda v: 255 - v)
        return mask.convert("RGB")
    if kind == "grade":
        # tone/color grade: all transforms compose into ONE float LUT per channel
        # and quantize once in a single .point() call. Alpha is never remapped.
        from PIL import ImageStat
        base = img.convert("RGBA") if "A" in img.getbands() else img.convert("RGB")
        alpha = base.getchannel("A") if base.mode == "RGBA" else None
        rgb = base.convert("RGB")
        shared = []
        # PRESENCE semantics, matching Go's ValidateOps (review finding: Go accepted
        # "levels":{} but the truthy check here treated {} as absent and raised —
        # validated-then-crashes). An empty levels object is an identity adjustment.
        if op.get("levels") is not None:
            L = op["levels"]
            shared.append(_levels_lut(int(L.get("black", 0)), int(L.get("white", 255)),
                                      float(L.get("gamma", 1.0))))
        if op.get("curve"):
            shared.append(_curve_lut(op["curve"]["points"]))
        if not shared and op.get("wb") is None:
            raise ValueError("grade requires at least one of levels/curve/wb")
        if op.get("luminance_only") and shared:
            y, cb, cr = rgb.convert("YCbCr").split()
            y = y.point(_compose8(*shared))
            rgb = Image.merge("YCbCr", (y, cb, cr)).convert("RGB")
            shared = []
        scales = (1.0, 1.0, 1.0)
        wb = op.get("wb")
        if wb:
            if wb.get("mode") == "gray_world":
                means = [ImageStat.Stat(c).mean[0] for c in rgb.split()]
                target = sum(means) / 3.0
                scales = tuple(target / m if m > 0 else 1.0 for m in means)
            else:
                scales = (float(wb.get("r", 1)), float(wb.get("g", 1)), float(wb.get("b", 1)))
        chans = []
        for c, s in zip(rgb.split(), scales):
            luts = list(shared)
            if s != 1.0:
                luts.append([min(255.0, i * s) for i in range(256)])
            chans.append(c.point(_compose8(*luts)) if luts else c)
        rgb = Image.merge("RGB", chans)
        if alpha is not None:
            rgb = rgb.convert("RGBA")
            rgb.putalpha(alpha)
        return rgb
    if kind == "lut_cube":
        from PIL import ImageFilter
        size, table = _load_cube(op["path"])
        alpha = img.getchannel("A") if "A" in img.getbands() else None
        rgb = img.convert("RGB")
        graded = rgb.filter(ImageFilter.Color3DLUT(size, table))
        s = float(op.get("strength", 1.0))
        if s < 1.0:
            graded = Image.blend(rgb, graded, max(0.0, s))
        if alpha is not None:
            graded = graded.convert("RGBA")
            graded.putalpha(alpha)
        return graded
    if kind == "perspective_composite":
        # mockup placement: warp the overlay into the destination quad
        # (UL,UR,LR,LL winding) and alpha-composite it over the working image.
        overlay = Image.open(op["overlay"]).convert("RGBA")
        quad = [tuple(map(float, p)) for p in op["quad"]]
        if len(quad) != 4:
            raise ValueError("quad needs exactly 4 [x,y] corners (UL,UR,LR,LL)")
        w, h = overlay.size
        coeffs = _find_coeffs(quad, [(0, 0), (w, 0), (w, h), (0, h)])
        base = img.convert("RGBA")
        warped = overlay.transform(base.size, Image.PERSPECTIVE, coeffs, Image.BICUBIC)
        return Image.alpha_composite(base, warped)  # paste would black the corners
    if kind == "finish":
        # delivery finishing: optional median speckle clean, then unsharp mask with
        # post-AI-upscale web defaults (radius 1.2 / percent 80 / threshold 3 —
        # Pillow's 150% default is tuned for camera-native files and over-crisps
        # upscaler output; radius >3 halos). MUST run as the LAST op, after any
        # resize — sharpening before a resize is undone by resampling.
        # Real sensor/upscaler noise reduction (NLM/BM3D-class) is OUT of PIL's
        # reach and deliberately not faked here.
        from PIL import ImageFilter
        out = img
        med = int(op.get("median") or 0)
        if med:
            if med not in (3, 5):
                raise ValueError("median must be 3 or 5")
            out = out.filter(ImageFilter.MedianFilter(size=med))
        # sharpen: absent -> tuned defaults; EXPLICIT null -> skip entirely (a
        # median-only finish, per the plan); explicit per-field 0 honored (percent 0
        # = no visible sharpening — the Go-path equivalent, since null cannot
        # survive the Go struct round-trip). `or`-defaults would silently turn an
        # explicit 0 back into the default (review finding).
        sh_raw = op.get("sharpen", False)
        if sh_raw is None:
            return out
        sh = sh_raw or {}
        def _dflt(v, d):
            return d if v is None else v
        out = out.filter(ImageFilter.UnsharpMask(
            radius=float(_dflt(sh.get("radius"), 1.2)),
            percent=int(_dflt(sh.get("percent"), 80)),
            threshold=int(_dflt(sh.get("threshold"), 3))))
        return out
    if kind in ("flatten_design", "instantiate_design"):
        # handled Go-side via GIMP before this worker runs; reaching here is a bug
        raise ValueError("%s must be resolved by the harness before PIL" % kind)
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
    # mask_boxes: white inside the (padded) box, black outside, image-sized
    m = apply_op(Image.new("RGB", (100, 80), "#000000"),
                 {"op": "mask_boxes", "boxes": [{"x": 10, "y": 10, "width": 30, "height": 20}], "pad": 2})
    assert m.size == (100, 80)
    assert m.getpixel((25, 20)) == (255, 255, 255), "inside box must be white"
    assert m.getpixel((90, 70)) == (0, 0, 0), "outside box must be black"
    # grade: identity levels leave pixels unchanged; wb scale doubles clamped;
    # RGBA alpha survives byte-identically; empty grade raises
    g = Image.new("RGB", (8, 8), (100, 50, 25))
    out = apply_op(g, {"op": "grade", "levels": {"black": 0, "white": 255, "gamma": 1.0}})
    assert out.getpixel((4, 4)) == (100, 50, 25), "identity grade must not change pixels: %r" % (out.getpixel((4, 4)),)
    out = apply_op(g, {"op": "grade", "wb": {"mode": "scale", "r": 2.0, "g": 1.0, "b": 2.0}})
    px = out.getpixel((4, 4))
    assert px[0] == 200 and px[1] == 50, "wb scale r=2 must double red: %r" % (px,)
    big = apply_op(Image.new("RGB", (8, 8), (200, 50, 25)), {"op": "grade", "wb": {"mode": "scale", "r": 2.0}})
    assert big.getpixel((4, 4))[0] == 255, "wb scale must clamp at 255"
    rgba = Image.new("RGBA", (8, 8), (100, 50, 25, 128))
    out = apply_op(rgba, {"op": "grade", "levels": {"gamma": 1.4}})
    assert out.mode == "RGBA" and out.getchannel("A").tobytes() == rgba.getchannel("A").tobytes(), \
        "grade must never remap the alpha band"
    try:
        apply_op(g, {"op": "grade"})
        raise SystemExit("empty grade must raise")
    except ValueError:
        pass
    # lut_cube: an identity 2x2x2 cube leaves pixels unchanged; 1D LUTs are rejected
    import os
    import tempfile
    identity = "LUT_3D_SIZE 2\n" + "\n".join(
        "%f %f %f" % (r, g, b)
        for b in (0.0, 1.0) for g in (0.0, 1.0) for r in (0.0, 1.0))
    with tempfile.TemporaryDirectory() as td:
        cube = os.path.join(td, "identity.cube")
        with open(cube, "w") as f:
            f.write(identity)
        out = apply_op(Image.new("RGB", (8, 8), (100, 50, 25)), {"op": "lut_cube", "path": cube})
        assert out.getpixel((4, 4)) == (100, 50, 25), \
            "identity cube must not change pixels: %r" % (out.getpixel((4, 4)),)
        oned = os.path.join(td, "oned.cube")
        with open(oned, "w") as f:
            f.write("LUT_1D_SIZE 2\n0 0 0\n1 1 1\n")
        try:
            apply_op(Image.new("RGB", (4, 4)), {"op": "lut_cube", "path": oned})
            raise SystemExit("1D cube must raise")
        except ValueError:
            pass
    # perspective_composite: a red square lands inside the quad, leaves the outside
    # untouched; a collinear quad raises "degenerate"
    with tempfile.TemporaryDirectory() as td:
        red = os.path.join(td, "red.png")
        Image.new("RGBA", (10, 10), (255, 0, 0, 255)).save(red)
        canvas = Image.new("RGB", (100, 80), (0, 0, 64))
        out = apply_op(canvas, {"op": "perspective_composite", "overlay": red,
                                "quad": [[20, 20], [60, 25], [58, 60], [18, 55]]})
        px = out.getpixel((40, 40))
        assert px[0] > 200 and px[1] < 50, "inside the quad must be red: %r" % (px,)
        assert out.getpixel((90, 70))[:3] == (0, 0, 64), "outside the quad must be untouched"
        try:
            apply_op(canvas, {"op": "perspective_composite", "overlay": red,
                              "quad": [[0, 0], [10, 10], [20, 20], [30, 30]]})
            raise SystemExit("collinear quad must raise")
        except ValueError as e:
            assert "degenerate" in str(e), str(e)
    # finish: default delivery sharpen changes an edged image; median=4 raises
    edged = Image.new("RGB", (32, 32), (40, 40, 40))
    ImageDraw.Draw(edged).rectangle((8, 8, 24, 24), fill=(220, 220, 220))
    out = apply_op(edged, {"op": "finish"})
    assert out.tobytes() != edged.tobytes(), "bare finish must sharpen (change pixels)"
    out = apply_op(edged, {"op": "finish", "median": 3})
    assert out.size == edged.size
    # review fixes: explicit sharpen null = median-only (identical to a bare median
    # filter); explicit percent 0 = no visible sharpening; grade levels {} = valid
    # identity (presence semantics matching Go validation).
    from PIL import ImageFilter as _IF
    med_only = apply_op(edged, {"op": "finish", "median": 3, "sharpen": None})
    assert med_only.tobytes() == edged.filter(_IF.MedianFilter(size=3)).tobytes(), \
        "sharpen:null must be median-only"
    nosharp = apply_op(edged, {"op": "finish", "sharpen": {"percent": 0}})
    assert nosharp.tobytes() == edged.tobytes(), "percent 0 must not change pixels"
    ident = apply_op(edged, {"op": "grade", "levels": {}})
    assert ident.tobytes() == edged.tobytes(), "grade levels {} must be identity, not raise"
    try:
        apply_op(edged, {"op": "finish", "median": 4})
        raise SystemExit("median=4 must raise")
    except ValueError:
        pass
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
