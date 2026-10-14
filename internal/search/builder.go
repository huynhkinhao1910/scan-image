package search

import (
	"path/filepath"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

// NoPrev means "no counterpart in the old index".
const NoPrev = ^uint32(0)

// RacyWindow is how recent a directory's mtime may be before its listing is
// not trusted as a basis for reuse (see flagStable).
const RacyWindow = 10 * time.Minute

// ColdAge is how long a branch must have gone without any change before an
// incremental scan stops walking into it (see BuildOptions.Reuse).
const ColdAge = 30 * 24 * time.Hour

// BuildOptions tune a Builder.
type BuildOptions struct {
	// Reuse lets unchanged directories be copied from the old index instead
	// of listed: a leaf whose mtime is the same, and a whole branch whose top
	// has the same mtime and in which nothing is newer than ColdAge. A
	// directory's mtime says nothing about what lies deeper, so a change deep
	// inside a cold branch is only seen when the branch's verify slot is due.
	// False lists everything (a full rescan) but Failed still keeps the old
	// subtree.
	Reuse bool
	// VerifySlot and VerifySlots rotate a forced re-listing: a reusable
	// directory whose path hashes to VerifySlot modulo VerifySlots is listed
	// anyway, together with everything below it. VerifySlots == 0 disables
	// the rotation.
	VerifySlot, VerifySlots uint32
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Child is a subdirectory the caller still has to list. Prev is the id of
// the same-named directory in the old index, or NoPrev.
type Child struct {
	Name     string
	ID, Prev uint32
}

// BuildStats counts what the builder took from the old index.
type BuildStats struct {
	// ReusedDirs and ReusedEntries count the directories copied instead of
	// listed (unchanged leaves and every directory of a cold branch) and the
	// entries inside them.
	ReusedDirs, ReusedEntries int
	// KeptDirs counts directories whose old contents were kept by Failed.
	KeptDirs int
}

// Builder grows a new Index from the scan of a tree, copying from the old
// Index what a cheap check proves unchanged. The old index is never
// modified. All methods are safe for concurrent use; Listed and Failed are
// each one critical section on the new index, so a Clone of Index() never
// shows a directory flagged stable without its children.
//
// Old children of a directory form a contiguous id range. parent[] is not
// sorted (directories are appended in scan order), so NewBuilder builds
// first, a firstChild table of 4 bytes per entry (12 MB for 3M entries) in
// one O(n) pass; a range is first[dir] extended while parent[i] == dir.
type Builder struct {
	old      *Index
	opt      BuildOptions
	ix       *Index
	first    []uint32
	hot      []bool
	oldRoots map[string]Root

	// guarded by ix.mu
	changed   bool
	matched   int
	rejected  map[uint32]struct{}
	verifying map[uint32]struct{} // new dirs whose branch is listed in full
	stats     BuildStats
}

// NewBuilder starts a build against old, which may be nil. old must be a
// published index that nobody appends to or modifies: the builder reads its
// arrays without taking its lock.
func NewBuilder(old *Index, opt BuildOptions) *Builder {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	b := &Builder{old: old, opt: opt, ix: New(), changed: old == nil}
	if old == nil {
		return b
	}
	b.first = firstChildren(old.parent)
	if opt.Reuse {
		b.hot = hotEntries(old, opt.Now().Add(-ColdAge).Unix())
		b.verifying = map[uint32]struct{}{}
	}
	b.oldRoots = map[string]Root{}
	for _, r := range old.RootList() {
		b.oldRoots[r.Key] = r
	}
	b.reserve()
	return b
}

func firstChildren(parent []uint32) []uint32 {
	first := make([]uint32, len(parent))
	for i := range first {
		first[i] = NoPrev
	}
	for i, p := range parent {
		if p != NoParent && first[p] == NoPrev {
			first[p] = uint32(i)
		}
	}
	return first
}

// hotEntries marks every old entry that has, in itself or anywhere below it,
// an mtime at or after cutoff or a directory without flagStable. Children
// have higher ids than their parent, so one backward pass carries the mark
// up the tree.
func hotEntries(old *Index, cutoff int64) []bool {
	hot := make([]bool, len(old.parent))
	for i := len(hot) - 1; i >= 0; i-- {
		p := old.parent[i]
		if p == NoParent {
			continue
		}
		if old.mtime[i] >= cutoff || old.flags[i]&(flagDir|flagStable) == flagDir {
			hot[i] = true
		}
		if hot[i] {
			hot[p] = true
		}
	}
	return hot
}

// reserve sizes the new arrays like the old ones so a mostly-reused build
// does not regrow them.
func (b *Builder) reserve() {
	ix, o := b.ix, b.old
	n := len(o.parent)
	ix.names = make([]byte, 0, len(o.names))
	ix.folded = make([]byte, 0, len(o.folded))
	ix.nameOff = append(make([]uint32, 0, n+1), 0)
	ix.foldOff = append(make([]uint32, 0, n+1), 0)
	ix.parent = make([]uint32, 0, n)
	ix.size = make([]int64, 0, n)
	ix.mtime = make([]int64, 0, n)
	ix.flags = make([]uint8, 0, n)
}

// Index is the new index under construction.
func (b *Builder) Index() *Index { return b.ix }

// Changed reports whether the new index differs from the old one: false only
// when every listed directory has the same children (compared in listing
// order) and flags as before and no root was added, removed or moved.
func (b *Builder) Changed() bool {
	b.ix.mu.RLock()
	defer b.ix.mu.RUnlock()
	return b.changed || (b.old != nil && b.matched != len(b.oldRoots))
}

// Stats reports reuse counters.
func (b *Builder) Stats() BuildStats {
	b.ix.mu.RLock()
	defer b.ix.mu.RUnlock()
	return b.stats
}

// AddRoot registers a root in the new index and returns its id and the id of
// the root with the same key in the old index, or NoPrev.
func (b *Builder) AddRoot(key, path string) (id, prev uint32) {
	id = b.ix.AddRoot(key, path)
	if b.old == nil {
		return id, NoPrev
	}
	b.ix.mu.Lock()
	defer b.ix.mu.Unlock()
	r, ok := b.oldRoots[key]
	if !ok {
		b.changed = true
		return id, NoPrev
	}
	b.matched++
	if r.Path != filepath.Clean(path) {
		b.changed = true
	}
	return id, r.ID
}

// Listed records the listing of dir, whose old counterpart is prev (or
// NoPrev). It returns the subdirectories that still need listing; unchanged
// stable leaves and cold branches are copied from the old index instead and
// not returned.
func (b *Builder) Listed(dir, prev uint32, entries []source.Entry) []Child {
	ix := b.ix
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if len(entries) == 0 && prev != NoPrev && ix.parent[dir] == NoParent && b.kidCount(prev) > 0 {
		b.keepLocked(dir, prev)
		if b.rejected == nil {
			b.rejected = map[uint32]struct{}{}
		}
		b.rejected[dir] = struct{}{}
		return nil
	}
	first := uint32(len(ix.parent))
	for _, e := range entries {
		ix.appendLocked(dir, e)
	}
	// Roots are never reused, so they never carry flagStable.
	if ix.parent[dir] != NoParent && ix.mtime[dir] < b.opt.Now().Add(-RacyWindow).Unix() {
		ix.flags[dir] |= flagStable
	}
	b.trackLocked(dir, prev, int(first), entries)
	return b.subdirsLocked(dir, prev, first, entries)
}

// Rejected reports whether Listed refused the listing of root dir because it
// was empty while the old root had children (an unmounted share looks like
// that). The old subtree was kept, so the root was not really examined.
func (b *Builder) Rejected(dir uint32) bool {
	b.ix.mu.RLock()
	defer b.ix.mu.RUnlock()
	_, ok := b.rejected[dir]
	return ok
}

// Failed records that dir could not be listed. The old subtree, if any, is
// kept and dir is left without flagStable so the next round lists it again.
func (b *Builder) Failed(dir, prev uint32) {
	b.ix.mu.Lock()
	defer b.ix.mu.Unlock()
	b.keepLocked(dir, prev)
}

func (b *Builder) keepLocked(dir, prev uint32) {
	if b.old == nil || prev == NoPrev {
		return
	}
	if b.old.flags[prev]&flagStable != 0 {
		b.changed = true
	}
	dirs, _ := b.copySubtreeLocked(prev, dir)
	b.stats.KeptDirs += dirs
}

// trackLocked clears b.changed's "unchanged" state when the block just
// appended for dir differs from prev's old block.
func (b *Builder) trackLocked(dir, prev uint32, first int, entries []source.Entry) {
	if b.changed {
		return
	}
	if prev == NoPrev {
		b.changed = true
		return
	}
	o := b.old
	oldFirst, n := b.kids(prev)
	if n != len(entries) || (o.flags[prev]^b.ix.flags[dir])&flagStable != 0 {
		b.changed = true
		return
	}
	for i, e := range entries {
		j := oldFirst + i
		if o.size[j] != e.Size || o.mtime[j] != e.ModTime.Unix() ||
			(o.flags[j]&flagDir != 0) != e.IsDir ||
			string(o.names[o.nameOff[j]:o.nameOff[j+1]]) != e.Name {
			b.changed = true
			return
		}
	}
}

// subdirsLocked reuses what it can among the subdirectories just appended
// at ids first.. and returns the rest.
func (b *Builder) subdirsLocked(dir, prev, first uint32, entries []source.Entry) []Child {
	var old map[string]uint32
	if prev != NoPrev {
		old = b.oldSubdirs(prev)
	}
	var h uint32
	if b.opt.Reuse && b.opt.VerifySlots > 0 && len(old) > 0 {
		h = b.pathHash(dir)
	}
	_, verify := b.verifying[dir]
	var out []Child
	for i, e := range entries {
		if !e.IsDir {
			continue
		}
		id := first + uint32(i)
		o, ok := old[e.Name]
		if !ok {
			out = append(out, Child{Name: e.Name, ID: id, Prev: NoPrev})
			continue
		}
		due := verify
		if !due && b.unchanged(o, e) {
			if !b.verifyDue(h, e.Name) {
				b.ix.flags[id] = b.old.flags[o]
				dirs, n := b.copySubtreeLocked(o, id)
				b.stats.ReusedDirs += dirs
				b.stats.ReusedEntries += n
				continue
			}
			due = true
		}
		if due {
			b.verifying[id] = struct{}{}
		}
		out = append(out, Child{Name: e.Name, ID: id, Prev: o})
	}
	return out
}

// unchanged reports whether old directory o may stand in for the listed
// entry e: its own listing is proven current by the mtime, and it is either
// a leaf or the top of a cold branch.
func (b *Builder) unchanged(o uint32, e source.Entry) bool {
	old := b.old
	if !b.opt.Reuse || old.flags[o]&flagStable == 0 || old.mtime[o] != e.ModTime.Unix() {
		return false
	}
	return !b.hot[o] || !b.hasSubdir(o)
}

// verifyDue reports whether the rotation forces a listing of the directory
// name under a parent whose path hash is h.
func (b *Builder) verifyDue(h uint32, name string) bool {
	s := b.opt.VerifySlots
	return s > 0 && fnvString(fnvString(h, "/"), name)%s == b.opt.VerifySlot
}

func (b *Builder) hasSubdir(dir uint32) bool {
	first, n := b.kids(dir)
	for _, f := range b.old.flags[first : first+n] {
		if f&flagDir != 0 {
			return true
		}
	}
	return false
}

// oldSubdirs maps the names of the old subdirectories of prev to their ids.
func (b *Builder) oldSubdirs(prev uint32) map[string]uint32 {
	first, n := b.kids(prev)
	var m map[string]uint32
	for j := first; j < first+n; j++ {
		if b.old.flags[j]&flagDir == 0 {
			continue
		}
		if m == nil {
			m = map[string]uint32{}
		}
		m[string(b.old.names[b.old.nameOff[j]:b.old.nameOff[j+1]])] = uint32(j)
	}
	return m
}

// kids returns the first id and number of the old children of dir.
func (b *Builder) kids(dir uint32) (first, n int) {
	f := b.first[dir]
	if f == NoPrev {
		return 0, 0
	}
	p := b.old.parent
	for i := int(f); i < len(p) && p[i] == dir; i++ {
		n++
	}
	return int(f), n
}

func (b *Builder) kidCount(dir uint32) int {
	_, n := b.kids(dir)
	return n
}

// pathHash hashes the path of dir relative to its root key, so the verify
// rotation does not move when a share is mounted somewhere else.
func (b *Builder) pathHash(dir uint32) uint32 {
	ix := b.ix
	var chain []uint32
	cur := dir
	for ix.parent[cur] != NoParent {
		chain = append(chain, cur)
		cur = ix.parent[cur]
	}
	h := fnvString(fnvOffset, ix.roots[cur].Key)
	for i := len(chain) - 1; i >= 0; i-- {
		h = fnvString(fnvString(h, "/"), ix.nameLocked(chain[i]))
	}
	return h
}

const (
	fnvOffset uint32 = 2166136261
	fnvPrime  uint32 = 16777619
)

func fnvString(h uint32, s string) uint32 {
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * fnvPrime
	}
	return h
}
