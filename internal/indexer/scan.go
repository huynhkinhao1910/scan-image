package indexer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/isuccesscorp/scan-image/internal/scanner"
	"github.com/isuccesscorp/scan-image/internal/search"
	"github.com/isuccesscorp/scan-image/internal/source"
)

// recheckDebounce is the minimum gap between two reachability re-checks
// triggered by read errors, so a burst of errors costs one probe.
const recheckDebounce = 10 * time.Second

// scan is one run: the builder, the scanner and the Sink between them.
type scan struct {
	svc     *Service
	ctx     context.Context
	cancel  context.CancelFunc
	b       *search.Builder
	sc      *scanner.Scanner
	watch   bool // server source: watch for network loss
	recent  bool // a quick pass: State keeps showing the last usual scan
	live    atomic.Bool
	listed  atomic.Int64
	offline atomic.Bool

	roots   map[uint32]string // id -> key of every root handed to the scanner
	bad     map[uint32]struct{}
	failed  []string     // first directories that could not be listed, with why
	beat    atomic.Int64 // unix nanos of the last Listed or Failed
	stalled atomic.Bool

	netMu   sync.Mutex
	netBusy bool
	netAt   time.Time
	netWG   sync.WaitGroup
}

// outcome is what runScan hands to endScan.
type outcome struct {
	b        *search.Builder
	ranToEnd bool
	offline  bool
	listed   int64
	progress scanner.Progress
	failed   []string
	warn     error
	err      error
	// unchecked wraps ErrRootNotChecked when a root was not examined.
	unchecked error
	// stalled marks a scan the watchdog cancelled.
	stalled bool
	// aborted marks a scan stopped before it began; it changes no state.
	aborted bool
	// recent marks a quick pass: it looked at part of the tree only, so it
	// does not move CheckedAt, replace what the last scan reported or write
	// the snapshot.
	recent bool
}

// start begins a scan unless the service is closing. While one runs, a queued
// request is remembered for when it ends and any other is dropped. It reports
// whether the request was started or remembered.
func (s *Service) start(req request, queue bool) bool {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return false
	}
	if s.run != nil {
		if queue {
			if s.pending != nil {
				req = s.pending.merge(req)
			}
			s.pending = &req
		}
		s.mu.Unlock()
		return queue
	}
	s.launchLocked(req)
	s.mu.Unlock()
	s.notify()
	return true
}

// launchLocked runs req in a new goroutine. s.mu is held and s.run is nil.
func (s *Service) launchLocked(req request) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &runHandle{cancel: cancel, done: make(chan struct{}), recent: req.mode == recent}
	s.run = h
	if req.mode != recent {
		s.scanErr = nil
	}
	go func() {
		defer close(h.done)
		defer cancel()
		s.endScan(s.runScan(ctx, cancel, req.mode, req.background))
	}()
}

func (s *Service) runScan(ctx context.Context, cancel context.CancelFunc, mode Mode, background bool) outcome {
	old := s.Current()
	p, err := s.prepare(ctx, old, background)
	if ctx.Err() != nil {
		return outcome{aborted: true}
	}
	if err != nil {
		return outcome{err: err}
	}
	if p.offline {
		return outcome{offline: true}
	}
	first := old.Len() == 0
	var base *search.Index
	if !first {
		base = old
	}
	s.mu.Lock()
	complete := s.complete
	s.mu.Unlock()
	if mode == recent && (first || !complete) {
		mode = Incremental
	}
	bo := search.BuildOptions{
		Reuse:       mode != Full,
		VerifySlot:  s.verifySlot(),
		VerifySlots: s.opt.VerifySlots,
		Now:         s.opt.Now,
	}
	if mode == recent {
		bo.Recent, bo.VerifySlots = s.opt.RecentAge, 0
	}
	b := search.NewBuilder(base, bo)
	r := &scan{
		svc: s, ctx: ctx, cancel: cancel, b: b,
		sc:    &scanner.Scanner{Src: s.opt.Source, Workers: s.opt.Workers},
		watch: s.sess.Mode == ModeServer, recent: mode == recent,
		roots: map[uint32]string{}, bad: map[uint32]struct{}{},
	}
	r.beat.Store(time.Now().UnixNano())
	roots := r.addRoots(p.targets)
	s.mu.Lock()
	s.active = r
	if first {
		s.cur = b.Index()
		s.gen++
	}
	s.mu.Unlock()
	if first {
		s.notify()
	}
	stopCheckpoints := func() {}
	if first {
		stopCheckpoints = s.checkpoints(b.Index())
	}
	stopWatch := r.watchStall(s.opt.StallTimeout)
	runErr := r.sc.Run(ctx, roots, r)
	stopWatch()
	stopCheckpoints()
	r.netWG.Wait()
	o := outcome{
		b: b, ranToEnd: runErr == nil, offline: r.offline.Load(),
		listed: r.listed.Load(), progress: r.sc.Progress(), failed: r.failed, warn: p.warn,
		recent: mode == recent,
	}
	switch {
	case r.stalled.Load():
		o.stalled = true
		o.err = fmt.Errorf("%w: no directory finished within %s", ErrScanStalled, s.opt.StallTimeout)
	case o.ranToEnd && !o.offline:
		o.unchecked = r.uncheckedErr(p.unmounted)
	}
	return o
}

// watchStall cancels the scan when no directory has finished for timeout. A
// non-positive timeout disables it. The returned func stops the watchdog.
func (r *scan) watchStall(timeout time.Duration) (stop func()) {
	if timeout <= 0 {
		return func() {}
	}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(max(timeout/4, time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				if time.Since(time.Unix(0, r.beat.Load())) >= timeout {
					r.stalled.Store(true)
					r.cancel()
					return
				}
			}
		}
	}()
	return func() { close(quit); <-done }
}

// uncheckedErr names the roots a finished scan did not examine: shares that
// were not mounted, roots whose own listing failed, and roots whose listing
// the builder rejected as an empty mountpoint.
func (r *scan) uncheckedErr(unmounted []string) error {
	keys := slices.Clone(unmounted)
	for id := range r.bad {
		keys = append(keys, r.roots[id])
	}
	if len(keys) == 0 {
		return nil
	}
	slices.Sort(keys)
	return fmt.Errorf("%w: %s", ErrRootNotChecked, strings.Join(slices.Compact(keys), ", "))
}

// addRoots registers every target in the builder. Unmounted ones are failed
// on the spot so their old data is kept without touching the network.
func (r *scan) addRoots(ts []target) []scanner.Dir {
	var dirs []scanner.Dir
	for _, t := range ts {
		id, prev := r.b.AddRoot(t.Key, t.Path)
		if !t.Mounted {
			r.b.Failed(id, prev)
			continue
		}
		r.roots[id] = t.Key
		dirs = append(dirs, scanner.Dir{Path: t.Path, ID: id, Prev: prev})
	}
	return dirs
}

// Listed implements scanner.Sink.
func (r *scan) Listed(d scanner.Dir, entries []source.Entry) []scanner.Dir {
	r.live.Store(true) // Run has started: its fields are safe to read now
	r.beat.Store(time.Now().UnixNano())
	r.listed.Add(1)
	kids := r.b.Listed(d.ID, d.Prev, entries)
	if _, isRoot := r.roots[d.ID]; isRoot && r.b.Rejected(d.ID) {
		r.bad[d.ID] = struct{}{}
	}
	out := make([]scanner.Dir, len(kids))
	for i, k := range kids {
		out[i] = scanner.Dir{Path: filepath.Join(d.Path, k.Name), ID: k.ID, Prev: k.Prev}
	}
	return out
}

// Failed implements scanner.Sink.
func (r *scan) Failed(d scanner.Dir, err error) {
	r.live.Store(true)
	r.beat.Store(time.Now().UnixNano())
	if _, isRoot := r.roots[d.ID]; isRoot {
		r.bad[d.ID] = struct{}{}
	}
	r.b.Failed(d.ID, d.Prev)
	r.noteFailed(d, err)
	if r.watch && looksLikeNetworkLoss(err) {
		r.recheckNetwork()
	}
}

// noteFailed remembers d for State.FailedDirs. Directories abandoned by a
// cancelled scan are not failures of the directory itself.
func (r *scan) noteFailed(d scanner.Dir, err error) {
	if err == nil || len(r.failed) == maxFailedDirs ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	reason := err
	var pe *fs.PathError
	if errors.As(err, &pe) {
		reason = pe.Err
	}
	r.failed = append(r.failed, d.Path+": "+reason.Error())
}

// recheckNetwork probes the host once per burst of errors, off the sink so
// the scan is not stalled. An unreachable host cancels the scan.
func (r *scan) recheckNetwork() {
	r.netMu.Lock()
	if r.netBusy || (!r.netAt.IsZero() && time.Since(r.netAt) < recheckDebounce) {
		r.netMu.Unlock()
		return
	}
	r.netBusy = true
	r.netWG.Add(1)
	r.netMu.Unlock()
	go func() {
		defer r.netWG.Done()
		up := r.svc.opt.Reachable(r.ctx, r.svc.sess.Host)
		r.netMu.Lock()
		r.netBusy, r.netAt = false, time.Now()
		r.netMu.Unlock()
		if !up && r.ctx.Err() == nil {
			r.offline.Store(true)
			r.cancel()
		}
	}()
}

func looksLikeNetworkLoss(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	for _, e := range []error{
		syscall.ETIMEDOUT, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ENOTCONN,
		syscall.EHOSTDOWN, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.ENETUNREACH,
		syscall.ESTALE, syscall.EIO, os.ErrDeadlineExceeded,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// verifySlot derives the rotating re-check slot from the clock, so no state
// has to be stored between runs.
func (s *Service) verifySlot() uint32 {
	step := int64(s.opt.Interval / time.Second)
	if step < 1 {
		step = int64(defaultInterval / time.Second)
	}
	return uint32(s.opt.Now().Unix() / step % int64(s.opt.VerifySlots))
}

// checkpoints saves a Clone of the growing index every Options.Checkpoint
// until the returned stop function is called.
func (s *Service) checkpoints(ix *search.Index) (stop func()) {
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(s.opt.Checkpoint)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				s.recordSave(s.save(ix.Clone(), search.Meta{ScannedAt: s.opt.Now()}))
			}
		}
	}()
	return func() { close(quit); <-done }
}

// endScan is the single exit of a scan: finished, cancelled or network lost.
func (s *Service) endScan(o outcome) {
	if o.b != nil {
		s.publish(o)
	}
	s.mu.Lock()
	if o.b != nil && !o.recent {
		s.progress, s.stats = o.progress, o.b.Stats()
		s.failedDirs = o.failed
	}
	switch {
	case o.offline:
		s.offline = true
	case o.stalled, o.err == nil && !o.aborted:
		s.offline = false
	}
	if err := errors.Join(o.err, o.warn, o.unchecked); err != nil || !o.recent {
		s.scanErr = err
	}
	s.run, s.active = nil, nil
	if p := s.pending; p != nil && !s.stopping {
		s.pending = nil
		s.launchLocked(*p)
	}
	s.mu.Unlock()
	s.notify()
}

// publish swaps the new index in when it differs and saves it. The snapshot
// file is written only when something changed (or a previous save failed, or
// the index just became complete). A recent pass leaves the write to the next
// scan that is not one: the file is the whole index.
func (s *Service) publish(o outcome) {
	complete := o.ranToEnd && !o.offline && o.listed > 0
	changed := o.b.Changed()
	ix := o.b.Index()
	s.mu.Lock()
	wasComplete := s.complete
	switch {
	case changed:
		s.cur, s.complete = ix, complete
		s.gen++
	case complete:
		s.complete = true
	}
	if complete && o.unchecked == nil && !o.recent {
		s.checkedAt = s.opt.Now()
	}
	needSave := changed || (complete && !wasComplete) || s.saveErr != nil || s.unsaved
	s.unsaved = o.recent && needSave
	needSave = needSave && !o.recent
	meta := search.Meta{Complete: s.complete, ScannedAt: s.opt.Now()}
	saveIx := s.cur
	s.mu.Unlock()
	if changed {
		s.notify()
	}
	if needSave {
		s.recordSave(s.save(saveIx, meta))
	}
}
