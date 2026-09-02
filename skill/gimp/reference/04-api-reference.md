# 04 — Python API reference (GIMP 3.2.4, `gi.repository.Gimp`)

Condensed catalog. Exact PDB signatures for 902 procedures, `dir()` of every class, 28 enums and
GEGL property tables are in `live-dump-qube-2026-09-01.json` — grep it instead of guessing.
Tags: [measured] on the workstation 2026-09-01 · [doc] developer.gimp.org · [community] · [inferred].

## Boilerplate [measured]
```python
import sys, gi
gi.require_version('Gimp', '3.0'); gi.require_version('Gegl', '0.4')
from gi.repository import Gimp, Gegl, Gio, GLib, GObject
# in python-fu-eval batch, Gimp is already imported; the rest is not
img = Gimp.file_load(Gimp.RunMode.NONINTERACTIVE, Gio.File.new_for_path(r'C:\in.png'))   # None if missing (+GIMP-Error on stderr)
layer = img.get_layers()[0]                       # list of root layers (GimpCoreObjectArray → Python list)
ok = Gimp.file_save(Gimp.RunMode.NONINTERACTIVE, img, Gio.File.new_for_path(r'C:\out.jpg'), None)  # bool
img.delete()                                      # free it (no display in console mode)
```

## The naming rule (PDB ↔ Python) [measured on dozens of calls]
`gimp-<class>-<verb>` → `Gimp.<Class>.<verb>(obj, …)` / `obj.<verb>(…)`:
`gimp-image-scale` → `img.scale(w,h)`; `gimp-layer-add-alpha` → `layer.add_alpha()`;
`gimp-text-layer-set-color` → `tl.set_color(color)`; `gimp-selection-grow` → `Gimp.Selection.grow(img, 10)`
(Selection functions are class methods taking the image); `gimp-context-*` → `Gimp.context_*()`;
`gimp-file-save` → `Gimp.file_save(...)`; `gimp-fonts-get-list` → `Gimp.fonts_get_list(filter_regex)`;
`gimp-font-get-by-name` → `Gimp.Font.get_by_name(name)`. Enum values drop the `GIMP_` prefix
(`Gimp.RunMode.NONINTERACTIVE`, `Gimp.LayerMode.NORMAL`) [doc + measured].
Procedures with several out-values return a **named tuple** (`_ResultTuple`): `img.get_resolution()`
→ `(True, xresolution=72.0, yresolution=72.0)`; `layer.get_offsets()` → `(True, offset_x, offset_y)`;
`Gimp.Selection.bounds(img)` → `(True, non_empty, x1, y1, x2, y2)`; `img.pick_color([layer], x, y, True, False, 0.0)`
→ `(True, <Gegl.Color>)` — index `[1]` before `.get_rgba()` [measured].
Single-out procedures return the value directly (`img.get_width()` → int, `layer.copy()` → Layer,
`img.merge_visible_layers(mode)` → Layer). Setters return True/False [measured].

## Core objects and the methods that matter [measured `dir()` + used]
**Gimp.Image** — `new(w,h,Gimp.ImageBaseType.RGB)`, `get_width/get_height`, `get_precision` (`.value_nick` e.g. `u8-non-linear`), `get_base_type`, `get_resolution/set_resolution(x,y)` (dpi),
`get_layers`, `get_selected_layers/set_selected_layers([..])` (the 3.x replacement for "active layer"), `get_layer_by_name`, `insert_layer(layer, parent, position)` (0 = top),
`remove_layer`, `reorder_item(item, parent, pos)`, `raise_item/lower_item/raise_item_to_top/lower_item_to_bottom`, `scale(w,h)`, `crop(w,h,offx,offy)`, `resize(w,h,offx,offy)`, `resize_to_layers`,
`flip(orientation)`, `rotate(Gimp.RotationType.DEGREES90)`, `flatten()` → layer (drops alpha), `merge_visible_layers(Gimp.MergeType.CLIP_TO_IMAGE)` → layer, `merge_down`,
`select_rectangle/select_round_rectangle/select_ellipse(op, x, y, w, h[, rx, ry])`, `select_polygon(op, [x0,y0,…])`, `select_color(op, drawable, GeglColor)`, `select_contiguous_color(op, drawable, x, y)`, `select_item(op, item)` (alpha/text shape → selection),
`get_selection()` → Selection, `pick_color(drawables, x, y, sample_merged, sample_average, radius)`, `convert_indexed(dither, palette_type, num_cols, alpha_dither, remove_unused, palette_name)`, `convert_rgb/convert_grayscale`, `convert_precision(Gimp.Precision.U16_NON_LINEAR)`,
`undo_group_start/undo_group_end`, `undo_disable/undo_enable/undo_freeze/undo_thaw`, `get_file/set_file`, `is_dirty`, `clean_all`, `duplicate`, `delete`, `autocrop(drawable)`, `autocrop_selected_layers`, `get_metadata/set_metadata`, `get_color_profile/set_color_profile/convert_color_profile` (Python methods exist; the PDB names differ), `get_thumbnail_data`, `get_channels/get_paths/insert_channel/insert_path`.
**Gimp.Layer** (⊂ Drawable ⊂ Item) — `new(img, name, w, h, Gimp.ImageType.RGBA_IMAGE, opacity_0_100, Gimp.LayerMode.NORMAL)`, `new_from_visible(img, dest_img, name)`, `new_from_drawable(drawable, dest_img)`, `copy()` (no args in 3.2),
`add_alpha/flatten/has_alpha`, `scale(w,h,local_origin)`, `resize(w,h,offx,offy)`, `resize_to_image_size()`, `set_offsets(x,y)/get_offsets`, `set_opacity/get_opacity` (0–100), `set_mode/get_mode`, `set_blend_space/set_composite_space/set_composite_mode`, `set_lock_alpha`, `set_visible`, `set_name/get_name`,
`create_mask(Gimp.AddMaskType.ALPHA|SELECTION|WHITE|…)` → LayerMask, `add_mask(mask)`, `get_mask`, `remove_mask(Gimp.MaskApplyMode.APPLY|DISCARD)`, `is_floating_sel`, `get_show_mask/set_edit_mask/set_apply_mask`.
**Gimp.Drawable** — `fill(Gimp.FillType.FOREGROUND|BACKGROUND|WHITE|TRANSPARENT|PATTERN)`, `edit_fill(filltype)` (respects selection), `edit_clear()`, `edit_stroke_selection()`, `edit_stroke_item(path)`, `edit_bucket_fill(filltype, x, y)`, `edit_gradient_fill(type, offset, supersample, depth, thr, dither, x1,y1,x2,y2)`,
`get_pixel(x,y)` → GeglColor (`.get_rgba()` floats 0–1), `set_pixel`, `get_width/get_height/get_bpp/get_format`, `has_alpha`, `type`, `type_with_alpha`, `update`, `get_buffer/get_shadow_buffer/merge_shadow` (Gegl.Buffer for pixel work), `mask_bounds/mask_intersect`, `histogram(channel, start, end)`,
`get_filters()` → [DrawableFilter], `merge_filter(f)`, `append_filter(f)`, `merge_new_filter/append_new_filter(op, name, mode, opacity, …)` [doc, not called], `foreground_extract(Gimp.ForegroundExtractMode.MATTING, trimap_drawable)` → True and **writes the result into the image SELECTION** (bounds became the trimap's white+grey region), pixels untouched — follow with `Gimp.Selection.invert(img); layer.edit_clear()` [measured],
deprecated-but-working colour ops: `brightness_contrast(-1..1,-1..1)`, `desaturate(Gimp.DesaturateMode.LUMINANCE)`, `levels/levels_stretch/curves_spline/curves_explicit/hue_saturation/colorize_hsl/color_balance/threshold/posterize/invert/equalize/shadows_highlights` (3.2 marks them deprecated in favour of the GEGL filter — DeprecationWarning printed).
**Gimp.Item** — `get_name/set_name`, `get_visible/set_visible`, `is_layer/is_text_layer/is_group/is_channel/is_layer_mask/is_path/is_valid`, `get_image`, `get_parent/get_children`, `transform_rotate(rad, auto_center, cx, cy)` → item, `transform_scale(x0,y0,x1,y1)`, `transform_translate(dx,dy)`, `transform_flip_simple(Gimp.OrientationType.HORIZONTAL, auto_center, axis)`, `transform_perspective/shear/matrix/2d`, `get_lock_*/set_lock_*`, `get_tattoo`, `attach_parasite/get_parasite`.
**Gimp.TextLayer** — `new(img, text, Gimp.Font, size, Gimp.Unit.pixel()|point())` (font is a **Font object**, never a string), `set_text/get_text`, `set_markup/get_markup` (Pango markup; after `set_markup`, `get_text()` → None and `get_markup()` → `<markup>…</markup>`; `set_text` clears markup) [measured], `set_font(Font)/get_font`, `set_font_size(size, unit)/get_font_size` → `(size, unit=…)`,
`set_color(GeglColor)/get_color`, `set_outline(Gimp.TextOutline.NONE|STROKE_ONLY|STROKE_FILL)`, `set_outline_color(GeglColor)`, `set_outline_width(w, unit)`, `get_outline()` (`.value_nick` → `stroke-only`), `set_outline_direction` [3.2: centered/outer/inner per release notes; enum `Gimp.TextOutlineDirection`], `set_justification(Gimp.TextJustification.LEFT|CENTER|RIGHT|FILL)`, `set_letter_spacing/set_line_spacing/set_indent`, `set_antialias(bool)`, `set_hint_style(Gimp.TextHintStyle.FULL)`, `set_base_direction`, `set_language`, `resize(w,h)` (fixed box: **content beyond the box is clipped** [measured]), `set_offsets` (inherited).
**Gimp.Selection** (class methods, image first) — `none/all/invert/is_empty/bounds/value(img,x,y)`, `grow(img,px)/shrink/border/feather(img, radius)/sharpen/flood`, `float(...)`, `save(img)` → Channel.
**Gimp.GroupLayer** — `new(img, name)`; add with `img.insert_layer(group, None, 0)`; `img.reorder_item(layer, group, 0)` moves a layer inside; `group.get_children()` [measured].
**Gimp.Font / Gimp.Unit / Gegl.Color** — `Gimp.Font.get_by_name("Impact Regular")` (exact listed name, else None), `Gimp.context_get_font()`, `Gimp.fonts_get_list("regex")` → [Font] (`.get_name()`); `Gimp.Unit.pixel()`, `.point()`, `.inch()`, `.mm()`; `Gegl.Color.new("white" | "#ff2a2a" | "rgb(0.2,0.2,0.9)" | "rgba(…)")` — components are **0.0–1.0 floats**; `c.get_rgba()` → (r,g,b,a); `c.set_rgba(r,g,b,a)`.
**Gimp context** — `context_set_foreground/background(GeglColor)`, `context_get_foreground/background()`, `context_set_interpolation(Gimp.InterpolationType.LOHALO|NOHALO|CUBIC|LINEAR|NONE)`, `context_set_sample_threshold(0..1)`, `context_set_antialias/feather/feather_radius/sample_merged/sample_transparent`, `context_set_line_width(px)`, `context_set_stroke_method(Gimp.StrokeMethod.LINE)`, `context_set_brush/brush_size/paint_mode/opacity`, `context_push/pop`, `context_set_default_colors`.
**Gimp module** — `version()`, `get_pdb()`, `get_images()`, `file_load(mode, GFile)`, `file_load_layer(mode, img, GFile)`, `file_save(mode, img, GFile, options)`, `message(str)`, `message_set_handler(Gimp.MessageHandlerType.CONSOLE)`, `displays_flush()` (no-op headless), `Display.new(img)` (**fails headless**), `directory()/data_directory()/sysconf_directory()/temp_directory()/locale_directory()` (no `plug_in_directory`), `progress_init/progress_update`, `pencil/paintbrush_default/airbrush/eraser(drawable, [x0,y0,x1,y1,…])`.

## Calling any PDB procedure (plug-ins, Script-Fu scripts, export procs) [measured]
```python
pdb = Gimp.get_pdb()
proc = pdb.lookup_procedure('file-png-export')     # None if unknown; pdb.procedure_exists(name) → bool
cfg  = proc.create_config()
cfg.set_property('run-mode', Gimp.RunMode.NONINTERACTIVE)
cfg.set_property('image', img); cfg.set_property('file', Gio.File.new_for_path(path))
cfg.set_property('compression', 1); cfg.set_property('include-thumbnail', False)
res = proc.run(cfg)                                # Gimp.ValueArray
status = res.index(0)                              # Gimp.PDBStatusType: SUCCESS==3, EXECUTION_ERROR==0, CALLING_ERROR==1, PASS_THROUGH==2, CANCEL==4
if status != Gimp.PDBStatusType.SUCCESS: raise RuntimeError(pdb.get_last_error())   # e.g. 'execution error'
value = res.index(1)                               # first real return value, if any
```
Introspection: `proc.get_arguments()` / `get_return_values()` → GParamSpec list (`.name`, `.value_type`, `.get_default_value()`, `.get_blurb()`), `proc.get_blurb()/get_menu_paths()/get_proc_type().value_nick`, `pdb.query_procedures('','','','','','','','')` → all 1033 names.
**Removed:** `pdb.run_procedure` (AttributeError on 3.2.4), `gimp-text-fontname`, `gimp-text-get-extents-fontname`, `gimp-image-get-active-layer/-drawable`, `gimp-image-list`, `gimp-image-thumbnail`, `plug-in-drop-shadow`, `plug-in-autocrop`, `gimp-layer-group-new` (use `Gimp.GroupLayer.new`), `gimp-drawable-color-to-alpha` (use the GEGL op) — all confirmed absent in the dump. Arg `quality=60` on a 0–1 double: dropped with a stderr Warning, export continues with the default [measured].

## GEGL filters via Gimp.DrawableFilter [measured]
```python
f = Gimp.DrawableFilter.new(layer, "gegl:dropshadow", "shadow")   # TypeError 'constructor returned NULL' + GIMP-Error 'not installed' for a bad op
f.set_blend_mode(Gimp.LayerMode.NORMAL); f.set_opacity(1.0)        # optional (defaults fine)
c = f.get_config()                                                 # Gimp.DrawableFilterConfig; c.list_properties() → GParamSpecs
c.set_property("x", 8.0); c.set_property("y", 8.0); c.set_property("radius", 12.0)
c.set_property("opacity", 0.8); c.set_property("color", Gegl.Color.new("black"))
f.update()                                                          # pushes ALL config changes to the core [doc]
layer.merge_filter(f)        # destructive (bakes pixels; layer bounds may grow — dropshadow did: 1358×798 export bbox)
# or: layer.append_filter(f) # non-destructive layer effect; layer.get_filters() → [f]; f.delete() removes; f.set_visible(False)
```
Other DrawableFilter methods: `get_name/get_operation_name/get_id/get_by_id/is_valid/get_blend_mode/get_opacity/get_visible/set_aux_input(pad, drawable)/delete`.
Non-destructive effects apply to **layers only** (3.0 doc); 3.2 extends them to channels [release notes]. Applying `merge_filter` to a **text layer** bakes the pixels (shadow rendered, layer bbox grew 31 px) and **freezes the text**: `is_text_layer()` still says True and `set_text` returns True, but the core logs `Gimp-Text-CRITICAL: gimp_text_layer_set: assertion 'gimp_item_is_text_layer' failed` and the exported image is byte-identical before/after [measured]. Finish all text edits first, or effect a `layer.copy()` / use `append_filter`.
Property tables (name:type=default) for the ops you will reach for — full set of ~120 in the dump under `gegl.props`:
- `gegl:dropshadow` x:20 y:20 radius:10 grow-shape(enum)=1 grow-radius:0 color opacity:0.5
- `gegl:long-shadow` style(enum)=0 angle:45 length:100 midpoint:100 midpoint-rel:0.5 color composition(enum)=0
- `gegl:inner-glow` grow-shape=1 x:0 y:0 radius:7.5 grow-radius:4 opacity:1.2 value(color) cover:60
- `gegl:color-to-alpha` color transparency-threshold:0 opacity-threshold:1
- `gegl:gaussian-blur` std-dev-x:1.5 std-dev-y:1.5 filter abyss-policy=1 clip-extent:True
- `gegl:unsharp-mask` std-dev:3 scale:0.5 threshold:0 · `gegl:high-pass` std-dev:4 contrast:1
- `gegl:vignette` shape=0 color radius:1.2 softness:0.8 gamma:2 proportion:1 squeeze:0 x:0.5 y:0.5 rotation:0
- `gegl:levels` in-low:0 in-high:1 out-low:0 out-high:1 · `gegl:brightness-contrast` contrast:1 brightness:0 · `gegl:exposure` black-level:0 exposure:0
- `gegl:saturation` scale:1 colorspace=0 · `gegl:hue-chroma` hue:0 chroma:0 lightness:0 · `gegl:color-temperature` original-temperature:6500 intended-temperature:6500
- `gegl:shadows-highlights` shadows:0 highlights:0 whitepoint:0 radius:100 compress:50 shadows-ccorrect:100 highlights-ccorrect:50
- `gegl:noise-reduction` iterations:4 · `gegl:median-blur` neighborhood=1 radius:3 percentile:50 alpha-percentile:50
- `gegl:opacity` value:1 · `gegl:threshold` value:0.5 high:1 · `gegl:pixelize` size-x:16 size-y:16 … · `gegl:bloom` threshold:50 softness:25 radius:10 strength:50
- `gegl:color-overlay` value(color) srgb:False · `gegl:softglow` glow-radius:10 brightness:0.3 sharpness:0.85 · `gegl:alpha-clip`
- `gegl:crop` x y width height reset-origin · `gegl:scale-size` x:100 y:100 sampler=1 · `gegl:text` string font size color wrap alignment width height
- Enum-typed props (grow-shape, style, shape, sampler…) take ints (nick lookup untested).
- NOT present on this build: `gegl:desaturate`, `gegl:selective-hue-saturation`, `gegl:little-planet`, `gegl:matting-levin`, `gegl:paint-select`, `gegl:vhs`. GIMP's own `gimp:*` ops (e.g. `gimp:brightness-contrast`, `gimp:threshold-alpha`) are not in `gegl.exe --list-all` but are what 3.2 deprecations point to [doc]; not exercised.
Total ops: 258 (`gegl.exe --list-all` and `Gegl.list_operations()` inside the plug-in agree after `Gegl.init(None)`).

## Enums (values measured via `dir()`)
- `LayerMode`: NORMAL MULTIPLY SCREEN OVERLAY ADDITION SUBTRACT DIFFERENCE DIVIDE DODGE BURN HARDLIGHT SOFTLIGHT GRAIN_EXTRACT GRAIN_MERGE HSV_HUE HSV_SATURATION HSL_COLOR HSV_VALUE LCH_* LUMINANCE DARKEN_ONLY LIGHTEN_ONLY LUMA_* LINEAR_BURN LINEAR_LIGHT VIVID_LIGHT PIN_LIGHT HARD_MIX EXCLUSION ERASE MERGE SPLIT REPLACE PASS_THROUGH DISSOLVE BEHIND COLOR_ERASE OVERWRITE (+ `_LEGACY` variants). `gimp-layer-new` default mode value 28 = NORMAL [measured default].
- `FillType`: FOREGROUND BACKGROUND WHITE TRANSPARENT PATTERN CIELAB_MIDDLE_GRAY · `ChannelOps`: REPLACE ADD SUBTRACT INTERSECT · `MergeType`: EXPAND_AS_NECESSARY CLIP_TO_IMAGE CLIP_TO_BOTTOM_LAYER FLATTEN_IMAGE
- `ImageType`: RGB_IMAGE RGBA_IMAGE GRAY_IMAGE GRAYA_IMAGE INDEXED_IMAGE INDEXEDA_IMAGE · `ImageBaseType`: RGB GRAY INDEXED
- `InterpolationType`: NONE LINEAR CUBIC NOHALO LOHALO · `Precision`: U8/U16/U32/HALF/FLOAT/DOUBLE × LINEAR/NON_LINEAR/PERCEPTUAL
- `RunMode`: INTERACTIVE NONINTERACTIVE WITH_LAST_VALS · `PDBStatusType`: EXECUTION_ERROR(0) CALLING_ERROR(1) PASS_THROUGH(2) SUCCESS(3) CANCEL(4) · `PDBProcType`: INTERNAL PLUGIN TEMPORARY PERSISTENT
- `TextOutline`: NONE STROKE_FILL STROKE_ONLY · `TextJustification`: LEFT RIGHT CENTER FILL · `TextHintStyle`: NONE SLIGHT MEDIUM FULL
- `AddMaskType`: WHITE BLACK ALPHA ALPHA_TRANSFER SELECTION COPY CHANNEL · `MaskApplyMode`: APPLY DISCARD · `DesaturateMode`: LIGHTNESS LUMA AVERAGE LUMINANCE VALUE
- `RotationType`: DEGREES90 DEGREES180 DEGREES270 · `OrientationType`: HORIZONTAL VERTICAL UNKNOWN · `GradientType`: LINEAR BILINEAR RADIAL SQUARE CONICAL_* SHAPEBURST_* SPIRAL_* · `StrokeMethod`: LINE PAINT_METHOD
- `ConvertPaletteType`: GENERATE WEB MONO CUSTOM · `ConvertDitherType`: NONE FS FS_LOWBLEED FIXED · `HistogramChannel`: VALUE RED GREEN BLUE ALPHA LUMINANCE · `ForegroundExtractMode`: MATTING · `MessageHandlerType`: MESSAGE_BOX CONSOLE ERROR_CONSOLE
- `ExportCapabilities` (what an exporter declares; GIMP auto-converts the image copy to fit): CAN_HANDLE_RGB/GRAY/INDEXED/BITMAP/ALPHA/LAYERS/LAYERS_AS_ANIMATION/LAYER_MASKS/LAYER_EFFECTS, NEEDS_ALPHA, NEEDS_CROP.

## Choice-typed argument values (exact nicks) [measured via `Gimp.param_spec_choice_get_choice(pspec).list_nicks()`]
| proc.arg | nicks (default first) |
|---|---|
| file-jpeg-export.sub-sampling | `sub-sampling-1x1` (4:4:4), `sub-sampling-2x1` (4:2:2), `sub-sampling-1x2` (4:4:0), `sub-sampling-2x2` (4:2:0) |
| file-jpeg-export.dct | `integer`, `fixed`, `float` |
| file-tiff-export.compression | `none`, `lzw`, `packbits`, `adobe_deflate`, `jpeg`, `ccittfax3`, `ccittfax4` |
| file-png-export.format | `auto`, `rgb8`, `gray8`, `rgba8`, `graya8`, `rgb16`, `gray16`, `rgba16`, `graya16` |
| file-webp-export.preset | `default`, `picture`, `photo`, `drawing`, `icon`, `text` |
| file-heif-av1-export.pixel-format (same for file-heif-export) | `yuv420`, `yuv444`, `rgb` |
| file-heif-av1-export.encoder-speed | `balanced`, `slow`, `fast` |
| file-jpegxl-export.speed | `squirrel`, `lightning`, `thunder`, `falcon`, `cheetah`, `hare`, `wombat`, `kitten`, `tortoise` |
| file-gif-export.default-dispose | `unspecified`, `combine`, `replace` |
| file-bmp-export.rgb-format | `rgb-888`, `rgb-565`, `rgba-5551`, `rgb-555`, `rgba-8888`, `rgbx-8888` |
| file-svg-export.raster-export-format | `png`, `jpeg`, `none` |
Recipe for any other choice arg: `p=pdb.lookup_procedure(n); [(a.name, Gimp.param_spec_choice_get_choice(a).list_nicks()) for a in p.get_arguments() if GObject.type_name(type(a))=='GimpParamChoice']`.

## Export procedures — signature shape [measured from the dump]
All `file-*-export` share `run-mode, image, file (GFile), options (GimpExportOptions, pass None)` then format args; `Gimp.file_save` dispatches on extension and uses their defaults. Format-specific args (defaults):
- `file-png-export`: interlaced F, compression 9, bkgd T, offs F, phys T, time T, save-transparent F, optimize-palette F, format 'auto', include-exif/iptc/xmp/color-profile F, include-thumbnail T, include-comment F
- `file-jpeg-export`: quality **0.9 (0–1 double)**, smoothing 0, optimize T, progressive T, cmyk F, sub-sampling 'sub-sampling-1x1' (4:4:4), baseline T, restart 0, dct 'integer', metadata flags as PNG
- `file-webp-export`: preset 'default', lossless F, quality 90, alpha-quality 100, use-sharp-yuv F, animation F, animation-loop T, minimize-size T, keyframe-distance 50, default-delay 200, force-delay F, + metadata flags
- `file-tiff-export`: bigtiff F, compression 'none', save-transparent-pixels T, cmyk F (+ layer handling: every layer → page unless merged)
- `file-heif-av1-export` (AVIF) / `file-heif-export` (HEIC): quality 50 (int 0–100), lossless F, save-bit-depth 8, pixel-format 'yuv420', encoder-speed 'balanced'
- `file-jpegxl-export`: lossless F, compression 1.0 (butteraugli distance 0–15), save-bit-depth 8, speed 'squirrel', cmyk F
- `file-pdf-export`: vectorize T, ignore-hidden T, apply-masks T, layers-as-pages F, reverse-order F, root-layers-only T, convert-text-layers F, fill-background-color T
- `file-gif-export`: interlace F, loop T, number-of-repeats 0, default-delay 100, default-dispose 'unspecified', as-animation F, force-delay F, force-dispose F
- `file-bmp-export`: use-rle F, write-color-space T, rgb-format 'rgb-888' · `file-psd-export`: clippingpath F, cmyk F, duotone F · `file-svg-export`: title, raster-export-format 'png' · `file-ico-export`: no args
- Load side: `file-*-load` (91, incl. apng/avci/psd/svg/pdf/webp/jxl/raw placeholders) via `Gimp.file_load`, or `Gimp.file_load_layer(mode, img, GFile)` to bring a file in as a layer.

## Version deltas you will hit [doc + measured]
2.10 → 3.0: `pdb.gimp_xxx()` and `gimpfu` are gone; objects instead of ids; `GimpRGB` → `Gegl.Color`;
`gimp-edit-fill` → `Drawable.edit_fill`; `gimp-image-add-layer` → `insert_layer`; `gimp-drawable-transform-*`
→ `Item.transform_*`; `gimp-rect-select/ellipse-select/fuzzy-select/by-color-select` → `Image.select_rectangle/ellipse/contiguous_color/color`;
`gimp-selection-layer-alpha/load/combine` → `Image.select_item`; `gimp-vectors-*` → `Path` / `Image.*_path*`; `layer.copy(False)` → `layer.copy()`;
`Gimp.fonts_get_list` returns Font objects; the full removed→replacement table is in the porting guide "Removed Functions" page (fetched 2026-09-01).
3.0 → 3.2: 111 new libgimp functions incl. `GimpLinkLayer`/`GimpVectorLayer` (`gimp-link-layer-new(image, file)`, `gimp-vector-layer-new(image, path)` present in the dump), text outline via PDB (`set_outline*`), `gimp_drawable_chooser_*` → `gimp_item_chooser_*`, 12 colour ops deprecated in favour of GEGL filters, non-destructive filters on channels, GEGL Filter Browser tool [release notes].
