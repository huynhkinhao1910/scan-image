// Package search is an Everything-style in-memory index of file names.
//
// Names live in two flat byte arenas (original and folded) addressed by
// uint32 offsets, so millions of entries cost a few allocations and a
// substring search is a linear, cache-friendly scan split across cores.
package search

import (
	"bytes"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/isuccesscorp/scan-image/internal/fold"
	"github.com/isuccesscorp/scan-image/internal/source"
)

// NoParent marks the root entry.
const NoParent = ^uint32(0)

// Index holds every scanned entry. It is safe for concurrent use: the
// scanner appends while the UI searches.
type Index struct {
	mu    sync.RWMutex
	roots map[uint32]string

	names   []byte
	nameOff []uint32
	folded  []byte
	foldOff []uint32

	parent  []uint32
	size    []int64
	mtime   []int64
	isDir   []bool
	nFiles  int
	nDirs   int
	byteSum int64
}

// New creates an empty index; add the folders to scan with AddRoot.
func New() *Index {
	return &Index{
		roots:   map[uint32]string{},
		nameOff: []uint32{0},
		foldOff: []uint32{0},
	}
}

// AddRoot registers a scanned folder (one NAS share, say) and returns the
// id its children hang from. Roots are not searchable themselves.
func (ix *Index) AddRoot(path string) uint32 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	path = filepath.Clean(path)
	id := ix.appendLocked(NoParent, source.Entry{Name: filepath.Base(path), IsDir: true})
	ix.nDirs--
	ix.roots[id] = path
	return id
}

// Roots lists the registered folders.
func (ix *Index) Roots() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]string, 0, len(ix.roots))
	for _, p := range ix.roots {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func (ix *Index) appendLocked(parent uint32, e source.Entry) uint32 {
	id := uint32(len(ix.parent))
	ix.names = append(ix.names, e.Name...)
	ix.nameOff = append(ix.nameOff, uint32(len(ix.names)))
	ix.folded = fold.Bytes(ix.folded, e.Name)
	ix.foldOff = append(ix.foldOff, uint32(len(ix.folded)))
	ix.parent = append(ix.parent, parent)
	ix.size = append(ix.size, e.Size)
	ix.mtime = append(ix.mtime, e.ModTime.Unix())
	ix.isDir = append(ix.isDir, e.IsDir)
	if e.IsDir {
		ix.nDirs++
	} else {
		ix.nFiles++
		ix.byteSum += e.Size
	}
	return id
}

// AddChildren appends the entries of directory parent and returns their
// ids in the same order.
func (ix *Index) AddChildren(parent uint32, entries []source.Entry) []uint32 {
	ids := make([]uint32, len(entries))
	ix.mu.Lock()
	for i, e := range entries {
		ids[i] = ix.appendLocked(parent, e)
	}
	ix.mu.Unlock()
	return ids
}

// Stats summarises the index.
type Stats struct {
	Files int
	Dirs  int
	Bytes int64
}

// Stats returns counts of files, directories and total file bytes.
func (ix *Index) Stats() Stats {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return Stats{Files: ix.nFiles, Dirs: ix.nDirs, Bytes: ix.byteSum}
}

// Len is the number of entries, roots included.
func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.parent)
}

// Item is one entry resolved for display.
type Item struct {
	ID      uint32
	Name    string
	Dir     string
	Path    string
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// Item resolves id into names and paths.
func (ix *Index) Item(id uint32) Item {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	path := ix.pathLocked(id)
	return Item{
		ID:      id,
		Name:    ix.nameLocked(id),
		Dir:     filepath.Dir(path),
		Path:    path,
		Size:    ix.size[id],
		ModTime: time.Unix(ix.mtime[id], 0),
		IsDir:   ix.isDir[id],
	}
}

func (ix *Index) nameLocked(id uint32) string {
	return string(ix.names[ix.nameOff[id]:ix.nameOff[id+1]])
}

func (ix *Index) pathLocked(id uint32) string {
	var parts []string
	cur := id
	for ix.parent[cur] != NoParent {
		parts = append(parts, ix.nameLocked(cur))
		cur = ix.parent[cur]
	}
	parts = append(parts, ix.roots[cur])
	slices.Reverse(parts)
	return filepath.Join(parts...)
}

// Options tune a search.
type Options struct {
	Limit     int
	FilesOnly bool
	// Extensions keeps only files with these lowercase extensions
	// (".jpg"); empty keeps everything.
	Extensions []string
}

// Result is a search outcome.
type Result struct {
	IDs   []uint32
	Total int
	Took  time.Duration
}

// Search finds entries whose folded name contains every space-separated
// term of query. An empty query matches everything.
func (ix *Index) Search(query string, opt Options) Result {
	start := time.Now()
	if opt.Limit <= 0 {
		opt.Limit = 1000
	}
	terms := make([][]byte, 0, 4)
	for _, t := range strings.Fields(fold.String(query)) {
		terms = append(terms, []byte(t))
	}
	exts := make([][]byte, len(opt.Extensions))
	for i, e := range opt.Extensions {
		exts[i] = []byte(strings.ToLower(e))
	}

	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n := len(ix.parent)
	workers := runtime.GOMAXPROCS(0)
	if n < 50_000 {
		workers = 1
	}
	chunk := (n + workers - 1) / workers

	type part struct {
		ids   []uint32
		total int
	}
	parts := make([]part, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := w*chunk, min((w+1)*chunk, n)
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			p := &parts[w]
			for i := lo; i < hi; i++ {
				if ix.parent[i] == NoParent || (opt.FilesOnly && ix.isDir[i]) {
					continue
				}
				name := ix.folded[ix.foldOff[i]:ix.foldOff[i+1]]
				if !matchAll(name, terms) || !matchExt(name, exts) {
					continue
				}
				p.total++
				if len(p.ids) < opt.Limit {
					p.ids = append(p.ids, uint32(i))
				}
			}
		}(w, lo, hi)
	}
	wg.Wait()

	res := Result{IDs: make([]uint32, 0, opt.Limit)}
	for _, p := range parts {
		res.Total += p.total
		for _, id := range p.ids {
			if len(res.IDs) == opt.Limit {
				break
			}
			res.IDs = append(res.IDs, id)
		}
	}
	res.Took = time.Since(start)
	return res
}

func matchAll(name []byte, terms [][]byte) bool {
	for _, t := range terms {
		if !bytes.Contains(name, t) {
			return false
		}
	}
	return true
}

func matchExt(name []byte, exts [][]byte) bool {
	if len(exts) == 0 {
		return true
	}
	for _, e := range exts {
		if bytes.HasSuffix(name, e) {
			return true
		}
	}
	return false
}
