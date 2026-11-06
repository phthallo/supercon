package main

import (
	"bytes"
	"compress/zlib"
	"image"
	"image/color"
	"io"
	"testing"
)

func unpack(t *testing.T, frame []byte) []byte {
	t.Helper()
	zr, err := zlib.NewReader(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	planes, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if len(planes) != 2*PlaneSize {
		t.Fatalf("planes are %d bytes, want %d", len(planes), 2*PlaneSize)
	}
	return planes
}

func TestPackFrameBitOrder(t *testing.T) {
	canvas := make([]byte, GridW*GridH)
	canvas[0] = Black       // panel pixels x 0-3, y 0-3
	canvas[1*GridW+2] = Red // x 8-11, y 4-7
	planes := unpack(t, PackFrame(Render(canvas, nil)))
	black, red := planes[:PlaneSize], planes[PlaneSize:]

	rowBytes := PanelW / 8
	if black[0] != 0xf0 || black[1] != 0 || red[0] != 0 {
		t.Fatalf("cell 0: black[0]=%#x black[1]=%#x red[0]=%#x", black[0], black[1], red[0])
	}
	if black[4*rowBytes] != 0 {
		t.Fatalf("cell 0 leaked into row 4")
	}
	if got := red[4*rowBytes+1]; got != 0xf0 {
		t.Fatalf("red cell: byte = %#x, want 0xf0", got)
	}
	if got := black[4*rowBytes+1]; got != 0 {
		t.Fatalf("red cell also set black: %#x", got)
	}
}

func TestPackFrameSinglePixelMSBFirst(t *testing.T) {
	img := Render(make([]byte, GridW*GridH), nil)
	img.Pix[3] = Black // x=3, y=0
	planes := unpack(t, PackFrame(img))
	if planes[0] != 0x10 {
		t.Fatalf("x=3 packed to %#08b, want 0b00010000", planes[0])
	}
}

func TestWidgetsDrawOverCanvas(t *testing.T) {
	canvas := make([]byte, GridW*GridH)
	for i := range canvas {
		canvas[i] = Black
	}
	box := Widget{ID: 1, Kind: KindBox, X: 10, Y: 10, W: 20, H: 20, Fill: RGBRed, Border: 2, Color: RGBWhite}
	img := Render(canvas, []Widget{box})
	at := func(x, y int) uint8 { return img.Pix[y*img.Stride+x] }
	if at(0, 0) != Black || at(10, 10) != White || at(20, 20) != Red || at(29, 29) != White || at(30, 30) != Black {
		t.Fatalf("got %d %d %d %d %d", at(0, 0), at(10, 10), at(20, 20), at(29, 29), at(30, 30))
	}
}

func TestWidgetZOrder(t *testing.T) {
	low := Widget{ID: 1, Kind: KindBox, X: 0, Y: 0, W: 10, H: 10, Fill: RGBRed, Z: 5}
	high := Widget{ID: 2, Kind: KindBox, X: 0, Y: 0, W: 10, H: 10, Fill: RGBBlack, Z: 1}
	img := Render(make([]byte, GridW*GridH), []Widget{low, high})
	if img.Pix[0] != Red {
		t.Fatalf("higher z should win, got %d", img.Pix[0])
	}
}

func TestTextRendersInk(t *testing.T) {
	w := NewWidget(KindText)
	w.Text = "HELLO"
	w.Color = RGBRed
	img := Render(nil, []Widget{w})
	red := 0
	for _, p := range img.Pix {
		if p == Red {
			red++
		}
	}
	if red == 0 {
		t.Fatal("no text pixels drawn")
	}
}

func TestWidgetLayerIsTransparentOutsideWidgets(t *testing.T) {
	img := Render(nil, []Widget{{Kind: KindBox, X: 0, Y: 0, W: 4, H: 4, Fill: RGBBlack}})
	if img.Pix[0] != Black || img.Pix[PanelW*PanelH-1] != Clear {
		t.Fatal("layer background should be Clear")
	}
	planes := unpack(t, PackFrame(img))
	if planes[PlaneSize-1] != 0 || planes[2*PlaneSize-1] != 0 {
		t.Fatal("Clear must pack as white")
	}
}

func TestDitherUsesOnlyInk(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			src.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 4), 128, uint8(255 - x)})
		}
	}
	out := Dither(src, 30, 20, false)
	if len(out) != 600 {
		t.Fatalf("len %d", len(out))
	}
	seen := map[byte]bool{}
	for _, p := range out {
		if p > Red {
			t.Fatalf("index %d outside the panel palette", p)
		}
		seen[p] = true
	}
	if len(seen) < 2 {
		t.Fatalf("gradient dithered to a single colour: %v", seen)
	}
}

func TestInkMixesColoursOverEachTile(t *testing.T) {
	count := func(rgb int) [3]int {
		var n [3]int
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				n[Ink(rgb, x, y)]++
			}
		}
		return n
	}
	for _, tc := range []struct {
		rgb  int
		want [3]int // white, black, red out of 16
	}{
		{RGBWhite, [3]int{16, 0, 0}},
		{RGBBlack, [3]int{0, 16, 0}},
		{RGBRed, [3]int{0, 0, 16}},
		{0x808080, [3]int{8, 8, 0}},
		{0x800000, [3]int{0, 8, 8}},
		{0x0000ff, [3]int{2, 14, 0}},
	} {
		if got := count(tc.rgb); got != tc.want {
			t.Errorf("Ink(%06x) = %v, want %v", tc.rgb, got, tc.want)
		}
	}
}

func TestDitherGreyHasNoRed(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			src.Set(x, y, color.RGBA{255, uint8(x * 4), uint8(y * 4), 255})
		}
	}
	seen := map[byte]bool{}
	for _, p := range Dither(src, 30, 20, true) {
		seen[p] = true
	}
	if seen[Red] || !seen[White] || !seen[Black] {
		t.Fatalf("want only white and black, got %v", seen)
	}
}
