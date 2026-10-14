package search

import (
	"slices"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

var lastWeekMT = fixedNow.Add(-10 * 24 * time.Hour)

// recentTree builds
//
//	/r: old/ new/
//	/r/old: o1/     /r/old/o1: o.jpg
//	/r/new: n1/     /r/new/n1: n.jpg
//
// where everything was last modified ten days ago except n.jpg, an hour ago.
func recentTree() *fakeTree {
	t := &fakeTree{dirs: map[string][]source.Entry{}, fail: map[string]bool{}}
	t.put("/r", dirEntry("old", lastWeekMT), dirEntry("new", lastWeekMT))
	t.put("/r/old", dirEntry("o1", lastWeekMT))
	t.put("/r/old/o1", fileEntry("o.jpg", 100, lastWeekMT))
	t.put("/r/new", dirEntry("n1", lastWeekMT))
	t.put("/r/new/n1", fileEntry("n.jpg", 200, oldMT))
	return t
}

func recentOpts() BuildOptions {
	o := opts(fixedNow)
	o.Recent = 3 * 24 * time.Hour
	return o
}

func TestRecentPassListsOnlyBranchesChangedLately(t *testing.T) {
	first := play(recentTree(), nil, opts(fixedNow))

	r := play(recentTree(), first.ix, recentOpts())

	want := []string{"/r", "/r/new", "/r/new/n1"}
	if !slices.Equal(r.listed, want) {
		t.Errorf("listed %v, want %v: nothing under /r/old changed for ten days", r.listed, want)
	}
	if !slices.Equal(dump(r.ix), dump(first.ix)) {
		t.Errorf("build differs from old:\n got %v\nwant %v", dump(r.ix), dump(first.ix))
	}
	if r.b.Changed() {
		t.Error("Changed() = true for an unchanged tree")
	}
	assertShape(t, r.ix)
}

func TestRecentPassSeesAFileOverwrittenInPlace(t *testing.T) {
	tree := recentTree()
	first := play(tree, nil, opts(fixedNow))
	tree.edit("/r/new/n1", "n.jpg", func(e *source.Entry) { e.Size = 999 })

	usual := play(tree, first.ix, opts(fixedNow))
	r := play(tree, first.ix, recentOpts())

	if usual.wasListed("/r/new/n1") {
		t.Fatal("the usual pass listed the unchanged leaf: this test proves nothing")
	}
	if !r.wasListed("/r/new/n1") || !r.b.Changed() {
		t.Errorf("listed %v changed %v: the folder holds a file of an hour ago, its mtime says nothing about an overwrite", r.listed, r.b.Changed())
	}
}

func TestRecentPassLeavesAnUnreadableOldFolderToTheUsualPass(t *testing.T) {
	tree := recentTree()
	tree.fail["/r/old/o1"] = true
	first := play(tree, nil, opts(fixedNow))

	r := play(tree, first.ix, recentOpts())

	if r.wasListed("/r/old") || r.wasListed("/r/old/o1") {
		t.Errorf("listed %v: a folder that failed is retried by the usual pass, not every few minutes", r.listed)
	}
	if isStable(t, r.ix, "/r/old/o1") {
		t.Error("the folder that was never read became stable: the usual pass would stop retrying it")
	}
}

func TestRecentPassListsAnOldFolderWhoseMTimeChanged(t *testing.T) {
	tree := recentTree()
	first := play(tree, nil, opts(fixedNow))
	tree.put("/r/old", dirEntry("o2", oldMT))
	tree.put("/r/old/o2", fileEntry("late.jpg", 5, oldMT))
	tree.touch("/r/old", oldMT)

	r := play(tree, first.ix, recentOpts())

	if !has(r.ix, "/r/old/o2/late.jpg") || !has(r.ix, "/r/old/o1/o.jpg") {
		t.Errorf("index: %v", paths(r.ix))
	}
	if r.wasListed("/r/old/o1") {
		t.Errorf("listed %v: /r/old/o1 did not change", r.listed)
	}
}
