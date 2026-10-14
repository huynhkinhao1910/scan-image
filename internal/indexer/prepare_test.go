package indexer

import (
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/netshare"
	"github.com/isuccesscorp/scan-image/internal/search"
)

const (
	smbHost   = "nas"
	smbUser   = "sunny"
	smbSource = "smb://nas"
)

// smbEnv is a server source whose shares are mounted under /Volumes/<share>.
type smbEnv struct {
	st     *Store
	src    *fakeSource
	clk    *fakeClock
	net    *fakeNet
	reach  *fakeReach
	sess   Session
	shares []string
}

func newSMBEnv(t *testing.T, shares ...string) *smbEnv {
	t.Helper()
	st, _ := newTestStore(t)
	e := &smbEnv{st: st, src: newFakeSource(t), clk: newClock(), net: newFakeNet(), reach: newReach(), shares: shares}
	e.sess = Session{Mode: ModeServer, Source: smbSource, Host: smbHost, User: smbUser}
	var mounts []fakeMount
	for _, s := range shares {
		addPhotos(e.src, "/Volumes/"+s)
		mounts = append(mounts, fakeMount{smbUser, smbHost, s, "/Volumes/" + s})
		e.sess.Shares = append(e.sess.Shares, SessionShare{Key: "smb://" + smbHost + "/" + s, Share: s})
	}
	e.net.setMounts(mounts...)
	e.net.listed = append(slices.Clone(shares), "Other")
	return e
}

func (e *smbEnv) opts() Options {
	return Options{
		Source:      e.src,
		Net:         &netshare.Client{Run: e.net.run},
		Reachable:   e.reach.check,
		Workers:     4,
		Interval:    -1,
		Checkpoint:  time.Hour,
		CloseWait:   2 * time.Second,
		Now:         e.clk.Now,
		VerifySlots: 1 << 30,
	}
}

func (e *smbEnv) open(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(e.st, e.sess, e.opts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

// seed scans every share once while all are mounted and reachable.
func (e *smbEnv) seed(t *testing.T) {
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

func (e *smbEnv) pathsOf(shares ...string) []string {
	var out []string
	for _, s := range shares {
		out = append(out, e.src.paths("/Volumes/"+s)...)
	}
	slices.Sort(out)
	return out
}

func rootKeys(ix *search.Index) []string {
	var keys []string
	for _, r := range ix.RootList() {
		keys = append(keys, r.Key)
	}
	slices.Sort(keys)
	return keys
}

// --- 11. SMB prepare ---------------------------------------------------------------

func TestSMBUnreachableSkipsScanNeverMountsAndServesSavedIndex(t *testing.T) {
	e := newSMBEnv(t, "Photos", "Archive")
	e.seed(t)
	want := e.pathsOf("Photos", "Archive")
	e.net.unmount("Archive") // a VPN-off Mac has nothing mounted
	e.reach.up.Store(false)
	svc := e.open(t)
	cmdsBefore := e.net.count("osascript")

	scanAndWait(t, svc, Incremental)

	if !svc.State().Offline {
		t.Error("State().Offline = false for an unreachable host")
	}
	if n := e.net.count("osascript") - cmdsBefore; n != 0 {
		t.Errorf("osascript ran %d times while the host was unreachable", n)
	}
	if n := e.src.total(); n != 0 {
		t.Errorf("%d ReadDir calls while offline, want 0", n)
	}
	assertPaths(t, svc.Current(), want)

	e.reach.up.Store(true)
	e.net.listed = []string{"Photos", "Archive"}
	e.clk.Advance(time.Minute)
	scanAndWait(t, svc, Incremental)
	if svc.State().Offline {
		t.Error("State().Offline stays true after the host is reachable again")
	}
	if e.src.total() == 0 {
		t.Error("no scan ran once the host was reachable")
	}
	assertPaths(t, svc.Current(), want)
}

func TestSMBReachableMountsMissingSharesAndScansOnlySavedShares(t *testing.T) {
	e := newSMBEnv(t, "Photos", "Archive")
	e.net.unmount("Archive")
	svc := e.open(t)

	scanAndWait(t, svc, Incremental)

	if n := e.net.count("osascript"); n != 1 {
		t.Errorf("osascript ran %d times, want 1 (mount Archive)", n)
	}
	want := []string{"smb://nas/Archive", "smb://nas/Photos"}
	if got := rootKeys(svc.Current()); !reflect.DeepEqual(got, want) {
		t.Errorf("root keys = %v, want %v (share Other is not part of the session)", got, want)
	}
	assertPaths(t, svc.Current(), e.pathsOf("Photos", "Archive"))
	if n := e.src.readsUnder("/Volumes/Other"); n != 0 {
		t.Errorf("share not in the session was listed %d times", n)
	}
	wantRoots := []RootInfo{{Name: "Archive", Path: "/Volumes/Archive"}, {Name: "Photos", Path: "/Volumes/Photos"}}
	got := svc.State().Roots
	slices.SortFunc(got, func(a, b RootInfo) int { return compareStrings(a.Name, b.Name) })
	if !reflect.DeepEqual(got, wantRoots) {
		t.Errorf("Roots = %+v, want %+v (Name is the share name)", got, wantRoots)
	}
	if n := e.reach.calls.Load(); n < 1 {
		t.Error("Reachable was not consulted before mounting")
	}
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestSMBRemountedShareKeepsKeyAndDataAndUsesNewMountpoint(t *testing.T) {
	e := newSMBEnv(t, "Photos")
	e.seed(t)
	// Another server's Photos took /Volumes/Photos; ours came back as Photos-1.
	addPhotos(e.src, "/Volumes/Photos-1")
	e.net.setMounts(
		fakeMount{"bob", "other", "Photos", "/Volumes/Photos"},
		fakeMount{smbUser, smbHost, "Photos", "/Volumes/Photos-1"},
	)
	svc := e.open(t)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	roots := svc.Current().RootList()
	if len(roots) != 1 || roots[0].Key != "smb://nas/Photos" || roots[0].Path != "/Volumes/Photos-1" {
		t.Fatalf("roots = %+v, want key smb://nas/Photos at /Volumes/Photos-1", roots)
	}
	assertPaths(t, svc.Current(), e.src.paths("/Volumes/Photos-1"))
	if n := e.src.readsUnder("/Volumes/Photos/"); n != 0 || e.src.readsOf("/Volumes/Photos") != 0 {
		t.Errorf("old mountpoint was listed (%d)", n)
	}
	want := readsOnce("/Volumes/Photos-1", nonLeafPhotoDirs...)
	if got := e.src.readCounts(); !reflect.DeepEqual(got, want) {
		t.Errorf("reads = %v, want %v: leaves must be reused across a remount (matched by key)", got, want)
	}
	if got := svc.State().Roots; !reflect.DeepEqual(got, []RootInfo{{Name: "Photos", Path: "/Volumes/Photos-1"}}) {
		t.Errorf("Roots = %+v", got)
	}
	ix, _, err := e.st.Load(smbSource)
	if err != nil {
		t.Fatal(err)
	}
	if r := ix.RootList(); len(r) != 1 || r[0].Path != "/Volumes/Photos-1" {
		t.Errorf("saved snapshot roots = %+v, want the new mountpoint persisted", r)
	}
}

func TestSMBShareThatCannotBeMountedKeepsOldDataWhileOthersScan(t *testing.T) {
	e := newSMBEnv(t, "Photos", "Archive")
	e.seed(t)
	archive := e.pathsOf("Archive")
	e.net.unmount("Archive")
	e.net.failMount["Archive"] = true
	e.src.addFile("/Volumes/Photos/new.jpg", 1, editAt)
	svc := e.open(t)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	assertPaths(t, svc.Current(), slices.Sorted(slices.Values(append(e.pathsOf("Photos"), archive...))))
	if n := e.src.readsUnder("/Volumes/Archive"); n != 0 {
		t.Errorf("unmounted share listed %d times", n)
	}
	if !contains(svc.Current(), "/Volumes/Photos/new.jpg") {
		t.Error("the healthy share was not scanned")
	}
	if got := rootKeys(svc.Current()); len(got) != 2 {
		t.Errorf("root keys = %v, want both shares kept", got)
	}
}

// --- 12. Background mount failure is remembered --------------------------------------

func TestSMBBackgroundMountFailureIsNotRetriedUntilUserScan(t *testing.T) {
	e := newSMBEnv(t, "Photos", "Archive")
	e.seed(t)
	e.net.unmount("Archive")
	e.net.failMount["Archive"] = true
	svc := e.open(t)
	photos := "/Volumes/Photos"

	kickAndWait := func() {
		t.Helper()
		before := e.src.readsOf(photos)
		e.clk.Advance(time.Minute)
		svc.Kick(0)
		eventually(t, "background scan to start", func() bool { return e.src.readsOf(photos) > before })
		waitIdle(t, svc)
	}

	kickAndWait()
	if n := e.net.count("osascript"); n != 1 {
		t.Fatalf("first background scan tried %d mounts, want 1", n)
	}
	kickAndWait()
	if n := e.net.count("osascript"); n != 1 {
		t.Errorf("second background scan retried the failed mount (%d attempts)", n)
	}

	scanAndWait(t, svc, Incremental)
	if n := e.net.count("osascript"); n != 2 {
		t.Errorf("user scan made %d mount attempts in total, want 2 (it must retry)", n)
	}
}

// --- 13. Network loss mid-scan ----------------------------------------------------------

var netTimeout = &fs.PathError{Op: "readdirent", Path: "/Volumes/x", Err: syscall.ETIMEDOUT}

func TestSMBNetworkLossMidScanRechecksOnceGoesOfflineAndKeepsIndex(t *testing.T) {
	e := newSMBEnv(t, "Photos", "Archive", "Video", "Docs")
	e.seed(t)
	want := e.pathsOf("Photos", "Archive", "Video", "Docs")
	svc := e.open(t)
	file := readSnapshotFile(t, e.st, smbSource)
	e.reach.calls.Store(0)
	e.reach.downAfter.Store(1) // reachable for prepare, gone for the re-check
	e.src.setFailAll(netTimeout)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Incremental)

	if !svc.State().Offline {
		t.Error("State().Offline = false after the network dropped mid-scan")
	}
	if n := e.reach.calls.Load(); n != 2 {
		t.Errorf("Reachable called %d times, want 2: prepare + one debounced re-check for the burst of errors", n)
	}
	assertPaths(t, svc.Current(), want)
	if !file.untouched(readSnapshotFile(t, e.st, smbSource)) {
		t.Error("snapshot changed although the scan lost the network")
	}
}

func TestSMBReadErrorWithReachableServerDoesNotGoOffline(t *testing.T) {
	e := newSMBEnv(t, "Photos")
	e.seed(t)
	want := e.pathsOf("Photos")
	svc := e.open(t)
	e.src.setErr(filepath.Join("/Volumes/Photos", "a/a1"), netTimeout)
	e.reach.calls.Store(0)
	e.clk.Advance(time.Minute)

	scanAndWait(t, svc, Full)

	if svc.State().Offline {
		t.Error("State().Offline = true although the host answers")
	}
	if n := e.reach.calls.Load(); n < 2 {
		t.Errorf("Reachable called %d times, want a re-check after the network-looking error", n)
	}
	assertPaths(t, svc.Current(), want)
}
