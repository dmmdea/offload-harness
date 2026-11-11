package mediaremote

import (
	"image"
	"image/color"
	pngenc "image/png"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// A render of decoded noise decodes whole and has the right size, and used to be `rendered` and then skipped by --resume. It is
// now a failed row that is set aside, and --resume renders it again; a smooth picture of the same size still proves itself.
func TestDecodedNoiseIsNotARenderedPictureNorAFinishedOne(t *testing.T) {
	const n = 512
	rng := rand.New(rand.NewSource(7))
	noise := image.NewNRGBA(image.Rect(0, 0, n, n))
	// Independent random colours in 2 px blocks: decoded noise changes every few pixels, so every 16 px cell is busy inside.
	for by := 0; by < n/2; by++ {
		for bx := 0; bx < n/2; bx++ {
			c := color.NRGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255}
			for y := by * 2; y < by*2+2; y++ {
				for x := bx * 2; x < bx*2+2; x++ {
					noise.SetNRGBA(x, y, c)
				}
			}
		}
	}
	smooth := image.NewNRGBA(image.Rect(0, 0, n, n)) // a gentle gradient: a picture, whatever else it is
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			smooth.SetNRGBA(x, y, color.NRGBA{uint8(x / 2), uint8(y / 2), uint8((x + y) / 4), 255})
		}
	}
	dir := t.TempDir()
	write := func(name string, img image.Image) string {
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := pngenc.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		return p
	}
	noisePath, goodPath := write("noise.png", noise), write("good.png", smooth)

	if ResumeVerifies(noisePath, n, n) {
		t.Error("--resume took a noise picture for a finished one")
	}
	if !ResumeVerifies(goodPath, n, n) {
		t.Error("--resume re-rendered a smooth picture")
	}

	row := BatchRow{Out: goodPath, Verdict: VerdictRendered}
	proveFile(&row)
	if row.Verdict != VerdictRendered || row.SHA256 == "" {
		t.Errorf("a smooth picture must stay rendered and hashed, got %+v", row)
	}
	row = BatchRow{Out: noisePath, Verdict: VerdictRendered}
	proveFile(&row)
	if row.Verdict != VerdictFailed || row.Error == "" {
		t.Errorf("a noise picture must be a failed row with a reason, got %+v", row)
	}
	if _, err := os.Stat(noisePath); err == nil {
		t.Error("the noise picture is still at out, so --resume could take it for finished")
	}
	if _, err := os.Stat(noisePath + ".noise"); err != nil {
		t.Errorf("the noise picture was not set aside: %v", err)
	}
}
