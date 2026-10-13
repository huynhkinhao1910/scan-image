// Package scanner walks a Source in parallel.
//
// Over a VPN each directory listing costs at least one round trip, so the
// scanner keeps many listings in flight at once: latency, not bandwidth, is
// what it hides.
package scanner

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

// Dir is one directory to list. ID and Prev belong to the caller: the scanner
// hands a Dir back to the Sink exactly as it was given.
type Dir struct {
	Path     string
	ID, Prev uint32
}

// Sink receives the outcome of every Dir given to Run. Each Dir ends with
// exactly one Listed or exactly one Failed. Calls are serialised, so a Sink
// may write to a store without locking of its own.
type Sink interface {
	// Listed receives the entries of d and returns the subdirectories that
	// still have to be listed.
	Listed(d Dir, entries []source.Entry) []Dir
	// Failed reports that d could not be listed, or that the scan was
	// cancelled before d finished.
	Failed(d Dir, err error)
}

// Progress is a snapshot of a running scan.
type Progress struct {
	Dirs    int64
	Files   int64
	Errors  int64
	Pending int64
	Elapsed time.Duration
	Done    bool
}

// FilesPerSec is the average listing throughput.
func (p Progress) FilesPerSec() float64 {
	if p.Elapsed <= 0 {
		return 0
	}
	return float64(p.Files+p.Dirs) / p.Elapsed.Seconds()
}

// Scanner lists a tree with a fixed number of workers.
type Scanner struct {
	Src     source.Source
	Workers int

	dirs, files, errs, pending atomic.Int64
	start                      time.Time
	done                       atomic.Bool
}

// Progress reports counters; safe to call from any goroutine.
func (s *Scanner) Progress() Progress {
	el := time.Duration(0)
	if !s.start.IsZero() {
		el = time.Since(s.start)
	}
	return Progress{
		Dirs:    s.dirs.Load(),
		Files:   s.files.Load(),
		Errors:  s.errs.Load(),
		Pending: s.pending.Load(),
		Elapsed: el,
		Done:    s.done.Load(),
	}
}

// Run scans every root, sharing one worker pool, until the trees are
// exhausted or ctx ends.
//
// On cancellation Run returns at once with ctx.Err(), without waiting for
// workers stuck in ReadDir (os.ReadDir cannot be cancelled). Every queued and
// in-flight Dir gets Failed before Run returns; a worker that comes back later
// finds the scan closed and drops its result.
func (s *Scanner) Run(ctx context.Context, roots []Dir, sink Sink) error {
	workers := s.Workers
	if workers <= 0 {
		workers = 32
	}
	s.start = time.Now()
	s.done.Store(false)
	defer s.done.Store(true)

	var (
		mu       sync.Mutex // guards queue, active, inflight
		cond     = sync.NewCond(&mu)
		queue    = append(make([]Dir, 0, len(roots)), roots...)
		active   = 0
		inflight = map[Dir]int{}
		sinkMu   sync.Mutex // serialises sink calls; guards closed
		closed   bool
		finished = make(chan struct{})
		finish   sync.Once
	)
	s.pending.Store(int64(len(queue)))

	next := func() (Dir, bool) {
		mu.Lock()
		defer mu.Unlock()
		for len(queue) == 0 && active > 0 && ctx.Err() == nil {
			cond.Wait()
		}
		if ctx.Err() != nil {
			return Dir{}, false
		}
		if len(queue) == 0 {
			finish.Do(func() { close(finished) })
			cond.Broadcast()
			return Dir{}, false
		}
		d := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		active++
		inflight[d]++
		return d, true
	}

	// settle delivers d's outcome under sinkMu and reports false when the
	// scan is closed or cancelled, leaving d for the coordinator to fail.
	settle := func(d Dir, entries []source.Entry, err error) bool {
		sinkMu.Lock()
		defer sinkMu.Unlock()
		if closed || ctx.Err() != nil {
			return false
		}
		var children []Dir
		if err != nil {
			s.errs.Add(1)
			sink.Failed(d, err)
		} else {
			children = sink.Listed(d, entries)
			for _, e := range entries {
				if e.IsDir {
					s.dirs.Add(1)
				} else {
					s.files.Add(1)
				}
			}
		}
		mu.Lock()
		if inflight[d]--; inflight[d] == 0 {
			delete(inflight, d)
		}
		queue = append(queue, children...)
		active--
		s.pending.Store(int64(len(queue) + active))
		cond.Broadcast()
		mu.Unlock()
		return true
	}

	for w := 0; w < workers; w++ {
		go func() {
			for {
				d, ok := next()
				if !ok {
					return
				}
				entries, err := s.Src.ReadDir(ctx, d.Path)
				if !settle(d, entries, err) {
					return
				}
			}
		}()
	}

	select {
	case <-finished:
		return ctx.Err()
	case <-ctx.Done():
	}

	sinkMu.Lock()
	closed = true
	mu.Lock()
	abandoned := queue
	for d, n := range inflight {
		for ; n > 0; n-- {
			abandoned = append(abandoned, d)
		}
	}
	queue, inflight = nil, map[Dir]int{}
	s.pending.Store(0)
	cond.Broadcast()
	mu.Unlock()
	for _, d := range abandoned {
		sink.Failed(d, ctx.Err())
	}
	sinkMu.Unlock()
	return ctx.Err()
}
