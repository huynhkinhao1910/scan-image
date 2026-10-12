// Package scanner walks a Source in parallel.
//
// Over a VPN each directory listing costs at least one round trip, so the
// scanner keeps many listings in flight at once: latency, not bandwidth, is
// what it hides.
package scanner

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

// Sink receives the entries of one directory and returns an id for each,
// in order. Ids of subdirectories are handed back when they are listed.
type Sink func(parentID uint32, entries []source.Entry) []uint32

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

// Root is one folder to scan and the id its entries hang from.
type Root struct {
	Path string
	ID   uint32
}

type job struct {
	path string
	id   uint32
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
// exhausted or ctx ends. Sink calls are serialised, so it may write to a
// store without locking of its own.
func (s *Scanner) Run(ctx context.Context, roots []Root, sink Sink) error {
	workers := s.Workers
	if workers <= 0 {
		workers = 32
	}
	s.start = time.Now()
	s.done.Store(false)
	defer s.done.Store(true)

	var (
		mu     sync.Mutex
		cond   = sync.NewCond(&mu)
		queue  = make([]job, 0, len(roots))
		active = 0
		sinkMu sync.Mutex
	)
	for _, r := range roots {
		queue = append(queue, job{path: r.Path, id: r.ID})
	}
	s.pending.Store(int64(len(queue)))

	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		cond.Broadcast()
		mu.Unlock()
	})
	defer stop()

	next := func() (job, bool) {
		mu.Lock()
		defer mu.Unlock()
		for len(queue) == 0 && active > 0 && ctx.Err() == nil {
			cond.Wait()
		}
		if len(queue) == 0 || ctx.Err() != nil {
			cond.Broadcast()
			return job{}, false
		}
		j := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		active++
		return j, true
	}

	finish := func(children []job) {
		mu.Lock()
		queue = append(queue, children...)
		active--
		s.pending.Store(int64(len(queue) + active))
		cond.Broadcast()
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, ok := next()
				if !ok {
					return
				}
				entries, err := s.Src.ReadDir(ctx, j.path)
				if err != nil {
					s.errs.Add(1)
					finish(nil)
					continue
				}
				sinkMu.Lock()
				ids := sink(j.id, entries)
				sinkMu.Unlock()

				var children []job
				for i, e := range entries {
					if e.IsDir {
						s.dirs.Add(1)
						children = append(children, job{path: filepath.Join(j.path, e.Name), id: ids[i]})
					} else {
						s.files.Add(1)
					}
				}
				finish(children)
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}
