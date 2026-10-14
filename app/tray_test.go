package main

import (
	"bytes"
	"image/color"
	"image/png"
	"testing"
)

func TestTrayIconIsBlackTemplate(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(trayIcon()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != trayIconSize || b.Dy() != trayIconSize {
		t.Fatalf("size = %v, want %dx%d", b, trayIconSize, trayIconSize)
	}
	var opaque, partial int
	for y := range trayIconSize {
		for x := range trayIconSize {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if c.A > 0 && (c.R != 0 || c.G != 0 || c.B != 0) {
				t.Fatalf("pixel (%d,%d) = %v, want black", x, y, c)
			}
			switch c.A {
			case 0:
			case 255:
				opaque++
			default:
				partial++
			}
		}
	}
	if opaque == 0 || partial == 0 {
		t.Fatalf("opaque=%d partial=%d, want both > 0", opaque, partial)
	}
}
