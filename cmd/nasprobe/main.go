// Command nasprobe measures how fast a mounted NAS can be listed over the
// current network (LAN or Kerio VPN), for several worker counts.
//
//	go run ./cmd/nasprobe -path /Volumes/Photos -limit 20000 -workers 8,32,64
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/isuccesscorp/scan-image/internal/scanner"
	"github.com/isuccesscorp/scan-image/internal/source"
)

func main() {
	path := flag.String("path", "", "mounted folder to probe, e.g. /Volumes/Photos")
	limit := flag.Int64("limit", 20000, "stop after this many entries per run")
	workerList := flag.String("workers", "8,32,64", "comma-separated worker counts to compare")
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "missing -path")
		os.Exit(2)
	}

	fmt.Printf("%-8s %10s %8s %8s %12s %8s\n", "workers", "entries", "dirs", "errors", "entries/s", "time")
	for _, f := range strings.Split(*workerList, ",") {
		w, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || w <= 0 {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		var seen atomic.Int64
		s := &scanner.Scanner{Src: source.LocalFS{SkipHidden: true}, Workers: w}
		var nextID atomic.Uint32
		sink := func(_ uint32, es []source.Entry) []uint32 {
			ids := make([]uint32, len(es))
			for i := range ids {
				ids[i] = nextID.Add(1)
			}
			if seen.Add(int64(len(es))) >= *limit {
				cancel()
			}
			return ids
		}
		_ = s.Run(ctx, []scanner.Root{{Path: *path}}, sink)
		cancel()
		p := s.Progress()
		fmt.Printf("%-8d %10d %8d %8d %12.0f %8s\n", w, p.Files+p.Dirs, p.Dirs, p.Errors, p.FilesPerSec(), p.Elapsed.Round(time.Millisecond))
	}
	fmt.Println("\nTip: macOS caches listings; re-run after a few minutes or on another folder for honest numbers.")
}
