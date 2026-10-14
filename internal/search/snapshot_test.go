package search

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

const maxHostileAlloc = 16 << 20

var scannedAt = time.Unix(1_700_000_000, 0)

func snapshotBytes(t testing.TB, ix *Index, m Meta) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, ix, m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.Bytes()
}

func readBytes(b []byte) (*Index, Meta, error) {
	return Read(bytes.NewReader(b), int64(len(b)))
}

func reseal(b []byte) []byte {
	out := slices.Clone(b)
	n := len(out) - 4
	binary.LittleEndian.PutUint32(out[n:], crc32.Checksum(out[:n], crc32.MakeTable(crc32.Castagnoli)))
	return out
}

func guardedRead(t *testing.T, label string, b []byte) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: Read panicked: %v", label, r)
		}
	}()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, err = readBytes(b)
	runtime.ReadMemStats(&after)
	if used := after.TotalAlloc - before.TotalAlloc; used > maxHostileAlloc {
		t.Errorf("%s: Read allocated %d bytes on a %d byte input", label, used, len(b))
	}
	return err
}

func TestSnapshotRoundTripKeepsSearchItemStatsRootsAndMeta(t *testing.T) {
	ix := buildRich()
	meta := Meta{Complete: true, ScannedAt: scannedAt}

	got, gotMeta, err := readBytes(snapshotBytes(t, ix, meta))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	assertSameIndex(t, ix, got)
	if gotMeta.Complete != meta.Complete || !gotMeta.ScannedAt.Equal(meta.ScannedAt) {
		t.Errorf("Meta = %+v, want %+v", gotMeta, meta)
	}
}

func TestSnapshotRoundTripKeepsFlags(t *testing.T) {
	ix := buildRich()
	got, _, err := readBytes(snapshotBytes(t, ix, Meta{}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !slices.Equal(ix.flags, got.flags) {
		t.Errorf("flags = %v, want %v", got.flags, ix.flags)
	}
}

func TestSnapshotRoundTripIncompleteMeta(t *testing.T) {
	_, got, err := readBytes(snapshotBytes(t, buildRich(), Meta{Complete: false, ScannedAt: scannedAt}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Complete {
		t.Error("Complete = true, want false")
	}
}

func TestSnapshotRoundTripEmptyIndexAndZeroMeta(t *testing.T) {
	ix := New()
	got, meta, err := readBytes(snapshotBytes(t, ix, Meta{}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	assertSameIndex(t, ix, got)
	if meta.Complete || !meta.ScannedAt.IsZero() {
		t.Errorf("Meta = %+v, want zero value", meta)
	}
}

func TestSnapshotLoadedIndexAcceptsMoreChildren(t *testing.T) {
	got, _, err := readBytes(snapshotBytes(t, buildRich(), Meta{}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	root := got.RootList()[0].ID
	got.AddChildren(root, []source.Entry{{Name: "Mới.JPG"}})
	if got.Search("moi", Options{}).Total != 1 {
		t.Error("entry appended after Read is not searchable")
	}
}

func TestSnapshotResealedUnmodifiedFileStillReads(t *testing.T) {
	b := snapshotBytes(t, buildRich(), Meta{})
	if _, _, err := readBytes(reseal(b)); err != nil {
		t.Fatalf("file ending in little-endian CRC32C over all preceding bytes was rejected: %v", err)
	}
}

func TestSnapshotRejectsEveryTruncation(t *testing.T) {
	b := snapshotBytes(t, buildRich(), Meta{Complete: true, ScannedAt: scannedAt})
	for n := 0; n < len(b); n++ {
		if err := guardedRead(t, fmt.Sprintf("truncated to %d", n), b[:n]); err == nil {
			t.Fatalf("truncated to %d of %d bytes: Read returned no error", n, len(b))
		}
	}
}

func TestSnapshotRejectsAnyFlippedByte(t *testing.T) {
	b := snapshotBytes(t, buildRich(), Meta{Complete: true, ScannedAt: scannedAt})
	for i := range b {
		c := slices.Clone(b)
		c[i] ^= 0x01
		if err := guardedRead(t, fmt.Sprintf("flip byte %d", i), c); err == nil {
			t.Fatalf("flipped byte %d of %d: Read returned no error", i, len(b))
		}
	}
}

func TestSnapshotRejectsWrongMagic(t *testing.T) {
	b := snapshotBytes(t, buildRich(), Meta{})
	c := slices.Clone(b)
	copy(c, "NOPE")
	for label, data := range map[string][]byte{"raw": c, "resealed": reseal(c)} {
		if err := guardedRead(t, label, data); err == nil {
			t.Errorf("%s: wrong magic accepted", label)
		}
	}
}

func TestSnapshotRejectsWrongVersion(t *testing.T) {
	b := snapshotBytes(t, buildRich(), Meta{})
	if string(b[:4]) != "SIDX" {
		t.Fatalf("magic = %q, want SIDX", b[:4])
	}
	c := slices.Clone(b)
	for i := 4; i < 8; i++ {
		c[i] = 0xEE
	}
	if err := guardedRead(t, "resealed wrong version", reseal(c)); err == nil {
		t.Error("file with an unknown version and a valid CRC was accepted")
	}
}

func TestSnapshotRejectsSizeLargerThanData(t *testing.T) {
	b := snapshotBytes(t, buildRich(), Meta{})
	_, _, err := Read(bytes.NewReader(b), int64(len(b))+1024)
	if err == nil {
		t.Error("Read accepted size larger than the data available")
	}
}

func TestSnapshotHugeSectionLengthsFailWithoutLargeAllocation(t *testing.T) {
	b := snapshotBytes(t, buildRich(), Meta{Complete: true, ScannedAt: scannedAt})
	rejected := 0
	for _, width := range []int{4, 8} {
		for p := 4; p+width <= len(b)-4; p++ {
			c := slices.Clone(b)
			for i := 0; i < width; i++ {
				c[p+i] = 0xFF
			}
			if guardedRead(t, fmt.Sprintf("0xFF x%d at %d", width, p), reseal(c)) != nil {
				rejected++
			}
		}
	}
	if rejected == 0 {
		t.Error("no corrupted length was rejected")
	}
}

func FuzzRead(f *testing.F) {
	var buf bytes.Buffer
	if err := Write(&buf, buildRich(), Meta{Complete: true, ScannedAt: scannedAt}); err != nil {
		f.Fatalf("Write: %v", err)
	}
	valid := buf.Bytes()
	f.Add(valid)
	f.Add(valid[:len(valid)/2])
	f.Add([]byte("SIDX"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		ix, _, err := Read(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		ix.Stats()
		ix.Search("a", Options{})
		for id := 0; id < ix.Len(); id++ {
			ix.Item(uint32(id))
		}
	})
}

func build3M(b *testing.B) *Index {
	b.Helper()
	ix := New()
	root := ix.AddRoot("smb://nas/Photos", "/Volumes/Photos")
	batch := make([]source.Entry, 0, 10_000)
	for i := 0; i < 3_000_000; i++ {
		batch = append(batch, source.Entry{
			Name:    fmt.Sprintf("IMG_%07d_Ảnh sản phẩm.jpg", i),
			Size:    int64(i),
			ModTime: time.Unix(1_700_000_000+int64(i), 0),
		})
		if len(batch) == cap(batch) {
			ix.AddChildren(root, batch)
			batch = batch[:0]
		}
	}
	return ix
}

func BenchmarkSnapshotWrite3M(b *testing.B) {
	ix := build3M(b)
	var buf bytes.Buffer
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		if err := Write(&buf, ix, Meta{Complete: true, ScannedAt: scannedAt}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(buf.Len())/(1<<20), "MiB")
}

func BenchmarkSnapshotLoad3M(b *testing.B) {
	data := snapshotBytes(b, build3M(b), Meta{Complete: true, ScannedAt: scannedAt})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := Read(bytes.NewReader(data), int64(len(data))); err != nil {
			b.Fatal(err)
		}
	}
	if per := b.Elapsed() / time.Duration(b.N); per > time.Second {
		b.Errorf("load took %v per snapshot, target < 1s", per)
	}
}

func TestSnapshotRejectsInterleavedChildrenOfOneDirectory(t *testing.T) {
	ix := New()
	root := ix.AddRoot("smb://nas/Photos", "/Volumes/Photos")
	ids := ix.AddChildren(root, []source.Entry{{Name: "a", IsDir: true}, {Name: "b", IsDir: true}})
	ix.AddChildren(ids[0], []source.Entry{{Name: "a1.jpg"}})
	ix.AddChildren(ids[1], []source.Entry{{Name: "b1.jpg"}})
	ix.AddChildren(ids[0], []source.Entry{{Name: "a2.jpg"}})

	_, _, err := readBytes(reseal(snapshotBytes(t, ix, Meta{})))

	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Read error = %v, want ErrCorrupt: children of a are interleaved with those of b", err)
	}
}
