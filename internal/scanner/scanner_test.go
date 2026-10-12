package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/isuccesscorp/scan-image/internal/search"
	"github.com/isuccesscorp/scan-image/internal/source"
)

func TestRunIndexesWholeTree(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a/b/c", "a/d", "Ảnh"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"a/1.jpg", "a/b/2.jpg", "a/b/c/3.png", "a/d/4.jpg", "Ảnh/cá.jpg"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ix := search.New()
	s := &Scanner{Src: source.LocalFS{}, Workers: 4}
	roots := []Root{{Path: filepath.Join(root, "a"), ID: ix.AddRoot(filepath.Join(root, "a"))}, {Path: filepath.Join(root, "Ảnh"), ID: ix.AddRoot(filepath.Join(root, "Ảnh"))}}
	if err := s.Run(context.Background(), roots, ix.AddChildren); err != nil {
		t.Fatal(err)
	}
	p := s.Progress()
	if p.Files != 5 || p.Dirs != 3 || !p.Done {
		t.Fatalf("progress %+v", p)
	}
	res := ix.Search("ca", search.Options{FilesOnly: true})
	if res.Total != 1 {
		t.Fatalf("total %d", res.Total)
	}
	if got := ix.Item(res.IDs[0]).Path; got != filepath.Join(root, "Ảnh", "cá.jpg") {
		t.Errorf("path %q", got)
	}
}
