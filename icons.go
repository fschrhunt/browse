// The bar's icons, SVG files drawn for browse, and the start screen's box,
// turned into PNGs sized to the terminal's cells. Only what the icons use is read: each path's d attribute
// with the M, L, H, V, C, and Z commands, on a 24x24 canvas, stroked
// stroke-width wide with round caps and joins.
package main

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"strconv"
	"strings"

	"github.com/srwiley/rasterx"
	"golang.org/x/image/math/fixed"
)

//go:embed icons/*.svg
var iconFiles embed.FS

var (
	pathData    = regexp.MustCompile(`\sd="([^"]*)"`)
	strokeWidth = regexp.MustCompile(`stroke-width="([0-9.]+)"`)
	pathToken   = regexp.MustCompile(`[A-Za-z]|-?[0-9]*\.?[0-9]+`)
)

// drawIcon draws the named icon in c, as large as fits and centered, on a
// transparent w x h PNG.
func drawIcon(name string, w, h int, c color.Color) ([]byte, error) {
	src, err := iconFiles.ReadFile("icons/" + name + ".svg")
	if err != nil {
		return nil, err
	}
	size := float64(min(w, h))
	scale := size / 24
	left, top := (float64(w)-size)/2, (float64(h)-size)/2
	at := func(x, y float64) fixed.Point26_6 { return rasterx.ToFixedP(left+x*scale, top+y*scale) }

	width := 2.0
	if m := strokeWidth.FindSubmatch(src); m != nil {
		width, _ = strconv.ParseFloat(string(m[1]), 64)
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	scanner := rasterx.NewScannerGV(w, h, img, img.Bounds())
	scanner.SetWinding(true) // where strokes overlap they stay solid
	stroker := rasterx.NewStroker(w, h, scanner)
	stroker.SetStroke(fixed.Int26_6(width*scale*64), 4<<6, rasterx.RoundCap, rasterx.RoundCap, rasterx.RoundGap, rasterx.Round)
	stroker.SetColor(c)
	for _, m := range pathData.FindAllSubmatch(src, -1) {
		if err := tracePath(string(m[1]), at, stroker); err != nil {
			return nil, fmt.Errorf("icon %s: %w", name, err)
		}
	}
	stroker.Draw()

	var out bytes.Buffer
	err = png.Encode(&out, img)
	return out.Bytes(), err
}

// tracePath adds an SVG path to path, mapping its points through at.
func tracePath(d string, at func(x, y float64) fixed.Point26_6, path rasterx.Adder) error {
	tokens := pathToken.FindAllString(d, -1)
	i := 0
	numbers := func(n int) ([]float64, error) {
		if i+n > len(tokens) {
			return nil, errors.New("the path ends early")
		}
		out := make([]float64, n)
		for j := range out {
			v, err := strconv.ParseFloat(tokens[i+j], 64)
			if err != nil {
				return nil, fmt.Errorf("want a number, got %q", tokens[i+j])
			}
			out[j] = v
		}
		i += n
		return out, nil
	}

	var cmd byte
	var x, y, startX, startY float64
	open := false
	for i < len(tokens) {
		if t := tokens[i]; len(t) == 1 && strings.Contains("MLHVCZmlhvcz", t) {
			cmd = t[0]
			i++
		}
		var dx, dy float64 // what relative coordinates are relative to
		if cmd >= 'a' {
			dx, dy = x, y
		}
		if cmd != 'M' && cmd != 'm' && !open {
			return errors.New("the path must start with M")
		}
		var p []float64
		var err error
		switch cmd | 0x20 { // lower case
		case 'm':
			if p, err = numbers(2); err != nil {
				return err
			}
			if open {
				path.Stop(false)
			}
			x, y = p[0]+dx, p[1]+dy
			startX, startY = x, y
			path.Start(at(x, y))
			open = true
			cmd -= 'M' - 'L' // more points after M are lines
		case 'l':
			if p, err = numbers(2); err != nil {
				return err
			}
			x, y = p[0]+dx, p[1]+dy
			path.Line(at(x, y))
		case 'h':
			if p, err = numbers(1); err != nil {
				return err
			}
			x = p[0] + dx
			path.Line(at(x, y))
		case 'v':
			if p, err = numbers(1); err != nil {
				return err
			}
			y = p[0] + dy
			path.Line(at(x, y))
		case 'c':
			if p, err = numbers(6); err != nil {
				return err
			}
			path.CubeBezier(at(p[0]+dx, p[1]+dy), at(p[2]+dx, p[3]+dy), at(p[4]+dx, p[5]+dy))
			x, y = p[4]+dx, p[5]+dy
		case 'z':
			path.Stop(true)
			x, y, open = startX, startY, false
			cmd = 0 // a number after Z is an error
		default:
			return fmt.Errorf("unsupported path command %q", cmd)
		}
	}
	if open {
		path.Stop(false)
	}
	return nil
}

// drawBox draws a w x h PNG of a rounded rectangle filled with fill and
// outlined in border, marginY pixels in from the top and bottom, for text to
// sit on.
func drawBox(w, h int, marginY, radius float64, fill, border color.Color) ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	scanner := rasterx.NewScannerGV(w, h, img, img.Bounds())
	filler := rasterx.NewFiller(w, h, scanner)
	top, bottom := marginY, float64(h)-marginY
	filler.SetColor(border)
	rasterx.AddRoundRect(0, top, float64(w), bottom, radius, radius, 0, rasterx.RoundGap, filler)
	filler.Draw()
	filler.Clear()
	filler.SetColor(fill)
	rasterx.AddRoundRect(1, top+1, float64(w-1), bottom-1, radius-1, radius-1, 0, rasterx.RoundGap, filler)
	filler.Draw()

	var out bytes.Buffer
	err := png.Encode(&out, img)
	return out.Bytes(), err
}
