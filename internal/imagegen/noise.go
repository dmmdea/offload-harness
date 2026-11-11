package imagegen

import (
	"bufio"
	"fmt"
	"image"
	"image/color"
	"os"
)

// The decoded-noise detector. A diffusion render that went wrong (a broken VAE decode, a sampler fed NaNs) can come back as a
// perfectly valid PNG of pure per-pixel noise: the file exists, is not empty and decodes, and nothing in the file says it is
// not a picture. A picture is spatially coherent at the scale of a few pixels, so the average luma of neighbouring 16 px and
// 8 px cells is strongly correlated; noise is not. Noise is also DENSE: nearly every 16 px cell is busy inside. A sparse
// picture (a star field, thin line art, text or one line on a plain ground) has uncorrelated cells too, but most of its cells
// are flat, so the third test keeps it a picture.
//
// Measured on real files, not chosen: 17 frames of decoded noise scored at most 0.149 at 16 px and 0.452 at 8 px, with at
// least 96.7 % busy cells; 400 real renders (contact sheets and strips included) scored at least 0.578 at 16 px and 0.721 at
// 8 px, and those under 0.7 at 16 px had at most 75 % busy cells; a star field, line art, a diagonal line on black and rows of
// text-like glyphs had 1 % to 66 %. The limits sit in those gaps, and the picture has to fail ALL THREE to be called noise.
const (
	noiseCorrLimit16 = 0.3 // decoded noise: the lag-1 correlation of 16 px cells is below this ...
	noiseCorrLimit8  = 0.6 // ... and that of 8 px cells is below this ...
	noiseBusyShare   = 0.9 // ... and at least this share of 16 px cells is busy inside
	noiseBusySD      = 24  // a cell is busy when the standard deviation of its luma (0..255) is above this
	// noiseMinSide is the smallest canvas side the detector will judge: under it there are too few cells for the correlation
	// to mean anything (icons, thumbnails, test fixtures), and the answer is "not noise".
	noiseMinSide = 256
	// noiseMaxPixels bounds the canvas a file check will decode (64 Mpx, as --resume does): a stray huge file is not one of the
	// harness's outputs and is not judged, the cheap direction for a check that must never take gigabytes of RAM.
	noiseMaxPixels = 64 << 20
)

// DecodedNoise reports whether img is decoded noise, with the two correlations it was judged on. A canvas with a side under
// 256 px cannot be judged and answers false with zero correlations. A grid with no variance (a blank image) scores 1: it is
// not noise, and a blank picture is another check's business.
func DecodedNoise(img image.Image) (noise bool, corr16, corr8 float64) {
	if img == nil {
		return false, 0, 0
	}
	b := img.Bounds()
	if b.Dx() < noiseMinSide || b.Dy() < noiseMinSide {
		return false, 0, 0
	}
	luma := lumaPlane(img)
	corr16 = cellCorrelation(luma, b.Dx(), b.Dy(), 16)
	corr8 = cellCorrelation(luma, b.Dx(), b.Dy(), 8)
	if corr16 >= noiseCorrLimit16 || corr8 >= noiseCorrLimit8 {
		return false, corr16, corr8
	}
	return busyCellShare(luma, b.Dx(), b.Dy(), 16, noiseBusySD) >= noiseBusyShare, corr16, corr8
}

// busyCellShare is the share of whole c x c cells whose luma standard deviation is above sd.
func busyCellShare(luma []float64, w, h, c int, sd float64) float64 {
	cw, ch := w/c, h/c
	if cw < 1 || ch < 1 {
		return 0
	}
	area := float64(c * c)
	busy := 0
	for cy := 0; cy < ch; cy++ {
		for cx := 0; cx < cw; cx++ {
			var sum, sq float64
			for y := cy * c; y < (cy+1)*c; y++ {
				for _, v := range luma[y*w+cx*c : y*w+cx*c+c] {
					sum += v
					sq += v * v
				}
			}
			m := sum / area
			if sq/area-m*m > sd*sd {
				busy++
			}
		}
	}
	return float64(busy) / float64(cw*ch)
}

// NoiseReason decodes the picture at path and returns why it is decoded noise, "" when it is not or cannot be judged (it is
// missing, does not decode, has a side under 256 px, or is past 64 Mpx).
func NoiseReason(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(bufio.NewReader(f))
	if err != nil || cfg.Width < noiseMinSide || cfg.Height < noiseMinSide || int64(cfg.Width)*int64(cfg.Height) > noiseMaxPixels {
		return ""
	}
	if _, err := f.Seek(0, 0); err != nil {
		return ""
	}
	img, _, err := image.Decode(bufio.NewReader(f))
	if err != nil {
		return ""
	}
	noise, c16, c8 := DecodedNoise(img)
	if !noise {
		return ""
	}
	return fmt.Sprintf("decoded noise: neighbour-cell correlation %.2f at 16 px, %.2f at 8 px, and busy across the frame; a picture scores above 0.5", c16, c8)
}

// SetAsideNoise moves the file a noise verdict rejected next to its path as <path>.noise, and returns the words that end the
// reason ("" when there is no file). If it cannot be moved it is removed, so nothing a failed render wrote stays at its path
// for --resume to take as finished; if neither works the words say the file is still there. The render took minutes and the
// file is evidence, so it is set aside, not destroyed.
func SetAsideNoise(path string) string {
	if path == "" {
		return ""
	}
	if _, err := os.Lstat(path); err != nil {
		return ""
	}
	aside := path + ".noise"
	_ = os.Remove(aside)
	if err := os.Rename(path, aside); err == nil {
		return "; the picture was set aside as " + aside + " and nothing is left at out"
	}
	if err := os.Remove(path); err == nil {
		return "; the picture could not be set aside and was removed, so nothing is left at out"
	}
	return "; WARNING: the picture could not be moved or removed and is still at out, so --resume may take it for finished"
}

// lumaPlane is the 0.299 R + 0.587 G + 0.114 B luma of every pixel, row by row, on a 0..255 scale.
func lumaPlane(img image.Image) []float64 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]float64, w*h)
	switch m := img.(type) {
	case *image.NRGBA:
		for y := 0; y < h; y++ {
			o := m.PixOffset(b.Min.X, b.Min.Y+y)
			row := m.Pix[o : o+4*w]
			for x := 0; x < w; x++ {
				out[y*w+x] = 0.299*float64(row[4*x]) + 0.587*float64(row[4*x+1]) + 0.114*float64(row[4*x+2])
			}
		}
	case *image.RGBA:
		for y := 0; y < h; y++ {
			o := m.PixOffset(b.Min.X, b.Min.Y+y)
			row := m.Pix[o : o+4*w]
			for x := 0; x < w; x++ {
				out[y*w+x] = 0.299*float64(row[4*x]) + 0.587*float64(row[4*x+1]) + 0.114*float64(row[4*x+2])
			}
		}
	default:
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				c := color.NRGBA64Model.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA64)
				out[y*w+x] = (0.299*float64(c.R) + 0.587*float64(c.G) + 0.114*float64(c.B)) / 257
			}
		}
	}
	return out
}

// cellCorrelation averages luma over whole c x c blocks and returns the lag-1 correlation of neighbouring cells, horizontal
// and vertical pairs pooled: (sum over pairs of (a-mean)(b-mean) / pairs) / (sum over cells of (v-mean)^2 / cells). A grid with
// no variance returns 1.
func cellCorrelation(luma []float64, w, h, c int) float64 {
	cw, ch := w/c, h/c
	if cw < 1 || ch < 1 || cw*ch < 2 {
		return 1
	}
	cells := make([]float64, cw*ch)
	area := float64(c * c)
	for cy := 0; cy < ch; cy++ {
		for cx := 0; cx < cw; cx++ {
			var sum float64
			for y := cy * c; y < (cy+1)*c; y++ {
				row := luma[y*w+cx*c : y*w+cx*c+c]
				for _, v := range row {
					sum += v
				}
			}
			cells[cy*cw+cx] = sum / area
		}
	}
	var mean float64
	for _, v := range cells {
		mean += v
	}
	mean /= float64(len(cells))
	var variance float64
	for _, v := range cells {
		variance += (v - mean) * (v - mean)
	}
	variance /= float64(len(cells))
	if variance <= 1e-12 {
		return 1
	}
	var cov float64
	pairs := 0
	for cy := 0; cy < ch; cy++ {
		for cx := 0; cx < cw; cx++ {
			a := cells[cy*cw+cx] - mean
			if cx+1 < cw {
				cov += a * (cells[cy*cw+cx+1] - mean)
				pairs++
			}
			if cy+1 < ch {
				cov += a * (cells[(cy+1)*cw+cx] - mean)
				pairs++
			}
		}
	}
	if pairs == 0 {
		return 1
	}
	return (cov / float64(pairs)) / variance
}
