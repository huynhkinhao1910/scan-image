// Package indexer persists search indexes to local disk and drives scans
// that keep them fresh.
package indexer

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/isuccesscorp/scan-image/internal/netshare"
	"github.com/isuccesscorp/scan-image/internal/search"
)

const (
	snapshotExt = ".sidx"
	sessionFile = "session.json"
	tempPattern = "tmp-*.part"
	ioBufSize   = 1 << 20

	tmutilTimeout = 5 * time.Second
)

// Store keeps one snapshot per source plus the last session, all in a single
// local directory.
type Store struct {
	dir string
	// snapMu serialises snapshot writes, sessMu session writes; they are
	// separate so a session save never waits for a multi-second snapshot.
	snapMu sync.Mutex
	sessMu sync.Mutex
}

// Session is what the app restores on the next launch.
type Session struct {
	Mode   int
	Source string
	Host   string
	User   string
	Shares []SessionShare
}

// SessionShare is one mounted share belonging to a Session.
type SessionShare struct {
	Key, Share string
}

// DefaultDir returns the per-user directory for snapshots.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ScanImage", "index"), nil
}

// NewStore creates dir if needed. A freshly created directory is excluded from
// Time Machine on a best-effort basis. A nil run uses netshare.Exec.
func NewStore(dir string, run netshare.Runner) (*Store, error) {
	if run == nil {
		run = netshare.Exec
	}
	_, statErr := os.Stat(dir)
	existed := statErr == nil
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create index dir: %w", err)
	}
	if !existed {
		ctx, cancel := context.WithTimeout(context.Background(), tmutilTimeout)
		_, _ = run(ctx, "tmutil", "addexclusion", dir)
		cancel()
	}
	removeStaleTemps(dir)
	return &Store{dir: dir}, nil
}

// removeStaleTemps deletes temp files a crash left mid-save; they are
// hundreds of MB each. A file that cannot be removed is left for the next
// start: the store works without the cleanup.
func removeStaleTemps(dir string) {
	stale, err := filepath.Glob(filepath.Join(dir, tempPattern))
	if err != nil {
		return
	}
	for _, p := range stale {
		_ = os.Remove(p)
	}
}

// Path returns the snapshot file for a source.
func (s *Store) Path(source string) string {
	sum := sha1.Sum([]byte(source))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+snapshotExt)
}

// Save writes ix and m as the snapshot of source, atomically.
func (s *Store) Save(source string, ix *search.Index, m search.Meta) error {
	return s.SaveWith(source, func(w io.Writer) error { return search.Write(w, ix, m) })
}

// SaveWith runs write against a temp file and renames it over the snapshot
// of source only if write and all file operations succeed.
func (s *Store) SaveWith(source string, write func(io.Writer) error) error {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	return s.replace(s.Path(source), write)
}

// Load reads the snapshot of source. A missing snapshot satisfies
// errors.Is(err, os.ErrNotExist); a damaged one wraps search.ErrCorrupt.
func (s *Store) Load(source string) (*search.Index, search.Meta, error) {
	f, err := os.Open(s.Path(source))
	if err != nil {
		return nil, search.Meta{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, search.Meta{}, err
	}
	return search.Read(f, fi.Size())
}

// SaveSession persists sess, replacing any previous one.
func (s *Store) SaveSession(sess Session) error {
	b, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	return s.replace(filepath.Join(s.dir, sessionFile), func(w io.Writer) error {
		_, err := w.Write(b)
		return err
	})
}

// LoadSession returns the saved session; a missing one satisfies
// errors.Is(err, os.ErrNotExist).
func (s *Store) LoadSession() (Session, error) {
	var sess Session
	b, err := os.ReadFile(filepath.Join(s.dir, sessionFile))
	if err != nil {
		return sess, err
	}
	if err := json.Unmarshal(b, &sess); err != nil {
		return Session{}, fmt.Errorf("decode session: %w", err)
	}
	return sess, nil
}

// ClearSession deletes the saved session. Snapshots are kept.
func (s *Store) ClearSession() error {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	err := os.Remove(filepath.Join(s.dir, sessionFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) replace(dest string, write func(io.Writer) error) (err error) {
	tmp, err := os.CreateTemp(s.dir, tempPattern)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	bw := bufio.NewWriterSize(tmp, ioBufSize)
	if err = write(bw); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(dest), err)
	}
	if err = bw.Flush(); err != nil {
		return fmt.Errorf("flush %s: %w", filepath.Base(dest), err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", filepath.Base(dest), err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(dest), err)
	}
	if err = os.Rename(tmp.Name(), dest); err != nil {
		return fmt.Errorf("rename %s: %w", filepath.Base(dest), err)
	}
	return nil
}
