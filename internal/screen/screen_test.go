package screen

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func solid(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 255
	}
	return img
}

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestDefaultScaleShrinksWideScreens(t *testing.T) {
	out, err := Encode(solid(2560, 1440, color.RGBA{10, 20, 30, 255}), 0)
	if err != nil {
		t.Fatal(err)
	}
	b := decode(t, out).Bounds()
	if b.Dx() != DefaultWidth || b.Dy() != 720 {
		t.Fatalf("got %dx%d", b.Dx(), b.Dy())
	}
}

func TestNarrowScreenNotUpscaled(t *testing.T) {
	out, _ := Encode(solid(800, 600, color.RGBA{}), 0)
	if b := decode(t, out).Bounds(); b.Dx() != 800 {
		t.Fatalf("width %d", b.Dx())
	}
}

func TestExplicitScaleAndColourPreserved(t *testing.T) {
	out, _ := Encode(solid(100, 100, color.RGBA{200, 100, 50, 255}), 0.5)
	img := decode(t, out)
	if b := img.Bounds(); b.Dx() != 50 || b.Dy() != 50 {
		t.Fatalf("got %v", b)
	}
	r, g, bl, _ := img.At(10, 10).RGBA()
	if r>>8 != 200 || g>>8 != 100 || bl>>8 != 50 {
		t.Fatalf("colour changed: %d %d %d", r>>8, g>>8, bl>>8)
	}
}
