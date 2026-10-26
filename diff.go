// Partial page frames for viewers across a network: the region of the page
// that changed since the last frame, instead of the whole page every time.
// Typing or a hover changes a few hundred pixels; sending them as a small
// picture costs a hundredth of a full frame. A scroll or a new page changes
// most of the frame, and then the whole frame goes, which also clears the
// partial pictures the viewer has placed.
package main

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"
)

// partialLimit is the share of the frame a change may cover and still go as
// a partial picture; past it the whole frame is cheaper to place.
const partialLimit = 0.5

// differ remembers the last full frame of a page.
type differ struct {
	last *image.RGBA
}

// next takes the page's newest picture and returns what to send: the
// changed region as a PNG with its offset, or the whole picture with full
// set. An unchanged picture returns nil.
func (d *differ) next(data []byte) (out []byte, x, y int, full bool) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return data, 0, 0, true
	}
	rgba, ok := img.(*image.RGBA)
	if !ok {
		rgba = image.NewRGBA(img.Bounds())
		draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
	}
	last := d.last
	d.last = rgba
	if last == nil || last.Bounds() != rgba.Bounds() {
		return data, 0, 0, true
	}
	r, changed := changedRect(last, rgba)
	if !changed {
		return nil, 0, 0, false
	}
	b := rgba.Bounds()
	if float64(r.Dx()*r.Dy()) > partialLimit*float64(b.Dx()*b.Dy()) {
		return data, 0, 0, true
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, rgba.SubImage(r)); err != nil {
		return data, 0, 0, true
	}
	return buf.Bytes(), r.Min.X, r.Min.Y, false
}

// changedRect is the smallest rectangle holding every pixel that differs
// between two pictures of the same size.
func changedRect(a, b *image.RGBA) (image.Rectangle, bool) {
	w, h := a.Bounds().Dx(), a.Bounds().Dy()
	minX, minY, maxX, maxY := w, h, -1, -1
	for y := 0; y < h; y++ {
		ra, rb := a.Pix[y*a.Stride:y*a.Stride+w*4], b.Pix[y*b.Stride:y*b.Stride+w*4]
		if bytes.Equal(ra, rb) {
			continue
		}
		minY = min(minY, y)
		maxY = y
		first, last := 0, w-1
		for first < w && bytes.Equal(ra[first*4:first*4+4], rb[first*4:first*4+4]) {
			first++
		}
		for last > first && bytes.Equal(ra[last*4:last*4+4], rb[last*4:last*4+4]) {
			last--
		}
		minX, maxX = min(minX, first), max(maxX, last)
	}
	if maxY < 0 {
		return image.Rectangle{}, false
	}
	return image.Rect(minX, minY, maxX+1, maxY+1), true
}
