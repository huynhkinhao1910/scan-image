package search

import (
	"fmt"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

// benchTree is a synthetic tree of mids directories holding leaves leaf
// directories of files files each, generated on demand.
type benchTree struct {
	mids, leaves int
	mid, leaf    []source.Entry
}

func newBenchTree(mids, leaves, files int) *benchTree {
	t := &benchTree{mids: mids, leaves: leaves}
	for i := 0; i < mids; i++ {
		t.mid = append(t.mid, dirEntry(fmt.Sprintf("m%03d", i), oldMT))
	}
	for i := 0; i < leaves; i++ {
		t.leaf = append(t.leaf, dirEntry(fmt.Sprintf("l%03d", i), oldMT))
	}
	return t
}

// build runs a full scan of the tree through a Builder.
func (t *benchTree) build(b *testing.B, old *Index, files []source.Entry) *Builder {
	bd := NewBuilder(old, BuildOptions{Reuse: true, Now: func() time.Time { return fixedNow }})
	root, prev := bd.AddRoot("bench", "/bench")
	for _, m := range bd.Listed(root, prev, t.mid) {
		for _, l := range bd.Listed(m.ID, m.Prev, t.leaf) {
			bd.Listed(l.ID, l.Prev, files)
		}
	}
	return bd
}

// BenchmarkBuilderReuse3M rebuilds a ~3M-entry index in which every leaf
// directory (300 files each) is unchanged and therefore copied, not listed.
func BenchmarkBuilderReuse3M(b *testing.B) {
	const mids, leaves, nfiles = 100, 100, 300
	t := newBenchTree(mids, leaves, nfiles)
	files := make([]source.Entry, nfiles)
	for i := range files {
		files[i] = fileEntry(fmt.Sprintf("IMG_%05d.JPG", i), int64(i), oldMT)
	}
	old := t.build(b, nil, files).Index()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bd := t.build(b, old, files)
		if st := bd.Stats(); st.ReusedDirs != mids*leaves {
			b.Fatalf("ReusedDirs = %d, want %d", st.ReusedDirs, mids*leaves)
		}
		if bd.Changed() {
			b.Fatal("Changed() = true for an unchanged tree")
		}
	}
}
