package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/isuccesscorp/scan-image/internal/netshare"
	"github.com/isuccesscorp/scan-image/internal/scanner"
	"github.com/isuccesscorp/scan-image/internal/search"
	"github.com/isuccesscorp/scan-image/internal/source"
)

// Session.Mode values.
const (
	ModeLocal = iota
	ModeServer
)

// Mode selects how much a scan re-lists.
type Mode int

const (
	// recent is the quick pass between two periodic scans: it lists only
	// what changed lately (see search.BuildOptions.Recent). Not for callers.
	recent Mode = iota - 1
	// Incremental reuses unchanged leaf directories and cold branches of the
	// old index (see search.BuildOptions.Reuse).
	Incremental
	// Full lists every directory.
	Full
)

const (
	defaultInterval = 30 * time.Minute
	// Measured on the designers' share (2.9M entries, 4-10 listings a second):
	// 3 days of changes are about 700 listings, 7 days about 1,500, which no
	// longer fits between two passes.
	defaultRecentEvery = 5 * time.Minute
	defaultRecentAge   = 3 * 24 * time.Hour
	defaultCheckpoint  = 5 * time.Minute
	defaultCloseWait   = 5 * time.Second
	defaultVerifySlots = 64
	defaultStall       = 5 * time.Minute
)

// Options configure a Service. Only Source is required.
type Options struct {
	// Source lists directories. Required.
	Source source.Source
	// Net mounts shares of a server source; nil means netshare.New().
	Net *netshare.Client
	// Reachable tells whether the server answers; nil means netshare.Reachable.
	Reachable func(ctx context.Context, host string) bool
	// Workers is the scanner's parallelism; 0 means the scanner default.
	Workers int
	// Interval between background scans; 0 means 30 minutes, negative
	// disables them.
	Interval time.Duration
	// RecentInterval is the gap between quick passes over what changed
	// within RecentAge, run between two background scans; 0 means 5 minutes,
	// negative disables them. They need background scans (Interval > 0).
	RecentInterval time.Duration
	// RecentAge is how far back a quick pass looks; 0 means 3 days.
	RecentAge time.Duration
	// Checkpoint is how often the first scan saves its partial index; 0
	// means 5 minutes.
	Checkpoint time.Duration
	// CloseWait bounds how long Close waits for the running scan to wind
	// down; 0 means 5 seconds.
	CloseWait time.Duration
	// StallTimeout cancels a scan in which no directory finished for this
	// long (a ReadDir hung on a dead mount); 0 means 5 minutes, negative
	// turns the watchdog off.
	StallTimeout time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// VerifySlots is the rotation size of the forced re-listing (see
	// search.BuildOptions); 0 means 64.
	VerifySlots uint32
	// Notify is called after every visible state change, never while the
	// Service holds a lock. It may call State.
	Notify func()
}

// RootInfo describes one root for display. Name is the share name for a
// server source and the folder name for a local one.
type RootInfo struct {
	Name, Path string
}

// State is a value snapshot for the UI.
type State struct {
	Scanning bool
	// Offline is set when the server was unreachable at the start of the
	// last scan or went away during it. The saved index stays in use.
	Offline  bool
	Progress scanner.Progress
	Stats    search.BuildStats
	// CheckedAt is when a scan last ran to the end.
	CheckedAt time.Time
	// Complete reports that the index comes from a scan that ran to the end.
	Complete bool
	// Gen increases every time Current() returns a different index.
	Gen   int
	Roots []RootInfo
	// SaveErr is the last failure to write the snapshot; the in-memory
	// index is still the new one. It clears on the next successful save.
	SaveErr error
	// ScanErr holds problems of the last scan: roots that were not examined
	// (wraps ErrRootNotChecked and names them), a stall (wraps ErrScanStalled)
	// or a failure before it began. Failures of single directories are not
	// here; they count in Progress.Errors.
	ScanErr error
	// LoadErr is set when a snapshot existed but could not be used; the
	// index then started empty.
	LoadErr error
	// FailedDirs names the first directories the last finished scan could
	// not list, each as "path: reason" (at most maxFailedDirs; the full
	// count is Progress.Errors). Their old contents, if any, were kept.
	FailedDirs []string
}

const maxFailedDirs = 20

var errClosed = errors.New("indexer: closed")

var (
	// ErrRootNotChecked is wrapped by State().ScanErr when a scan did not
	// really examine every root: its share was not mounted, its root could
	// not be listed, or it came back empty while the old index had content.
	// CheckedAt does not move then.
	ErrRootNotChecked = errors.New("indexer: root not checked")
	// ErrScanStalled is wrapped by State().ScanErr when the scan was
	// cancelled because no directory finished within Options.StallTimeout.
	ErrScanStalled = errors.New("indexer: scan stalled")
)

// request is a scan asked for while another one runs.
type request struct {
	mode       Mode
	background bool
}

// merge combines two requests into one that satisfies both: Full beats
// Incremental and a user request (which may retry mounts) beats a background
// one.
func (r request) merge(o request) request {
	if o.mode > r.mode {
		r.mode = o.mode
	}
	r.background = r.background && o.background
	return r
}

// Service keeps the index of one source fresh. Locking: mu guards the
// fields below it and is never held across I/O or Notify; saveMu serialises
// writes to the store with the closed flag, so nothing is written once Close
// has returned.
type Service struct {
	store *Store
	sess  Session
	opt   Options

	closed atomic.Bool
	saveMu sync.Mutex
	loops  sync.WaitGroup
	quit   chan struct{}

	mu          sync.Mutex
	stopping    bool
	cur         *search.Index
	gen         int
	complete    bool
	checkedAt   time.Time
	offline     bool
	run         *runHandle
	active      *scan
	progress    scanner.Progress
	stats       search.BuildStats
	saveErr     error
	unsaved     bool // a recent pass changed the index and left the snapshot alone
	scanErr     error
	loadErr     error
	failedDirs  []string
	mountFailed map[string]struct{}
	kick        *time.Timer
	pending     *request
}

type runHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
	recent bool // asked for as a quick pass
}

// Open loads the snapshot of sess.Source so Current() is searchable at once.
// A missing snapshot gives an empty index; a damaged one is discarded and
// reported in State().LoadErr. Open never lists a directory.
func Open(store *Store, sess Session, opt Options) (*Service, error) {
	if opt.Source == nil {
		return nil, errors.New("indexer: Options.Source is required")
	}
	if opt.Net == nil {
		opt.Net = netshare.New()
	}
	if opt.Reachable == nil {
		opt.Reachable = netshare.Reachable
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Interval == 0 {
		opt.Interval = defaultInterval
	}
	if opt.RecentInterval == 0 {
		opt.RecentInterval = defaultRecentEvery
	}
	if opt.RecentAge <= 0 {
		opt.RecentAge = defaultRecentAge
	}
	if opt.Checkpoint <= 0 {
		opt.Checkpoint = defaultCheckpoint
	}
	if opt.StallTimeout == 0 {
		opt.StallTimeout = defaultStall
	}
	if opt.CloseWait <= 0 {
		opt.CloseWait = defaultCloseWait
	}
	if opt.VerifySlots == 0 {
		opt.VerifySlots = defaultVerifySlots
	}
	s := &Service{
		store: store, sess: sess, opt: opt,
		quit:        make(chan struct{}),
		mountFailed: map[string]struct{}{},
	}
	ix, meta, err := store.Load(sess.Source)
	switch {
	case err == nil:
		s.cur, s.complete, s.checkedAt = ix, meta.Complete, meta.ScannedAt
	case errors.Is(err, os.ErrNotExist):
		s.cur = search.New()
	default:
		s.cur = search.New()
		s.loadErr = fmt.Errorf("discarded snapshot: %w", err)
	}
	if opt.Interval > 0 {
		s.loops.Add(1)
		go s.tick()
	}
	return s, nil
}

// Current returns the index to search. It is never nil.
func (s *Service) Current() *search.Index {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// State returns a snapshot of the service for the UI.
func (s *Service) State() State {
	s.mu.Lock()
	st := State{
		Scanning: s.run != nil, Offline: s.offline,
		Progress: s.progress, Stats: s.stats,
		CheckedAt: s.checkedAt, Complete: s.complete, Gen: s.gen,
		SaveErr: s.saveErr, ScanErr: s.scanErr, LoadErr: s.loadErr,
		FailedDirs: slices.Clone(s.failedDirs),
	}
	cur, act := s.cur, s.active
	s.mu.Unlock()
	if act != nil && !act.recent && act.live.Load() {
		st.Progress, st.Stats = act.sc.Progress(), act.b.Stats()
	}
	for _, r := range cur.RootList() {
		st.Roots = append(st.Roots, RootInfo{Name: filepath.Base(r.Key), Path: r.Path})
	}
	return st
}

// Scan starts a user-initiated scan in the background. While a scan runs the
// request is remembered (one slot) and starts when that scan ends; after
// Close it is ignored.
func (s *Service) Scan(m Mode) { s.start(request{m, false}, true) }

// Kick starts a background incremental scan after delay, replacing any
// earlier Kick that has not fired. If a scan runs when it fires, it is
// remembered like Scan.
func (s *Service) Kick(delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return
	}
	if s.kick != nil {
		s.kick.Stop()
	}
	s.kick = time.AfterFunc(delay, func() { s.start(request{Incremental, true}, true) })
}

// Stop cancels the running scan and forgets any remembered request. It
// returns at once; the scan finishes its bookkeeping (keep old data, swap,
// save) in the background.
func (s *Service) Stop() {
	s.mu.Lock()
	s.pending = nil
	h := s.run
	s.mu.Unlock()
	if h != nil {
		h.cancel()
	}
}

// Close stops the scan and the schedule, waits up to Options.CloseWait for
// the scan to wind down, and guarantees that nothing is written to the store
// after it returns. It is idempotent.
func (s *Service) Close() {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.stopping = true
	s.pending = nil
	if s.kick != nil {
		s.kick.Stop()
	}
	h := s.run
	s.mu.Unlock()
	close(s.quit)
	s.loops.Wait()
	if h != nil {
		h.cancel()
		select {
		case <-h.done:
		case <-time.After(s.opt.CloseWait):
		}
	}
	s.closed.Store(true)
	// An in-flight save aborts at its next write once closed is set; this
	// waits for it to unwind.
	s.saveMu.Lock()
	s.saveMu.Unlock() //nolint:staticcheck // barrier, not a critical section
	s.mu.Lock()
	s.run, s.active = nil, nil
	s.mu.Unlock()
}

// tick starts the background scans: a usual one every Options.Interval and,
// when Options.RecentInterval is shorter, quick passes in between. A usual
// scan that could not start because a quick pass was running is tried again
// at the next step; one that met another scan counts that scan as its own.
func (s *Service) tick() {
	defer s.loops.Done()
	every, step := s.opt.Interval, s.opt.Interval
	if r := s.opt.RecentInterval; r > 0 && r < every {
		step = r
	}
	due := time.Now().Add(every - step/2)
	t := time.NewTicker(step)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case now := <-t.C:
			mode := recent
			if !now.Before(due) {
				mode = Incremental
			}
			started := s.start(request{mode, true}, false)
			if mode == Incremental && (started || !s.recentRunning()) {
				due = now.Add(every - step/2)
			}
		}
	}
}

func (s *Service) recentRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run != nil && s.run.recent
}

func (s *Service) notify() {
	if s.opt.Notify != nil && !s.closed.Load() {
		s.opt.Notify()
	}
}

// save writes the snapshot unless the service is closed, in which case it
// returns errClosed and writes nothing.
func (s *Service) save(ix *search.Index, m search.Meta) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if s.closed.Load() {
		return errClosed
	}
	return s.store.SaveWith(s.sess.Source, func(w io.Writer) error {
		return search.Write(&abortWriter{w: w, closed: &s.closed}, ix, m)
	})
}

// recordSave keeps a save outcome in State. errClosed is dropped on purpose:
// the service is gone and nobody reads its state. A successful save replaced
// whatever file failed to load, so LoadErr clears.
func (s *Service) recordSave(err error) {
	if errors.Is(err, errClosed) {
		return
	}
	s.mu.Lock()
	s.saveErr = err
	if err == nil {
		s.loadErr = nil
	}
	s.mu.Unlock()
}

// abortWriter fails writes once the service is closed, so a long save ends
// before its rename.
type abortWriter struct {
	w      io.Writer
	closed *atomic.Bool
}

func (a *abortWriter) Write(p []byte) (int, error) {
	if a.closed.Load() {
		return 0, errClosed
	}
	return a.w.Write(p)
}
