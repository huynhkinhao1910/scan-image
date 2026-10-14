package search

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

var (
	fixedNow = time.Unix(2_000_000_000, 0)
	oldMT    = fixedNow.Add(-time.Hour)
	recentMT = fixedNow.Add(-time.Minute)

	defaultRoot = rootSpec{key: "smb://nas/r", path: "/r"}
)

type rootSpec struct{ key, path string }

// fakeTree is an in-memory file system: directory path -> listing.
type fakeTree struct {
	dirs map[string][]source.Entry
	fail map[string]bool
}

func dirEntry(name string, mt time.Time) source.Entry {
	return source.Entry{Name: name, IsDir: true, ModTime: mt, Size: 64}
}

func fileEntry(name string, size int64, mt time.Time) source.Entry {
	return source.Entry{Name: name, Size: size, ModTime: mt}
}

// sampleTree builds, under every given root:
//
//	root: a/ b/ c/ top.txt
//	a: a1/ a.txt     a/a1: x.jpg y.jpg
//	b: b1.jpg        c: c1.jpg c2.jpg c3.jpg
//
// so the non-leaf directories are root and a, and the leaves are a1, b, c.
func sampleTree(roots ...string) *fakeTree {
	t := &fakeTree{dirs: map[string][]source.Entry{}, fail: map[string]bool{}}
	for _, r := range roots {
		t.dirs[r] = []source.Entry{
			dirEntry("a", oldMT), dirEntry("b", oldMT), dirEntry("c", oldMT),
			fileEntry("top.txt", 10, oldMT),
		}
		t.dirs[path.Join(r, "a")] = []source.Entry{dirEntry("a1", oldMT), fileEntry("a.txt", 20, oldMT)}
		t.dirs[path.Join(r, "a/a1")] = []source.Entry{fileEntry("x.jpg", 100, oldMT), fileEntry("y.jpg", 200, oldMT)}
		t.dirs[path.Join(r, "b")] = []source.Entry{fileEntry("b1.jpg", 300, oldMT)}
		t.dirs[path.Join(r, "c")] = []source.Entry{
			fileEntry("c1.jpg", 1, oldMT), fileEntry("c2.jpg", 2, oldMT), fileEntry("c3.jpg", 3, oldMT),
		}
	}
	return t
}

func (t *fakeTree) put(dir string, e ...source.Entry) {
	t.dirs[dir] = append(t.dirs[dir], e...)
}

func (t *fakeTree) drop(dir, name string) {
	t.dirs[dir] = slices.DeleteFunc(t.dirs[dir], func(e source.Entry) bool { return e.Name == name })
}

func (t *fakeTree) edit(dir, name string, fn func(*source.Entry)) {
	for i := range t.dirs[dir] {
		if t.dirs[dir][i].Name == name {
			fn(&t.dirs[dir][i])
			return
		}
	}
	panic("no entry " + path.Join(dir, name))
}

// touch sets the mtime of directory p as its parent lists it.
func (t *fakeTree) touch(p string, mt time.Time) {
	t.edit(path.Dir(p), path.Base(p), func(e *source.Entry) { e.ModTime = mt })
}

type round struct {
	b      *Builder
	ix     *Index
	listed []string          // directories the driver had to list, sorted
	failed []string          // directories the driver reported as failed, sorted
	prevs  map[string]uint32 // root key -> prev returned by AddRoot
}

func (r round) wasListed(p string) bool { return slices.Contains(r.listed, p) }

func opts(now time.Time) BuildOptions {
	return BuildOptions{Reuse: true, Now: func() time.Time { return now }}
}

// play drives b the way the scanner does: AddRoot, then Listed/Failed for
// every directory Listed hands back.
func play(tree *fakeTree, old *Index, opt BuildOptions, roots ...rootSpec) round {
	if len(roots) == 0 {
		roots = []rootSpec{defaultRoot}
	}
	type job struct {
		path     string
		id, prev uint32
	}
	b := NewBuilder(old, opt)
	r := round{b: b, prevs: map[string]uint32{}}
	var stack []job
	for _, rs := range roots {
		id, prev := b.AddRoot(rs.key, rs.path)
		r.prevs[rs.key] = prev
		stack = append(stack, job{rs.path, id, prev})
	}
	for len(stack) > 0 {
		j := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if tree.fail[j.path] {
			b.Failed(j.id, j.prev)
			r.failed = append(r.failed, j.path)
			continue
		}
		r.listed = append(r.listed, j.path)
		for _, c := range b.Listed(j.id, j.prev, slices.Clone(tree.dirs[j.path])) {
			stack = append(stack, job{path.Join(j.path, c.Name), c.ID, c.Prev})
		}
	}
	slices.Sort(r.listed)
	slices.Sort(r.failed)
	r.ix = b.Index()
	return r
}

func dump(ix *Index) []string {
	out := make([]string, 0, ix.Len())
	for i := 0; i < ix.Len(); i++ {
		it := ix.Item(uint32(i))
		out = append(out, fmt.Sprintf("%s|%v|%d|%d", it.Path, it.IsDir, it.Size, it.ModTime.Unix()))
	}
	slices.Sort(out)
	return out
}

func paths(ix *Index) []string {
	out := make([]string, 0, ix.Len())
	for i := 0; i < ix.Len(); i++ {
		out = append(out, ix.Item(uint32(i)).Path)
	}
	slices.Sort(out)
	return out
}

func has(ix *Index, p string) bool { return slices.Contains(paths(ix), p) }

func idOf(t *testing.T, ix *Index, p string) uint32 {
	t.Helper()
	for i := 0; i < ix.Len(); i++ {
		if ix.Item(uint32(i)).Path == p {
			return uint32(i)
		}
	}
	t.Fatalf("%s not in index; have %v", p, paths(ix))
	return 0
}

func isStable(t *testing.T, ix *Index, p string) bool {
	t.Helper()
	return ix.flags[idOf(t, ix, p)]&flagStable != 0
}

// plainScan is the reference: what a scan without any reuse produces.
func plainScan(tree *fakeTree, roots ...rootSpec) *Index {
	ix := New()
	var walk func(id uint32, p string)
	walk = func(id uint32, p string) {
		entries := tree.dirs[p]
		ids := ix.AddChildren(id, entries)
		for i, e := range entries {
			if e.IsDir {
				walk(ids[i], path.Join(p, e.Name))
			}
		}
	}
	for _, rs := range roots {
		walk(ix.AddRoot(rs.key, rs.path), rs.path)
	}
	return ix
}

// assertShape checks the documented invariants: parent[i] < i and the
// children of a directory occupy one contiguous id range.
func assertShape(t *testing.T, ix *Index) {
	t.Helper()
	last := map[uint32]uint32{}
	for i := uint32(0); i < uint32(ix.Len()); i++ {
		p := ix.parent[i]
		if p == NoParent {
			continue
		}
		if p >= i {
			t.Fatalf("parent[%d] = %d, want < id", i, p)
		}
		if prev, ok := last[p]; ok && prev+1 != i {
			t.Fatalf("children of %d not contiguous: %d then %d", p, prev, i)
		}
		last[p] = i
	}
}

func baseRound(t *testing.T) round {
	t.Helper()
	return play(sampleTree("/r"), nil, opts(fixedNow))
}

func TestBuilderFirstBuildEqualsPlainScan(t *testing.T) {
	tree := sampleTree("/r")
	r := play(tree, nil, opts(fixedNow))
	want := plainScan(tree, defaultRoot)

	if !slices.Equal(dump(r.ix), dump(want)) {
		t.Errorf("entries differ:\n got %v\nwant %v", dump(r.ix), dump(want))
	}
	if r.ix.Stats() != want.Stats() {
		t.Errorf("Stats = %+v, want %+v", r.ix.Stats(), want.Stats())
	}
	for _, q := range []string{"", "jpg", "c2", "TOP", "zzz"} {
		g, w := r.ix.Search(q, Options{}), want.Search(q, Options{})
		if g.Total != w.Total {
			t.Errorf("Search(%q).Total = %d, want %d", q, g.Total, w.Total)
		}
	}
	if len(r.listed) != 5 {
		t.Errorf("listed %v, want all 5 directories", r.listed)
	}
	if st := r.b.Stats(); st.ReusedDirs != 0 || st.ReusedEntries != 0 || st.KeptDirs != 0 {
		t.Errorf("Stats = %+v, want zeros on a first build", st)
	}
	if r.prevs[defaultRoot.key] != NoPrev {
		t.Errorf("prev = %d, want NoPrev when old is nil", r.prevs[defaultRoot.key])
	}
	assertShape(t, r.ix)
}

func TestBuilderReusesUnchangedStableLeaves(t *testing.T) {
	first := baseRound(t)

	r := play(sampleTree("/r"), first.ix, opts(fixedNow))

	if want := []string{"/r", "/r/a"}; !slices.Equal(r.listed, want) {
		t.Errorf("listed %v, want only the non-leaf dirs %v", r.listed, want)
	}
	if !slices.Equal(dump(r.ix), dump(first.ix)) {
		t.Errorf("reused build differs from old:\n got %v\nwant %v", dump(r.ix), dump(first.ix))
	}
	for _, p := range []string{"/r/a/a1/x.jpg", "/r/a/a1/y.jpg", "/r/b/b1.jpg", "/r/c/c1.jpg", "/r/c/c3.jpg"} {
		if !has(r.ix, p) {
			t.Errorf("reused file %s missing from new index", p)
		}
	}
	st := r.b.Stats()
	if st.ReusedDirs != 3 {
		t.Errorf("ReusedDirs = %d, want 3 (a1, b, c)", st.ReusedDirs)
	}
	if st.ReusedEntries != 6 {
		t.Errorf("ReusedEntries = %d, want 6 (2+1+3 files)", st.ReusedEntries)
	}
	if r.ix.Stats() != first.ix.Stats() {
		t.Errorf("index Stats = %+v, want %+v", r.ix.Stats(), first.ix.Stats())
	}
	assertShape(t, r.ix)
}

func TestBuilderReuseKeepsStableFlagSoNextRoundReusesAgain(t *testing.T) {
	first := baseRound(t)
	second := play(sampleTree("/r"), first.ix, opts(fixedNow))
	third := play(sampleTree("/r"), second.ix, opts(fixedNow))

	if !slices.Equal(third.listed, []string{"/r", "/r/a"}) {
		t.Errorf("third round listed %v, want only non-leaf dirs", third.listed)
	}
	for _, p := range []string{"/r/a/a1", "/r/b", "/r/c"} {
		if !isStable(t, second.ix, p) {
			t.Errorf("%s lost flagStable after reuse", p)
		}
	}
}

func TestBuilderListsLeafAgainWhenOldCopyIsNotStable(t *testing.T) {
	first := baseRound(t)
	first.ix.flags[idOf(t, first.ix, "/r/b")] &^= flagStable

	r := play(sampleTree("/r"), first.ix, opts(fixedNow))

	if !r.wasListed("/r/b") {
		t.Errorf("listed %v, want /r/b listed again", r.listed)
	}
	if r.wasListed("/r/c") {
		t.Errorf("/r/c should still be reused, listed %v", r.listed)
	}
	if !has(r.ix, "/r/b/b1.jpg") {
		t.Error("b1.jpg missing")
	}
}

func TestBuilderListsLeafAgainWhenMTimeDiffers(t *testing.T) {
	first := baseRound(t)
	tree := sampleTree("/r")
	tree.touch("/r/b", oldMT.Add(5*time.Minute))
	tree.put("/r/b", fileEntry("b2.jpg", 9, oldMT))

	r := play(tree, first.ix, opts(fixedNow))

	if !r.wasListed("/r/b") {
		t.Errorf("listed %v, want /r/b listed again", r.listed)
	}
	if r.wasListed("/r/c") {
		t.Errorf("/r/c should be reused, listed %v", r.listed)
	}
	if !has(r.ix, "/r/b/b2.jpg") {
		t.Error("new file b2.jpg not found after relist")
	}
	if got := r.b.Stats().ReusedDirs; got != 2 {
		t.Errorf("ReusedDirs = %d, want 2", got)
	}
}

func TestBuilderListsDirWithSubdirectoriesEvenWhenStableAndUnchanged(t *testing.T) {
	first := baseRound(t)
	if !isStable(t, first.ix, "/r/a") {
		t.Fatal("setup: /r/a should be stable in the old index")
	}

	r := play(sampleTree("/r"), first.ix, opts(fixedNow))

	if !r.wasListed("/r/a") {
		t.Errorf("listed %v, want /r/a listed: its old copy has a subdirectory", r.listed)
	}
	if r.wasListed("/r/a/a1") {
		t.Errorf("/r/a/a1 is an unchanged leaf and must be reused, listed %v", r.listed)
	}
}

func TestBuilderListsLeafAgainWhenItsVerifySlotIsDue(t *testing.T) {
	first := baseRound(t)
	const slots = 64
	dueRounds := 0
	for slot := uint32(0); slot < slots; slot++ {
		o := opts(fixedNow)
		o.VerifySlots, o.VerifySlot = slots, slot
		r := play(sampleTree("/r"), first.ix, o)
		if !r.wasListed("/r/b") {
			continue
		}
		dueRounds++
		if !has(r.ix, "/r/b/b1.jpg") {
			t.Errorf("slot %d: relisted /r/b lost b1.jpg", slot)
		}
		if got, want := r.b.Stats().ReusedDirs, 3-countPrefix(r.listed, "/r/a/a1", "/r/b", "/r/c"); got != want {
			t.Errorf("slot %d: ReusedDirs = %d, want %d", slot, got, want)
		}
	}
	if dueRounds != 1 {
		t.Errorf("/r/b relisted in %d of %d slots, want exactly 1", dueRounds, slots)
	}
}

func countPrefix(listed []string, leaves ...string) int {
	n := 0
	for _, l := range leaves {
		if slices.Contains(listed, l) {
			n++
		}
	}
	return n
}

func TestBuilderVerifyRotationRelistsEveryLeafExactlyOnce(t *testing.T) {
	const leaves, slots = 40, 64
	tree := &fakeTree{dirs: map[string][]source.Entry{}, fail: map[string]bool{}}
	for i := 0; i < leaves; i++ {
		name := fmt.Sprintf("d%02d", i)
		tree.put("/r", dirEntry(name, oldMT))
		tree.put(path.Join("/r", name), fileEntry("f.jpg", int64(i), oldMT), fileEntry("g.jpg", 1, oldMT))
	}
	first := play(tree, nil, opts(fixedNow))

	relisted := map[string]int{}
	for slot := uint32(0); slot < slots; slot++ {
		o := opts(fixedNow)
		o.VerifySlots, o.VerifySlot = slots, slot
		r := play(tree, first.ix, o)
		for _, p := range r.listed {
			if p != "/r" {
				relisted[p]++
			}
		}
		if !slices.Equal(dump(r.ix), dump(first.ix)) {
			t.Fatalf("slot %d: relisting an unchanged tree changed the index", slot)
		}
	}
	for i := 0; i < leaves; i++ {
		p := fmt.Sprintf("/r/d%02d", i)
		if relisted[p] != 1 {
			t.Errorf("%s relisted %d times over %d slots, want 1", p, relisted[p], slots)
		}
	}
}

func TestBuilderSetsStableOnlyWhenMTimeIsOlderThanRacyWindow(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"older than window", RacyWindow + time.Minute, true},
		{"an hour old", time.Hour, true},
		{"changed a minute ago", time.Minute, false},
		{"changed just now", 0, false},
		{"inside window", RacyWindow - time.Minute, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree := sampleTree("/r")
			tree.touch("/r/b", fixedNow.Add(-tt.age))
			r := play(tree, nil, opts(fixedNow))
			if got := isStable(t, r.ix, "/r/b"); got != tt.want {
				t.Errorf("flagStable(/r/b) = %v, want %v", got, tt.want)
			}
			if !isStable(t, r.ix, "/r/c") {
				t.Error("/r/c is an hour old and should be stable")
			}
		})
	}
}

func TestBuilderRecentlyChangedDirIsListedAgainEvenWithIdenticalMTime(t *testing.T) {
	tree := sampleTree("/r")
	tree.touch("/r/b", recentMT)
	first := play(tree, nil, opts(fixedNow))

	r := play(tree, first.ix, opts(fixedNow))

	if !r.wasListed("/r/b") {
		t.Errorf("listed %v, want /r/b listed again: it changed inside the racy window", r.listed)
	}
	if r.wasListed("/r/c") {
		t.Errorf("/r/c is stable and unchanged, listed %v", r.listed)
	}
}

func TestBuilderDirBecomesStableOnceRacyWindowHasPassed(t *testing.T) {
	tree := sampleTree("/r")
	tree.touch("/r/b", recentMT)
	first := play(tree, nil, opts(fixedNow))
	later := fixedNow.Add(RacyWindow + time.Minute)

	second := play(tree, first.ix, opts(later))
	third := play(tree, second.ix, opts(later))

	if !second.wasListed("/r/b") {
		t.Fatalf("second round listed %v, want /r/b", second.listed)
	}
	if !isStable(t, second.ix, "/r/b") {
		t.Error("/r/b should be stable once its mtime is older than the window")
	}
	if third.wasListed("/r/b") {
		t.Errorf("third round listed %v, want /r/b reused", third.listed)
	}
}

func TestBuilderDirNeverListedHasNoStableFlag(t *testing.T) {
	b := NewBuilder(nil, opts(fixedNow))
	root, _ := b.AddRoot(defaultRoot.key, defaultRoot.path)
	kids := b.Listed(root, NoPrev, sampleTree("/r").dirs["/r"])
	if len(kids) != 3 {
		t.Fatalf("Listed returned %d children, want 3 subdirectories", len(kids))
	}

	ix := b.Index()
	for _, p := range []string{"/r/a", "/r/b", "/r/c"} {
		if isStable(t, ix, p) {
			t.Errorf("%s was never listed but has flagStable", p)
		}
	}
}

func TestBuilderListedReturnsOnlySubdirectoriesWithNamesAndIDs(t *testing.T) {
	b := NewBuilder(nil, opts(fixedNow))
	root, _ := b.AddRoot(defaultRoot.key, defaultRoot.path)

	kids := b.Listed(root, NoPrev, sampleTree("/r").dirs["/r"])

	var names []string
	for _, c := range kids {
		names = append(names, c.Name)
		if c.Prev != NoPrev {
			t.Errorf("child %s Prev = %d, want NoPrev without an old index", c.Name, c.Prev)
		}
		if got := b.Index().Item(c.ID); got.Name != c.Name || !got.IsDir {
			t.Errorf("child %+v does not point at its directory: %+v", c, got)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Errorf("children = %v, want [a b c]", names)
	}
}

func TestBuilderChildPrevPointsAtSameNamedOldDirectory(t *testing.T) {
	first := baseRound(t)
	b := NewBuilder(first.ix, opts(fixedNow))
	root, prev := b.AddRoot(defaultRoot.key, defaultRoot.path)
	tree := sampleTree("/r")
	tree.touch("/r/b", oldMT.Add(time.Minute))
	tree.put("/r", dirEntry("fresh", oldMT))

	kids := b.Listed(root, prev, tree.dirs["/r"])

	byName := map[string]Child{}
	for _, c := range kids {
		byName[c.Name] = c
	}
	if got := byName["b"].Prev; got != idOf(t, first.ix, "/r/b") {
		t.Errorf("b.Prev = %d, want old id %d", got, idOf(t, first.ix, "/r/b"))
	}
	if got := byName["a"].Prev; got != idOf(t, first.ix, "/r/a") {
		t.Errorf("a.Prev = %d, want old id %d", got, idOf(t, first.ix, "/r/a"))
	}
	if got := byName["fresh"].Prev; got != NoPrev {
		t.Errorf("fresh.Prev = %d, want NoPrev", got)
	}
	if _, ok := byName["c"]; ok {
		t.Error("c is an unchanged stable leaf and must not be returned")
	}
}

func TestBuilderFailedDirHasNoStableFlagAndNextRoundListsIt(t *testing.T) {
	first := baseRound(t)
	tree := sampleTree("/r")
	tree.touch("/r/b", oldMT.Add(time.Minute))
	tree.fail["/r/b"] = true

	second := play(tree, first.ix, opts(fixedNow))

	if !slices.Equal(second.failed, []string{"/r/b"}) {
		t.Fatalf("failed = %v, want [/r/b]", second.failed)
	}
	if !has(second.ix, "/r/b/b1.jpg") {
		t.Error("failed dir lost its old files")
	}
	if isStable(t, second.ix, "/r/b") {
		t.Error("failed dir must not have flagStable")
	}

	delete(tree.fail, "/r/b")
	tree.put("/r/b", fileEntry("b2.jpg", 5, oldMT))
	third := play(tree, second.ix, opts(fixedNow))
	if !third.wasListed("/r/b") {
		t.Errorf("third round listed %v, want /r/b listed again", third.listed)
	}
	if !has(third.ix, "/r/b/b2.jpg") {
		t.Error("b2.jpg missing after the dir was listed again")
	}
}

func TestBuilderFailedKeepsWholeOldSubtreeAndDescendantFlags(t *testing.T) {
	first := baseRound(t)
	tree := sampleTree("/r")
	tree.fail["/r/a"] = true

	r := play(tree, first.ix, opts(fixedNow))

	if !slices.Equal(r.failed, []string{"/r/a"}) {
		t.Fatalf("failed = %v, want [/r/a]", r.failed)
	}
	if r.wasListed("/r/a/a1") {
		t.Errorf("descendants of a failed dir must not be listed, listed %v", r.listed)
	}
	if !slices.Equal(dump(r.ix), dump(first.ix)) {
		t.Errorf("failed subtree not kept:\n got %v\nwant %v", dump(r.ix), dump(first.ix))
	}
	if got := r.ix.Search("x.jpg", Options{}).Total; got != 1 {
		t.Errorf("Search(x.jpg).Total = %d, want 1", got)
	}
	if isStable(t, r.ix, "/r/a") {
		t.Error("failed dir /r/a must not have flagStable")
	}
	if !isStable(t, r.ix, "/r/a/a1") {
		t.Error("descendant /r/a/a1 must keep its flagStable")
	}
	if got := r.b.Stats().KeptDirs; got < 1 {
		t.Errorf("KeptDirs = %d, want >= 1", got)
	}
	assertShape(t, r.ix)
}

func TestBuilderFailedWithNoPrevAddsNothingAndDoesNotPanic(t *testing.T) {
	first := baseRound(t)
	tree := sampleTree("/r")
	tree.put("/r", dirEntry("fresh", oldMT))
	tree.put("/r/fresh", fileEntry("n.jpg", 1, oldMT))
	tree.fail["/r/fresh"] = true

	r := play(tree, first.ix, opts(fixedNow))

	if !slices.Equal(r.failed, []string{"/r/fresh"}) {
		t.Fatalf("failed = %v, want [/r/fresh]", r.failed)
	}
	if !has(r.ix, "/r/fresh") {
		t.Error("the failed directory itself should still be an entry")
	}
	if has(r.ix, "/r/fresh/n.jpg") {
		t.Error("nothing is known about a failed new directory")
	}
	if isStable(t, r.ix, "/r/fresh") {
		t.Error("failed dir must not have flagStable")
	}
}

func TestBuilderFailedWithoutOldIndexDoesNotPanic(t *testing.T) {
	b := NewBuilder(nil, opts(fixedNow))
	root, prev := b.AddRoot(defaultRoot.key, defaultRoot.path)
	if prev != NoPrev {
		t.Fatalf("prev = %d, want NoPrev", prev)
	}
	b.Failed(root, NoPrev)
	if got := b.Index().Stats(); got != (Stats{}) {
		t.Errorf("Stats = %+v, want empty", got)
	}
}

func TestBuilderEmptyRootListingWhenOldRootHadChildrenIsTreatedAsFailed(t *testing.T) {
	first := baseRound(t)
	tree := sampleTree("/r")
	tree.dirs["/r"] = nil

	r := play(tree, first.ix, opts(fixedNow))

	if !slices.Equal(paths(r.ix), paths(first.ix)) {
		t.Errorf("index wiped by an empty mountpoint:\n got %v\nwant %v", paths(r.ix), paths(first.ix))
	}
	if got := r.ix.Search("jpg", Options{}).Total; got != 6 {
		t.Errorf("Search(jpg).Total = %d, want 6", got)
	}
	if len(r.listed) > 1 {
		t.Errorf("listed %v: nothing below an empty root should be listed", r.listed)
	}
	assertShape(t, r.ix)
}

func TestBuilderEmptyRootStaysEmptyWhenOldRootWasEmpty(t *testing.T) {
	tree := &fakeTree{dirs: map[string][]source.Entry{}, fail: map[string]bool{}}
	first := play(tree, nil, opts(fixedNow))

	r := play(tree, first.ix, opts(fixedNow))

	if r.ix.Len() != 1 || r.b.Changed() {
		t.Errorf("Len = %d, Changed = %v; want a single root entry and no change", r.ix.Len(), r.b.Changed())
	}
}

func TestBuilderEmptiedLeafLosesItsFiles(t *testing.T) {
	first := baseRound(t)
	tree := sampleTree("/r")
	tree.dirs["/r/c"] = nil
	tree.touch("/r/c", oldMT.Add(time.Minute))

	r := play(tree, first.ix, opts(fixedNow))

	if has(r.ix, "/r/c/c1.jpg") {
		t.Error("files of an emptied leaf are still indexed")
	}
	if !has(r.ix, "/r/c") {
		t.Error("the emptied directory itself should remain")
	}
}

func TestBuilderDeletedAndNewEntriesAreReflected(t *testing.T) {
	first := baseRound(t)
	tree := sampleTree("/r")
	tree.drop("/r", "top.txt")
	tree.drop("/r", "b")
	tree.put("/r", fileEntry("new.png", 77, oldMT))
	tree.drop("/r/a", "a.txt")
	tree.drop("/r/c", "c2.jpg")
	tree.put("/r/c", fileEntry("c4.jpg", 4, oldMT))
	tree.touch("/r/c", oldMT.Add(time.Minute))

	r := play(tree, first.ix, opts(fixedNow))

	for _, p := range []string{"/r/top.txt", "/r/b", "/r/b/b1.jpg", "/r/a/a.txt", "/r/c/c2.jpg"} {
		if has(r.ix, p) {
			t.Errorf("%s should be gone", p)
		}
	}
	for _, p := range []string{"/r/new.png", "/r/c/c4.jpg", "/r/c/c1.jpg", "/r/a/a1/x.jpg"} {
		if !has(r.ix, p) {
			t.Errorf("%s should be present", p)
		}
	}
	want := plainScan(tree, defaultRoot)
	if !slices.Equal(dump(r.ix), dump(want)) {
		t.Errorf("result differs from a plain scan:\n got %v\nwant %v", dump(r.ix), dump(want))
	}
	if r.ix.Stats() != want.Stats() {
		t.Errorf("Stats = %+v, want %+v", r.ix.Stats(), want.Stats())
	}
	assertShape(t, r.ix)
}

func TestBuilderRenamedDirectoryIsFoundUnderNewName(t *testing.T) {
	first := baseRound(t)
	tree := sampleTree("/r")
	tree.edit("/r/a", "a1", func(e *source.Entry) { e.Name = "a2" })
	tree.dirs["/r/a/a2"] = tree.dirs["/r/a/a1"]
	delete(tree.dirs, "/r/a/a1")

	r := play(tree, first.ix, opts(fixedNow))

	if !has(r.ix, "/r/a/a2/x.jpg") {
		t.Error("renamed directory contents not found under the new name")
	}
	if has(r.ix, "/r/a/a1") || has(r.ix, "/r/a/a1/x.jpg") {
		t.Error("old name still indexed")
	}
	if !r.wasListed("/r/a/a2") {
		t.Errorf("listed %v, want the renamed directory listed (no previous copy)", r.listed)
	}
}

func TestBuilderRootsMatchByKeyNotPath(t *testing.T) {
	const key = "smb://nas/Photos"
	first := play(sampleTree("/Volumes/Photos"), nil, opts(fixedNow), rootSpec{key, "/Volumes/Photos"})

	r := play(sampleTree("/Volumes/Photos-1"), first.ix, opts(fixedNow), rootSpec{key, "/Volumes/Photos-1"})

	if r.prevs[key] == NoPrev {
		t.Fatal("prev = NoPrev, want the old root matched by key")
	}
	if want := []string{"/Volumes/Photos-1", "/Volumes/Photos-1/a"}; !slices.Equal(r.listed, want) {
		t.Errorf("listed %v, want %v", r.listed, want)
	}
	if got := r.b.Stats().ReusedDirs; got != 3 {
		t.Errorf("ReusedDirs = %d, want 3", got)
	}
	if !has(r.ix, "/Volumes/Photos-1/b/b1.jpg") || has(r.ix, "/Volumes/Photos/b/b1.jpg") {
		t.Errorf("paths = %v, want the new mountpoint everywhere", paths(r.ix))
	}
	rl := r.ix.RootList()
	if len(rl) != 1 || rl[0].Key != key || rl[0].Path != "/Volumes/Photos-1" {
		t.Errorf("RootList = %+v", rl)
	}
}

func TestBuilderRootAbsentFromNewBuildDisappearsAndNewKeyHasNoPrev(t *testing.T) {
	k1, k2, k3 := rootSpec{"k1", "/r1"}, rootSpec{"k2", "/r2"}, rootSpec{"k3", "/r3"}
	first := play(sampleTree("/r1", "/r2"), nil, opts(fixedNow), k1, k2)

	r := play(sampleTree("/r1", "/r3"), first.ix, opts(fixedNow), k1, k3)

	if r.prevs["k1"] == NoPrev {
		t.Error("k1 existed in old, prev = NoPrev")
	}
	if r.prevs["k3"] != NoPrev {
		t.Errorf("k3 is new, prev = %d, want NoPrev", r.prevs["k3"])
	}
	for _, p := range paths(r.ix) {
		if strings.HasPrefix(p, "/r2") {
			t.Errorf("%s from the removed root is still indexed", p)
		}
	}
	if !has(r.ix, "/r3/c/c1.jpg") || !has(r.ix, "/r1/c/c1.jpg") {
		t.Errorf("paths = %v", paths(r.ix))
	}
	if got := r.b.Stats().ReusedDirs; got != 3 {
		t.Errorf("ReusedDirs = %d, want 3 (only k1's leaves)", got)
	}
	if keys := rootKeys(r.ix); !slices.Equal(keys, []string{"k1", "k3"}) {
		t.Errorf("root keys = %v, want [k1 k3]", keys)
	}
}

func rootKeys(ix *Index) []string {
	var keys []string
	for _, r := range ix.RootList() {
		keys = append(keys, r.Key)
	}
	slices.Sort(keys)
	return keys
}

func TestBuilderChanged(t *testing.T) {
	extra := rootSpec{"smb://nas/extra", "/e"}
	tests := []struct {
		name     string
		oldRoots []rootSpec
		prep     func(*fakeTree) // applied to the tree of the first round
		mutate   func(*fakeTree) // applied to the tree of the second round
		roots    []rootSpec
		now      time.Time
		want     bool
	}{
		{name: "nothing differs", want: false},
		{
			name:   "file added",
			mutate: func(t *fakeTree) { t.put("/r/a", fileEntry("new.txt", 1, oldMT)) },
			want:   true,
		},
		{
			name:   "file removed",
			mutate: func(t *fakeTree) { t.drop("/r/a", "a.txt") },
			want:   true,
		},
		{
			name:   "file size changed",
			mutate: func(t *fakeTree) { t.edit("/r/a", "a.txt", func(e *source.Entry) { e.Size++ }) },
			want:   true,
		},
		{
			name: "file mtime changed",
			mutate: func(t *fakeTree) {
				t.edit("/r/a", "a.txt", func(e *source.Entry) { e.ModTime = e.ModTime.Add(time.Minute) })
			},
			want: true,
		},
		{
			name: "file added in a relisted leaf",
			mutate: func(t *fakeTree) {
				t.touch("/r/b", oldMT.Add(time.Minute))
				t.put("/r/b", fileEntry("b2.jpg", 1, oldMT))
			},
			want: true,
		},
		{
			name: "directory gained flagStable",
			prep: func(t *fakeTree) { t.touch("/r/b", recentMT) },
			now:  fixedNow.Add(RacyWindow + time.Minute),
			want: true,
		},
		{
			name:  "root added",
			roots: []rootSpec{defaultRoot, extra},
			mutate: func(t *fakeTree) {
				t.dirs["/e"] = []source.Entry{fileEntry("e.txt", 1, oldMT)}
			},
			want: true,
		},
		{
			name:     "root removed",
			oldRoots: []rootSpec{defaultRoot, extra},
			prep: func(t *fakeTree) {
				t.dirs["/e"] = []source.Entry{fileEntry("e.txt", 1, oldMT)}
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree1 := sampleTree("/r")
			if tt.prep != nil {
				tt.prep(tree1)
			}
			first := play(tree1, nil, opts(fixedNow), tt.oldRoots...)

			tree2 := sampleTree("/r")
			if tt.prep != nil {
				tt.prep(tree2)
			}
			if tt.mutate != nil {
				tt.mutate(tree2)
			}
			now := tt.now
			if now.IsZero() {
				now = fixedNow
			}
			r := play(tree2, first.ix, opts(now), tt.roots...)

			if got := r.b.Changed(); got != tt.want {
				t.Errorf("Changed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuilderChangedIsTrueWithoutOldIndex(t *testing.T) {
	if r := baseRound(t); !r.b.Changed() {
		t.Error("Changed() = false for a first build, want true")
	}
}

func TestBuilderFullModeListsEverythingAndReusesNothing(t *testing.T) {
	first := baseRound(t)
	o := opts(fixedNow)
	o.Reuse = false

	r := play(sampleTree("/r"), first.ix, o)

	want := []string{"/r", "/r/a", "/r/a/a1", "/r/b", "/r/c"}
	if !slices.Equal(r.listed, want) {
		t.Errorf("listed %v, want every directory %v", r.listed, want)
	}
	if st := r.b.Stats(); st.ReusedDirs != 0 || st.ReusedEntries != 0 {
		t.Errorf("Stats = %+v, want nothing reused", st)
	}
	if !slices.Equal(dump(r.ix), dump(first.ix)) {
		t.Error("full rebuild of an unchanged tree differs from the old index")
	}
}

func TestBuilderFullModeStillKeepsOldSubtreeOnFailed(t *testing.T) {
	first := baseRound(t)
	o := opts(fixedNow)
	o.Reuse = false
	tree := sampleTree("/r")
	tree.fail["/r/b"] = true

	r := play(tree, first.ix, o)

	if !slices.Equal(r.failed, []string{"/r/b"}) {
		t.Fatalf("failed = %v, want [/r/b]", r.failed)
	}
	if !has(r.ix, "/r/b/b1.jpg") {
		t.Error("full mode dropped the subtree of a failed directory")
	}
	if isStable(t, r.ix, "/r/b") {
		t.Error("failed dir must not have flagStable")
	}
}

func TestBuilderResultKeepsShapeInvariantsAfterReuseAndFailed(t *testing.T) {
	first := baseRound(t)
	tree := sampleTree("/r")
	tree.touch("/r/c", oldMT.Add(time.Minute))
	tree.put("/r/c", fileEntry("c4.jpg", 4, oldMT))
	tree.put("/r", dirEntry("n", oldMT))
	tree.put("/r/n", fileEntry("n1.jpg", 1, oldMT))
	tree.fail["/r/a"] = true

	r := play(tree, first.ix, opts(fixedNow))

	assertShape(t, r.ix)
	if r.b.Stats().ReusedDirs == 0 || r.b.Stats().KeptDirs == 0 {
		t.Errorf("Stats = %+v, test setup must exercise both reuse and Failed", r.b.Stats())
	}
}

func TestBuilderResultSurvivesSnapshotRoundTrip(t *testing.T) {
	first := baseRound(t)
	tree := sampleTree("/r")
	tree.fail["/r/a"] = true
	tree.put("/r", dirEntry("n", oldMT))
	tree.put("/r/n", fileEntry("n1.jpg", 1, oldMT))
	r := play(tree, first.ix, opts(fixedNow))

	var buf bytes.Buffer
	if err := Write(&buf, r.ix, Meta{Complete: true}); err != nil {
		t.Fatal(err)
	}
	got, _, err := Read(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	assertSameIndex(t, r.ix, got)
	if !slices.Equal(got.flags, r.ix.flags) {
		t.Error("flags differ after the round trip")
	}
}

func TestBuilderListedIsAtomicWithRespectToClone(t *testing.T) {
	const dirs, files = 100, 1000
	b := NewBuilder(nil, opts(fixedNow))
	root, _ := b.AddRoot(defaultRoot.key, defaultRoot.path)
	rootEntries := make([]source.Entry, dirs)
	for i := range rootEntries {
		rootEntries[i] = dirEntry(fmt.Sprintf("d%03d", i), oldMT)
	}
	kids := b.Listed(root, NoPrev, rootEntries)
	fileEntries := make([]source.Entry, files)
	for i := range fileEntries {
		fileEntries[i] = fileEntry(fmt.Sprintf("f%04d.jpg", i), int64(i), oldMT)
	}

	var stop atomic.Bool
	var clones atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			c := b.Index().Clone()
			clones.Add(1)
			counts := make([]int, c.Len())
			for _, p := range c.parent {
				if p != NoParent {
					counts[p]++
				}
			}
			for _, k := range kids {
				if c.flags[k.ID]&flagStable != 0 && counts[k.ID] != files {
					t.Errorf("clone saw dir %d with flagStable and %d children, want %d", k.ID, counts[k.ID], files)
					return
				}
			}
		}
	}()
	for _, k := range kids {
		b.Listed(k.ID, k.Prev, fileEntries)
	}
	stop.Store(true)
	wg.Wait()

	if clones.Load() == 0 {
		t.Error("reader never cloned")
	}
	for _, k := range kids {
		if !isStableID(b.Index(), k.ID) {
			t.Fatalf("dir %d not stable at the end", k.ID)
		}
	}
}

func isStableID(ix *Index, id uint32) bool { return ix.flags[id]&flagStable != 0 }

func TestBuilderFailedIsAtomicWithRespectToClone(t *testing.T) {
	const subs, files = 50, 100
	tree := &fakeTree{dirs: map[string][]source.Entry{}, fail: map[string]bool{}}
	tree.put("/r", dirEntry("big", oldMT))
	for i := 0; i < subs; i++ {
		name := fmt.Sprintf("s%02d", i)
		tree.put("/r/big", dirEntry(name, oldMT))
		for j := 0; j < files; j++ {
			tree.put(path.Join("/r/big", name), fileEntry(fmt.Sprintf("f%03d.jpg", j), 1, oldMT))
		}
	}
	first := play(tree, nil, opts(fixedNow))

	for rep := 0; rep < 30; rep++ {
		b := NewBuilder(first.ix, opts(fixedNow))
		root, prev := b.AddRoot(defaultRoot.key, defaultRoot.path)
		kids := b.Listed(root, prev, slices.Clone(tree.dirs["/r"]))
		if len(kids) != 1 {
			t.Fatalf("Listed returned %d children, want [big]", len(kids))
		}
		big := kids[0]

		var stop atomic.Bool
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				c := b.Index().Clone()
				counts := make([]int, c.Len())
				for _, p := range c.parent {
					if p != NoParent {
						counts[p]++
					}
				}
				for id, p := range c.parent {
					if p == big.ID && c.flags[id]&flagStable != 0 && counts[id] != files {
						t.Errorf("clone saw dir %d with flagStable and %d children, want %d", id, counts[id], files)
						return
					}
				}
			}
		}()
		b.Failed(big.ID, big.Prev)
		stop.Store(true)
		wg.Wait()

		if got := b.Index().Len(); got != first.ix.Len() {
			t.Fatalf("Len = %d, want %d: Failed must keep the whole old subtree", got, first.ix.Len())
		}
	}
}
