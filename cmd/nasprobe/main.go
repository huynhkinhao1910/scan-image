// Command nasprobe measures how fast a mounted NAS can be listed over the
// current network (LAN or Kerio VPN), for several worker counts.
//
//	go run ./cmd/nasprobe -path /Volumes/Photos -limit 20000 -workers 8,32,64
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/isuccesscorp/scan-image/internal/scanner"
	"github.com/isuccesscorp/scan-image/internal/source"
)

const maxShownFailures = 10

// countSink counts entries, cancels once limit is reached and queues every
// subdirectory. It keeps the first listings that failed, so a lister that
// cannot read some folders shows which ones and why.
type countSink struct {
	seen     *atomic.Int64
	limit    int64
	cancel   context.CancelFunc
	failures []string
}

func (c *countSink) Listed(d scanner.Dir, es []source.Entry) []scanner.Dir {
	if c.seen.Add(int64(len(es))) >= c.limit {
		c.cancel()
	}
	var subs []scanner.Dir
	for _, e := range es {
		if e.IsDir {
			subs = append(subs, scanner.Dir{Path: filepath.Join(d.Path, e.Name)})
		}
	}
	return subs
}

func (c *countSink) Failed(d scanner.Dir, err error) {
	if errors.Is(err, context.Canceled) || len(c.failures) == maxShownFailures {
		return
	}
	c.failures = append(c.failures, fmt.Sprintf("%s: %v", d.Path, err))
}

func main() {
	path := flag.String("path", "", "mounted folder to probe, e.g. /Volumes/Photos")
	limit := flag.Int64("limit", 20000, "stop after this many entries per run")
	workerList := flag.String("workers", "8,32,64", "comma-separated worker counts to compare")
	listerList := flag.String("lister", "readdir", "comma-separated listers to compare: readdir, bulk")
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "missing -path")
		os.Exit(2)
	}

	fmt.Printf("%-8s %-8s %10s %8s %8s %12s %8s\n", "lister", "workers", "entries", "dirs", "errors", "entries/s", "time")
	for _, lister := range strings.Split(*listerList, ",") {
		lister = strings.TrimSpace(lister)
		if lister != "readdir" && lister != "bulk" {
			fmt.Fprintf(os.Stderr, "unknown lister %q (want readdir or bulk)\n", lister)
			os.Exit(2)
		}
		for _, f := range strings.Split(*workerList, ",") {
			w, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || w <= 0 {
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			var seen atomic.Int64
			src := source.LocalFS{SkipHidden: true, Bulk: lister == "bulk"}
			s := &scanner.Scanner{Src: src, Workers: w}
			sink := &countSink{seen: &seen, limit: *limit, cancel: cancel}
			_ = s.Run(ctx, []scanner.Dir{{Path: *path}}, sink)
			cancel()
			p := s.Progress()
			fmt.Printf("%-8s %-8d %10d %8d %8d %12.0f %8s\n", lister, w, p.Files+p.Dirs, p.Dirs, p.Errors, p.FilesPerSec(), p.Elapsed.Round(time.Millisecond))
			for _, f := range sink.failures {
				fmt.Println("  failed:", f)
			}
		}
	}
	fmt.Println("\nTip: macOS caches listings; re-run after a few minutes or on another folder for honest numbers.")
}
