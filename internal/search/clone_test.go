package search

import (
	"bytes"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

func richEntries() ([]source.Entry, []source.Entry, []source.Entry) {
	mt := time.Unix(1_700_000_000, 0)
	top := []source.Entry{
		{Name: "Ảnh Cá", IsDir: true, ModTime: mt},
		{Name: "Ảnh Cũ", IsDir: true, ModTime: mt.Add(time.Hour)},
		{Name: "IMG_0001.JPG", Size: 1234, ModTime: mt.Add(2 * time.Hour)},
		{Name: "readme.txt", Size: 10},
	}
	sub := []source.Entry{
		{Name: "Cá Hồi 01.JPG", Size: 100, ModTime: mt},
		{Name: "ca_thu.png", Size: 200, ModTime: mt.Add(time.Minute)},
		{Name: "ĐÀ NẴNG.jpg", Size: 300},
	}
	pics := []source.Entry{
		{Name: "holiday.jpeg", Size: 5_000_000, ModTime: mt},
		{Name: "Holiday 2.JPG", Size: 7, ModTime: mt},
	}
	return top, sub, pics
}

func buildRich() *Index {
	top, sub, pics := richEntries()
	ix := New()
	photos := ix.AddRoot("smb://nas/Photos", "/Volumes/Photos")
	local := ix.AddRoot("/Users/me/Pictures", "/Users/me/Pictures")
	ids := ix.AddChildren(photos, top)
	ix.AddChildren(ids[0], sub)
	ix.AddChildren(local, pics)
	ix.flags[ids[0]] |= flagStable
	return ix
}

var sameIndexQueries = []struct {
	q   string
	opt Options
}{
	{"", Options{}},
	{"anh", Options{}},
	{"ca hoi", Options{FilesOnly: true}},
	{"da nang", Options{}},
	{"img", Options{Limit: 1}},
	{"", Options{Extensions: []string{".jpg", ".png"}}},
	{"holiday", Options{}},
	{"zzzz", Options{}},
}

func assertSameIndex(t *testing.T, want, got *Index) {
	t.Helper()
	if want.Len() != got.Len() {
		t.Fatalf("Len = %d, want %d", got.Len(), want.Len())
	}
	if want.Stats() != got.Stats() {
		t.Errorf("Stats = %+v, want %+v", got.Stats(), want.Stats())
	}
	if !slices.Equal(want.RootList(), got.RootList()) {
		t.Errorf("RootList = %+v, want %+v", got.RootList(), want.RootList())
	}
	for _, c := range sameIndexQueries {
		w, g := want.Search(c.q, c.opt), got.Search(c.q, c.opt)
		if w.Total != g.Total || !slices.Equal(w.IDs, g.IDs) {
			t.Errorf("Search(%q, %+v) = %+v, want %+v", c.q, c.opt, g, w)
		}
	}
	for id := 0; id < want.Len(); id++ {
		w, g := want.Item(uint32(id)), got.Item(uint32(id))
		if w.ID != g.ID || w.Name != g.Name || w.Dir != g.Dir || w.Path != g.Path ||
			w.Size != g.Size || !w.ModTime.Equal(g.ModTime) || w.IsDir != g.IsDir {
			t.Errorf("Item(%d) = %+v, want %+v", id, g, w)
		}
	}
}

func TestCloneEqualsOriginalAtCloneTime(t *testing.T) {
	ix := buildRich()
	assertSameIndex(t, ix, ix.Clone())
}

func TestCloneIgnoresLaterAddChildren(t *testing.T) {
	ix := buildRich()
	c := ix.Clone()

	roots := ix.RootList()
	ix.AddChildren(roots[0].ID, []source.Entry{{Name: "IMG_LATER.JPG", Size: 1}, {Name: "later", IsDir: true}})
	ix.AddChildren(roots[1].ID, []source.Entry{{Name: "holiday late.jpg"}})
	ix.AddRoot("smb://nas/Extra", "/Volumes/Extra")

	if ix.Len() == c.Len() {
		t.Fatal("original did not grow; test setup is wrong")
	}
	assertSameIndex(t, buildRich(), c)
}

func TestCloneIgnoresLaterFlagChanges(t *testing.T) {
	ix := buildRich()
	c := ix.Clone()
	before := slices.Clone(c.flags)

	ix.mu.Lock()
	for i := range ix.flags {
		ix.flags[i] ^= flagStable
	}
	ix.mu.Unlock()

	if !slices.Equal(before, c.flags) {
		t.Error("clone flags changed when the original's flags were modified")
	}
}

func TestCloneIgnoresLaterSetRootPath(t *testing.T) {
	ix := buildRich()
	c := ix.Clone()
	ix.SetRootPath("smb://nas/Photos", "/Volumes/Photos-1")
	assertSameIndex(t, buildRich(), c)
}

func TestCloneDoesNotCopyEntries(t *testing.T) {
	ix := New()
	root := ix.AddRoot("k", "/r")
	batch := make([]source.Entry, 20_000)
	for i := range batch {
		batch[i] = source.Entry{Name: fmt.Sprintf("IMG_%06d.JPG", i)}
	}
	ix.AddChildren(root, batch)

	allocs := testing.AllocsPerRun(20, func() { _ = ix.Clone() })
	if allocs > 32 {
		t.Errorf("Clone allocates %v times; want a constant handful, not per-entry", allocs)
	}
}

func TestCloneIsSafeToSerialiseWhileOriginalGrows(t *testing.T) {
	ix := New()
	root := ix.AddRoot("k", "/r")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for round := 0; round < 200; round++ {
			batch := make([]source.Entry, 50)
			for i := range batch {
				batch[i] = source.Entry{Name: fmt.Sprintf("Ảnh %d-%d.JPG", round, i), Size: int64(i), IsDir: i%5 == 0}
			}
			ids := ix.AddChildren(root, batch)
			ix.mu.Lock()
			ix.flags[ids[0]] |= flagStable
			ix.mu.Unlock()
		}
	}()

	for i := 0; i < 20; i++ {
		c := ix.Clone()
		var buf bytes.Buffer
		if err := Write(&buf, c, Meta{Complete: false, ScannedAt: time.Unix(1_700_000_000, 0)}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		got, _, err := Read(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
		if err != nil {
			t.Fatalf("Read of a clone written during AddChildren: %v", err)
		}
		if got.Len() != c.Len() {
			t.Fatalf("round trip Len = %d, clone Len = %d", got.Len(), c.Len())
		}
		c.Search("anh", Options{})
		if n := c.Len(); n > 0 {
			c.Item(uint32(n - 1))
		}
	}
	wg.Wait()
}
