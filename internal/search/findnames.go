package search

import (
	"runtime"
	"sync"

	"github.com/isuccesscorp/scan-image/internal/fold"
)

type nameHit struct {
	pos int
	id  uint32
}

// FindNames returns, for each name, the ids of the files (not directories, not
// roots) whose folded name equals its folded form: case and Vietnamese
// diacritics are ignored, as in Search. Diacritics have to be ignored because
// macOS lists SMB names decomposed (NFD) while lists from elsewhere spell them
// composed (NFC), and only the folded forms of the two agree. One pass over
// the index whatever len(names) is.
func (ix *Index) FindNames(names []string) [][]uint32 {
	out := make([][]uint32, len(names))
	if len(names) == 0 {
		return out
	}
	want := make(map[string][]int, len(names))
	for i, n := range names {
		k := fold.String(n)
		want[k] = append(want[k], i)
	}

	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n := len(ix.parent)
	workers := runtime.GOMAXPROCS(0)
	if n < 50_000 {
		workers = 1
	}
	chunk := (n + workers - 1) / workers
	parts := make([][]nameHit, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := w*chunk, min((w+1)*chunk, n)
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			parts[w] = ix.findNamesLocked(lo, hi, want)
		}()
	}
	wg.Wait()
	for _, hits := range parts {
		for _, h := range hits {
			out[h.pos] = append(out[h.pos], h.id)
		}
	}
	return out
}

func (ix *Index) findNamesLocked(lo, hi int, want map[string][]int) []nameHit {
	var hits []nameHit
	for i := lo; i < hi; i++ {
		if ix.parent[i] == NoParent || ix.flags[i]&flagDir != 0 {
			continue
		}
		for _, p := range want[string(ix.folded[ix.foldOff[i]:ix.foldOff[i+1]])] {
			hits = append(hits, nameHit{p, uint32(i)})
		}
	}
	return hits
}
