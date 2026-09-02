# 06 — Export formats: what `Gimp.file_save` actually wrote here

Measured 2026-09-01 on the Qube, GIMP 3.2.4, from a 1280×720 RGBA u8 image with 4 layers (fill,
outline, text with baked drop shadow overflowing the canvas, background) at 300 dpi, via
`Gimp.file_save(Gimp.RunMode.NONINTERACTIVE, img, Gio.File.new_for_path(path), None)`. Verified
with ffprobe 8.1.2 + Pillow 12.3.0 (+ pdfinfo, zipfile, hexdump). The exporter is chosen by
**extension**; GIMP makes a temporary copy adapted to the exporter's `ExportCapabilities`
(flatten, index, down-convert) so the working image is untouched [doc + measured: layer count
unchanged after every export].

## Matrix (60 `file-*-export` procedures exist; 23 extensions exercised)
| ext | procedure | ret | bytes | ms | file really is | alpha | dpi kept | gotcha |
|---|---|---|---|---|---|---|---|---|
| png | file-png-export | True | 67 691 | ~400 | png 1280×720 rgba | yes | 299.9994 | compression 9 default; `compression=1` → 103 728 B |
| jpg / jpeg | file-jpeg-export | True | 54 530 | ~300 | mjpeg 1280×720 **yuvj444p** | flattened | 300 | default q 0.9 (0–1!), 4:4:4 (`sub-sampling-1x1`); q 0.6 → 34 800 B |
| webp | file-webp-export | True | 16 112 | ~300 | webp 1280×720 yuv420p RGB | dropped (image was opaque) | none | q 90 default; lossless flag available |
| tif / tiff | file-tiff-export | True | 11 964 006 | — | tiff rgba, **5 pages = 4 layers + 1** | yes | 300 | layers become pages; merge first. Even a 1-layer image gave 2 IFDs (Pillow frames=2) — second likely a reduced-res/thumb IFD [inferred] |
| gif | file-gif-export | True | 51 930 | — | gif 1280×720 palette | 1-bit | none | auto-indexed copy; source stays RGB |
| bmp | file-bmp-export | True | 2 764 938 | — | bmp bgr24 | dropped | ~300 | |
| avif | file-heif-av1-export | True | 4 060 | — | av1 1280×720 yuv420p | yes (Pillow RGBA) | none | quality 50 int default → tiny file, soft |
| heic | file-heif-export | True | 8 682 | — | hevc yuvj420p + gray alpha aux stream | yes | none | Pillow can't open (needs pillow-heif); ffprobe can |
| jxl | file-jpegxl-export | True | 22 729 | — | jpegxl rgba | yes | none | Pillow can't open without plugin |
| jp2 | file-jp2-export | True | 87 629 | — | jpeg2000 rgba | yes | none | 3.2 new exporter [release notes] |
| pdf | file-pdf-export | True | 86 003 | — | 1 page 307.2×172.8 pt (= px/300 in × 72) | n/a | via page size | text layers stay vector unless `convert-text-layers` |
| psd | file-psd-export | True | 776 927 | — | psd 1280×720, 4 layers | yes | none read by Pillow | |
| psb | file-psb-export | True | 797 665 | — | Photoshop large doc (3.2 new) | — | — | no verifier here besides size |
| xcf | (native) | True | 281 830 | — | `gimp xcf v022` | yes | yes | the only format that keeps text layers editable |
| ora | file-openraster-export | True | 335 495 | — | zip: mimetype, data/000-003.png, mergedimage.png, stack.xml | yes | — | |
| tga | file-tga-export | True | 312 458 | — | targa bgra | yes | none | |
| ico | file-ico-export | True | 118 608 | — | png-in-ico **1358×798** | yes | none | size = union of layer bounds (shadow overflow), NOT canvas; Pillow warns |
| dds | file-dds-export | True | 4 334 864 | — | dds **1358×798** RGBA | yes | none | same overflow |
| exr | file-exr-export | True | 302 269 | — | exr gbrapf32le (float) | yes | none | Pillow can't open |
| qoi | file-qoi-export | True | 134 726 | — | qoi rgba | yes | none | |
| svg | file-svg-export | True | 136 124 | — | `viewBox 0 0 1280 720`, layers as embedded `data:image/png` `<image>` (overflowing layer at x=-31 y=-31 1358×798) | yes | in/px | raster-export-format 'png'; text layers exported as vectors per 3.2 notes [doc] |
| foo | — | **False** | 0 | 60 | nothing | | | stderr `GIMP-Error: Execution error for procedure 'gimp-file-save': Unknown file type` |
| png into missing folder | — | **False** | 0 | 446 | nothing | | | `GIMP-Error: Calling error … Could not open '…' for writing: No such file or directory` — create dirs yourself |

Other exporters present but not run: aa, ani, cel, colorxhtml, csource, cur, dicom, eps, farbfeld, fits, fli, gbr, gih, header, heif-hej2, html-table, icns, mng, pam, pat, pbm, pcx, pfm, pgm, pix, pnm, ppm, ps, raw, rgbe, sgi, sunras, tim, xbm, xpm, xwd, plus compressors gz/bz2/xz (wrap any format: `out.xcf.gz`). **APNG export does not exist on this build** [measured]: `Gimp.file_save(..., "anim.apng")` → False + `GIMP-Error: Unknown file type`; the PDB has only `file-apng-load` ("Loads files in APNG file format"); `file-png-export` has no animation argument; the `file-png.exe` binary contains no "apng" string. The 3.2 release notes' APNG export claim does not apply to 3.2.4 Windows — use `file-gif-export` (`as-animation`) or `file-webp-export` (`animation`) for animated output, or ffmpeg (`-f apng`) from a PNG sequence.

## Precision / mode behaviour [measured probe3]
- Indexed: `img.convert_indexed(Gimp.ConvertDitherType.FS, Gimp.ConvertPaletteType.GENERATE, 64, False, True, "")` → PNG `pal8` 55 974 B (1600×900).
- 16-bit: `img.convert_precision(Gimp.Precision.U16_NON_LINEAR)` → PNG `rgba64be`; the same image to `.jpg` auto-down-converts to 8-bit (yuvj444p) — no error.
- 6400×3600 RGBA-16 PNG: 8.4 MB, 11.5 s. Budget time for big exports.
- After `img.flatten()` the exported PNG loses alpha (rgb) — use `merge_visible_layers(CLIP_TO_IMAGE)` to keep it.

## Explicit export with options (when defaults are wrong) [measured]
```python
proc = Gimp.get_pdb().lookup_procedure('file-jpeg-export'); c = proc.create_config()
c.set_property('run-mode', Gimp.RunMode.NONINTERACTIVE); c.set_property('image', img)
c.set_property('file', Gio.File.new_for_path(p)); c.set_property('quality', 0.85)   # 0–1 double; 85 is silently dropped → default 0.9 used, status still SUCCESS
c.set_property('sub-sampling', 'sub-sampling-2x2')                                  # 4:2:0 for smaller web files [nick measured from the GimpParamChoice]
r = proc.run(c); assert r.index(0) == Gimp.PDBStatusType.SUCCESS
```
Same shape for `file-png-export` (`compression` 0–9, `include-thumbnail`, `save-transparent`, `format` `auto|rgb8|gray8|rgba8|graya8|rgb16|gray16|rgba16|graya16`), `file-webp-export` (`lossless`, `quality`, `alpha-quality`, `preset` `default|picture|photo|drawing|icon|text`), `file-heif-av1-export` (`quality` int 0–100, `lossless`, `pixel-format` `yuv420|yuv444|rgb`, `encoder-speed` `balanced|slow|fast`), `file-jpegxl-export` (`compression` distance, `lossless`, `speed` `lightning…tortoise`), `file-tiff-export` (`compression` `none|lzw|packbits|adobe_deflate|jpeg|ccittfax3|ccittfax4`, `bigtiff`), `file-pdf-export` (`layers-as-pages`, `convert-text-layers`), `file-gif-export` (`as-animation`, `default-delay`, `loop`, `default-dispose` `unspecified|combine|replace`), `file-bmp-export` (`rgb-format` `rgb-888|rgb-565|rgba-5551|rgb-555|rgba-8888|rgbx-8888`). All choice nicks measured 2026-09-01 (table in 04). Full argument lists with defaults: 04 and the dump (`pdb_subset_signatures["file-…-export"]`).
Metadata flags on PNG/JPEG/WebP default to `include-exif/iptc/xmp/color-profile = False`, `include-thumbnail = True` (an embedded thumbnail inflates small files; set False for web).

## Verify recipes
```bash
ffprobe -v error -show_entries stream=codec_name,width,height,pix_fmt -of csv=p=0 out.webp     # webp,1280,720,yuv420p
python -c "from PIL import Image;im=Image.open('out.png');print(im.size,im.mode,im.info.get('dpi'),getattr(im,'n_frames',1),im.getextrema())"
pdfinfo out.pdf | grep -E "Pages|Page size"
python -c "import zipfile;print(zipfile.ZipFile('out.ora').namelist())"
head -c 14 out.xcf     # 'gimp xcf v022'
```
Pillow cannot read heic/jxl/psb/exr/xcf/ora/svg/pdf; ffprobe covers heic/jxl/exr/psd/ico/dds.
Alpha check: mode RGBA **and** `getextrema()[3][0] < 255` (an RGBA file with alpha min 255 is opaque).
