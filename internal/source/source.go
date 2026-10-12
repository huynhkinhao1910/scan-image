// Package source abstracts where file metadata comes from.
//
// The scanner and UI only depend on the Source interface, so a future
// "host mode" (another machine serving the index) is a new adapter, not a
// rewrite.
package source

import (
	"context"
	"io"
	"time"
)

// Entry is one item of a directory listing.
type Entry struct {
	Name    string
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// Source lists directories and opens files.
type Source interface {
	ReadDir(ctx context.Context, path string) ([]Entry, error)
	Open(ctx context.Context, path string) (io.ReadCloser, error)
}
