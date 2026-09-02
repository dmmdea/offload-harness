# 05 — Editing playbook: designer vocabulary → GIMP operations

Everything below ran headless on the Qube (GIMP 3.2.4) on 2026-09-01 unless tagged [doc]/[community]/[inferred].
Pattern for every job: **load → (add alpha) → geometry → layers/text/effects → resolution → export → verify by file → delete image**.
Colours are `Gegl.Color.new("#rrggbb" | "white" | "rgb(0..1,0..1,0..1)")`; sizes are integer pixels; opacity 0–100 on layers, 0–1 in GEGL ops.

## Geometry
| Designer says | Do | Notes |
|---|---|---|
| "resize to 1280×720" (stretch) | `Gimp.context_set_interpolation(Gimp.InterpolationType.LOHALO); img.scale(1280, 720)` | scales all layers; 0.3 s for 1600→1280; LOHALO/NOHALO = best downscale, CUBIC fine for upscales [doc] |
| "fit inside W×H, keep aspect" | `s=min(W/w, H/h); img.scale(round(w*s), round(h*s))` then optionally canvas (below) | compute in Python; GIMP has no fit-to-box call for images (`scale_to_fit` exists only as an MCP tool) |
| "fill W×H, crop excess" (cover) | `s=max(W/w,H/h); img.scale(round(w*s),round(h*s)); img.crop(W,H,(img.get_width()-W)//2,(img.get_height()-H)//2)` | |
| "crop to x,y,w,h" | `img.crop(w, h, x, y)` — **args are (w, h, offx, offy)**, returns False (no raise) when out of range | crop to selection: `ok,ne,x1,y1,x2,y2 = Gimp.Selection.bounds(img); img.crop(x2-x1, y2-y1, x1, y1)` |
| "trim empty borders" | `img.autocrop(layer)` / `img.autocrop_selected_layers(layer)` | |
| "make the canvas 1280×720 without scaling" | `img.resize(1280, 720, offx, offy)` where off = (new−old)//2 to centre; then per layer `layer.resize_to_image_size()` | **add alpha first** or the new area becomes the BACKGROUND colour (white) on layers without alpha [measured pixel (1,1,1,1)] |
| "rotate 90/180" | `img.rotate(Gimp.RotationType.DEGREES90)`; arbitrary: `layer.transform_rotate(math.radians(15), True, 0, 0)` (1.5 s at 1280×720) | `Gimp.context_set_interpolation` applies; transform_rotate returns the (possibly new) item |
| "flip" | `img.flip(Gimp.OrientationType.HORIZONTAL)` or `layer.transform_flip_simple(orient, True, 0)` | |
| "scale just this layer" | `layer.scale(w, h, False)`; move with `layer.set_offsets(x, y)` | `False` = scale around layer origin |
| "300 dpi / print size" | `img.set_resolution(300.0, 300.0)` before export | written to PNG pHYs, JPEG JFIF, TIFF, BMP [measured]; WebP/AVIF/GIF carry none. A JPEG without density loaded at GIMP's default (300 dpi, from the default template) [measured, cause inferred] |

## Layers, compositing, masks
| Designer says | Do |
|---|---|
| "put this on top" | `lay = Gimp.Layer.new(img, "name", w, h, Gimp.ImageType.RGBA_IMAGE, 100.0, Gimp.LayerMode.NORMAL); img.insert_layer(lay, None, 0)` (0 = top; `len(img.get_layers())` = bottom) |
| "bring in another image as a layer" (logo, product cut-out) | `lay = Gimp.file_load_layer(Gimp.RunMode.NONINTERACTIVE, img, Gio.File.new_for_path(p)); img.insert_layer(lay, None, 0); lay.set_offsets(x, y)` — a 300×120 RGBA PNG arrived as a layer named after the file with alpha intact, 0.23 s [measured] |
| "solid colour background" | new layer at bottom → `Gimp.context_set_foreground(col); lay.fill(Gimp.FillType.FOREGROUND)` (`edit_fill` respects a selection, `fill` ignores it) |
| "gradient background" | `Gimp.context_set_foreground(c1); Gimp.context_set_background(c2); Gimp.context_set_gradient_fg_bg_rgb()` then `lay.edit_gradient_fill(Gimp.GradientType.LINEAR, 0.0, False, 1, 0.0, True, x1, y1, x2, y2)` — vertical top→bottom fill measured 0.45 s at 1600×900, top pixel ≈ FG, bottom ≈ BG [measured]; other stock gradients via `Gimp.context_set_gradient(Gimp.Gradient.get_by_name("…"))` |
| "50 % opacity, multiply" | `lay.set_opacity(50.0); lay.set_mode(Gimp.LayerMode.MULTIPLY)` |
| "mask it with its alpha / with the selection" | `m = lay.create_mask(Gimp.AddMaskType.ALPHA \| SELECTION \| WHITE); lay.add_mask(m)`; bake: `lay.remove_mask(Gimp.MaskApplyMode.APPLY)` |
| "group these" | `g = Gimp.GroupLayer.new(img, "grp"); img.insert_layer(g, None, 0); img.reorder_item(lay, g, 0)`; empty group in PASS_THROUGH = adjustment-layer trick [3.2 release notes] |
| "duplicate" | `d = lay.copy(); img.insert_layer(d, None, 0)` |
| "merge everything" | `img.merge_visible_layers(Gimp.MergeType.CLIP_TO_IMAGE)` (keeps alpha) or `img.flatten()` (drops alpha → opaque) |
| "which pixel colour is at x,y" | `layer.get_pixel(x, y).get_rgba()` (layer coords) or `img.pick_color([layer], x, y, True, False, 0.0)[1].get_rgba()` (composite) |

## Text (titles, captions, thumbnails)
```python
font = Gimp.Font.get_by_name("Impact Regular") or Gimp.context_get_font()     # exact listed name; family alone → None
tl = Gimp.TextLayer.new(img, "THUMB TITLE", font, 120.0, Gimp.Unit.pixel())    # dynamic box sized to the text (819×147 for 11 chars @120px Sans-serif)
img.insert_layer(tl, None, 0); tl.set_offsets(60, 60)
tl.set_color(Gegl.Color.new("white")); tl.set_justification(Gimp.TextJustification.CENTER)
tl.set_letter_spacing(2.0); tl.set_line_spacing(-10.0); tl.set_antialias(True); tl.set_hint_style(Gimp.TextHintStyle.FULL)
# built-in outline (3.2, no extra layer):
tl.set_outline(Gimp.TextOutline.STROKE_ONLY)      # or STROKE_FILL (stroke behind fill)
tl.set_outline_color(Gegl.Color.new("black")); tl.set_outline_width(8.0, Gimp.Unit.pixel())
# rich text: tl.set_markup('<span foreground="#ffcc00" weight="bold">MARK</span>UP')  — Pango markup; get_text() becomes None
# multi-line: "\n" in the string. Fixed box: tl.resize(800, 300) → text beyond the box is CLIPPED (measured, 2nd line cut).
# centre horizontally: tl.set_offsets((img.get_width()-tl.get_width())//2, y)   (read tl.get_width() AFTER styling)
```
- Size units: `Gimp.Unit.pixel()` for px, `Gimp.Unit.point()` for pt (48 pt at 300 dpi = 200 px — huge; thumbnails want pixels).
- Font names: list with `[f.get_name() for f in Gimp.fonts_get_list("(?i)impact|segoe")]`; 453 on the Qube; brand fonts (Montserrat/Anton/League Gothic) are **absent here** (01).
- Never run with `-f`: 0 fonts → `TextLayer.new` cannot get a font.
- Outline the "classic" way (separate layer, any colour/blur, for glow effects):
  `img.select_item(Gimp.ChannelOps.REPLACE, tl); Gimp.Selection.grow(img, 10); ol = Layer(...RGBA...); img.insert_layer(ol, None, 1); Gimp.context_set_foreground(col); ol.edit_fill(Gimp.FillType.FOREGROUND); Gimp.Selection.none(img)` [measured; bounds came back (60,94)–(909,199)].

## Effects (GEGL) — see 04 for the DrawableFilter pattern and property tables
| Designer says | Op + typical values |
|---|---|
| "drop shadow" | `gegl:dropshadow` x 8 y 8 radius 12 opacity 0.8 color black → `merge_filter` (bakes; bbox grows) or `append_filter` (live). On a TEXT layer, merge only after the last `set_text`: afterwards text edits are silently rejected (CRITICAL on stderr, pixels unchanged) [measured] |
| "long shadow / retro shadow" | `gegl:long-shadow` angle 45 length 100 color |
| "glow" | `gegl:inner-glow` (inner) or `gegl:dropshadow` with x=y=0, larger radius, bright colour (outer) [inferred from params] |
| "blur the background" | `gegl:gaussian-blur` std-dev-x/y 1.5–30 (0.19 s at 1280×720 with 1.5) |
| "sharpen" | `gegl:unsharp-mask` std-dev 3 scale 0.5 |
| "vignette" | `gegl:vignette` radius 1.2 softness 0.8 |
| "pop the colours / more saturation" | `gegl:saturation` scale 1.3; warmth `gegl:color-temperature`; `gegl:shadows-highlights`; `gegl:levels`; `gegl:exposure` |
| "denoise" | `gegl:noise-reduction` iterations 4; `gegl:median-blur` |
| "auto levels" | `layer.levels_stretch()` (deprecated but works) or `gegl:stretch-contrast` |
| "black & white" | `layer.desaturate(Gimp.DesaturateMode.LUMINANCE)` (deprecated, works) or `gegl:saturation` scale 0; `img.convert_grayscale()` for a true grey image |
| "tint / colour overlay" | `gegl:color-overlay` value=colour on a layer set to MULTIPLY/OVERLAY [inferred] |

## Remove background
1. Flat/keyed background (screenshots, product on white/green): `layer.add_alpha(); Gimp.context_set_sample_threshold(0.15); img.select_contiguous_color(Gimp.ChannelOps.REPLACE, layer, x_bg, y_bg); Gimp.Selection.grow(img, 2); Gimp.Selection.feather(img, 1.0); layer.edit_clear(); Gimp.Selection.none(img)` — measured on a green field: bounds (140,60)–(1140,660), cleared. Add more seeds with `ChannelOps.ADD`.
2. Colour anywhere: `img.select_color(Gimp.ChannelOps.REPLACE, layer, Gegl.Color.new("rgb(0.12,0.78,0.24)"))` then clear; or soft: `gegl:color-to-alpha` (color + thresholds) via `merge_filter` (0.1 s).
3. Real photos (hair, soft edges) — matting [measured]: paint a trimap layer (same size, RGB: black = sure background, grey 50 % = unknown band, white = sure foreground; fill via selections + `edit_fill`), then `layer.foreground_extract(Gimp.ForegroundExtractMode.MATTING, trimap)` (0.7 s at 1600×900). It returns True and puts the result in the **selection** (bounds equalled the white+grey region), pixels unchanged — finish with `Gimp.Selection.invert(img); layer.edit_clear(); Gimp.Selection.none(img); img.remove_layer(trimap)`. Alternatively do the matte outside GIMP (harness `remove-background` skill / `offload_edit_image`) and composite here [inferred].
4. Iterative loop from gimp-mcp (`bg_remove_iterative.py`): edge-seeded contiguous selects, snapshot, scan for leftover pixels, refine grids 25→1 px, despeckle [community].
Check: `layer.has_alpha()` True and `layer.get_pixel(x,y).get_rgba()[3]` == 0 in the cleared zone; export PNG (alpha kept), never JPEG.

## Composite ("put the product on the background, title top-left, logo bottom-right")
Order: background layer (bottom) → subject layer with alpha (offsets) → shadow (dropshadow on subject) → text layers → logo via `file_load_layer` + `scale` + `set_offsets` → `set_resolution` → export. Coordinates are image pixels from the top-left; `layer.get_offsets()` → `(True, x, y)`.

## Export sizes (targets) [doc/community — platform specs, not GIMP facts]
| Target | Size | Notes |
|---|---|---|
| YouTube thumbnail | 1280×720 (16:9), < 2 MB, JPG/PNG/GIF | export PNG then JPG q 0.9; check bytes |
| YouTube Shorts / TikTok / IG Reels / Stories | 1080×1920 (9:16) | |
| Instagram feed | 1080×1080 (1:1) or 1080×1350 (4:5) | |
| X / Twitter card | 1600×900 (16:9) · Facebook link 1200×630 · LinkedIn 1200×627 | |
| Web hero | 1920×1080 / 2× for retina; WebP q 80–90 or AVIF q 50–65 | WebP/AVIF exported here carried no dpi (irrelevant for web) |
| Print | set 300 dpi; TIFF (merge layers first) or PDF | PDF page size = px / dpi × 72 pt (1280×720 @300 → 307.2×172.8 pt) |
The gimp-mcp tool `export_social_media_kit` bakes a similar table [community].

## Verify checklist (every deliverable)
1. `Gimp.file_save(...)` returned True (False = wrong extension, missing folder, unsupported).
2. `ffprobe -v error -show_entries stream=codec_name,width,height,pix_fmt -of csv=p=0 out.png` → expected WxH and `rgba` if alpha was required (`rgb24`/`yuvj444p` means no alpha).
3. Pillow: `Image.open(p)` → `.size`, `.mode` (RGBA/RGB/P), `.info.get('dpi')`, `n_frames` (TIFF pages!), `getextrema()` alpha channel `(255,255)` = fully opaque.
4. Look at it (`Read` the PNG, or `offload_vqa`): text present, not clipped, colours right — the fixed-box clipping and white-border traps were only visible in the image.
5. Byte size against the platform cap; bounding-box formats (ICO/DDS/SVG) equal the canvas.
