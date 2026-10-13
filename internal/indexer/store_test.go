package indexer

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/search"
	"github.com/isuccesscorp/scan-image/internal/source"
)

const (
	nasSource   = "smb://nas.local/Photos"
	otherSource = "/Users/me/Pictures"
)

var testScannedAt = time.Unix(1_700_000_000, 0)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := NewStore(dir, func(context.Context, string, ...string) ([]byte, error) { return nil, nil })
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return st, dir
}

func buildIndex(t testing.TB, key string, names ...string) *search.Index {
	t.Helper()
	ix := search.New()
	root := ix.AddRoot(key, "/Volumes/"+filepath.Base(key))
	entries := []source.Entry{{Name: "sub", IsDir: true, ModTime: testScannedAt}}
	for i, n := range names {
		entries = append(entries, source.Entry{Name: n, Size: int64(100 + i), ModTime: testScannedAt})
	}
	ix.AddChildren(root, entries)
	return ix
}

func allPaths(ix *search.Index) []string {
	res := ix.Search("", search.Options{Limit: 1_000_000})
	paths := make([]string, 0, len(res.IDs))
	for _, id := range res.IDs {
		paths = append(paths, ix.Item(id).Path)
	}
	slices.Sort(paths)
	return paths
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	for _, n := range dirNames(t, dir) {
		if n != "session.json" && !strings.HasSuffix(n, ".sidx") {
			t.Errorf("stray file %q left in store dir", n)
		}
	}
}

func TestDefaultDirEndsWithScanImageIndex(t *testing.T) {
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	want := filepath.Join("ScanImage", "index")
	if !strings.HasSuffix(dir, want) {
		t.Errorf("DefaultDir = %q, want suffix %q", dir, want)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("DefaultDir = %q, want absolute path", dir)
	}
}

func TestSaveLoadRoundTripKeepsSearchRootsAndMeta(t *testing.T) {
	st, _ := newTestStore(t)
	ix := buildIndex(t, nasSource, "IMG_0001.JPG", "IMG_0002.CR2", "notes.txt")
	meta := search.Meta{Complete: true, ScannedAt: testScannedAt}

	if err := st.Save(nasSource, ix, meta); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, gotMeta, err := st.Load(nasSource)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !reflect.DeepEqual(allPaths(got), allPaths(ix)) {
		t.Errorf("paths = %v, want %v", allPaths(got), allPaths(ix))
	}
	if !reflect.DeepEqual(got.RootList(), ix.RootList()) {
		t.Errorf("roots = %v, want %v", got.RootList(), ix.RootList())
	}
	if gotMeta.Complete != meta.Complete || !gotMeta.ScannedAt.Equal(meta.ScannedAt) {
		t.Errorf("meta = %+v, want %+v", gotMeta, meta)
	}
	res := got.Search("img_0001", search.Options{})
	if res.Total != 1 {
		t.Errorf("search img_0001 total = %d, want 1", res.Total)
	}
}

func TestDifferentSourcesUseDifferentFilesNamedBySha1(t *testing.T) {
	st, dir := newTestStore(t)
	a := buildIndex(t, nasSource, "a.jpg")
	b := buildIndex(t, otherSource, "b.jpg", "c.jpg")

	if err := st.Save(nasSource, a, search.Meta{ScannedAt: testScannedAt}); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	if err := st.Save(otherSource, b, search.Meta{ScannedAt: testScannedAt}); err != nil {
		t.Fatalf("Save b: %v", err)
	}

	for _, src := range []string{nasSource, otherSource} {
		sum := sha1.Sum([]byte(src))
		want := filepath.Join(dir, hex.EncodeToString(sum[:])+".sidx")
		if got := st.Path(src); got != want {
			t.Errorf("Path(%q) = %q, want %q", src, got, want)
		}
		if _, err := os.Stat(want); err != nil {
			t.Errorf("snapshot file for %q missing: %v", src, err)
		}
	}
	if st.Path(nasSource) == st.Path(otherSource) {
		t.Fatal("two sources share one file")
	}
	gotA, _, err := st.Load(nasSource)
	if err != nil {
		t.Fatalf("Load a: %v", err)
	}
	gotB, _, err := st.Load(otherSource)
	if err != nil {
		t.Fatalf("Load b: %v", err)
	}
	if reflect.DeepEqual(allPaths(gotA), allPaths(gotB)) {
		t.Error("sources returned identical contents")
	}
}

func TestLoadWithoutSnapshotReturnsErrNotExist(t *testing.T) {
	st, _ := newTestStore(t)

	_, _, err := st.Load(nasSource)

	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load err = %v, want os.ErrNotExist", err)
	}
}

func TestLoadCorruptSnapshotReturnsErrorThatIsNotErrNotExist(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.Save(nasSource, buildIndex(t, nasSource, "a.jpg", "b.jpg"), search.Meta{ScannedAt: testScannedAt}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	good, err := os.ReadFile(st.Path(nasSource))
	if err != nil {
		t.Fatal(err)
	}

	flipped := slices.Clone(good)
	flipped[len(flipped)/2] ^= 0xFF
	tests := []struct {
		name string
		data []byte
	}{
		{"garbage", []byte("this is not a snapshot at all")},
		{"empty file", nil},
		{"truncated", good[:len(good)/2]},
		{"flipped byte", flipped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(st.Path(nasSource), tt.data, 0o600); err != nil {
				t.Fatal(err)
			}

			ix, _, err := st.Load(nasSource)

			if err == nil {
				t.Fatalf("Load succeeded with %d entries, want error", ix.Len())
			}
			if errors.Is(err, os.ErrNotExist) {
				t.Errorf("corrupt file reported as ErrNotExist: %v", err)
			}
		})
	}
}

// Seam under test: SaveWith(source, write) runs write against the temp
// file; Save is SaveWith with search.Write.
func TestSaveWithFailureMidwayKeepsPreviousSnapshotAndLeavesNoTempFile(t *testing.T) {
	st, dir := newTestStore(t)
	old := buildIndex(t, nasSource, "old1.jpg", "old2.jpg")
	if err := st.Save(nasSource, old, search.Meta{Complete: true, ScannedAt: testScannedAt}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := os.ReadFile(st.Path(nasSource))
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("disk full")

	err = st.SaveWith(nasSource, func(w io.Writer) error {
		if _, werr := w.Write([]byte("partial garbage")); werr != nil {
			return werr
		}
		return boom
	})

	if !errors.Is(err, boom) {
		t.Fatalf("SaveWith err = %v, want wrapping %v", err, boom)
	}
	after, err := os.ReadFile(st.Path(nasSource))
	if err != nil {
		t.Fatalf("read after failed save: %v", err)
	}
	if string(after) != string(before) {
		t.Error("previous snapshot bytes changed by failed save")
	}
	got, _, err := st.Load(nasSource)
	if err != nil {
		t.Fatalf("Load after failed save: %v", err)
	}
	if !reflect.DeepEqual(allPaths(got), allPaths(old)) {
		t.Errorf("paths = %v, want %v", allPaths(got), allPaths(old))
	}
	assertNoTempFiles(t, dir)
}

func TestSaveWithFailureOnFirstSaveLeavesNoSnapshot(t *testing.T) {
	st, dir := newTestStore(t)

	err := st.SaveWith(nasSource, func(w io.Writer) error {
		_, _ = w.Write([]byte("partial"))
		return errors.New("boom")
	})

	if err == nil {
		t.Fatal("SaveWith returned nil, want error")
	}
	if _, _, lerr := st.Load(nasSource); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("Load err = %v, want os.ErrNotExist", lerr)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveLeavesNoTempFilesBehind(t *testing.T) {
	st, dir := newTestStore(t)

	for i := 0; i < 3; i++ {
		ix := buildIndex(t, nasSource, fmt.Sprintf("f%d.jpg", i))
		if err := st.Save(nasSource, ix, search.Meta{ScannedAt: testScannedAt}); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}

	names := dirNames(t, dir)
	want := []string{filepath.Base(st.Path(nasSource))}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("dir entries = %v, want %v", names, want)
	}
}

func TestConcurrentSavesAreSerialisedAndLeaveValidSnapshot(t *testing.T) {
	st, dir := newTestStore(t)
	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ix := buildIndex(t, nasSource, fmt.Sprintf("writer%d.jpg", i))
			errs <- st.Save(nasSource, ix, search.Meta{Complete: true, ScannedAt: testScannedAt})
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("Save: %v", err)
		}
	}
	got, meta, err := st.Load(nasSource)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !meta.Complete {
		t.Error("meta.Complete = false, want true")
	}
	if got.Search("writer", search.Options{FilesOnly: true}).Total != 1 {
		t.Errorf("final snapshot is not exactly one writer's index: %v", allPaths(got))
	}
	assertNoTempFiles(t, dir)
}

type call struct {
	name string
	args []string
}

func recorder(calls *[]call, err error) func(context.Context, string, ...string) ([]byte, error) {
	var mu sync.Mutex
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		*calls = append(*calls, call{name, slices.Clone(args)})
		return []byte("tmutil output"), err
	}
}

func TestNewStoreRunsTmutilExactlyOnceWhenDirectoryIsCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ScanImage", "index")
	var calls []call

	st, err := NewStore(dir, recorder(&calls, nil))

	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if st == nil {
		t.Fatal("NewStore returned nil store")
	}
	if fi, serr := os.Stat(dir); serr != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", serr)
	}
	want := []call{{"tmutil", []string{"addexclusion", dir}}}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("runner calls = %v, want %v", calls, want)
	}
}

func TestNewStoreDoesNotRunTmutilWhenDirectoryAlreadyExists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ScanImage", "index")
	var first []call
	if _, err := NewStore(dir, recorder(&first, nil)); err != nil {
		t.Fatalf("first NewStore: %v", err)
	}
	var second []call

	if _, err := NewStore(dir, recorder(&second, nil)); err != nil {
		t.Fatalf("second NewStore: %v", err)
	}

	if len(first) != 1 {
		t.Fatalf("first open ran tmutil %d times, want 1", len(first))
	}
	if len(second) != 0 {
		t.Errorf("reopen ran tmutil %d times, want 0: %v", len(second), second)
	}
}

func TestNewStoreSwallowsTmutilFailureAndStillWorks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ScanImage", "index")
	var calls []call

	st, err := NewStore(dir, recorder(&calls, errors.New("tmutil: not found")))

	if err != nil {
		t.Fatalf("NewStore failed because of tmutil: %v", err)
	}
	if len(calls) != 1 {
		t.Errorf("tmutil calls = %d, want 1", len(calls))
	}
	if err := st.Save(nasSource, buildIndex(t, nasSource, "a.jpg"), search.Meta{ScannedAt: testScannedAt}); err != nil {
		t.Fatalf("Save after tmutil failure: %v", err)
	}
	if _, _, err := st.Load(nasSource); err != nil {
		t.Errorf("Load after tmutil failure: %v", err)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	st, _ := newTestStore(t)
	want := Session{
		Mode:   1,
		Source: "smb://nas.local",
		Host:   "nas.local",
		User:   "alice",
		Shares: []SessionShare{
			{Key: "smb://nas.local/Photos", Share: "Photos"},
			{Key: "smb://nas.local/Raw", Share: "Raw"},
		},
	}

	if err := st.SaveSession(want); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	got, err := st.LoadSession()

	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("session = %+v, want %+v", got, want)
	}
}

func TestLoadSessionWithoutFileReturnsErrNotExist(t *testing.T) {
	st, _ := newTestStore(t)

	_, err := st.LoadSession()

	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadSession err = %v, want os.ErrNotExist", err)
	}
}

func TestClearSessionRemovesSavedSession(t *testing.T) {
	st, dir := newTestStore(t)
	if err := st.SaveSession(Session{Source: otherSource}); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	if err := st.ClearSession(); err != nil {
		t.Fatalf("ClearSession: %v", err)
	}

	if _, err := st.LoadSession(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("LoadSession after clear err = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "session.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("session.json still present: %v", err)
	}
}

func TestClearSessionWithoutSessionIsNotAnError(t *testing.T) {
	st, _ := newTestStore(t)

	if err := st.ClearSession(); err != nil {
		t.Fatalf("ClearSession on empty store: %v", err)
	}
}

func TestClearSessionKeepsSnapshots(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.Save(nasSource, buildIndex(t, nasSource, "a.jpg"), search.Meta{ScannedAt: testScannedAt}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := st.SaveSession(Session{Source: nasSource}); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	if err := st.ClearSession(); err != nil {
		t.Fatalf("ClearSession: %v", err)
	}

	if _, _, err := st.Load(nasSource); err != nil {
		t.Errorf("snapshot gone after ClearSession: %v", err)
	}
}

func TestLoadSessionMalformedJSONReturnsErrorThatIsNotErrNotExist(t *testing.T) {
	st, dir := newTestStore(t)
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"Mode": `), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := st.LoadSession()

	if err == nil {
		t.Fatal("LoadSession succeeded on malformed JSON")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("malformed session reported as ErrNotExist: %v", err)
	}
}

func TestSaveSessionOverwritesPreviousSession(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.SaveSession(Session{Source: "first"}); err != nil {
		t.Fatal(err)
	}

	if err := st.SaveSession(Session{Source: "second", Shares: []SessionShare{{Key: "k", Share: "s"}}}); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	got, err := st.LoadSession()

	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got.Source != "second" || len(got.Shares) != 1 {
		t.Errorf("session = %+v, want second with 1 share", got)
	}
}
