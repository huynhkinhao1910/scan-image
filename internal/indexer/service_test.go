package indexer

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/search"
)

// env is a local-folder source with the standard photo tree.
type env struct {
	st   *Store
	src  *fakeSource
	clk  *fakeClock
	sess Session
}

func newLocalEnv(t *testing.T) *env {
	t.Helper()
	st, _ := newTestStore(t)
	src := newFakeSource(t)
	addPhotos(src, localBase)
	return &env{st: st, src: src, clk: newClock(), sess: Session{Mode: ModeLocal, Source: localBase}}
}

// opts: no periodic scan, no checkpoints, verify slot effectively off so
// read counts are exact.
func (e *env) opts() Options {
	return Options{
		Source:      e.src,
		Workers:     4,
		Interval:    -1,
		Checkpoint:  time.Hour,
		CloseWait:   2 * time.Second,
		Now:         e.clk.Now,
		VerifySlots: 1 << 30,
	}
}

func (e *env) open(t *testing.T) *Service { return e.openWith(t, e.opts()) }

func (e *env) openWith(t *testing.T, opt Options) *Service {
	t.Helper()
	svc, err := Open(e.st, e.sess, opt)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

// seed runs a complete first scan, closes the service and clears counters.
func (e *env) seed(t *testing.T) {
	t.Helper()
	svc, err := Open(e.st, e.sess, e.opts())
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	scanAndWait(t, svc, Incremental)
	if !svc.State().Complete {
		t.Fatal("seed scan did not complete")
	}
	svc.Close()
	e.src.resetReads()
}

func (e *env) expected() []string { return e.src.paths(localBase) }

func contains(ix *search.Index, path string) bool { return slices.Contains(allPaths(ix), path) }

// --- 1. Open ---------------------------------------------------------------

func TestOpenWithSnapshotIsSearchableAtOnceWithZeroReadDir(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)

	svc := e.open(t)

	assertPaths(t, svc.Current(), e.expected())
	if n := e.src.total(); n != 0 {
		t.Errorf("Open made %d ReadDir calls, want 0", n)
	}
	st := svc.State()
	if st.Scanning || !st.Complete {
		t.Errorf("state after Open = %+v, want idle and Complete", st)
	}
}

func TestOpenWithoutSnapshotGivesEmptyIndexNotError(t *testing.T) {
	e := newLocalEnv(t)

	svc, err := Open(e.st, e.sess, e.opts())
	if err != nil {
		t.Fatalf("Open without snapshot: %v", err)
	}
	t.Cleanup(svc.Close)

	if svc.Current() == nil || svc.Current().Len() != 0 {
		t.Errorf("Current() = %v, want empty non-nil index", svc.Current())
	}
	if n := e.src.total(); n != 0 {
		t.Errorf("Open made %d ReadDir calls, want 0", n)
	}
	if svc.State().Complete {
		t.Error("empty state reports Complete")
	}
}

func TestOpenDiscardsCorruptSnapshotAndNextScanStartsFromScratch(t *testing.T) {
	e := newLocalEnv(t)
	if err := os.WriteFile(e.st.Path(e.sess.Source), []byte("SIDX not really a snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}

	svc, err := Open(e.st, e.sess, e.opts())
	if err != nil {
		t.Fatalf("Open with corrupt snapshot: %v", err)
	}
	t.Cleanup(svc.Close)
	if svc.Current().Len() != 0 {
		t.Fatalf("corrupt snapshot was used: %d entries", svc.Current().Len())
	}

	scanAndWait(t, svc, Incremental)

	assertPaths(t, svc.Current(), e.expected())
	if got := e.src.readCounts(); !reflect.DeepEqual(got, readsOnce(localBase, allPhotoDirs...)) {
		t.Errorf("reads = %v, want every directory once", got)
	}
	ix, m, err := e.st.Load(e.sess.Source)
	if err != nil || !m.Complete {
		t.Fatalf("snapshot after scan: meta=%+v err=%v", m, err)
	}
	assertPaths(t, ix, e.expected())
}

// --- 2. Swap / no swap -----------------------------------------------------

func TestScanWithNothingChangedKeepsIndexGenAndSnapshotFileAndMovesCheckedAt(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	svc := e.open(t)
	before, gen := svc.Current(), svc.State().Gen
	file := readSnapshotFile(t, e.st, e.sess.Source)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	if svc.Current() != before {
		t.Error("index was swapped although nothing changed")
	}
	st := svc.State()
	if st.Gen != gen {
		t.Errorf("Gen %d -> %d, want unchanged", gen, st.Gen)
	}
	if !st.CheckedAt.Equal(e.clk.Now()) {
		t.Errorf("CheckedAt = %v, want %v", st.CheckedAt, e.clk.Now())
	}
	if !file.untouched(readSnapshotFile(t, e.st, e.sess.Source)) {
		t.Error("snapshot file was rewritten although nothing changed")
	}
}

func TestScanWithChangesSwapsIndexBumpsGenAndSavesSnapshot(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	svc := e.open(t)
	before, gen := svc.Current(), svc.State().Gen
	editPhotos(e.src, localBase)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	if svc.Current() == before {
		t.Error("index was not swapped after the tree changed")
	}
	if got := svc.State().Gen; got <= gen {
		t.Errorf("Gen %d -> %d, want increase", gen, got)
	}
	assertPaths(t, svc.Current(), e.expected())
	ix, m, err := e.st.Load(e.sess.Source)
	if err != nil || !m.Complete {
		t.Fatalf("saved snapshot: meta=%+v err=%v", m, err)
	}
	assertPaths(t, ix, e.expected())
}

// --- 3. Which index is searched during a scan ------------------------------

func TestFirstScanExposesGrowingIndexWhileScanning(t *testing.T) {
	e := newLocalEnv(t)
	blk := e.src.block(filepath.Join(localBase, "a"))
	svc := e.open(t)

	svc.Scan(Incremental)
	blk.waitEntered(t)

	if !svc.State().Scanning {
		t.Error("State().Scanning = false while a scan is blocked")
	}
	mid := svc.Current()
	if !contains(mid, filepath.Join(localBase, "top.jpg")) {
		t.Error("entries already listed are not searchable during the first scan")
	}
	if contains(mid, filepath.Join(localBase, "a/a1/1.jpg")) {
		t.Error("entries of the blocked directory are searchable")
	}

	blk.unblock()
	waitIdle(t, svc)
	assertPaths(t, svc.Current(), e.expected())
	st := svc.State()
	if !st.Complete || st.Gen < 1 {
		t.Errorf("state after first scan = %+v, want Complete and Gen >= 1", st)
	}
}

func TestRescanKeepsOldIndexCurrentUntilScanEnds(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	oldPaths := e.expected()
	svc := e.open(t)
	old := svc.Current()
	editPhotos(e.src, localBase)
	blk := e.src.block(filepath.Join(localBase, "a"))

	svc.Scan(Incremental)
	blk.waitEntered(t)

	if svc.Current() != old {
		t.Error("Current() changed before the scan ended")
	}
	assertPaths(t, svc.Current(), oldPaths)

	blk.unblock()
	waitIdle(t, svc)
	if svc.Current() == old {
		t.Error("Current() was not swapped when the scan ended")
	}
	assertPaths(t, svc.Current(), e.expected())
}

// --- 4. Read counts --------------------------------------------------------

func TestIncrementalScanWithNothingChangedListsOnlyNonLeafDirs(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	svc := e.open(t)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	if got := e.src.readCounts(); !reflect.DeepEqual(got, readsOnce(localBase, nonLeafPhotoDirs...)) {
		t.Errorf("reads = %v, want only the %d non-leaf directories once each", got, photoNonLeafDirs)
	}
	if st := svc.State().Stats; st.ReusedDirs == 0 {
		t.Errorf("Stats.ReusedDirs = 0, want the skipped leaves counted (%+v)", st)
	}
}

func TestIncrementalScanAfterEditsListsNonLeafDirsPlusChangedAndNewLeaves(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	svc := e.open(t)
	editPhotos(e.src, localBase)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	if got := e.src.readCounts(); !reflect.DeepEqual(got, readsOnce(localBase, editedReads...)) {
		t.Errorf("reads = %v, want base,a,b + a1,a2 (changed) + c (new)", got)
	}
	assertPaths(t, svc.Current(), e.expected())
	if len(e.expected()) == 0 || !contains(svc.Current(), filepath.Join(localBase, "c/c1.jpg")) ||
		!contains(svc.Current(), filepath.Join(localBase, "a/a2/two-renamed.jpg")) ||
		contains(svc.Current(), filepath.Join(localBase, "a/a2/two.jpg")) ||
		contains(svc.Current(), filepath.Join(localBase, "b/b2/b2.jpg")) {
		t.Error("added, renamed or removed entries are not reflected in search")
	}
}

func TestFullScanListsEveryDirectory(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	svc := e.open(t)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Full)

	if got := e.src.readCounts(); !reflect.DeepEqual(got, readsOnce(localBase, allPhotoDirs...)) {
		t.Errorf("reads = %v, want all %d directories once", got, photoDirs)
	}
}

func TestVerifySlotRelistsUnchangedLeaves(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	opt := e.opts()
	opt.VerifySlots = 1 // every leaf hashes into the one slot, which is always due
	svc := e.openWith(t, opt)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	if got := e.src.readCounts(); !reflect.DeepEqual(got, readsOnce(localBase, allPhotoDirs...)) {
		t.Errorf("reads = %v, want unchanged leaves in the verify slot listed too", got)
	}
}

// --- 5. Source goes away, save fails -----------------------------------------

func TestSourceGoneKeepsIndexAndSnapshotIntact(t *testing.T) {
	cases := map[string]func(*fakeSource){
		"every directory fails": func(s *fakeSource) { s.setFailAll(os.ErrDeadlineExceeded) },
		"root unreadable":       func(s *fakeSource) { s.setErr(localBase, os.ErrPermission) },
	}
	for name, breakSource := range cases {
		t.Run(name, func(t *testing.T) {
			e := newLocalEnv(t)
			e.seed(t)
			svc := e.open(t)
			file := readSnapshotFile(t, e.st, e.sess.Source)
			breakSource(e.src)
			e.clk.Advance(time.Minute)

			scanAndWait(t, svc, Full)

			assertPaths(t, svc.Current(), e.expected())
			after := readSnapshotFile(t, e.st, e.sess.Source)
			if string(after.data) != string(file.data) {
				t.Error("snapshot content changed although the source was unreadable")
			}
			ix, _, err := e.st.Load(e.sess.Source)
			if err != nil {
				t.Fatalf("snapshot unreadable after scan: %v", err)
			}
			assertPaths(t, ix, e.expected())
		})
	}
}

func TestFailedSaveKeepsPreviousFileAndStillSwapsInMemory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	e := newLocalEnv(t)
	e.seed(t)
	oldPaths := e.expected()
	svc := e.open(t)
	gen := svc.State().Gen
	file := readSnapshotFile(t, e.st, e.sess.Source)
	editPhotos(e.src, localBase)
	dir := filepath.Dir(e.st.Path(e.sess.Source))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	assertPaths(t, svc.Current(), e.expected())
	if svc.State().Gen <= gen {
		t.Error("Gen did not increase although the index was swapped")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !file.untouched(readSnapshotFile(t, e.st, e.sess.Source)) {
		t.Error("previous snapshot file was modified by a failed save")
	}
	ix, _, err := e.st.Load(e.sess.Source)
	if err != nil {
		t.Fatalf("previous snapshot no longer loads: %v", err)
	}
	assertPaths(t, ix, oldPaths)
	assertNoTempFiles(t, dir)
}

// --- 6. Overlap, Stop, Close ---------------------------------------------------

func TestScanWhileScanningDoesNotStartSecondScan(t *testing.T) {
	e := newLocalEnv(t)
	blk := e.src.block(filepath.Join(localBase, "a"))
	svc := e.open(t)
	svc.Scan(Incremental)
	blk.waitEntered(t)

	svc.Scan(Incremental)
	svc.Scan(Full)
	time.Sleep(100 * time.Millisecond)

	if n := e.src.readsOf(localBase); n != 1 {
		t.Errorf("root listed %d times while a scan was running, want 1", n)
	}
	blk.unblock()
	waitIdle(t, svc)
	if n := e.src.readsOf(localBase); n != 2 {
		t.Errorf("root listed %d times in total, want 2: the overlapping calls collapse into one follow-up scan", n)
	}
	assertPaths(t, svc.Current(), e.expected())
}

func TestStopCancelsBlockedFirstScanQuicklyAndKeepsCompletedPart(t *testing.T) {
	e := newLocalEnv(t)
	blk := e.src.block(filepath.Join(localBase, "a"))
	svc := e.open(t)
	svc.Scan(Incremental)
	blk.waitEntered(t)

	start := time.Now()
	svc.Stop()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Stop took %v with a blocked ReadDir, want it to return quickly", d)
	}
	waitIdle(t, svc)

	st := svc.State()
	if st.Complete {
		t.Error("cancelled first scan reports Complete")
	}
	top := filepath.Join(localBase, "top.jpg")
	if !contains(svc.Current(), top) {
		t.Error("completed part of the scan was dropped by Stop")
	}
	ix, m, err := e.st.Load(e.sess.Source)
	if err != nil {
		t.Fatalf("cancelled scan left no snapshot: %v", err)
	}
	if m.Complete || !contains(ix, top) {
		t.Errorf("snapshot after Stop: Complete=%v has top.jpg=%v, want incomplete partial result", m.Complete, contains(ix, top))
	}

	blk.unblock()
	scanAndWait(t, svc, Incremental)
	assertPaths(t, svc.Current(), e.expected())
	if !svc.State().Complete {
		t.Error("scan after Stop did not complete")
	}
}

func TestStopDuringRescanLeavesIndexNoWorseThanBefore(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	before := e.expected()
	svc := e.open(t)
	editPhotos(e.src, localBase)
	// Entries the edit deleted may legitimately disappear if their parent was
	// listed before the cancel, so only what still exists must survive.
	oldPaths := slices.DeleteFunc(slices.Clone(before), func(p string) bool {
		return !slices.Contains(e.expected(), p)
	})
	blk := e.src.block(filepath.Join(localBase, "a"))
	svc.Scan(Incremental)
	blk.waitEntered(t)

	svc.Stop()
	waitIdle(t, svc)

	got := allPaths(svc.Current())
	for _, p := range oldPaths {
		if !slices.Contains(got, p) {
			t.Errorf("%s was lost by a cancelled rescan", p)
		}
	}
	ix, _, err := e.st.Load(e.sess.Source)
	if err != nil {
		t.Fatalf("snapshot unreadable after Stop: %v", err)
	}
	saved := allPaths(ix)
	for _, p := range oldPaths {
		if !slices.Contains(saved, p) {
			t.Errorf("%s missing from the snapshot after a cancelled rescan", p)
		}
	}
}

func TestCloseIsBoundedAndNothingIsWrittenAfterIt(t *testing.T) {
	e := newLocalEnv(t)
	blk := e.src.block(filepath.Join(localBase, "a"))
	opt := e.opts()
	opt.Checkpoint = 20 * time.Millisecond
	opt.CloseWait = 300 * time.Millisecond
	svc := e.openWith(t, opt)
	svc.Scan(Incremental)
	blk.waitEntered(t)
	waitSnapshot(t, e.st, e.sess.Source, "first checkpoint", func(*search.Index, search.Meta) bool { return true })

	start := time.Now()
	svc.Close()
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Close took %v, want it bounded by CloseWait", d)
	}
	if svc.State().Scanning {
		t.Error("State().Scanning after Close")
	}
	file := readSnapshotFile(t, e.st, e.sess.Source)
	reads := e.src.total()

	blk.unblock()
	svc.Scan(Incremental)
	time.Sleep(150 * time.Millisecond)

	if !file.untouched(readSnapshotFile(t, e.st, e.sess.Source)) {
		t.Error("snapshot was written after Close")
	}
	if n := e.src.total(); n != reads {
		t.Errorf("Scan after Close started a scan: %d -> %d reads", reads, n)
	}
	svc.Close() // idempotent
}

// --- 7. Checkpoint -------------------------------------------------------------

func TestFirstScanSavesIncompleteCheckpointThenCompleteSnapshot(t *testing.T) {
	e := newLocalEnv(t)
	blk := e.src.block(filepath.Join(localBase, "a"))
	opt := e.opts()
	opt.Checkpoint = 20 * time.Millisecond
	svc := e.openWith(t, opt)
	top := filepath.Join(localBase, "top.jpg")

	svc.Scan(Incremental)
	blk.waitEntered(t)
	waitSnapshot(t, e.st, e.sess.Source, "incomplete checkpoint with top.jpg", func(ix *search.Index, m search.Meta) bool {
		return !m.Complete && contains(ix, top)
	})

	blk.unblock()
	waitIdle(t, svc)
	ix, m, err := e.st.Load(e.sess.Source)
	if err != nil || !m.Complete {
		t.Fatalf("final snapshot: meta=%+v err=%v", m, err)
	}
	assertPaths(t, ix, e.expected())
}

func TestNoCheckpointsDuringRescan(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	editPhotos(e.src, localBase)
	opt := e.opts()
	opt.Checkpoint = 20 * time.Millisecond
	svc := e.openWith(t, opt)
	blk := e.src.block(filepath.Join(localBase, "a"))
	file := readSnapshotFile(t, e.st, e.sess.Source)

	svc.Scan(Incremental)
	blk.waitEntered(t)
	time.Sleep(200 * time.Millisecond)

	if !file.untouched(readSnapshotFile(t, e.st, e.sess.Source)) {
		t.Error("checkpoint written during a rescan; only the first scan checkpoints")
	}
	blk.unblock()
	waitIdle(t, svc)
}

func TestReopenAfterInterruptedFirstScanContinuesFromCheckpoint(t *testing.T) {
	e := newLocalEnv(t)
	blk := e.src.block(filepath.Join(localBase, "b/b1"))
	opt := e.opts()
	opt.Checkpoint = 20 * time.Millisecond
	svc := e.openWith(t, opt)
	svc.Scan(Incremental)
	blk.waitEntered(t)
	a1 := filepath.Join(localBase, "a/a1/1.jpg")
	waitSnapshot(t, e.st, e.sess.Source, "checkpoint with a/a1", func(ix *search.Index, m search.Meta) bool {
		return !m.Complete && contains(ix, a1)
	})
	svc.Close()
	blk.unblock()
	e.src.resetReads()

	again := e.open(t)
	if !contains(again.Current(), a1) {
		t.Error("checkpoint was not loaded on reopen")
	}
	scanAndWait(t, again, Incremental)

	want := readsOnce(localBase, "", "a", "b", "b/b1")
	if got := e.src.readCounts(); !reflect.DeepEqual(got, want) {
		t.Errorf("reads = %v, want %v: stable leaves of the checkpoint must be reused", got, want)
	}
	assertPaths(t, again.Current(), e.expected())
	if !again.State().Complete {
		t.Error("resumed scan did not complete")
	}
}

// --- 8. Schedule ------------------------------------------------------------------

func TestPeriodicScanStartsOnItsOwnEveryInterval(t *testing.T) {
	e := newLocalEnv(t)
	opt := e.opts()
	opt.Interval = 30 * time.Millisecond
	svc := e.openWith(t, opt)

	eventually(t, "periodic scan to list the root", func() bool { return e.src.readsOf(localBase) > 0 })
	eventually(t, "periodic scan to finish", func() bool {
		return !svc.State().Scanning && len(allPaths(svc.Current())) == len(e.expected())
	})
}

func TestNegativeIntervalDisablesPeriodicScan(t *testing.T) {
	e := newLocalEnv(t)
	_ = e.open(t)

	time.Sleep(150 * time.Millisecond)

	if n := e.src.total(); n != 0 {
		t.Errorf("scan ran with periodic scanning off: %d reads", n)
	}
}

func TestKickStartsIncrementalScanOnlyAfterDelay(t *testing.T) {
	e := newLocalEnv(t)
	svc := e.open(t)

	svc.Kick(400 * time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if n := e.src.total(); n != 0 {
		t.Fatalf("Kick scanned before its delay: %d reads", n)
	}

	eventually(t, "kicked scan to start", func() bool { return e.src.total() > 0 })
	waitIdle(t, svc)
	assertPaths(t, svc.Current(), e.expected())
}

func TestKickAndPeriodicScansAreNoOpsWhileScanning(t *testing.T) {
	e := newLocalEnv(t)
	blk := e.src.block(filepath.Join(localBase, "a"))
	opt := e.opts()
	opt.Interval = 20 * time.Millisecond
	svc := e.openWith(t, opt)
	svc.Scan(Incremental)
	blk.waitEntered(t)

	svc.Kick(0)
	svc.Kick(0)
	time.Sleep(150 * time.Millisecond)

	if n := e.src.readsOf(localBase); n != 1 {
		t.Errorf("root listed %d times while a scan was blocked, want 1", n)
	}
	blk.unblock()
	waitIdle(t, svc)
}

// --- 9. State and Notify -------------------------------------------------------------

func TestStateReportsRootsCompleteGenCheckedAtAndProgress(t *testing.T) {
	e := newLocalEnv(t)
	svc := e.open(t)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	st := svc.State()
	if st.Scanning || st.Offline || !st.Complete {
		t.Errorf("flags = scanning:%v offline:%v complete:%v", st.Scanning, st.Offline, st.Complete)
	}
	if st.Gen < 1 {
		t.Errorf("Gen = %d, want >= 1 after the first scan", st.Gen)
	}
	if !st.CheckedAt.Equal(e.clk.Now()) {
		t.Errorf("CheckedAt = %v, want %v", st.CheckedAt, e.clk.Now())
	}
	if !st.Progress.Done || st.Progress.Dirs != photoDirs-1 {
		t.Errorf("Progress = %+v, want Done with %d directories listed", st.Progress, photoDirs-1)
	}
	if want := []RootInfo{{Name: "Pictures", Path: localBase}}; !reflect.DeepEqual(st.Roots, want) {
		t.Errorf("Roots = %+v, want %+v", st.Roots, want)
	}
}

func TestNotifyFiresOnStartSwapAndEndWithoutHoldingLocks(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	var (
		holder atomic.Pointer[Service]
		mu     sync.Mutex
		seen   []bool
	)
	opt := e.opts()
	opt.Notify = func() {
		s := holder.Load()
		if s == nil {
			return
		}
		scanning := s.State().Scanning // must not deadlock inside Notify
		mu.Lock()
		seen = append(seen, scanning)
		mu.Unlock()
	}
	svc := e.openWith(t, opt)
	holder.Store(svc)
	editPhotos(e.src, localBase)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	eventually(t, "start, swap and end notifications", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) >= 3 && slices.Contains(seen, true) && !seen[len(seen)-1]
	})
}

// --- 10. Local prepare ------------------------------------------------------------------

func TestLocalSourceUsesAbsoluteFolderAsRootKeyAndNeverChecksReachability(t *testing.T) {
	e := newLocalEnv(t)
	reach := newReach()
	opt := e.opts()
	opt.Reachable = reach.check
	svc := e.openWith(t, opt)

	scanAndWait(t, svc, Incremental)

	roots := svc.Current().RootList()
	if len(roots) != 1 || roots[0].Key != localBase || roots[0].Path != localBase {
		t.Errorf("roots = %+v, want one root keyed and mounted at %s", roots, localBase)
	}
	if n := reach.calls.Load(); n != 0 {
		t.Errorf("Reachable called %d times for a local source", n)
	}
}

func TestLocalRootThatCannotBeListedKeepsOldDataAndIsNotAnError(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	svc := e.open(t)
	e.src.setErr(localBase, &os.PathError{Op: "readdir", Path: localBase, Err: os.ErrPermission})
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	assertPaths(t, svc.Current(), e.expected())
	if svc.State().Scanning {
		t.Error("still scanning after an unreadable root")
	}
}
