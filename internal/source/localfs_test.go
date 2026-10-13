package source

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeDirEntry struct {
	name    string
	dir     bool
	size    int64
	infoErr error
}

func (f fakeDirEntry) Name() string { return f.name }
func (f fakeDirEntry) IsDir() bool  { return f.dir }
func (f fakeDirEntry) Type() fs.FileMode {
	if f.dir {
		return fs.ModeDir
	}
	return 0
}
func (f fakeDirEntry) Info() (fs.FileInfo, error) {
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	return fakeInfo{f}, nil
}

type fakeInfo struct{ e fakeDirEntry }

func (i fakeInfo) Name() string       { return i.e.name }
func (i fakeInfo) Size() int64        { return i.e.size }
func (i fakeInfo) Mode() fs.FileMode  { return i.e.Type() }
func (i fakeInfo) ModTime() time.Time { return time.Unix(1_700_000_000, 0) }
func (i fakeInfo) IsDir() bool        { return i.e.dir }
func (i fakeInfo) Sys() any           { return nil }

// stubReadDir swaps the package-level readDir seam (type
// func(string) ([]os.DirEntry, error), defaulting to os.ReadDir).
func stubReadDir(t *testing.T, des ...fakeDirEntry) {
	t.Helper()
	orig := readDir
	t.Cleanup(func() { readDir = orig })
	readDir = func(string) ([]os.DirEntry, error) {
		out := make([]os.DirEntry, len(des))
		for i, d := range des {
			out[i] = d
		}
		return out, nil
	}
}

func TestReadDirFailsWhenInfoErrorsOtherThanNotExist(t *testing.T) {
	stubReadDir(t,
		fakeDirEntry{name: "a.jpg", size: 3},
		fakeDirEntry{name: "b.jpg", infoErr: errors.New("smb: connection reset")},
		fakeDirEntry{name: "c.jpg", size: 5},
	)

	got, err := LocalFS{}.ReadDir(context.Background(), "/x")
	if err == nil {
		t.Fatalf("ReadDir succeeded with %d entries; a partial listing must not look complete", len(got))
	}
	if got != nil {
		t.Errorf("entries returned alongside error: %+v", got)
	}
}

func TestReadDirSkipsEntryDeletedBetweenReadDirAndInfo(t *testing.T) {
	dir := t.TempDir()
	gone := &fs.PathError{Op: "lstat", Path: filepath.Join(dir, "b.jpg"), Err: fs.ErrNotExist}
	stubReadDir(t,
		fakeDirEntry{name: "a.jpg", size: 3},
		fakeDirEntry{name: "b.jpg", infoErr: gone},
		fakeDirEntry{name: "sub", dir: true},
	)

	got, err := LocalFS{}.ReadDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(got) != 2 || got[0].Name != "a.jpg" || got[0].Size != 3 || got[1].Name != "sub" || !got[1].IsDir {
		t.Fatalf("entries %+v, want a.jpg and sub only", got)
	}
}

func TestReadDirFailsWhenDirectoryDisappearedWhileEntriesWereSkipped(t *testing.T) {
	gone := &fs.PathError{Op: "lstat", Err: fs.ErrNotExist}
	stubReadDir(t,
		fakeDirEntry{name: "a.jpg", size: 3},
		fakeDirEntry{name: "b.jpg", infoErr: gone},
	)

	got, err := LocalFS{}.ReadDir(context.Background(), filepath.Join(t.TempDir(), "unmounted"))

	if err == nil {
		t.Fatalf("ReadDir succeeded with %+v although the directory itself is gone", got)
	}
}

func TestReadDirFailsWhenEveryEntryVanished(t *testing.T) {
	gone := &fs.PathError{Op: "lstat", Err: fs.ErrNotExist}
	stubReadDir(t,
		fakeDirEntry{name: "a.jpg", infoErr: gone},
		fakeDirEntry{name: "b.jpg", infoErr: gone},
	)

	got, err := LocalFS{}.ReadDir(context.Background(), t.TempDir())

	if err == nil {
		t.Fatalf("ReadDir returned %+v with no error although every entry vanished", got)
	}
}

func TestReadDirKeepsRealFileDeletedMidListing(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orig := readDir
	t.Cleanup(func() { readDir = orig })
	readDir = func(p string) ([]os.DirEntry, error) {
		des, err := orig(p)
		_ = os.Remove(filepath.Join(dir, "b.jpg"))
		return des, err
	}

	got, err := LocalFS{}.ReadDir(context.Background(), dir)

	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(got) != 2 || got[0].Name != "a.jpg" || got[1].Name != "c.jpg" {
		t.Errorf("entries %+v, want a.jpg and c.jpg", got)
	}
}
