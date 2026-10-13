package datefind

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/isuccesscorp/scan-image/internal/fold"
	"github.com/isuccesscorp/scan-image/internal/scanner"
	"github.com/isuccesscorp/scan-image/internal/source"
)

// Defaults for zero Options fields.
const (
	defaultWorkers  = 48
	defaultMaxDirs  = 5000
	defaultPreDepth = 3
	maxDayDepth     = 4
	// maxPreFanout is how many subfolders a folder below the root may have
	// and still be explored while no year folder has been seen: a designer's
	// tree is narrow there, a mockup or archive dump is not.
	maxPreFanout = 40
)

// Hit is one file found.
type Hit struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// Options tunes a Find.
type Options struct {
	Workers  int // 0 → 48
	MaxDirs  int // stop after listing this many directories; 0 → 5000
	PreDepth int // how deep below a root to look for a year folder when none has been seen; 0 → 3
}

// Result is the outcome of a Find.
type Result struct {
	Hits      map[string][]Hit // key: the name as given in names
	Dirs      int              // directories listed
	Truncated bool             // MaxDirs was reached before the walk ended
	Skipped   []string         // names without a date in them (not searched)
	Errors    int              // directories that could not be listed
}

type stage int

const (
	stageNoYear stage = iota
	stageYear
	stageMonth
	stageDay
)

// state is where a directory sits in the guided walk. depth counts levels
// below the root in stageNoYear and below the day folder in stageDay.
type state struct {
	stage   stage
	depth   int
	targets []Date
}

// Find walks roots looking for the given file names, visiting only folders
// whose names can lead to the dates in those names.
func Find(ctx context.Context, src source.Source, roots, names []string, opt Options) (Result, error) {
	if opt.Workers <= 0 {
		opt.Workers = defaultWorkers
	}
	if opt.MaxDirs <= 0 {
		opt.MaxDirs = defaultMaxDirs
	}
	if opt.PreDepth <= 0 {
		opt.PreDepth = defaultPreDepth
	}
	res := Result{Hits: map[string][]Hit{}}
	w := &walker{
		opt:    opt,
		res:    &res,
		byFold: map[string][]string{},
		dates:  map[string]Date{},
		states: map[uint32]state{},
	}
	var targets []Date
	for _, n := range names {
		d, ok := ParseDate(n)
		if !ok {
			res.Skipped = append(res.Skipped, n)
			continue
		}
		if _, dup := w.dates[n]; dup {
			continue
		}
		w.dates[n] = d
		k := fold.String(n)
		w.byFold[k] = append(w.byFold[k], n)
		if !slices.Contains(targets, d) {
			targets = append(targets, d)
		}
	}
	if len(targets) == 0 || len(roots) == 0 {
		return res, nil
	}

	inner, cancel := context.WithCancel(ctx)
	defer cancel()
	w.ictx, w.cancel = inner, cancel

	rootDirs := make([]scanner.Dir, 0, len(roots))
	for _, r := range roots {
		rootDirs = append(rootDirs, scanner.Dir{Path: r, ID: w.put(state{stage: stageNoYear, targets: targets})})
	}
	sc := &scanner.Scanner{Src: src, Workers: opt.Workers}
	_ = sc.Run(inner, rootDirs, w)

	for _, hs := range res.Hits {
		slices.SortFunc(hs, func(a, b Hit) int { return strings.Compare(a.Path, b.Path) })
	}
	return res, ctx.Err()
}

type walker struct {
	opt    Options
	res    *Result
	byFold map[string][]string
	dates  map[string]Date
	states map[uint32]state
	nextID uint32
	full   bool
	ictx   context.Context
	cancel context.CancelFunc
}

func (w *walker) put(s state) uint32 {
	w.nextID++
	w.states[w.nextID] = s
	return w.nextID
}

// Listed implements scanner.Sink.
func (w *walker) Listed(d scanner.Dir, entries []source.Entry) []scanner.Dir {
	st := w.states[d.ID]
	delete(w.states, d.ID)
	if w.full {
		w.res.Truncated = true
		return nil
	}
	w.res.Dirs++
	w.collect(d.Path, st, entries)

	kids := w.children(d.Path, st, entries)
	if w.res.Dirs >= w.opt.MaxDirs {
		w.full = true
		w.res.Truncated = w.res.Truncated || len(kids) > 0
		w.cancel()
		return nil
	}
	return kids
}

// Failed implements scanner.Sink.
func (w *walker) Failed(d scanner.Dir, err error) {
	delete(w.states, d.ID)
	if w.ictx.Err() != nil && errors.Is(err, w.ictx.Err()) {
		if w.full {
			w.res.Truncated = true
		}
		return
	}
	w.res.Errors++
}

func (w *walker) collect(dir string, st state, entries []source.Entry) {
	for _, e := range entries {
		if e.IsDir || hidden(e.Name) {
			continue
		}
		for _, orig := range w.byFold[fold.String(e.Name)] {
			if st.stage != stageNoYear && !slices.Contains(st.targets, w.dates[orig]) {
				continue
			}
			w.res.Hits[orig] = append(w.res.Hits[orig], Hit{Path: filepath.Join(dir, e.Name), Size: e.Size, ModTime: e.ModTime})
		}
	}
}

func (w *walker) children(dir string, st state, entries []source.Entry) []scanner.Dir {
	prune := st.stage == stageNoYear && st.depth > 0 && notADesignerTree(entries)
	var kids []scanner.Dir
	for _, e := range entries {
		if !e.IsDir || hidden(e.Name) {
			continue
		}
		next, ok := w.step(st, parseFolder(e.Name), prune)
		if ok {
			kids = append(kids, scanner.Dir{Path: filepath.Join(dir, e.Name), ID: w.put(next)})
		}
	}
	return kids
}

// notADesignerTree reports whether a folder below the root should not have
// its ordinary subfolders explored for a year folder: it already holds year
// folders, so the others are beside the date tree, or it is too wide to be
// one. The root itself is exempt, because a stray "2024" among the designers'
// folders must not hide them.
func notADesignerTree(entries []source.Entry) bool {
	dirs := 0
	for _, e := range entries {
		if !e.IsDir || hidden(e.Name) {
			continue
		}
		if parseFolder(e.Name).pureYear() {
			return true
		}
		dirs++
	}
	return dirs > maxPreFanout
}

// step decides whether a folder f below a directory in state st is worth
// listing, and in which state. prune stops the search for a year folder
// below ordinary subfolders.
func (w *walker) step(st state, f folder, prune bool) (state, bool) {
	switch st.stage {
	case stageNoYear:
		if f.looksYear() {
			ts := keep(st.targets, func(t Date) bool { return f.isYear(t.Year) })
			return state{stage: stageYear, targets: ts}, len(ts) > 0
		}
		if prune || st.depth >= w.opt.PreDepth {
			return state{}, false
		}
		return state{stage: stageNoYear, depth: st.depth + 1, targets: st.targets}, true
	case stageYear:
		if ts := keep(st.targets, func(t Date) bool { return f.isDayMonth(t.Day, t.Month, t.Year) }); len(ts) > 0 {
			return state{stage: stageDay, targets: ts}, true
		}
		ts := keep(st.targets, func(t Date) bool { return f.isMonth(t.Month, t.Year) })
		return state{stage: stageMonth, targets: ts}, len(ts) > 0
	case stageMonth:
		ts := keep(st.targets, func(t Date) bool { return f.isDay(t.Day, t.Month, t.Year) })
		return state{stage: stageDay, targets: ts}, len(ts) > 0
	default:
		return state{stage: stageDay, depth: st.depth + 1, targets: st.targets}, st.depth < maxDayDepth
	}
}

func keep(ts []Date, pred func(Date) bool) []Date {
	var out []Date
	for _, t := range ts {
		if pred(t) {
			out = append(out, t)
		}
	}
	return out
}

func hidden(name string) bool { return strings.HasPrefix(name, ".") }
