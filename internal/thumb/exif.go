package thumb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"io"
)

// headSize is how much of a file is read to find an embedded preview: one
// round trip over the network instead of the whole photo.
const headSize = 128 << 10

// ErrNoEmbedded means the file carries no usable preview.
var ErrNoEmbedded = errors.New("thumb: no embedded preview")

// Embedded finds the JPEG preview a camera or editor stored in a JPEG's
// EXIF block or in a TIFF-based raw file (CR2, NEF, ARW, DNG…), reading
// only the bytes it needs. It returns the preview and the photo's EXIF
// orientation (1 when unknown).
func Embedded(r io.ReaderAt, size int64) ([]byte, int, error) {
	head := make([]byte, min(size, headSize))
	n, err := r.ReadAt(head, 0)
	if n < 16 {
		if err == nil {
			err = ErrNoEmbedded
		}
		return nil, 1, err
	}
	head = head[:n]

	var tiff []byte
	var tiffBase int64
	switch {
	case head[0] == 0xFF && head[1] == 0xD8:
		off, ok := exifOffset(head)
		if !ok {
			return nil, 1, ErrNoEmbedded
		}
		tiff, tiffBase = head[off:], int64(off)
	case bytes.HasPrefix(head, []byte("II*\x00")) || bytes.HasPrefix(head, []byte("MM\x00*")):
		tiff, tiffBase = head, 0
	default:
		return nil, 1, ErrNoEmbedded
	}

	orient, cands := walkTIFF(tiff)
	if len(cands) == 0 {
		return nil, orient, ErrNoEmbedded
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if pick(c.length, best.length) {
			best = c
		}
	}
	if best.length <= 0 || best.length > 8<<20 {
		return nil, orient, ErrNoEmbedded
	}
	buf := make([]byte, best.length)
	start := tiffBase + best.offset
	if start+best.length <= int64(len(head)) {
		copy(buf, head[start:])
	} else if _, err := r.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, orient, err
	}
	if len(buf) < 4 || buf[0] != 0xFF || buf[1] != 0xD8 {
		return nil, orient, ErrNoEmbedded
	}
	return buf, orient, nil
}

// pick prefers previews of 20KB–600KB (sharp enough for a grid cell,
// small enough to fetch quickly), then the larger below that range and
// the smaller above it.
func pick(a, b int64) bool {
	const lo, hi = 20 << 10, 600 << 10
	in := func(v int64) bool { return v >= lo && v <= hi }
	switch {
	case in(a) && !in(b):
		return true
	case !in(a) && in(b):
		return false
	case in(a) && in(b):
		return a > b
	case a < lo && b < lo:
		return a > b
	case a > hi && b > hi:
		return a < b
	default:
		return a < b
	}
}

func exifOffset(b []byte) (int, bool) {
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 0, false
		}
		marker := b[i+1]
		if marker == 0xDA || marker == 0xD9 {
			return 0, false
		}
		segLen := int(binary.BigEndian.Uint16(b[i+2:]))
		if marker == 0xE1 && i+10 <= len(b) && bytes.Equal(b[i+4:i+10], []byte("Exif\x00\x00")) {
			return i + 10, true
		}
		i += 2 + segLen
	}
	return 0, false
}

type candidate struct{ offset, length int64 }

func walkTIFF(t []byte) (orientation int, cands []candidate) {
	orientation = 1
	if len(t) < 8 {
		return
	}
	var bo binary.ByteOrder = binary.LittleEndian
	if t[0] == 'M' {
		bo = binary.BigEndian
	}
	seen := map[uint32]bool{}
	queue := []uint32{bo.Uint32(t[4:])}
	first := true
	for len(queue) > 0 && len(seen) < 16 {
		off := queue[0]
		queue = queue[1:]
		if off == 0 || seen[off] || int(off)+2 > len(t) {
			continue
		}
		seen[off] = true
		count := int(bo.Uint16(t[off:]))
		var jOff, jLen, sOff, sLen int64
		compression := 0
		for k := 0; k < count; k++ {
			e := int(off) + 2 + k*12
			if e+12 > len(t) {
				break
			}
			tag := bo.Uint16(t[e:])
			typ := bo.Uint16(t[e+2:])
			cnt := bo.Uint32(t[e+4:])
			val := func() int64 {
				if typ == 3 {
					return int64(bo.Uint16(t[e+8:]))
				}
				return int64(bo.Uint32(t[e+8:]))
			}
			switch tag {
			case 0x0112:
				if first {
					orientation = int(val())
				}
			case 0x0103:
				compression = int(val())
			case 0x0201:
				jOff = val()
			case 0x0202:
				jLen = val()
			case 0x0111:
				if cnt == 1 {
					sOff = val()
				}
			case 0x0117:
				if cnt == 1 {
					sLen = val()
				}
			case 0x014A:
				if cnt == 1 {
					queue = append(queue, uint32(val()))
				} else if p := int(bo.Uint32(t[e+8:])); p+4*int(cnt) <= len(t) {
					for s := 0; s < int(cnt); s++ {
						queue = append(queue, bo.Uint32(t[p+4*s:]))
					}
				}
			}
		}
		if jOff > 0 && jLen > 0 {
			cands = append(cands, candidate{jOff, jLen})
		}
		if (compression == 6 || compression == 7) && sOff > 0 && sLen > 0 {
			cands = append(cands, candidate{sOff, sLen})
		}
		if next := int(off) + 2 + count*12; next+4 <= len(t) {
			queue = append(queue, bo.Uint32(t[next:]))
		}
		first = false
	}
	if orientation < 1 || orientation > 8 {
		orientation = 1
	}
	return
}

// Orient turns img upright according to an EXIF orientation.
func Orient(img image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for sy := 0; sy < h; sy++ {
		for sx := 0; sx < w; sx++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-sx, sy
			case 3:
				dx, dy = w-1-sx, h-1-sy
			case 4:
				dx, dy = sx, h-1-sy
			case 5:
				dx, dy = sy, sx
			case 6:
				dx, dy = h-1-sy, sx
			case 7:
				dx, dy = h-1-sy, w-1-sx
			case 8:
				dx, dy = sy, w-1-sx
			}
			dst.Set(dx, dy, img.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}
