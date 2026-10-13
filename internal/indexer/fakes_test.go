package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/search"
	"github.com/isuccesscorp/scan-image/internal/source"
)

// waitLimit bounds every wait so a broken implementation fails fast.
const waitLimit = 5 * time.Second

const (
	localBase = "/Users/me/Pictures"
	// addPhotos builds 8 directories: 3 with subdirectories (base, a, b)
	// and 5 leaves (a/a1, a/a2, a/a3, b/b1, b/b2).
	photoDirs        = 8
	photoNonLeafDirs = 3
)

var (
	clockStart = time.Unix(1_800_000_000, 0)
	// oldAt is far older than search.RacyWindow relative to clockStart.
	oldAt = clockStart.Add(-time.Hour)
	// editAt differs from oldAt but is still older than the racy window.
	editAt = clockStart.Add(-30 * time.Minute)
)

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// fakeClock is the injected Options.Now.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock { return &fakeClock{now: clockStart} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// blocker holds ReadDir of one path until unblock, ignoring ctx the way
// os.ReadDir on a dead SMB mount does.
type blocker struct {
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func (b *blocker) unblock() { b.releaseOnce.Do(func() { close(b.release) }) }

func (b *blocker) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(waitLimit):
		t.Fatal("ReadDir never reached the blocked directory")
	}
}

type fakeNode struct {
	dir   bool
	size  int64
	mtime time.Time
}

// fakeSource is an in-memory tree that counts ReadDir calls per path.
type fakeSource struct {
	mu      sync.Mutex
	nodes   map[string]fakeNode
	reads   map[string]int
	errs    map[string]error
	failAll error
	blocks  map[string]*blocker
}

func newFakeSource(t *testing.T) *fakeSource {
	s := &fakeSource{
		nodes:  map[string]fakeNode{},
		reads:  map[string]int{},
		errs:   map[string]error{},
		blocks: map[string]*blocker{},
	}
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, b := range s.blocks {
			b.unblock()
		}
	})
	return s
}

func (s *fakeSource) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("fakeSource: Open not supported")
}

func (s *fakeSource) ReadDir(_ context.Context, path string) ([]source.Entry, error) {
	path = filepath.Clean(path)
	s.mu.Lock()
	s.reads[path]++
	b := s.blocks[path]
	s.mu.Unlock()
	if b != nil {
		b.enterOnce.Do(func() { close(b.entered) })
		<-b.release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.errs[path]; err != nil {
		return nil, err
	}
	if s.failAll != nil {
		return nil, s.failAll
	}
	if n, ok := s.nodes[path]; !ok || !n.dir {
		return nil, &fs.PathError{Op: "readdir", Path: path, Err: fs.ErrNotExist}
	}
	var out []source.Entry
	for p, n := range s.nodes {
		if filepath.Dir(p) != path {
			continue
		}
		out = append(out, source.Entry{Name: filepath.Base(p), Size: n.size, ModTime: n.mtime, IsDir: n.dir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *fakeSource) mkdir(path string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[filepath.Clean(path)] = fakeNode{dir: true, mtime: at}
}

func (s *fakeSource) mkfile(path string, size int64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[filepath.Clean(path)] = fakeNode{size: size, mtime: at}
}

func (s *fakeSource) touchLocked(dir string, at time.Time) {
	if n, ok := s.nodes[dir]; ok {
		n.mtime = at
		s.nodes[dir] = n
	}
}

func (s *fakeSource) touch(dir string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchLocked(filepath.Clean(dir), at)
}

// addFile adds a file and bumps the mtime of its directory, as a real
// filesystem does.
func (s *fakeSource) addFile(path string, size int64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path = filepath.Clean(path)
	s.nodes[path] = fakeNode{size: size, mtime: at}
	s.touchLocked(filepath.Dir(path), at)
}

// remove deletes path and everything below it.
func (s *fakeSource) remove(path string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path = filepath.Clean(path)
	for p := range s.nodes {
		if p == path || strings.HasPrefix(p, path+"/") {
			delete(s.nodes, p)
		}
	}
	s.touchLocked(filepath.Dir(path), at)
}

func (s *fakeSource) rename(oldPath, newPath string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldPath, newPath = filepath.Clean(oldPath), filepath.Clean(newPath)
	n := s.nodes[oldPath]
	delete(s.nodes, oldPath)
	n.mtime = at
	s.nodes[newPath] = n
	s.touchLocked(filepath.Dir(oldPath), at)
	s.touchLocked(filepath.Dir(newPath), at)
}

// paths lists everything below base, sorted, as search results report it.
func (s *fakeSource) paths(base string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for p := range s.nodes {
		if strings.HasPrefix(p, base+"/") {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

func (s *fakeSource) readCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.reads)
}

func (s *fakeSource) readsOf(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[path]
}

func (s *fakeSource) readsUnder(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p, c := range s.reads {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			n += c
		}
	}
	return n
}

func (s *fakeSource) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.reads {
		n += c
	}
	return n
}

func (s *fakeSource) resetReads() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = map[string]int{}
}

func (s *fakeSource) setErr(path string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[filepath.Clean(path)] = err
}

func (s *fakeSource) setFailAll(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAll = err
}

func (s *fakeSource) block(path string) *blocker {
	b := &blocker{entered: make(chan struct{}), release: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocks[filepath.Clean(path)] = b
	return b
}

// addPhotos builds the standard tree under base (see photoDirs).
func addPhotos(s *fakeSource, base string) {
	s.mkdir(base, oldAt)
	s.mkfile(filepath.Join(base, "top.jpg"), 10, oldAt)
	s.mkdir(filepath.Join(base, "a"), oldAt)
	s.mkdir(filepath.Join(base, "b"), oldAt)
	leaves := map[string][]string{
		"a/a1": {"1.jpg", "2.jpg"},
		"a/a2": {"two.jpg"},
		"a/a3": {"3.jpg"},
		"b/b1": {"b1.jpg"},
		"b/b2": {"b2.jpg"},
	}
	for d, files := range leaves {
		s.mkdir(filepath.Join(base, d), oldAt)
		for _, f := range files {
			s.mkfile(filepath.Join(base, d, f), 100, oldAt)
		}
	}
}

// editPhotos changes two leaves, adds a directory and removes another.
// An incremental scan then has to list: base, a, b (non-leaf), a/a1, a/a2
// (changed leaves) and c (new).
func editPhotos(s *fakeSource, base string) {
	s.addFile(filepath.Join(base, "a/a1/new.jpg"), 5, editAt)
	s.rename(filepath.Join(base, "a/a2/two.jpg"), filepath.Join(base, "a/a2/two-renamed.jpg"), editAt)
	s.remove(filepath.Join(base, "b/b2"), editAt)
	s.mkdir(filepath.Join(base, "c"), editAt)
	s.mkfile(filepath.Join(base, "c/c1.jpg"), 7, editAt)
	s.touch(base, editAt)
}

var editedReads = []string{"", "a", "b", "a/a1", "a/a2", "c"}
var allPhotoDirs = []string{"", "a", "b", "a/a1", "a/a2", "a/a3", "b/b1", "b/b2"}
var nonLeafPhotoDirs = []string{"", "a", "b"}

// readsOnce is the expected read-count map: each rel dir under base once.
func readsOnce(base string, rels ...string) map[string]int {
	m := map[string]int{}
	for _, r := range rels {
		m[filepath.Join(base, r)] = 1
	}
	return m
}

func assertPaths(t *testing.T, ix *search.Index, want []string) {
	t.Helper()
	got := allPaths(ix)
	if slices.Equal(got, want) {
		return
	}
	t.Errorf("searchable paths differ\n missing: %v\n extra:   %v", diff(want, got), diff(got, want))
}

func diff(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// snapshotFile captures a snapshot file's identity and content.
type snapshotFile struct {
	info os.FileInfo
	data []byte
}

func readSnapshotFile(t *testing.T, st *Store, sourceKey string) snapshotFile {
	t.Helper()
	p := st.Path(sourceKey)
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read snapshot file: %v", err)
	}
	return snapshotFile{info: info, data: data}
}

// untouched reports that the file was neither replaced nor rewritten.
func (f snapshotFile) untouched(g snapshotFile) bool {
	return os.SameFile(f.info, g.info) && f.info.ModTime().Equal(g.info.ModTime()) && bytes.Equal(f.data, g.data)
}

func waitIdle(t *testing.T, svc *Service) {
	t.Helper()
	eventually(t, "scan to end", func() bool { return !svc.State().Scanning })
}

func scanAndWait(t *testing.T, svc *Service, m Mode) {
	t.Helper()
	svc.Scan(m)
	waitIdle(t, svc)
}

func waitSnapshot(t *testing.T, st *Store, sourceKey, what string, pred func(*search.Index, search.Meta) bool) {
	t.Helper()
	eventually(t, what, func() bool {
		ix, m, err := st.Load(sourceKey)
		return err == nil && pred(ix, m)
	})
}

func noopRunner(context.Context, string, ...string) ([]byte, error) { return nil, nil }

// fakeNet fakes mount, smbutil and osascript for the netshare Client.
type fakeNet struct {
	mu        sync.Mutex
	mounted   []fakeMount
	listed    []string
	failMount map[string]bool
	cmds      []string
}

type fakeMount struct{ user, host, share, path string }

func newFakeNet() *fakeNet { return &fakeNet{failMount: map[string]bool{}} }

func (f *fakeNet) setMounts(ms ...fakeMount) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounted = ms
}

func (f *fakeNet) unmount(share string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounted = slices.DeleteFunc(f.mounted, func(m fakeMount) bool { return m.share == share })
}

func (f *fakeNet) count(cmd string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cmds {
		if c == cmd {
			n++
		}
	}
	return n
}

func (f *fakeNet) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, name)
	switch name {
	case "mount":
		var b strings.Builder
		b.WriteString("/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)\n")
		for _, m := range f.mounted {
			fmt.Fprintf(&b, "//%s@%s/%s on %s (smbfs, nodev, nosuid, mounted by %s)\n",
				m.user, m.host, url.PathEscape(m.share), m.path, m.user)
		}
		return []byte(b.String()), nil
	case "smbutil":
		var b strings.Builder
		b.WriteString("Share                                           Type    Comments\n")
		b.WriteString("-------------------------------\n")
		for _, s := range f.listed {
			fmt.Fprintf(&b, "%-48sDisk\n", s)
		}
		fmt.Fprintf(&b, "%d shares listed\n", len(f.listed))
		return []byte(b.String()), nil
	case "osascript":
		return f.mountVolume(args)
	}
	return nil, fmt.Errorf("fakeNet: unexpected command %q", name)
}

// mountVolume handles `osascript -e 'mount volume "smb://user@host/share"'`.
func (f *fakeNet) mountVolume(args []string) ([]byte, error) {
	script := strings.Join(args, " ")
	i := strings.Index(script, "smb://")
	if i < 0 {
		return nil, fmt.Errorf("fakeNet: no smb url in %q", script)
	}
	raw := script[i:]
	if j := strings.IndexByte(raw, '"'); j >= 0 {
		raw = raw[:j]
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	share := strings.TrimPrefix(u.Path, "/")
	if f.failMount[share] {
		return []byte("execution error: could not mount"), errors.New("exit status 1")
	}
	f.mounted = append(f.mounted, fakeMount{user: u.User.Username(), host: u.Hostname(), share: share, path: "/Volumes/" + share})
	return nil, nil
}

// fakeReach is the injected Options.Reachable.
type fakeReach struct {
	up        atomic.Bool
	calls     atomic.Int32
	downAfter atomic.Int32 // when > 0, every call after this many reports down
}

func newReach() *fakeReach {
	r := &fakeReach{}
	r.up.Store(true)
	return r
}

func (r *fakeReach) check(context.Context, string) bool {
	n := r.calls.Add(1)
	if d := r.downAfter.Load(); d > 0 && n > d {
		return false
	}
	return r.up.Load()
}
