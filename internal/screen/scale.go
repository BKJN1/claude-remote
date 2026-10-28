// Package screen captures the display as a (downscaled) PNG.
package screen

import (
	"bytes"
	"image"
	"image/png"
)

// DefaultWidth is the target width when the caller gives no scale, to keep
// mobile traffic small.
const DefaultWidth = 1280

// Encode downsamples img (area averaging, so text stays readable) and
// encodes it as PNG. scale<=0 means "shrink to DefaultWidth if wider".
func Encode(img *image.RGBA, scale float64) ([]byte, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if scale <= 0 {
		scale = 1
		if w > DefaultWidth {
			scale = float64(DefaultWidth) / float64(w)
		}
	}
	out := img
	if scale < 1 {
		nw, nh := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
		out = downscale(img, nw, nh)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func downscale(src *image.RGBA, nw, nh int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := y*sh/nh, max(y*sh/nh+1, (y+1)*sh/nh)
		for x := 0; x < nw; x++ {
			x0, x1 := x*sw/nw, max(x*sw/nw+1, (x+1)*sw/nw)
			var r, g, bl, n uint32
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[(sy+b.Min.Y-src.Rect.Min.Y)*src.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[(sx+b.Min.X-src.Rect.Min.X)*4:]
					r += uint32(p[0])
					g += uint32(p[1])
					bl += uint32(p[2])
					n++
				}
			}
			o := dst.Pix[y*dst.Stride+x*4:]
			o[0], o[1], o[2], o[3] = uint8(r/n), uint8(g/n), uint8(bl/n), 255
		}
	}
	return dst
}
