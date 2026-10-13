package main

import (
	"cmp"
	"slices"
	"strings"

	"github.com/isuccesscorp/scan-image/internal/fold"
	"github.com/isuccesscorp/scan-image/internal/indexer"
	"github.com/isuccesscorp/scan-image/internal/s3up"
)

type destRowKind int

const (
	rowDefault destRowKind = iota
	rowNew
	rowFolder
)

// destRow is one line of the popup's list; Folder is "" for the default.
type destRow struct {
	Kind   destRowKind
	Folder string
}

func (r destRow) label() string {
	switch r.Kind {
	case rowDefault:
		return "Theo cấu trúc share (mặc định)"
	case rowNew:
		return "Dùng thư mục mới: " + r.Folder
	}
	return r.Folder
}

const (
	rankWholePath = iota
	rankLastEqual
	rankLastPrefix
	rankSegmentPrefix
	rankContains
)

// folderRank classes a folded folder path against a folded, non-empty query,
// or reports false when it does not match.
func folderRank(ff, q string) (int, bool) {
	segs := strings.Split(ff, "/")
	last := segs[len(segs)-1]
	switch {
	case ff == q:
		return rankWholePath, true
	case last == q:
		return rankLastEqual, true
	case strings.HasPrefix(last, q):
		return rankLastPrefix, true
	case slices.ContainsFunc(segs, func(s string) bool { return strings.HasPrefix(s, q) }):
		return rankSegmentPrefix, true
	case strings.Contains(ff, q):
		return rankContains, true
	}
	return 0, false
}

// filterFolders keeps the folders matching the folded query, best match first:
// whole path, last segment equal, last segment prefix, any segment prefix,
// contains; inside a class fewer segments first, then alphabetical. An empty
// query returns every folder sorted.
func filterFolders(folders []string, query string) []string {
	q := fold.String(strings.Trim(strings.TrimSpace(query), "/"))
	type hit struct {
		folder      string
		rank, depth int
	}
	hits := make([]hit, 0, len(folders))
	for _, f := range folders {
		if q == "" {
			hits = append(hits, hit{folder: f})
			continue
		}
		ff := strings.Trim(fold.String(f), "/")
		if rank, ok := folderRank(ff, q); ok {
			hits = append(hits, hit{f, rank, strings.Count(ff, "/")})
		}
	}
	slices.SortFunc(hits, func(a, b hit) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), cmp.Compare(a.depth, b.depth), strings.Compare(a.folder, b.folder))
	})
	var out []string
	for _, h := range hits {
		out = append(out, h.folder)
	}
	return out
}

// destRows is the popup's list for query: the default layout, the typed path
// as a new folder when it is valid and not on the bucket, then the matches.
func destRows(folders []string, query string, allowNew bool) []destRow {
	matches := filterFolders(folders, query)
	rows := make([]destRow, 0, len(matches)+2)
	rows = append(rows, destRow{Kind: rowDefault})
	if f, err := parseFolder(query); allowNew && err == nil && f != "" && !slices.Contains(folders, f) {
		rows = append(rows, destRow{Kind: rowNew, Folder: f})
	}
	for _, m := range matches {
		rows = append(rows, destRow{Kind: rowFolder, Folder: m})
	}
	return rows
}

// autoChoice is the folder to pick after the query changed: the top match,
// else the typed new folder, else the current choice.
func autoChoice(rows []destRow, query, current string) string {
	if strings.TrimSpace(query) == "" {
		return current
	}
	for _, kind := range []destRowKind{rowFolder, rowNew} {
		for _, r := range rows {
			if r.Kind == kind {
				return r.Folder
			}
		}
	}
	return current
}

func destRowIndex(rows []destRow, choice string) int {
	return slices.IndexFunc(rows, func(r destRow) bool { return r.Folder == choice })
}

// destPreview is where the first of paths would land, or why it cannot.
func destPreview(cfg s3up.Config, roots []indexer.RootInfo, folder string, paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	key, err := destKey(cfg, roots, folder, paths[0])
	if err != nil {
		return err.Error()
	}
	return previewLine(key)
}

// destCanPush reports whether the Push button may act.
func destCanPush(folder string, files int, checking, scanning bool) bool {
	if files == 0 || checking || scanning {
		return false
	}
	norm, err := parseFolder(folder)
	return err == nil && norm == folder
}
