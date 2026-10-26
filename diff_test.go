package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// encodeRGBA is a w x h picture filled with c, with the given pixels set to
// red, as a PNG.
func encodeRGBA(w, h int, c color.RGBA, red ...image.Point) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = []byte{c.R, c.G, c.B, c.A}[i%4]
	}
	for _, p := range red {
		img.SetRGBA(p.X, p.Y, color.RGBA{255, 0, 0, 255})
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// The first picture goes whole; a small change goes as just its region with
// its offset; the same picture again goes not at all; a change to most of
// the picture goes whole.
func TestDifferSendsOnlyChanges(t *testing.T) {
	var d differ
	white := color.RGBA{255, 255, 255, 255}
	if _, _, _, full := d.next(encodeRGBA(200, 100, white)); !full {
		t.Fatal("the first picture should go whole")
	}

	out, x, y, full := d.next(encodeRGBA(200, 100, white, image.Pt(50, 20), image.Pt(60, 25)))
	if full || x != 50 || y != 20 {
		t.Fatalf("a small change went full=%t at %d,%d", full, x, y)
	}
	if w, h := pngSize(out); w != 11 || h != 6 {
		t.Errorf("the region is %dx%d, want 11x6", w, h)
	}

	if out, _, _, _ := d.next(encodeRGBA(200, 100, white, image.Pt(50, 20), image.Pt(60, 25))); out != nil {
		t.Error("an unchanged picture should send nothing")
	}

	if _, _, _, full := d.next(encodeRGBA(200, 100, color.RGBA{0, 0, 0, 255})); !full {
		t.Error("a change to the whole picture should go whole")
	}
}
