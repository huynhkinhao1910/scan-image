package indexer

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/search"
)

// recentEnv is the photo tree with a complete index in which a/a1, a/a2, b
// and c hold something of the last 45 minutes (editPhotos: b lost b2) and
// a/a3 and b/b1 are an hour old. Read counters are zero.
func recentEnv(t *testing.T) (*env, Options) {
	t.Helper()
	e := newLocalEnv(t)
	e.seed(t)
	editPhotos(e.src, localBase)
	e.seed(t)
	opt := e.opts()
	opt.RecentAge = 45 * time.Minute
	return e, opt
}

func recentAndWait(t *testing.T, svc *Service) {
	t.Helper()
	svc.start(request{recent, true}, true)
	waitIdle(t, svc)
}

func TestRecentPassListsOnlyRecentFoldersAndSeesAnOverwrite(t *testing.T) {
	e, opt := recentEnv(t)
	svc := e.openWith(t, opt)
	gen := svc.State().Gen
	e.src.mkfile(filepath.Join(localBase, "a/a1/1.jpg"), 4242, oldAt)

	recentAndWait(t, svc)

	want := readsOnce(localBase, "", "a", "b", "a/a1", "a/a2", "c")
	if got := e.src.readCounts(); !reflect.DeepEqual(got, want) {
		t.Errorf("reads %v, want %v: a/a3 and b/b1 hold nothing recent", got, want)
	}
	ix := svc.Current()
	found := false
	for _, id := range ix.Search("1.jpg", search.Options{Limit: 100}).IDs {
		if it := ix.Item(id); it.Path == filepath.Join(localBase, "a/a1/1.jpg") {
			found = it.Size == 4242
		}
	}
	if !found || svc.State().Gen == gen {
		t.Errorf("the overwritten file is not in the index with its new size (gen %d -> %d)", gen, svc.State().Gen)
	}
	assertPaths(t, ix, e.expected())
}

func TestRecentPassLeavesTheSnapshotToTheNextUsualScan(t *testing.T) {
	e, opt := recentEnv(t)
	svc := e.openWith(t, opt)
	before := readSnapshotFile(t, e.st, e.sess.Source)
	e.src.addFile(filepath.Join(localBase, "a/a1/late.jpg"), 9, editAt)

	recentAndWait(t, svc)

	late := filepath.Join(localBase, "a/a1/late.jpg")
	if !contains(svc.Current(), late) {
		t.Fatal("the recent pass missed the new file")
	}
	if !before.untouched(readSnapshotFile(t, e.st, e.sess.Source)) {
		t.Error("the recent pass wrote the snapshot: a write of the whole index every few minutes")
	}
	scanAndWait(t, svc, Incremental)
	ix, _, err := e.st.Load(e.sess.Source)
	if err != nil || !contains(ix, late) {
		t.Errorf("the next usual scan did not save what the recent pass found (err %v)", err)
	}
}

func TestRecentPassKeepsWhatTheUsualScanReported(t *testing.T) {
	e, opt := recentEnv(t)
	e.src.setErr(filepath.Join(localBase, "b/b1"), errors.New("no such file or directory"))
	svc := e.openWith(t, opt)
	scanAndWait(t, svc, Full)
	usual := svc.State()
	if len(usual.FailedDirs) != 1 || usual.Progress.Errors != 1 {
		t.Fatalf("setup: failed dirs %v errors %d", usual.FailedDirs, usual.Progress.Errors)
	}
	e.clk.Advance(time.Minute)
	blk := e.src.block(filepath.Join(localBase, "a"))

	svc.start(request{recent, true}, true)
	blk.waitEntered(t)
	if mid := svc.State(); !mid.Scanning || mid.Progress.Errors != 1 {
		t.Errorf("during the pass: scanning %v errors %d, want the usual scan's count", mid.Scanning, mid.Progress.Errors)
	}
	blk.unblock()
	waitIdle(t, svc)

	st := svc.State()
	if len(st.FailedDirs) != 1 || st.Progress.Errors != 1 {
		t.Errorf("failed dirs %v errors %d: the recent pass does not visit b/b1, its silence is no news", st.FailedDirs, st.Progress.Errors)
	}
	if !st.CheckedAt.Equal(usual.CheckedAt) {
		t.Errorf("CheckedAt moved from %v to %v: only part of the tree was looked at", usual.CheckedAt, st.CheckedAt)
	}
	if !st.Complete {
		t.Error("Complete was lost")
	}
}

func TestRecentPassOnAnIndexThatIsNotCompleteIsAUsualScan(t *testing.T) {
	e := newLocalEnv(t)
	opt := e.opts()
	opt.RecentAge = time.Minute
	svc := e.openWith(t, opt)

	recentAndWait(t, svc)

	assertPaths(t, svc.Current(), e.expected())
	st := svc.State()
	if !st.Complete || st.CheckedAt.IsZero() {
		t.Errorf("state %+v, want a complete, checked index", st)
	}
}

// Nothing in the seeded tree is recent, so a recent pass lists the root alone
// and a usual scan lists a and b as well.
func TestRecentPassesRunBetweenUsualScans(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	opt := e.opts()
	opt.RecentAge, opt.Interval, opt.RecentInterval = 45*time.Minute, 2*time.Second, 25*time.Millisecond
	_ = e.openWith(t, opt)

	eventually(t, "two recent passes", func() bool { return e.src.readsOf(localBase) >= 2 })
	if n := e.src.readsOf(filepath.Join(localBase, "b")); n != 0 {
		t.Fatalf("b listed %d times before the usual scan was due", n)
	}
	eventually(t, "the usual scan", func() bool { return e.src.readsOf(filepath.Join(localBase, "b")) > 0 })
}

// A usual scan still running when the next one is due stands for it: the
// share is slow, and a second scan right behind the first would double the
// load.
func TestUsualScanStillRunningWhenTheNextIsDueIsNotFollowedByAnother(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	a := filepath.Join(localBase, "a")
	blk := e.src.block(a)
	opt := e.opts()
	opt.RecentAge, opt.Interval, opt.RecentInterval = 45*time.Minute, time.Second, 30*time.Millisecond
	svc := e.openWith(t, opt)
	blk.waitEntered(t)

	time.Sleep(1200 * time.Millisecond)
	blk.unblock()
	waitIdle(t, svc)
	time.Sleep(250 * time.Millisecond)

	if n := e.src.readsOf(a); n != 1 {
		t.Errorf("a listed %d times: a second usual scan started right after the long one", n)
	}
}
