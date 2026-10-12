package thumb

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func solid(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func encJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// exifJPEG builds a JPEG whose EXIF block has an orientation in IFD0 and
// a thumbnail in IFD1, followed by a large main image.
func exifJPEG(t *testing.T, thumbJPEG []byte, orientation uint16, mainSize int) []byte {
	t.Helper()
	le := binary.LittleEndian
	var tiff bytes.Buffer
	tiff.WriteString("II*\x00")
	binary.Write(&tiff, le, uint32(8))
	binary.Write(&tiff, le, uint16(1))
	binary.Write(&tiff, le, uint16(0x0112))
	binary.Write(&tiff, le, uint16(3))
	binary.Write(&tiff, le, uint32(1))
	binary.Write(&tiff, le, uint32(orientation))
	ifd1 := uint32(8 + 2 + 12 + 4)
	binary.Write(&tiff, le, ifd1)
	binary.Write(&tiff, le, uint16(2))
	thumbOff := ifd1 + 2 + 2*12 + 4
	for _, e := range [][2]uint32{{0x0201, thumbOff}, {0x0202, uint32(len(thumbJPEG))}} {
		binary.Write(&tiff, le, uint16(e[0]))
		binary.Write(&tiff, le, uint16(4))
		binary.Write(&tiff, le, uint32(1))
		binary.Write(&tiff, le, e[1])
	}
	binary.Write(&tiff, le, uint32(0))
	tiff.Write(thumbJPEG)

	var out bytes.Buffer
	out.Write([]byte{0xFF, 0xD8, 0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(2+6+tiff.Len()))
	out.WriteString("Exif\x00\x00")
	out.Write(tiff.Bytes())
	main := encJPEG(t, solid(64, 64, color.White))
	out.Write(main[2:])
	out.Write(make([]byte, mainSize))
	return out.Bytes()
}

func TestEmbeddedFromJPEGExif(t *testing.T) {
	thumbJ := encJPEG(t, solid(160, 120, color.RGBA{255, 0, 0, 255}))
	file := exifJPEG(t, thumbJ, 6, 5<<20)
	r := &countingReader{data: file}
	got, orient, err := Embedded(r, int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, thumbJ) || orient != 6 {
		t.Fatalf("got %d bytes, orientation %d", len(got), orient)
	}
	if r.read > headSize+len(thumbJ) {
		t.Errorf("read %d bytes of a %d-byte file", r.read, len(file))
	}
}

type countingReader struct {
	data []byte
	read int
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	n := copy(p, c.data[off:])
	c.read += n
	return n, nil
}

func TestOrient(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	src.Set(0, 0, color.RGBA{255, 0, 0, 255})
	src.Set(1, 0, color.RGBA{0, 0, 255, 255})
	got := Orient(src, 6)
	if b := got.Bounds(); b.Dx() != 1 || b.Dy() != 2 {
		t.Fatalf("bounds %v", b)
	}
	if r, _, _, _ := got.At(0, 0).RGBA(); r == 0 {
		t.Error("rotating 90° clockwise should put the left pixel on top")
	}
}

func TestLoadPrefersEmbeddedAndCaches(t *testing.T) {
	dir := t.TempDir()
	file := exifJPEG(t, encJPEG(t, solid(160, 120, color.White)), 1, 1000)
	p := filepath.Join(dir, "Ảnh.jpg")
	os.WriteFile(p, file, 0o644)
	ql := 0
	m := &Maker{Dir: filepath.Join(dir, "cache"), Size: 256, Timeout: time.Second, MinEmbedded: 120,
		Run: func(context.Context, string, ...string) ([]byte, error) { ql++; return nil, nil }}
	mt := time.Unix(5, 0)
	_, src, err := m.Load(context.Background(), p, int64(len(file)), mt)
	if err != nil || src != FromEmbedded {
		t.Fatalf("first load src %v err %v", src, err)
	}
	_, src, err = m.Load(context.Background(), p, int64(len(file)), mt)
	if err != nil || src != FromCache || ql != 0 {
		t.Fatalf("second load src %v err %v quicklook %d", src, err, ql)
	}
}

func TestLoadFallsBackToQuickLook(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.png")
	os.WriteFile(p, []byte("not a jpeg"), 0o644)
	m := &Maker{Dir: filepath.Join(dir, "cache"), Size: 64, Timeout: time.Second, MinEmbedded: 120}
	m.Run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		var b bytes.Buffer
		png.Encode(&b, solid(64, 64, color.Black))
		return nil, os.WriteFile(filepath.Join(args[len(args)-2], "a.png.png"), b.Bytes(), 0o644)
	}
	img, src, err := m.Load(context.Background(), p, 10, time.Unix(1, 0))
	if err != nil || src != FromQuickLook || img.Bounds().Dx() != 64 {
		t.Fatalf("src %v err %v", src, err)
	}
}
