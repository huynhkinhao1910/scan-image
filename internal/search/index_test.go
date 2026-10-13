package search

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

func build() *Index {
	ix := New()
	root := ix.AddRoot("/Volumes/NAS", "/Volumes/NAS")
	ids := ix.AddChildren(root, []source.Entry{
		{Name: "Ảnh Cá", IsDir: true},
		{Name: "readme.txt", Size: 10},
	})
	ix.AddChildren(ids[0], []source.Entry{
		{Name: "Cá Hồi 01.JPG", Size: 100, ModTime: time.Unix(1, 0)},
		{Name: "ca_thu.png", Size: 200},
	})
	return ix
}

func TestSearchFoldsAndJoinsPath(t *testing.T) {
	ix := build()
	res := ix.Search("ca hoi", Options{FilesOnly: true})
	if res.Total != 1 {
		t.Fatalf("total %d, want 1", res.Total)
	}
	it := ix.Item(res.IDs[0])
	want := filepath.Join("/Volumes/NAS", "Ảnh Cá", "Cá Hồi 01.JPG")
	if it.Path != want || it.Name != "Cá Hồi 01.JPG" {
		t.Errorf("item %+v, want path %q", it, want)
	}
}

func TestExtensionsAndStats(t *testing.T) {
	ix := build()
	res := ix.Search("", Options{Extensions: []string{".jpg", ".png"}})
	if res.Total != 2 {
		t.Errorf("total %d, want 2", res.Total)
	}
	st := ix.Stats()
	if st.Files != 3 || st.Dirs != 1 || st.Bytes != 310 {
		t.Errorf("stats %+v", st)
	}
}

func BenchmarkSearch3M(b *testing.B) {
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
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix.Search("1234 san pham", Options{Limit: 500})
	}
}

func TestMultipleRoots(t *testing.T) {
	ix := New()
	a := ix.AddRoot("smb://nas/Design Team", "/Volumes/Design Team")
	b := ix.AddRoot("smb://nas/Photos", "/Volumes/Photos")
	ix.AddChildren(a, []source.Entry{{Name: "logo.png"}})
	ix.AddChildren(b, []source.Entry{{Name: "logo.jpg"}})
	res := ix.Search("logo", Options{})
	if res.Total != 2 {
		t.Fatalf("total %d", res.Total)
	}
	if p := ix.Item(res.IDs[1]).Path; p != "/Volumes/Photos/logo.jpg" {
		t.Errorf("path %q", p)
	}
	if ix.Search("photos", Options{}).Total != 0 {
		t.Error("roots must not match searches")
	}
	if st := ix.Stats(); st.Dirs != 0 || st.Files != 2 {
		t.Errorf("stats %+v", st)
	}
}

func TestFlagsMarkDirectories(t *testing.T) {
	ix := New()
	root := ix.AddRoot("k", "/r")
	ids := ix.AddChildren(root, []source.Entry{
		{Name: "d", IsDir: true},
		{Name: "f.jpg"},
	})
	if ix.flags[ids[0]]&flagDir == 0 || !ix.Item(ids[0]).IsDir {
		t.Errorf("dir lacks flagDir: flags=%08b", ix.flags[ids[0]])
	}
	if ix.flags[ids[1]]&flagDir != 0 || ix.Item(ids[1]).IsDir {
		t.Errorf("file has flagDir: flags=%08b", ix.flags[ids[1]])
	}
	if ix.flags[ids[0]]&flagStable != 0 {
		t.Error("new directory must not start with flagStable")
	}
}

func TestRootsCarryKeyAndPath(t *testing.T) {
	ix := New()
	a := ix.AddRoot("smb://nas/Photos", "/Volumes/Photos")
	b := ix.AddRoot("/Users/me/Pics", "/Users/me/Pics")
	got := ix.RootList()
	want := []Root{
		{ID: a, Key: "smb://nas/Photos", Path: "/Volumes/Photos"},
		{ID: b, Key: "/Users/me/Pics", Path: "/Users/me/Pics"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("RootList() = %+v, want %+v", got, want)
	}
}

func TestSetRootPathMovesItemPaths(t *testing.T) {
	ix := New()
	root := ix.AddRoot("smb://nas/Photos", "/Volumes/Photos")
	ids := ix.AddChildren(root, []source.Entry{{Name: "a", IsDir: true}})
	files := ix.AddChildren(ids[0], []source.Entry{{Name: "x.jpg"}})

	ix.SetRootPath("smb://nas/Photos", "/Volumes/Photos-1")

	if p := ix.Item(files[0]).Path; p != "/Volumes/Photos-1/a/x.jpg" {
		t.Errorf("path %q, want remounted path", p)
	}
	rl := ix.RootList()
	if len(rl) != 1 || rl[0].Key != "smb://nas/Photos" || rl[0].Path != "/Volumes/Photos-1" {
		t.Errorf("RootList() = %+v", rl)
	}
}

func TestChildrenOfOneDirectoryAreContiguous(t *testing.T) {
	ix := New()
	root := ix.AddRoot("k", "/r")
	top := ix.AddChildren(root, []source.Entry{{Name: "a", IsDir: true}, {Name: "b", IsDir: true}, {Name: "c.jpg"}})
	underA := ix.AddChildren(top[0], []source.Entry{{Name: "a1", IsDir: true}, {Name: "a2.jpg"}, {Name: "a3.jpg"}})
	underB := ix.AddChildren(top[1], []source.Entry{{Name: "b1.jpg"}, {Name: "b2.jpg"}})
	underA1 := ix.AddChildren(underA[0], []source.Entry{{Name: "deep.jpg"}})

	for name, ids := range map[string][]uint32{"top": top, "underA": underA, "underB": underB, "underA1": underA1} {
		for i := 1; i < len(ids); i++ {
			if ids[i] != ids[i-1]+1 {
				t.Errorf("%s ids not contiguous: %v", name, ids)
				break
			}
		}
	}
	for i := range ix.parent {
		if ix.parent[i] != NoParent && ix.parent[i] >= uint32(i) {
			t.Errorf("parent[%d] = %d, want < %d", i, ix.parent[i], i)
		}
	}
}
