package search

import (
	"slices"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

var ancientMT = fixedNow.Add(-60 * 24 * time.Hour)

// coldTree builds
//
//	/r: a/ hot/
//	/r/a: a1/ a.txt     /r/a/a1: a2/ x.jpg     /r/a/a1/a2: y.jpg
//	/r/hot: h1/         /r/hot/h1: h.jpg
//
// where everything under /r/a was last modified two months ago and everything
// under /r/hot an hour ago.
func coldTree() *fakeTree {
	t := &fakeTree{dirs: map[string][]source.Entry{}, fail: map[string]bool{}}
	t.put("/r", dirEntry("a", ancientMT), dirEntry("hot", oldMT))
	t.put("/r/a", dirEntry("a1", ancientMT), fileEntry("a.txt", 20, ancientMT))
	t.put("/r/a/a1", dirEntry("a2", ancientMT), fileEntry("x.jpg", 100, ancientMT))
	t.put("/r/a/a1/a2", fileEntry("y.jpg", 200, ancientMT))
	t.put("/r/hot", dirEntry("h1", oldMT))
	t.put("/r/hot/h1", fileEntry("h.jpg", 300, oldMT))
	return t
}

func TestBuilderSkipsColdBranchWithoutListingIt(t *testing.T) {
	first := play(coldTree(), nil, opts(fixedNow))

	second := play(coldTree(), first.ix, opts(fixedNow))
	third := play(coldTree(), second.ix, opts(fixedNow))

	want := []string{"/r", "/r/hot"}
	if !slices.Equal(second.listed, want) {
		t.Errorf("listed %v, want %v: nothing under /r/a changed for two months", second.listed, want)
	}
	if !slices.Equal(third.listed, want) {
		t.Errorf("third round listed %v, want %v", third.listed, want)
	}
	if !slices.Equal(dump(second.ix), dump(first.ix)) {
		t.Errorf("reused build differs from old:\n got %v\nwant %v", dump(second.ix), dump(first.ix))
	}
	if got := second.b.Stats().ReusedDirs; got != 4 {
		t.Errorf("ReusedDirs = %d, want 4 (a, a1, a2, h1)", got)
	}
	if second.b.Changed() {
		t.Error("Changed() = true for an unchanged tree")
	}
	assertShape(t, second.ix)
}

func TestBuilderListsBranchHoldingARecentFile(t *testing.T) {
	tree := coldTree()
	tree.edit("/r/a/a1/a2", "y.jpg", func(e *source.Entry) { e.ModTime = oldMT })
	first := play(tree, nil, opts(fixedNow))

	r := play(tree, first.ix, opts(fixedNow))

	for _, p := range []string{"/r/a", "/r/a/a1"} {
		if !r.wasListed(p) {
			t.Errorf("listed %v, want %s listed: a file below it changed an hour ago", r.listed, p)
		}
	}
}

func TestBuilderListsBranchHoldingADirThatWasNotListedLastTime(t *testing.T) {
	tree := coldTree()
	tree.fail["/r/a/a1/a2"] = true
	first := play(tree, nil, opts(fixedNow))
	tree.fail = map[string]bool{}

	r := play(tree, first.ix, opts(fixedNow))

	if !r.wasListed("/r/a/a1/a2") {
		t.Errorf("listed %v, want /r/a/a1/a2 listed: it failed in the previous round", r.listed)
	}
	if !has(r.ix, "/r/a/a1/a2/y.jpg") {
		t.Error("y.jpg missing after the directory became readable")
	}
}

func TestBuilderListsColdDirWhoseMTimeChangedButSkipsColdBranchesInside(t *testing.T) {
	tree := coldTree()
	first := play(tree, nil, opts(fixedNow))
	tree.put("/r/a", dirEntry("new", oldMT))
	tree.put("/r/a/new", fileEntry("n.jpg", 5, oldMT))
	tree.touch("/r/a", oldMT)

	r := play(tree, first.ix, opts(fixedNow))

	want := []string{"/r", "/r/a", "/r/a/new", "/r/hot"}
	if !slices.Equal(r.listed, want) {
		t.Errorf("listed %v, want %v", r.listed, want)
	}
	if !has(r.ix, "/r/a/new/n.jpg") || !has(r.ix, "/r/a/a1/a2/y.jpg") {
		t.Errorf("index lost entries: %v", paths(r.ix))
	}
}

func TestBuilderRelistsWholeColdBranchWhenItsVerifySlotIsDue(t *testing.T) {
	first := play(coldTree(), nil, opts(fixedNow))
	changed := coldTree()
	changed.put("/r/a/a1/a2", fileEntry("late.jpg", 7, ancientMT))
	const slots = 64
	due := 0
	for slot := uint32(0); slot < slots; slot++ {
		o := opts(fixedNow)
		o.VerifySlots, o.VerifySlot = slots, slot
		r := play(changed, first.ix, o)
		if !r.wasListed("/r/a") {
			if has(r.ix, "/r/a/a1/a2/late.jpg") {
				t.Errorf("slot %d: late.jpg indexed although /r/a was not listed", slot)
			}
			continue
		}
		due++
		for _, p := range []string{"/r/a/a1", "/r/a/a1/a2"} {
			if !r.wasListed(p) {
				t.Errorf("slot %d: listed %v, want %s listed with its branch", slot, r.listed, p)
			}
		}
		if !has(r.ix, "/r/a/a1/a2/late.jpg") {
			t.Errorf("slot %d: verified branch missed late.jpg", slot)
		}
	}
	if due != 1 {
		t.Errorf("/r/a relisted in %d of %d slots, want exactly 1", due, slots)
	}
}

func TestBuilderFullModeListsColdBranches(t *testing.T) {
	first := play(coldTree(), nil, opts(fixedNow))
	o := opts(fixedNow)
	o.Reuse = false

	r := play(coldTree(), first.ix, o)

	want := []string{"/r", "/r/a", "/r/a/a1", "/r/a/a1/a2", "/r/hot", "/r/hot/h1"}
	if !slices.Equal(r.listed, want) {
		t.Errorf("listed %v, want every directory %v", r.listed, want)
	}
}
