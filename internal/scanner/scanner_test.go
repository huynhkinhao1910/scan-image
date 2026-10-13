package scanner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

type listedCall struct {
	dir     Dir
	entries []source.Entry
}

// recSink records every call and hands back child Dirs for the entries that
// pick accepts. It has no locking: the scanner must serialise its calls.
type recSink struct {
	pick   func(parent Dir, e source.Entry) bool
	nextID uint32
	inside atomic.Int32
	nested atomic.Bool

	listed []listedCall
	failed []Dir
	errs   []error
}

func (s *recSink) enter() func() {
	if s.inside.Add(1) > 1 {
		s.nested.Store(true)
	}
	return func() { s.inside.Add(-1) }
}

func (s *recSink) Listed(d Dir, entries []source.Entry) []Dir {
	defer s.enter()()
	s.listed = append(s.listed, listedCall{dir: d, entries: entries})
	var out []Dir
	for _, e := range entries {
		if !e.IsDir || (s.pick != nil && !s.pick(d, e)) {
			continue
		}
		s.nextID++
		out = append(out, Dir{Path: filepath.Join(d.Path, e.Name), ID: 1000 + s.nextID, Prev: d.ID})
	}
	return out
}

func (s *recSink) Failed(d Dir, err error) {
	defer s.enter()()
	s.failed = append(s.failed, d)
	s.errs = append(s.errs, err)
}

func (s *recSink) listedPaths() []string {
	var out []string
	for _, c := range s.listed {
		out = append(out, c.dir.Path)
	}
	sort.Strings(out)
	return out
}

func (s *recSink) failedPaths() []string {
	var out []string
	for _, d := range s.failed {
		out = append(out, d.Path)
	}
	sort.Strings(out)
	return out
}

// memSource serves an in-memory tree keyed by directory path.
type memSource struct {
	tree map[string][]source.Entry
	errs map[string]error

	mu    sync.Mutex
	calls []string
}

func (m *memSource) ReadDir(_ context.Context, path string) ([]source.Entry, error) {
	m.mu.Lock()
	m.calls = append(m.calls, path)
	m.mu.Unlock()
	if err := m.errs[path]; err != nil {
		return nil, err
	}
	return m.tree[path], nil
}

func (m *memSource) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not supported")
}

func (m *memSource) called(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.calls {
		if c == path {
			return true
		}
	}
	return false
}

func dirEntry(name string) source.Entry  { return source.Entry{Name: name, IsDir: true} }
func fileEntry(name string) source.Entry { return source.Entry{Name: name, Size: 1} }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunIndexesWholeTree(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a/b/c", "a/d", "Ảnh"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"a/1.jpg", "a/b/2.jpg", "a/b/c/3.png", "a/d/4.jpg", "Ảnh/cá.jpg"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	sink := &recSink{}
	s := &Scanner{Src: source.LocalFS{}, Workers: 4}
	roots := []Dir{
		{Path: filepath.Join(root, "a"), ID: 1, Prev: 7},
		{Path: filepath.Join(root, "Ảnh"), ID: 2, Prev: 8},
	}
	if err := s.Run(context.Background(), roots, sink); err != nil {
		t.Fatal(err)
	}

	p := s.Progress()
	if p.Files != 5 || p.Dirs != 3 || p.Errors != 0 || !p.Done {
		t.Fatalf("progress %+v", p)
	}
	want := []string{
		filepath.Join(root, "a"),
		filepath.Join(root, "a/b"),
		filepath.Join(root, "a/b/c"),
		filepath.Join(root, "a/d"),
		filepath.Join(root, "Ảnh"),
	}
	sort.Strings(want)
	if got := sink.listedPaths(); !equalStrings(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	if len(sink.failed) != 0 {
		t.Fatalf("unexpected Failed: %v", sink.failedPaths())
	}
	for _, c := range sink.listed {
		switch c.dir.Path {
		case roots[0].Path:
			if c.dir != roots[0] {
				t.Errorf("root a handed back as %+v, want %+v", c.dir, roots[0])
			}
		case roots[1].Path:
			if c.dir != roots[1] {
				t.Errorf("root Ảnh handed back as %+v, want %+v", c.dir, roots[1])
			}
		case filepath.Join(root, "Ảnh"):
		case filepath.Join(root, "a/b/c"):
			if len(c.entries) != 1 || c.entries[0].Name != "3.png" {
				t.Errorf("a/b/c entries %+v", c.entries)
			}
		}
	}
}

func TestEveryDirGetsExactlyOneListedOrFailed(t *testing.T) {
	src := &memSource{
		tree: map[string][]source.Entry{
			"r1":    {dirEntry("ok"), dirEntry("bad"), fileEntry("f")},
			"r1/ok": {dirEntry("deep")},
			"r2":    {fileEntry("g")},
		},
		errs: map[string]error{
			"r1/bad": errors.New("boom"),
			"r3":     errors.New("share not mounted"),
		},
	}
	sink := &recSink{}
	s := &Scanner{Src: src, Workers: 4}
	roots := []Dir{{Path: "r1", ID: 1}, {Path: "r2", ID: 2}, {Path: "r3", ID: 3}}
	if err := s.Run(context.Background(), roots, sink); err != nil {
		t.Fatal(err)
	}

	counts := map[string]int{}
	for _, c := range sink.listed {
		counts[c.dir.Path]++
	}
	for _, d := range sink.failed {
		counts[d.Path]++
	}
	wantAll := []string{"r1", "r1/bad", "r1/ok", "r1/ok/deep", "r2", "r3"}
	if len(counts) != len(wantAll) {
		t.Fatalf("dirs seen %v, want %v", counts, wantAll)
	}
	for _, p := range wantAll {
		if counts[p] != 1 {
			t.Errorf("%s got %d terminal calls, want exactly 1", p, counts[p])
		}
	}
	if sink.nested.Load() {
		t.Error("sink calls overlapped")
	}
}

func TestReadDirErrorFailsThatDirAndScanCarriesOn(t *testing.T) {
	boom := errors.New("smb: connection reset")
	src := &memSource{
		tree: map[string][]source.Entry{
			"root":      {dirEntry("good"), dirEntry("bad"), dirEntry("later")},
			"root/good": {fileEntry("1.jpg")},
			"root/later": {
				dirEntry("sub"),
			},
			"root/later/sub": {fileEntry("2.jpg")},
		},
		errs: map[string]error{"root/bad": boom},
	}
	sink := &recSink{}
	s := &Scanner{Src: src, Workers: 2}
	if err := s.Run(context.Background(), []Dir{{Path: "root", ID: 1}}, sink); err != nil {
		t.Fatalf("Run returned %v, want nil: a failed dir is not a failed scan", err)
	}

	if got := sink.failedPaths(); !equalStrings(got, []string{"root/bad"}) {
		t.Fatalf("failed %v", got)
	}
	if !errors.Is(sink.errs[0], boom) {
		t.Errorf("Failed err %v, want %v", sink.errs[0], boom)
	}
	want := []string{"root", "root/good", "root/later", "root/later/sub"}
	if got := sink.listedPaths(); !equalStrings(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	if p := s.Progress(); p.Errors != 1 || !p.Done {
		t.Fatalf("progress %+v", p)
	}
}

func TestSinkReturningSubsetListsOnlyThatSubset(t *testing.T) {
	src := &memSource{tree: map[string][]source.Entry{
		"root":      {dirEntry("x"), dirEntry("y"), dirEntry("z")},
		"root/x":    {dirEntry("xx")},
		"root/x/xx": {fileEntry("a.jpg")},
		"root/y":    {fileEntry("b.jpg")},
		"root/z":    {fileEntry("c.jpg")},
	}}
	sink := &recSink{pick: func(_ Dir, e source.Entry) bool { return e.Name == "x" || e.Name == "xx" }}
	s := &Scanner{Src: src, Workers: 3}
	if err := s.Run(context.Background(), []Dir{{Path: "root", ID: 1}}, sink); err != nil {
		t.Fatal(err)
	}

	want := []string{"root", "root/x", "root/x/xx"}
	if got := sink.listedPaths(); !equalStrings(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	for _, skipped := range []string{"root/y", "root/z"} {
		if src.called(skipped) {
			t.Errorf("%s was read although the sink did not return it", skipped)
		}
	}
	if len(sink.failed) != 0 {
		t.Errorf("skipped dirs must not be Failed: %v", sink.failedPaths())
	}
}

func TestSinkCallsAreSerialised(t *testing.T) {
	const fan = 20
	tree := map[string][]source.Entry{}
	var top []source.Entry
	for i := 0; i < fan; i++ {
		name := string(rune('a' + i))
		top = append(top, dirEntry(name))
		var kids []source.Entry
		for j := 0; j < fan; j++ {
			kid := string(rune('a' + j))
			kids = append(kids, dirEntry(kid), fileEntry(kid+".jpg"))
			tree["root/"+name+"/"+kid] = []source.Entry{fileEntry("x.jpg")}
		}
		tree["root/"+name] = kids
	}
	tree["root"] = top

	sink := &recSink{}
	s := &Scanner{Src: &memSource{tree: tree}, Workers: 16}
	if err := s.Run(context.Background(), []Dir{{Path: "root", ID: 1}}, sink); err != nil {
		t.Fatal(err)
	}
	if want := 1 + fan + fan*fan; len(sink.listed) != want {
		t.Fatalf("listed %d dirs, want %d", len(sink.listed), want)
	}
	if sink.nested.Load() {
		t.Error("sink calls overlapped")
	}
}

// blockingSource parks every ReadDir until released, ignoring ctx the way
// os.ReadDir on a dead SMB mount does. A released call returns a directory
// and a file so a late result would be visible if it were delivered.
type blockingSource struct {
	entered  chan string
	release  chan struct{}
	once     sync.Once
	returned atomic.Int32
}

func newBlockingSource(t *testing.T) *blockingSource {
	b := &blockingSource{entered: make(chan string, 64), release: make(chan struct{})}
	t.Cleanup(func() { b.once.Do(func() { close(b.release) }) })
	return b
}

func (b *blockingSource) unblock() { b.once.Do(func() { close(b.release) }) }

func (b *blockingSource) ReadDir(_ context.Context, path string) ([]source.Entry, error) {
	b.entered <- path
	<-b.release
	b.returned.Add(1)
	return []source.Entry{dirEntry("late"), fileEntry("late.jpg")}, nil
}

func (b *blockingSource) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not supported")
}

// lockedSink is a recSink that is also safe to read while a misbehaving
// late worker might still call it.
type lockedSink struct {
	mu     sync.Mutex
	listed map[Dir]int
	failed map[Dir]int
}

func newLockedSink() *lockedSink {
	return &lockedSink{listed: map[Dir]int{}, failed: map[Dir]int{}}
}

func (s *lockedSink) Listed(d Dir, entries []source.Entry) []Dir {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed[d]++
	return []Dir{{Path: d.Path + "/late", ID: d.ID + 100}}
}

func (s *lockedSink) Failed(d Dir, _ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[d]++
}

func (s *lockedSink) snapshot() (listed, failed map[Dir]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	listed, failed = map[Dir]int{}, map[Dir]int{}
	for k, v := range s.listed {
		listed[k] = v
	}
	for k, v := range s.failed {
		failed[k] = v
	}
	return
}

func TestCancelReturnsImmediatelyAndFailsQueuedAndInFlightDirs(t *testing.T) {
	src := newBlockingSource(t)
	sink := newLockedSink()
	roots := []Dir{
		{Path: "r1", ID: 1}, {Path: "r2", ID: 2}, {Path: "r3", ID: 3},
		{Path: "r4", ID: 4}, {Path: "r5", ID: 5},
	}
	s := &Scanner{Src: src, Workers: 2}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, roots, sink) }()

	for i := 0; i < 2; i++ {
		select {
		case <-src.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("workers never started a ReadDir")
		}
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return promptly after cancel while ReadDir was blocked")
	}

	listed, failed := sink.snapshot()
	if len(listed) != 0 {
		t.Errorf("Listed after cancel: %v", listed)
	}
	for _, r := range roots {
		if failed[r] != 1 {
			t.Errorf("%s Failed %d times, want exactly 1", r.Path, failed[r])
		}
	}
	if len(failed) != len(roots) {
		t.Errorf("failed set %v, want exactly the %d roots", failed, len(roots))
	}
	if p := s.Progress(); !p.Done {
		t.Errorf("progress %+v, want Done", p)
	}

	src.unblock()
	deadline := time.Now().Add(2 * time.Second)
	for src.returned.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("blocked ReadDir calls never returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	listed, failed = sink.snapshot()
	if len(listed) != 0 {
		t.Errorf("late result delivered via Listed: %v", listed)
	}
	for _, r := range roots {
		if failed[r] != 1 {
			t.Errorf("after release %s Failed %d times, want exactly 1", r.Path, failed[r])
		}
	}
	if len(failed) != len(roots) {
		t.Errorf("after release failed set %v, want exactly the %d roots", failed, len(roots))
	}
}
