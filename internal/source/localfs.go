package source

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
)

// readDir is os.ReadDir, replaceable in tests.
var readDir = os.ReadDir

// LocalFS reads a directory tree through the operating system, which covers
// a NAS mounted with Cmd+K (SMB/AFP/NFS) under /Volumes.
type LocalFS struct {
	// SkipHidden drops dot-files and macOS metadata (.DS_Store, ._*).
	SkipHidden bool
	// Bulk lists with getattrlistbulk(2) on macOS, fetching name, type, size
	// and mtime in batches. Elsewhere it is ignored and readdir is used.
	Bulk bool
}

// ReadDir lists path. An entry that vanished since the listing is skipped; any
// other failure to read an entry's metadata fails the whole directory, so a
// partial listing is never mistaken for a complete one.
func (l LocalFS) ReadDir(ctx context.Context, path string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.Bulk && bulkSupported {
		return listBulk(ctx, path, l.SkipHidden)
	}
	des, err := readDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	visible, vanished := 0, 0
	for _, de := range des {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := de.Name()
		if l.SkipHidden && strings.HasPrefix(name, ".") {
			continue
		}
		visible++
		info, err := de.Info()
		if errors.Is(err, fs.ErrNotExist) {
			vanished++
			continue
		}
		if err != nil {
			return nil, err
		}
		e := Entry{Name: name, ModTime: info.ModTime(), IsDir: de.IsDir()}
		if !e.IsDir {
			e.Size = info.Size()
		}
		out = append(out, e)
	}
	if err := confirmListing(path, visible, vanished); err != nil {
		return nil, err
	}
	return out, nil
}

// confirmListing rejects a listing made short by vanished entries unless the
// directory itself is still there and not every entry vanished: when a mount
// dies mid-listing each remaining lstat answers ENOENT, which must not pass
// for a successful, shorter directory.
func confirmListing(path string, visible, vanished int) error {
	if vanished == 0 {
		return nil
	}
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	if vanished == visible {
		return &fs.PathError{Op: "readdir", Path: path, Err: errAllVanished}
	}
	return nil
}

var errAllVanished = errors.New("every entry vanished while listing")

// Open opens a file for reading.
func (l LocalFS) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.Open(path)
}
