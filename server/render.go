package main

import (
	"bytes"
	"compress/zlib"
	"image"
	"image/color"
	"image/draw"
	"sort"
	"strings"
	"sync"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/gofont/gosmallcaps"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	GridW  = 240
	GridH  = 168
	Cell   = 4
	PanelW = GridW * Cell
	PanelH = GridH * Cell
	// Largest stroke or shape one paint request may set.
	MaxPaintCells = 10000
	// One bit per pixel, MONO_HLSB, matching EPaperDisplay._framebuffer_* in firmware/lib/epd.py.
	PlaneSize = PanelW * PanelH / 8
)

const (
	White uint8 = iota
	Black
	Red
	Clear // only in the web widget layer; packs as white
)

var Palette = color.Palette{
	color.RGBA{0xff, 0xff, 0xff, 0xff},
	color.RGBA{0x00, 0x00, 0x00, 0xff},
	color.RGBA{0xff, 0x00, 0x00, 0xff},
	color.RGBA{0, 0, 0, 0},
}

var inkPalette = Palette[:3]

// Widget and brush colours are 0xRRGGBB.
const (
	RGBWhite = 0xffffff
	RGBBlack = 0x000000
	RGBRed   = 0xff0000
)

var bayer4 = [16]float64{0, 8, 2, 10, 12, 4, 14, 6, 3, 11, 1, 9, 15, 7, 13, 5}

// Ink returns the panel colour for rgb at (x, y), using an ordered dither so a flat
// colour becomes an even mix of white, black and red. paint.js has the same function.
func Ink(rgb, x, y int) uint8 {
	r := float64(rgb>>16&0xff) / 255
	g := float64(rgb>>8&0xff) / 255
	b := float64(rgb&0xff) / 255
	red := max(0, r-max(g, b))
	white := 0.299*r + 0.587*g + 0.114*b - 0.299*red
	black := 1 - white - red
	t := (bayer4[(y&3)*4+(x&3)] + 0.5) / 16
	switch {
	case t < black:
		return Black
	case t < black+red:
		return Red
	}
	return White
}

func solid(c uint8) func(x, y int) uint8 { return func(int, int) uint8 { return c } }

// Fonts maps the widget font name to its TTF. The admin page lists the same names.
var Fonts = map[string][]byte{
	"mono":      gomono.TTF,
	"mono-bold": gomonobold.TTF,
	"sans":      goregular.TTF,
	"sans-bold": gobold.TTF,
	"italic":    goitalic.TTF,
	"smallcaps": gosmallcaps.TTF,
}

type faceKey struct {
	font string
	size int
}

var (
	faceMu sync.Mutex
	parsed = map[string]*opentype.Font{}
	faces  = map[faceKey]font.Face{}
)

func face(name string, size int) font.Face {
	faceMu.Lock()
	defer faceMu.Unlock()
	key := faceKey{name, size}
	if f, ok := faces[key]; ok {
		return f
	}
	data, ok := parsed[name]
	if !ok {
		var err error
		if data, err = opentype.Parse(Fonts[name]); err != nil {
			panic(err)
		}
		parsed[name] = data
	}
	f, err := opentype.NewFace(data, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err)
	}
	faces[key] = f
	return f
}

// Render composes the badge frame. With canvas == nil the background is Clear,
// which gives the widget layer the web page draws over its own canvas.
func Render(canvas []byte, widgets []Widget) *image.Paletted {
	img := image.NewPaletted(image.Rect(0, 0, PanelW, PanelH), Palette)
	if canvas == nil {
		for i := range img.Pix {
			img.Pix[i] = Clear
		}
	} else {
		for cy := 0; cy < GridH; cy++ {
			for cx := 0; cx < GridW; cx++ {
				fillRect(img, image.Rect(cx*Cell, cy*Cell, (cx+1)*Cell, (cy+1)*Cell), canvas[cy*GridW+cx])
			}
		}
	}
	sorted := append([]Widget(nil), widgets...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Z != sorted[j].Z {
			return sorted[i].Z < sorted[j].Z
		}
		return sorted[i].ID < sorted[j].ID
	})
	for _, w := range sorted {
		drawWidget(img, w)
	}
	return img
}

func fillRect(img *image.Paletted, r image.Rectangle, c uint8) {
	r = r.Intersect(img.Rect)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		row := img.Pix[y*img.Stride:]
		for x := r.Min.X; x < r.Max.X; x++ {
			row[x] = c
		}
	}
}

func fillInk(img *image.Paletted, r image.Rectangle, rgb int) {
	r = r.Intersect(img.Rect)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		row := img.Pix[y*img.Stride:]
		for x := r.Min.X; x < r.Max.X; x++ {
			row[x] = Ink(rgb, x, y)
		}
	}
}

func drawWidget(img *image.Paletted, w Widget) {
	r := image.Rect(w.X, w.Y, w.X+w.W, w.Y+w.H)
	if w.Fill >= 0 {
		fillInk(img, r, w.Fill)
	}
	switch w.Kind {
	case KindText:
		drawText(img, r, w)
	case KindImage:
		if len(w.dither) == w.W*w.H {
			clip := r.Intersect(img.Rect)
			for y := clip.Min.Y; y < clip.Max.Y; y++ {
				for x := clip.Min.X; x < clip.Max.X; x++ {
					img.Pix[y*img.Stride+x] = w.dither[(y-w.Y)*w.W+(x-w.X)]
				}
			}
		}
	}
	if b := w.Border; b > 0 {
		c := w.Color
		fillInk(img, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+b), c)
		fillInk(img, image.Rect(r.Min.X, r.Max.Y-b, r.Max.X, r.Max.Y), c)
		fillInk(img, image.Rect(r.Min.X, r.Min.Y, r.Min.X+b, r.Max.Y), c)
		fillInk(img, image.Rect(r.Max.X-b, r.Min.Y, r.Max.X, r.Max.Y), c)
	}
}

// drawText wraps on spaces and newlines inside the widget box. Glyph coverage is
// thresholded because the panel has no grey.
func drawText(img *image.Paletted, r image.Rectangle, w Widget) {
	f := face(w.Font, w.FontSize)
	pad := w.Border + 2
	inner := r.Inset(pad)
	if inner.Empty() {
		return
	}
	mask := image.NewAlpha(image.Rect(0, 0, inner.Dx(), inner.Dy()))
	d := &font.Drawer{Dst: mask, Src: image.Opaque, Face: f}
	m := f.Metrics()
	lineH := m.Height.Ceil()
	y := m.Ascent.Ceil()
	for _, line := range wrap(d, w.Text, inner.Dx()) {
		if y-m.Ascent.Ceil() >= inner.Dy() {
			break
		}
		x := 0
		if w.Align == "center" {
			x = (inner.Dx() - d.MeasureString(line).Ceil()) / 2
		}
		d.Dot = fixed.P(x, y)
		d.DrawString(line)
		y += lineH
	}
	clip := inner.Intersect(img.Rect)
	for py := clip.Min.Y; py < clip.Max.Y; py++ {
		for px := clip.Min.X; px < clip.Max.X; px++ {
			if mask.AlphaAt(px-inner.Min.X, py-inner.Min.Y).A >= 0x80 {
				img.Pix[py*img.Stride+px] = Ink(w.Color, px, py)
			}
		}
	}
}

func wrap(d *font.Drawer, text string, width int) []string {
	var lines []string
	for _, para := range strings.Split(text, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		cur := words[0]
		for _, word := range words[1:] {
			if d.MeasureString(cur+" "+word).Ceil() <= width {
				cur += " " + word
			} else {
				lines = append(lines, cur)
				cur = word
			}
		}
		lines = append(lines, cur)
	}
	return lines
}

// Dither scales src to w×h, flattens transparency onto white and
// Floyd–Steinberg dithers it to white/black/red palette indices.
// grey dithers the image's brightness to white and black only.
func Dither(src image.Image, w, h int, grey bool) []byte {
	flat := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(flat, flat.Rect, image.White, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(flat, flat.Rect, src, src.Bounds(), xdraw.Over, nil)
	var in image.Image = flat
	pal := inkPalette
	if grey {
		g := image.NewGray(flat.Rect)
		draw.Draw(g, g.Rect, flat, image.Point{}, draw.Src)
		in, pal = g, Palette[:Red]
	}
	dst := image.NewPaletted(flat.Rect, pal)
	draw.FloydSteinberg.Draw(dst, dst.Rect, in, image.Point{})
	return dst.Pix
}

// PackFrame returns the black plane followed by the red plane, zlib-compressed.
// Bit 7 of each byte is the leftmost pixel.
func PackFrame(img *image.Paletted) []byte {
	planes := make([]byte, 2*PlaneSize)
	black, red := planes[:PlaneSize], planes[PlaneSize:]
	for y := 0; y < PanelH; y++ {
		for x := 0; x < PanelW; x++ {
			idx := y*PanelW + x
			bit := byte(0x80) >> (idx & 7)
			switch img.Pix[y*img.Stride+x] {
			case Black:
				black[idx>>3] |= bit
			case Red:
				red[idx>>3] |= bit
			}
		}
	}
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, zlib.BestCompression)
	zw.Write(planes)
	zw.Close()
	return buf.Bytes()
}
