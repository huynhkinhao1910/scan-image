package search

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

func findNamesIndex() (*Index, map[string]uint32) {
	ix := New()
	root := ix.AddRoot("/Volumes/NAS", "/Volumes/NAS")
	top := ix.AddChildren(root, []source.Entry{
		{Name: "A", IsDir: true},
		{Name: "B", IsDir: true},
		{Name: "S-1.PNG", Size: 1},
		{Name: "Anh", IsDir: true},
	})
	a := ix.AddChildren(top[0], []source.Entry{
		{Name: "dup.png", Size: 1, ModTime: time.Unix(10, 0)},
		{Name: "Ảnh.png", Size: 2},
		{Name: "S-1.png.bak", Size: 3},
		{Name: "report.pdf", IsDir: true},
	})
	b := ix.AddChildren(top[1], []source.Entry{
		{Name: "DUP.PNG", Size: 4, ModTime: time.Unix(20, 0)},
	})
	return ix, map[string]uint32{
		"top":  top[2],
		"dirA": top[3],
		"dupA": a[0],
		"anh":  a[1],
		"dirR": a[3],
		"dupB": b[0],
	}
}

func assertFindNames(t *testing.T, ix *Index, ids map[string]uint32) {
	t.Helper()
	names := []string{"s-1.png", "S-1.PNG", "s-1", "anh.png", "ảnh.PNG", "Anh", "report.pdf", "dup.png", "nope.png"}
	got := ix.FindNames(names)
	want := [][]uint32{
		{ids["top"]},
		{ids["top"]},
		nil,
		{ids["anh"]},
		{ids["anh"]},
		nil,
		nil,
		{ids["dupA"], ids["dupB"]},
		nil,
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("FindNames[%q] = %v, want %v", names[i], got[i], want[i])
		}
	}
}

func TestFindNamesMatchesWholeNamesIgnoringCaseAndDiacritics(t *testing.T) {
	ix, ids := findNamesIndex()
	assertFindNames(t, ix, ids)
}

func TestFindNamesSameNameAskedTwiceFillsBothPositions(t *testing.T) {
	ix, ids := findNamesIndex()
	got := ix.FindNames([]string{"dup.png", "x", "DUP.png"})
	want := []uint32{ids["dupA"], ids["dupB"]}
	if !slices.Equal(got[0], want) || !slices.Equal(got[2], want) || got[1] != nil {
		t.Errorf("got %v, want %v at 0 and 2", got, want)
	}
}

func TestFindNamesEmptyInput(t *testing.T) {
	ix, _ := findNamesIndex()
	if got := ix.FindNames(nil); len(got) != 0 {
		t.Errorf("FindNames(nil) = %v, want empty", got)
	}
}

func TestFindNamesOnClone(t *testing.T) {
	ix, ids := findNamesIndex()
	assertFindNames(t, ix.Clone(), ids)
}

func TestFindNamesOnSnapshotLoadedIndex(t *testing.T) {
	ix, ids := findNamesIndex()
	loaded, _, err := readBytes(snapshotBytes(t, ix, Meta{Complete: true, ScannedAt: scannedAt}))
	if err != nil {
		t.Fatal(err)
	}
	assertFindNames(t, loaded, ids)
}

func TestFindNamesAcrossParallelChunks(t *testing.T) {
	ix := New()
	root := ix.AddRoot("/r", "/r")
	entries := make([]source.Entry, 0, 60_000)
	for i := 0; i < 60_000; i++ {
		entries = append(entries, source.Entry{Name: fmt.Sprintf("f%05d.png", i)})
	}
	ids := ix.AddChildren(root, entries)
	got := ix.FindNames([]string{"F00000.PNG", "f59999.png", "f30000.png"})
	for i, want := range []uint32{ids[0], ids[59_999], ids[30_000]} {
		if !slices.Equal(got[i], []uint32{want}) {
			t.Errorf("position %d = %v, want [%d]", i, got[i], want)
		}
	}
}

func BenchmarkFindNames3M(b *testing.B) {
	ix := New()
	root := ix.AddRoot("/r", "/r")
	batch := make([]source.Entry, 0, 10_000)
	for i := 0; i < 3_000_000; i++ {
		batch = append(batch, source.Entry{Name: fmt.Sprintf("IMG_%07d_Ảnh sản phẩm.jpg", i)})
		if len(batch) == cap(batch) {
			ix.AddChildren(root, batch)
			batch = batch[:0]
		}
	}
	names := make([]string, 100)
	for i := range names {
		names[i] = fmt.Sprintf("img_%07d_ảnh sản phẩm.JPG", i*29_000)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got := ix.FindNames(names)
		if len(got[0]) != 1 {
			b.Fatalf("first name not found: %v", got[0])
		}
	}
}

func TestFindNamesFindsDecomposedNameByItsComposedSpelling(t *testing.T) {
	ix := New()
	root := ix.AddRoot("/Volumes/NAS", "/Volumes/NAS")
	ids := ix.AddChildren(root, []source.Entry{{Name: "Huye\u0302\u0300n.png", Size: 1}})

	got := ix.FindNames([]string{"Huy\u1ec1n.png"})

	if !slices.Equal(got[0], ids) {
		t.Fatalf("FindNames = %v, want %v: macOS lists SMB names decomposed, lists spell them composed", got[0], ids)
	}
}
