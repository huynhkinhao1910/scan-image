package search

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/isuccesscorp/scan-image/internal/fold"
)

// Snapshot file layout, all integers little-endian:
//
//	"SIDX"  version u32
//	complete u8  scannedAt i64 (unix nanoseconds, 0 = never)
//	nRoots u32, then per root: id u32, keyLen u32, key, pathLen u32, path
//	n u32 (entries, roots included)
//	sections until 4 bytes remain: tag u32, length u64, payload
//	crc32c u32 over every preceding byte
//
// Sections: names (arena bytes), nameOff (n end offsets), parent (n u32),
// size (n i64), mtime (n i64, unix seconds), flags (n u8). Readers skip
// unknown tags. The folded arena is not stored; Read rebuilds it so a
// change to the fold table can never leave stale search data on disk.
const (
	snapshotMagic   = "SIDX"
	snapshotVersion = 1

	tagNames   uint32 = 1
	tagNameOff uint32 = 2
	tagParent  uint32 = 3
	tagSize    uint32 = 4
	tagMTime   uint32 = 5
	tagFlags   uint32 = 6

	numRequiredTags = 6
	ioChunk         = 16 << 10
	sectionHeader   = 12
	minRootBytes    = 12
	crcBytes        = 4
)

// ErrCorrupt is wrapped by every error Read returns for a bad file.
var ErrCorrupt = errors.New("search: corrupt snapshot")

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Meta describes how complete a snapshot is.
type Meta struct {
	// Complete reports that the scan that produced the index finished.
	Complete  bool
	ScannedAt time.Time
}

// Write serialises ix to w. It works on a Clone, so it holds no lock on ix
// while writing and is safe during concurrent AddChildren.
func Write(w io.Writer, ix *Index, m Meta) error {
	c := ix.Clone()
	bw := bufio.NewWriterSize(w, 1<<20)
	h := crc32.New(crcTable)
	out := io.MultiWriter(bw, h)

	n := len(c.parent)
	hdr := []byte(snapshotMagic)
	hdr = binary.LittleEndian.AppendUint32(hdr, snapshotVersion)
	hdr = append(hdr, b2u8(m.Complete))
	var at int64
	if !m.ScannedAt.IsZero() {
		at = m.ScannedAt.UnixNano()
	}
	hdr = binary.LittleEndian.AppendUint64(hdr, uint64(at))
	roots := rootList(c.roots)
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(len(roots)))
	for _, r := range roots {
		hdr = binary.LittleEndian.AppendUint32(hdr, r.ID)
		hdr = appendString(hdr, r.Key)
		hdr = appendString(hdr, r.Path)
	}
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(n))
	if _, err := out.Write(hdr); err != nil {
		return err
	}

	if err := writeSection(out, tagNames, uint64(len(c.names)), func() error {
		_, err := out.Write(c.names)
		return err
	}); err != nil {
		return err
	}
	if err := writeFixed(out, tagNameOff, c.nameOff[1:], 4, binary.LittleEndian.PutUint32); err != nil {
		return err
	}
	if err := writeFixed(out, tagParent, c.parent, 4, binary.LittleEndian.PutUint32); err != nil {
		return err
	}
	if err := writeFixed(out, tagSize, c.size, 8, putInt64); err != nil {
		return err
	}
	if err := writeFixed(out, tagMTime, c.mtime, 8, putInt64); err != nil {
		return err
	}
	if err := writeSection(out, tagFlags, uint64(n), func() error {
		_, err := out.Write(c.flags)
		return err
	}); err != nil {
		return err
	}

	if _, err := bw.Write(binary.LittleEndian.AppendUint32(nil, h.Sum32())); err != nil {
		return err
	}
	return bw.Flush()
}

func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func putInt64(b []byte, v int64) { binary.LittleEndian.PutUint64(b, uint64(v)) }
func getInt64(b []byte) int64    { return int64(binary.LittleEndian.Uint64(b)) }

func appendString(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

func writeSection(w io.Writer, tag uint32, length uint64, body func() error) error {
	var h [sectionHeader]byte
	binary.LittleEndian.PutUint32(h[:], tag)
	binary.LittleEndian.PutUint64(h[4:], length)
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	return body()
}

func writeFixed[T any](w io.Writer, tag uint32, s []T, width int, put func([]byte, T)) error {
	return writeSection(w, tag, uint64(len(s))*uint64(width), func() error {
		buf := make([]byte, ioChunk*width)
		for len(s) > 0 {
			k := min(len(s), ioChunk)
			for i, v := range s[:k] {
				put(buf[i*width:], v)
			}
			if _, err := w.Write(buf[:k*width]); err != nil {
				return err
			}
			s = s[k:]
		}
		return nil
	})
}

// decoder reads the snapshot body while hashing it and tracking how many
// body bytes the file still has, so no length is trusted before it has been
// compared with what is actually left.
type decoder struct {
	r    io.Reader
	h    hash.Hash32
	left int64
}

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

func (d *decoder) fits(n uint64) error {
	if n > uint64(d.left) {
		return corrupt("length %d exceeds remaining %d bytes", n, d.left)
	}
	return nil
}

func (d *decoder) read(p []byte) error {
	if err := d.fits(uint64(len(p))); err != nil {
		return err
	}
	if _, err := io.ReadFull(d.r, p); err != nil {
		return corrupt("short read: %v", err)
	}
	_, _ = d.h.Write(p)
	d.left -= int64(len(p))
	return nil
}

func (d *decoder) u32() (uint32, error) {
	var b [4]byte
	if err := d.read(b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

func (d *decoder) str() (string, error) {
	n, err := d.u32()
	if err != nil {
		return "", err
	}
	if err := d.fits(uint64(n)); err != nil {
		return "", err
	}
	b := make([]byte, n)
	if err := d.read(b); err != nil {
		return "", err
	}
	return string(b), nil
}

func (d *decoder) skip(n uint64) error {
	if err := d.fits(n); err != nil {
		return err
	}
	var buf [4096]byte
	for n > 0 {
		k := min(n, uint64(len(buf)))
		if err := d.read(buf[:k]); err != nil {
			return err
		}
		n -= k
	}
	return nil
}

func readFixed[T any](d *decoder, count int, width int, get func([]byte) T) ([]T, error) {
	out := make([]T, count)
	buf := make([]byte, ioChunk*width)
	for done := 0; done < count; {
		k := min(count-done, ioChunk)
		if err := d.read(buf[:k*width]); err != nil {
			return nil, err
		}
		for i := 0; i < k; i++ {
			out[done+i] = get(buf[i*width:])
		}
		done += k
	}
	return out, nil
}

// Read parses a snapshot of size bytes from r. It never trusts the file:
// every length is checked against the bytes remaining before anything is
// allocated, then the checksum and the structure are verified. Errors wrap
// ErrCorrupt.
func Read(r io.Reader, size int64) (*Index, Meta, error) {
	var meta Meta
	if size < int64(len(snapshotMagic)+4+crcBytes) {
		return nil, meta, corrupt("file too small (%d bytes)", size)
	}
	h := crc32.New(crcTable)
	br := bufio.NewReaderSize(io.LimitReader(r, size), 1<<20)
	d := &decoder{r: br, h: h, left: size - crcBytes}

	head := make([]byte, len(snapshotMagic)+4)
	if err := d.read(head); err != nil {
		return nil, meta, err
	}
	if string(head[:4]) != snapshotMagic {
		return nil, meta, corrupt("bad magic %q", head[:4])
	}
	if v := binary.LittleEndian.Uint32(head[4:]); v != snapshotVersion {
		return nil, meta, corrupt("unsupported version %d", v)
	}

	var mb [9]byte
	if err := d.read(mb[:]); err != nil {
		return nil, meta, err
	}
	meta.Complete = mb[0] == 1
	if at := getInt64(mb[1:]); at != 0 {
		meta.ScannedAt = time.Unix(0, at)
	}

	roots, err := readRoots(d)
	if err != nil {
		return nil, meta, err
	}
	n32, err := d.u32()
	if err != nil {
		return nil, meta, err
	}
	n := int(n32)

	ix, err := readSections(d, n)
	if err != nil {
		return nil, meta, err
	}

	var tail [crcBytes]byte
	if _, err := io.ReadFull(br, tail[:]); err != nil {
		return nil, meta, corrupt("missing checksum: %v", err)
	}
	if binary.LittleEndian.Uint32(tail[:]) != h.Sum32() {
		return nil, meta, corrupt("checksum mismatch")
	}

	if err := ix.validate(n, roots); err != nil {
		return nil, meta, err
	}
	ix.roots = roots
	ix.count(n)
	if err := ix.refold(); err != nil {
		return nil, meta, err
	}
	return ix, meta, nil
}

func readRoots(d *decoder) (map[uint32]Root, error) {
	count, err := d.u32()
	if err != nil {
		return nil, err
	}
	if err := d.fits(uint64(count) * minRootBytes); err != nil {
		return nil, err
	}
	roots := make(map[uint32]Root, count)
	for i := uint32(0); i < count; i++ {
		var r Root
		if r.ID, err = d.u32(); err != nil {
			return nil, err
		}
		if r.Key, err = d.str(); err != nil {
			return nil, err
		}
		if r.Path, err = d.str(); err != nil {
			return nil, err
		}
		if _, dup := roots[r.ID]; dup {
			return nil, corrupt("duplicate root id %d", r.ID)
		}
		roots[r.ID] = r
	}
	return roots, nil
}

func readSections(d *decoder, n int) (*Index, error) {
	ix := &Index{}
	seen := 0
	for d.left > 0 {
		var h [sectionHeader]byte
		if err := d.read(h[:]); err != nil {
			return nil, err
		}
		tag, length := binary.LittleEndian.Uint32(h[:]), binary.LittleEndian.Uint64(h[4:])
		if err := d.fits(length); err != nil {
			return nil, err
		}
		if seen&(1<<tag) != 0 && tag >= tagNames && tag <= tagFlags {
			return nil, corrupt("duplicate section %d", tag)
		}
		var err error
		switch tag {
		case tagNames:
			ix.names = make([]byte, length)
			err = d.read(ix.names)
		case tagNameOff:
			var ends []uint32
			if ends, err = fixedSection(d, length, n, 4, binary.LittleEndian.Uint32); err == nil {
				ix.nameOff = append(make([]uint32, 0, n+1), 0)
				ix.nameOff = append(ix.nameOff, ends...)
			}
		case tagParent:
			ix.parent, err = fixedSection(d, length, n, 4, binary.LittleEndian.Uint32)
		case tagSize:
			ix.size, err = fixedSection(d, length, n, 8, getInt64)
		case tagMTime:
			ix.mtime, err = fixedSection(d, length, n, 8, getInt64)
		case tagFlags:
			if length != uint64(n) {
				return nil, corrupt("flags section holds %d bytes for %d entries", length, n)
			}
			ix.flags = make([]uint8, n)
			err = d.read(ix.flags)
		default:
			err = d.skip(length)
		}
		if err != nil {
			return nil, err
		}
		if tag >= tagNames && tag <= tagFlags {
			seen |= 1 << tag
		}
	}
	if bits := seen >> tagNames; bits != 1<<numRequiredTags-1 {
		return nil, corrupt("missing sections (mask %b)", bits)
	}
	return ix, nil
}

func fixedSection[T any](d *decoder, length uint64, n, width int, get func([]byte) T) ([]T, error) {
	if length != uint64(n)*uint64(width) {
		return nil, corrupt("section holds %d bytes for %d entries of %d", length, n, width)
	}
	return readFixed(d, n, width, get)
}

func (ix *Index) validate(n int, roots map[uint32]Root) error {
	if ix.nameOff[n] != uint32(len(ix.names)) || uint64(len(ix.names)) > math.MaxUint32 {
		return corrupt("name offsets end at %d, arena is %d bytes", ix.nameOff[n], len(ix.names))
	}
	closed := make([]uint64, (n+63)/64)
	prev := NoParent
	for i := 0; i < n; i++ {
		if ix.nameOff[i+1] < ix.nameOff[i] {
			return corrupt("name offsets decrease at %d", i)
		}
		p := ix.parent[i]
		if p != prev && prev != NoParent {
			closed[prev/64] |= 1 << (prev % 64)
		}
		prev = p
		if p == NoParent {
			if _, ok := roots[uint32(i)]; !ok {
				return corrupt("entry %d has no parent and is not a root", i)
			}
			continue
		}
		if p >= uint32(i) {
			return corrupt("parent[%d] = %d", i, p)
		}
		if closed[p/64]&(1<<(p%64)) != 0 {
			return corrupt("children of %d are not contiguous (entry %d)", p, i)
		}
	}
	for id := range roots {
		if int64(id) >= int64(n) || ix.parent[id] != NoParent {
			return corrupt("invalid root id %d", id)
		}
	}
	return nil
}

func (ix *Index) count(n int) {
	for i := 0; i < n; i++ {
		switch {
		case ix.parent[i] == NoParent:
		case ix.flags[i]&flagDir != 0:
			ix.nDirs++
		default:
			ix.nFiles++
			ix.byteSum += ix.size[i]
		}
	}
}

// refold rebuilds folded and foldOff from names, in parallel.
func (ix *Index) refold() error {
	n := len(ix.parent)
	workers := min(runtime.GOMAXPROCS(0), max(1, n/50_000))
	chunk := (n + workers - 1) / workers
	foldOff := make([]uint32, n+1)
	bufs := make([][]byte, workers)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := min(w*chunk, n), min((w+1)*chunk, n)
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 0, ix.nameOff[hi]-ix.nameOff[lo])
			for i := lo; i < hi; i++ {
				buf = fold.Append(buf, ix.names[ix.nameOff[i]:ix.nameOff[i+1]])
				foldOff[i+1] = uint32(len(buf))
			}
			bufs[w] = buf
		}()
	}
	wg.Wait()

	var total uint64
	bases := make([]uint32, workers)
	for w, b := range bufs {
		bases[w] = uint32(total)
		total += uint64(len(b))
	}
	if total > math.MaxUint32 {
		return corrupt("folded names exceed 4 GiB")
	}
	folded := make([]byte, total)
	for w := 0; w < workers; w++ {
		lo, hi := min(w*chunk, n), min((w+1)*chunk, n)
		wg.Add(1)
		go func() {
			defer wg.Done()
			copy(folded[bases[w]:], bufs[w])
			for i := lo; i < hi; i++ {
				foldOff[i+1] += bases[w]
			}
		}()
	}
	wg.Wait()
	ix.folded, ix.foldOff = folded, foldOff
	return nil
}
