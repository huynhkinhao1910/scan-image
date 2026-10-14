package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/netshare"
	"github.com/isuccesscorp/scan-image/internal/scanner"
)

// --- A. honest "checked" state ------------------------------------------------

func TestUnmountedShareWithOldDataDoesNotMoveCheckedAtAndIsNamed(t *testing.T) {
	e := newSMBEnv(t, "Photos", "Archive")
	e.seed(t)
	e.net.unmount("Archive")
	e.net.failMount["Archive"] = true
	svc := e.open(t)
	before := svc.State().CheckedAt
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	st := svc.State()
	if !st.CheckedAt.Equal(before) {
		t.Errorf("CheckedAt moved to %v although a share was not examined", st.CheckedAt)
	}
	if !errors.Is(st.ScanErr, ErrRootNotChecked) {
		t.Fatalf("ScanErr = %v, want ErrRootNotChecked", st.ScanErr)
	}
	if !strings.Contains(st.ScanErr.Error(), "Archive") {
		t.Errorf("ScanErr = %q does not name the share", st.ScanErr)
	}
}

func TestUnmountedShareWithoutOldDataDoesNotMoveCheckedAt(t *testing.T) {
	e := newSMBEnv(t, "Photos", "Archive")
	e.net.unmount("Archive")
	e.net.failMount["Archive"] = true
	svc := e.open(t)

	scanAndWait(t, svc, Incremental)

	st := svc.State()
	if !st.CheckedAt.IsZero() {
		t.Errorf("CheckedAt = %v, want zero", st.CheckedAt)
	}
	if !errors.Is(st.ScanErr, ErrRootNotChecked) || !strings.Contains(st.ScanErr.Error(), "Archive") {
		t.Errorf("ScanErr = %v, want ErrRootNotChecked naming Archive", st.ScanErr)
	}
}

func TestBackgroundSkippedShareDoesNotMoveCheckedAt(t *testing.T) {
	e := newSMBEnv(t, "Photos", "Archive")
	e.seed(t)
	e.net.unmount("Archive")
	e.net.failMount["Archive"] = true
	svc := e.open(t)
	scanAndWait(t, svc, Incremental)
	checked := svc.State().CheckedAt
	reads := e.src.readsOf("/Volumes/Photos")
	e.clk.Advance(time.Minute)
	svc.Kick(0)
	eventually(t, "background scan", func() bool { return e.src.readsOf("/Volumes/Photos") > reads })
	waitIdle(t, svc)

	st := svc.State()
	if !st.CheckedAt.Equal(checked) || !errors.Is(st.ScanErr, ErrRootNotChecked) {
		t.Errorf("CheckedAt=%v ScanErr=%v after a background scan that skipped a share", st.CheckedAt, st.ScanErr)
	}
}

func TestUnreadableRootDoesNotMoveCheckedAtAndIsNamed(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	svc := e.open(t)
	before := svc.State().CheckedAt
	e.src.setErr(localBase, os.ErrPermission)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	st := svc.State()
	if !st.CheckedAt.Equal(before) {
		t.Errorf("CheckedAt moved to %v although the root was not listed", st.CheckedAt)
	}
	if !errors.Is(st.ScanErr, ErrRootNotChecked) || !strings.Contains(st.ScanErr.Error(), localBase) {
		t.Errorf("ScanErr = %v, want ErrRootNotChecked naming %s", st.ScanErr, localBase)
	}
	assertPaths(t, svc.Current(), e.expected())
}

func TestEmptyListedRootWithChildrenInOldIndexIsNotChecked(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	svc := e.open(t)
	before := svc.State().CheckedAt
	want := e.expected()
	for _, name := range []string{"top.jpg", "a", "b"} {
		e.src.remove(filepath.Join(localBase, name), editAt)
	}
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Full)

	st := svc.State()
	if !st.CheckedAt.Equal(before) {
		t.Errorf("CheckedAt moved to %v although the root came back empty", st.CheckedAt)
	}
	if !errors.Is(st.ScanErr, ErrRootNotChecked) {
		t.Errorf("ScanErr = %v, want ErrRootNotChecked", st.ScanErr)
	}
	assertPaths(t, svc.Current(), want)
}

func TestFailingSubdirectoryKeepsCheckedAtCompleteAndShowsInProgress(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	svc := e.open(t)
	e.src.setErr(filepath.Join(localBase, "a/a1"), os.ErrPermission)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Full)

	st := svc.State()
	if !st.CheckedAt.Equal(e.clk.Now()) {
		t.Errorf("CheckedAt = %v, want %v: a failing sub-directory must not block it", st.CheckedAt, e.clk.Now())
	}
	if !st.Complete {
		t.Error("Complete = false after a scan that ran to the end")
	}
	if st.ScanErr != nil {
		t.Errorf("ScanErr = %v, want nil", st.ScanErr)
	}
	if !st.Progress.Done || st.Progress.Errors != 1 {
		t.Errorf("Progress = %+v, want Done with 1 error kept after the scan", st.Progress)
	}
}

// --- B. stall watchdog ----------------------------------------------------------

func TestStalledScanIsCancelledReportedAndDoesNotBlockLaterScans(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	blk := e.src.block(filepath.Join(localBase, "a"))
	opt := e.opts()
	opt.StallTimeout = 100 * time.Millisecond
	svc := e.openWith(t, opt)
	before := svc.State().CheckedAt
	e.clk.Advance(time.Minute)

	svc.Scan(Full)
	blk.waitEntered(t)
	waitIdle(t, svc)

	st := svc.State()
	if !errors.Is(st.ScanErr, ErrScanStalled) {
		t.Errorf("ScanErr = %v, want ErrScanStalled", st.ScanErr)
	}
	if !st.CheckedAt.Equal(before) {
		t.Errorf("CheckedAt moved to %v after a stalled scan", st.CheckedAt)
	}
	blk.unblock()
	scanAndWait(t, svc, Full)
	st = svc.State()
	if st.ScanErr != nil || !st.CheckedAt.Equal(e.clk.Now()) {
		t.Errorf("scan after a stall: ScanErr=%v CheckedAt=%v", st.ScanErr, st.CheckedAt)
	}
}

func TestMountedLookupHasATimeout(t *testing.T) {
	old := mountedTimeout
	mountedTimeout = 50 * time.Millisecond
	t.Cleanup(func() { mountedTimeout = old })
	e := newSMBEnv(t, "Photos")
	opt := e.opts()
	opt.Net = &netshare.Client{Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "mount" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return e.net.run(ctx, name, args...)
	}}
	svc, err := Open(e.st, e.sess, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)

	scanAndWait(t, svc, Incremental)

	st := svc.State()
	if st.ScanErr == nil || st.Offline {
		t.Errorf("ScanErr=%v Offline=%v, want a prepare error and not offline", st.ScanErr, st.Offline)
	}
}

// --- E. requests are not dropped ------------------------------------------------

func TestScanArrivingWhileScanningRunsOnceAfterwards(t *testing.T) {
	e := newLocalEnv(t)
	blk := e.src.block(filepath.Join(localBase, "a"))
	svc := e.open(t)
	svc.Scan(Incremental)
	blk.waitEntered(t)

	svc.Scan(Incremental)
	svc.Scan(Incremental)
	svc.Kick(0)
	time.Sleep(100 * time.Millisecond)
	if n := e.src.readsOf(localBase); n != 1 {
		t.Fatalf("root listed %d times while the first scan ran, want 1", n)
	}
	blk.unblock()
	waitIdle(t, svc)

	if n := e.src.readsOf(localBase); n != 2 {
		t.Errorf("root listed %d times, want 2: exactly one remembered request", n)
	}
	if n := e.src.readsOf(filepath.Join(localBase, "a/a1")); n != 1 {
		t.Errorf("leaf listed %d times, want 1: the remembered request was incremental", n)
	}
}

func TestFullRequestBeatsIncrementalWhilePending(t *testing.T) {
	e := newLocalEnv(t)
	blk := e.src.block(filepath.Join(localBase, "a"))
	svc := e.open(t)
	svc.Scan(Incremental)
	blk.waitEntered(t)

	svc.Scan(Incremental)
	svc.Scan(Full)
	svc.Scan(Incremental)
	blk.unblock()
	waitIdle(t, svc)

	if n := e.src.readsOf(filepath.Join(localBase, "a/a1")); n != 2 {
		t.Errorf("leaf listed %d times, want 2: the remembered request was Full", n)
	}
}

func TestStopForgetsPendingRequest(t *testing.T) {
	e := newLocalEnv(t)
	blk := e.src.block(filepath.Join(localBase, "a"))
	svc := e.open(t)
	svc.Scan(Incremental)
	blk.waitEntered(t)
	svc.Scan(Full)

	svc.Stop()
	waitIdle(t, svc)
	time.Sleep(50 * time.Millisecond)

	if n := e.src.readsOf(localBase); n != 1 {
		t.Errorf("root listed %d times, want 1: Stop must clear the pending request", n)
	}
}

func TestClosedServiceNeverStartsPendingRequest(t *testing.T) {
	e := newLocalEnv(t)
	blk := e.src.block(filepath.Join(localBase, "a"))
	svc := e.open(t)
	svc.Scan(Incremental)
	blk.waitEntered(t)
	svc.Scan(Full)

	svc.Close()
	time.Sleep(50 * time.Millisecond)

	if n := e.src.readsOf(localBase); n != 1 {
		t.Errorf("root listed %d times, want 1", n)
	}
}

func TestMergeRequestRules(t *testing.T) {
	cases := []struct {
		name string
		a, b request
		want request
	}{
		{"full beats incremental", request{Incremental, false}, request{Full, false}, request{Full, false}},
		{"incremental does not weaken full", request{Full, false}, request{Incremental, false}, request{Full, false}},
		{"user beats background", request{Incremental, true}, request{Incremental, false}, request{Incremental, false}},
		{"user keeps privilege", request{Incremental, false}, request{Incremental, true}, request{Incremental, false}},
		{"both background", request{Incremental, true}, request{Full, true}, request{Full, true}},
	}
	for _, c := range cases {
		if got := c.a.merge(c.b); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// --- F. a user Stop is not "offline" ----------------------------------------------

func TestStopDuringPrepareIsNeitherOfflineNorAnError(t *testing.T) {
	e := newSMBEnv(t, "Photos")
	entered := make(chan struct{})
	var once sync.Once
	opt := e.opts()
	opt.Reachable = func(ctx context.Context, _ string) bool {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return false
	}
	svc, err := Open(e.st, e.sess, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	svc.Scan(Incremental)
	<-entered

	svc.Stop()
	waitIdle(t, svc)

	st := svc.State()
	if st.Offline || st.ScanErr != nil {
		t.Errorf("Offline=%v ScanErr=%v after Stop during prepare, want neither", st.Offline, st.ScanErr)
	}
}

// --- I. LoadErr clears once the bad file is replaced -----------------------------

func TestLoadErrClearsAfterSuccessfulSave(t *testing.T) {
	e := newLocalEnv(t)
	if err := os.WriteFile(e.st.Path(e.sess.Source), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := e.open(t)
	if svc.State().LoadErr == nil {
		t.Fatal("LoadErr not set for a corrupt snapshot")
	}

	scanAndWait(t, svc, Incremental)

	if err := svc.State().LoadErr; err != nil {
		t.Errorf("LoadErr = %v after the snapshot was replaced", err)
	}
}

// --- G, H. store -----------------------------------------------------------------

func TestSaveSessionDoesNotWaitForSnapshotWrite(t *testing.T) {
	st, _ := newTestStore(t)
	inWrite, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- st.SaveWith(nasSource, func(io.Writer) error {
			close(inWrite)
			<-release
			return nil
		})
	}()
	<-inWrite
	t.Cleanup(func() { close(release); <-done })

	saved := make(chan error, 1)
	go func() { saved <- st.SaveSession(Session{Source: nasSource}) }()

	select {
	case err := <-saved:
		if err != nil {
			t.Errorf("SaveSession: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SaveSession blocked behind a snapshot write")
	}
}

func TestNewStoreRemovesStaleTempFilesOnly(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "tmp-12345.part")
	keep := filepath.Join(dir, "abc"+snapshotExt)
	for _, p := range []string{stale, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := NewStore(dir, noopRunner); err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale temp file still there: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("snapshot was removed: %v", err)
	}
}

func TestTmutilRunsUnderATimeout(t *testing.T) {
	var hasDeadline bool
	run := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		_, hasDeadline = ctx.Deadline()
		return nil, nil
	}

	if _, err := NewStore(filepath.Join(t.TempDir(), "new"), run); err != nil {
		t.Fatal(err)
	}

	if !hasDeadline {
		t.Error("tmutil ran without a deadline")
	}
}

func TestStateNamesTheDirectoriesThatCouldNotBeListed(t *testing.T) {
	e := newLocalEnv(t)
	e.seed(t)
	svc := e.open(t)
	bad := filepath.Join(localBase, "a/a1")
	e.src.setErr(bad, &fs.PathError{Op: "open", Path: bad, Err: fs.ErrNotExist})

	scanAndWait(t, svc, Full)

	got := svc.State().FailedDirs
	want := []string{bad + ": " + fs.ErrNotExist.Error()}
	if !slices.Equal(got, want) {
		t.Fatalf("FailedDirs = %q, want %q", got, want)
	}

	e.src.setErr(bad, nil)
	scanAndWait(t, svc, Full)

	if got := svc.State().FailedDirs; len(got) != 0 {
		t.Errorf("FailedDirs = %q after a clean scan, want none", got)
	}
}

func TestFailedDirsAreCapped(t *testing.T) {
	r := &scan{}
	for i := range maxFailedDirs + 5 {
		r.noteFailed(scanner.Dir{Path: fmt.Sprintf("/d/%d", i)}, os.ErrPermission)
	}
	r.noteFailed(scanner.Dir{Path: "/cancelled"}, context.Canceled)

	if len(r.failed) != maxFailedDirs {
		t.Fatalf("kept %d failed dirs, want %d", len(r.failed), maxFailedDirs)
	}
}
