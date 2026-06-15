//go:build ignore

package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
)

// drawRect fills a rectangle with rounded corners (radius r) in the given color.
func drawRect(img *image.RGBA, x0, y0, x1, y1, r int, c color.Color) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			// Round corners: compute distance from nearest corner.
			cx, cy := x, y
			if x < x0+r {
				cx = x0 + r
			} else if x >= x1-r {
				cx = x1 - r - 1
			}
			if y < y0+r {
				cy = y0 + r
			} else if y >= y1-r {
				cy = y1 - r - 1
			}
			dx := x - cx
			dy := y - cy
			if dx*dx+dy*dy <= r*r {
				img.Set(x, y, c)
			}
		}
	}
}

func main() {
	const sz = 1024
	img := image.NewRGBA(image.Rect(0, 0, sz, sz))

	// Background — deep slate, almost black, with subtle warmth.
	bg := color.RGBA{0x14, 0x18, 0x20, 0xff}
	for y := 0; y < sz; y++ {
		for x := 0; x < sz; x++ {
			img.Set(x, y, bg)
		}
	}

	// Three kanban columns, centered. Outer columns are dim, middle (in-progress) glows red.
	colors := []color.RGBA{
		{0x32, 0x38, 0x44, 0xff}, // backlog
		{0xd9, 0x2b, 0x2b, 0xff}, // in-progress (accent: Manifold-ish red)
		{0x32, 0x38, 0x44, 0xff}, // done
	}

	colW := 200
	colH := 700
	gap := 60
	startX := (sz - 3*colW - 2*gap) / 2
	startY := (sz - colH) / 2
	colR := 28

	for c := 0; c < 3; c++ {
		x0 := startX + c*(colW+gap)
		drawRect(img, x0, startY, x0+colW, startY+colH, colR, colors[c])
	}

	// Stacked cards inside each column. Three per column, lightly translucent on dim cols.
	cardR := 14
	cardH := 90
	cardGap := 24
	cardLight := color.RGBA{0xee, 0xee, 0xee, 0xff}
	cardMid := color.RGBA{0xff, 0xff, 0xff, 0xff}

	for c := 0; c < 3; c++ {
		x0 := startX + c*(colW+gap) + 24
		x1 := startX + (c+1)*colW + c*gap - 24
		ccol := cardLight
		if c == 1 {
			ccol = cardMid
		}
		for ci := 0; ci < 3; ci++ {
			cy0 := startY + 60 + ci*(cardH+cardGap)
			cy1 := cy0 + cardH
			drawRect(img, x0, cy0, x1, cy1, cardR, ccol)
		}
	}

	f, err := os.Create("/tmp/openkanban-icon/icon-1024.png")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}
