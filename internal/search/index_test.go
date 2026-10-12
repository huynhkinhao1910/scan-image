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
	root := ix.AddRoot("/Volumes/NAS")
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
	root := ix.AddRoot("/r")
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
	a := ix.AddRoot("/Volumes/Design Team")
	b := ix.AddRoot("/Volumes/Photos")
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
