package source

import (
	"context"
	"io"
	"os"
	"strings"
)

// LocalFS reads a directory tree through the operating system, which covers
// a NAS mounted with Cmd+K (SMB/AFP/NFS) under /Volumes.
type LocalFS struct {
	// SkipHidden drops dot-files and macOS metadata (.DS_Store, ._*).
	SkipHidden bool
}

// ReadDir lists path. Entries whose metadata cannot be read are skipped
// rather than failing the whole directory.
func (l LocalFS) ReadDir(ctx context.Context, path string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	des, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := de.Name()
		if l.SkipHidden && strings.HasPrefix(name, ".") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{
			Name:    name,
			Size:    info.Size(),
			ModTime: info.ModTime(),
			IsDir:   de.IsDir(),
		})
	}
	return out, nil
}

// Open opens a file for reading.
func (l LocalFS) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.Open(path)
}
