package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/icons"
	"github.com/isuccesscorp/scan-image/internal/brokenlist"
	"github.com/isuccesscorp/scan-image/internal/datefind"
	"github.com/isuccesscorp/scan-image/internal/s3up"
	"github.com/isuccesscorp/scan-image/internal/search"
	"github.com/isuccesscorp/scan-image/internal/source"
)

const (
	brokenFetchTimeout = 30 * time.Second
	rejectedShown      = 5
	brokenMatchEvery   = 5 * time.Second
	quickFindTimeout   = 3 * time.Minute
	quickMaxDirs       = 5000
)

// matchStamp is what the last match was computed from.
type matchStamp struct{ sess, gen, n int }

// shouldMatch decides whether the rows are matched again. While a scan runs
// the index grows constantly and each match holds its read lock, so growth
// alone waits for brokenMatchEvery; a swap, a new session or the end of the
// scan matches at once.
func shouldMatch(cur, last matchStamp, scanning bool, lastAt, now time.Time) bool {
	switch {
	case cur == last:
		return false
	case !scanning, cur.sess != last.sess, cur.gen != last.gen:
		return true
	}
	return now.Sub(lastAt) >= brokenMatchEvery
}

type brokenState struct {
	rows     []brokenRow
	rejected []string
	msg      string
	err      string
	fetching bool
	matching bool
	dryRun   bool
	confirm  bool
	seq      int
	stamp    matchStamp
	lastAt   time.Time
	table    ui.ListState
	selected int
	report   reportState

	quicking    bool
	quickCancel context.CancelFunc

	auto autoState
}

func listURL(configured string) string {
	if u := strings.TrimSpace(configured); u != "" {
		return u
	}
	return brokenlist.DefaultURL
}

// findCandidates looks each distinct name up once, in one pass, and resolves
// the hits, newest first, at most maxCandidates exact and as many look-alikes.
// Rows with the same name share one slice, which nothing mutates. Safe off the
// UI thread.
func findCandidates(ix *search.Index, names []string) [][]search.Item {
	slot := make(map[string]int, len(names))
	var uniq []string
	for _, n := range names {
		if _, ok := slot[n]; !ok {
			slot[n] = len(uniq)
			uniq = append(uniq, n)
		}
	}
	found := make([][]search.Item, len(uniq))
	for i, hit := range ix.FindNames(uniq) {
		if len(hit) == 0 {
			continue
		}
		items := make([]search.Item, len(hit))
		for k, id := range hit {
			items[k] = ix.Item(id)
		}
		exact, near := splitCapped(uniq[i], newestFirst(items))
		found[i] = newestFirst(slices.Concat(exact, near))
	}
	out := make([][]search.Item, len(names))
	for i, n := range names {
		out[i] = found[slot[n]]
	}
	return out
}

// fetchBroken downloads the list off the UI thread, then matches it.
func (a *app) fetchBroken() {
	b := &a.broken
	if b.fetching {
		return
	}
	b.fetching, b.err = true, ""
	url := listURL(a.up.cfg.ListURL)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), brokenFetchTimeout)
		defer cancel()
		items, rejected, err := brokenlist.Fetch(ctx, nil, url)
		a.update(func() { a.applyFetch(items, rejected, err) })
	}()
}

func (a *app) applyFetch(items []brokenlist.Item, rejected []string, err error) {
	b := &a.broken
	b.fetching = false
	if err != nil {
		b.err = oneLine(err)
		a.autoAfterFetch(false)
		return
	}
	b.rows, b.rejected, b.dryRun = mergeRows(b.rows, items), rejected, false
	b.msg = fmt.Sprintf("%d file, %d dòng bị loại", len(items), len(rejected))
	a.matchBroken()
	a.autoAfterFetch(b.matching)
}

// matchBroken looks the rows up in the current index off the UI thread. A
// newer match, a new list or a session change drops its result.
func (a *app) matchBroken() {
	b := &a.broken
	if a.svc == nil || len(b.rows) == 0 {
		return
	}
	ix := a.svc.Current()
	names := make([]string, len(b.rows))
	for i, r := range b.rows {
		names[i] = r.Item.File
	}
	b.seq++
	seq, sess := b.seq, a.sessGen
	b.matching, b.stamp = true, matchStamp{sess, a.st.Gen, ix.Len()}
	go func() {
		cands := findCandidates(ix, names)
		a.update(func() { a.applyMatches(seq, sess, cands) })
	}()
}

func (a *app) applyMatches(seq, sess int, cands [][]search.Item) {
	b := &a.broken
	if b.seq != seq {
		return
	}
	b.matching = false
	if a.sessGen != sess {
		return
	}
	rows := slices.Clone(b.rows)
	for i := range rows {
		rows[i] = matchRow(rows[i], cands[i])
	}
	b.rows, b.lastAt = rows, time.Now()
	if !a.st.Scanning {
		a.rematchBroken()
	}
	if !b.matching {
		a.autoAfterMatch()
	}
}

// rematchBroken starts a match when the index was swapped or grew since the
// last one, as shouldMatch allows; it never overlaps two matches. UI thread
// only.
func (a *app) rematchBroken() {
	b := &a.broken
	if len(b.rows) == 0 || b.matching || a.svc == nil {
		return
	}
	cur := matchStamp{a.sessGen, a.st.Gen, a.svc.Current().Len()}
	if shouldMatch(cur, b.stamp, a.st.Scanning, b.lastAt, time.Now()) {
		a.matchBroken()
	}
}

// quickFind walks the date folders for the rows without a source, off the UI
// thread, and merges the hits into the rows. UI thread only.
func (a *app) quickFind() {
	b := &a.broken
	names, roots := quickNames(b.rows), a.rootPaths()
	if b.quicking || a.svc == nil || len(names) == 0 || len(roots) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), quickFindTimeout)
	b.quicking, b.quickCancel = true, cancel
	sess, undated := a.sessGen, undatedCount(b.rows)
	go func() {
		defer cancel()
		src := source.LocalFS{SkipHidden: true, Bulk: true}
		res, err := datefind.Find(ctx, src, roots, names, datefind.Options{MaxDirs: quickMaxDirs})
		a.update(func() { a.applyQuick(sess, len(names), undated, res, err) })
	}()
}

func (a *app) rootPaths() []string {
	var out []string
	for _, r := range a.st.Roots {
		if r.Path != "" {
			out = append(out, r.Path)
		}
	}
	return out
}

func (a *app) applyQuick(sess, asked, undated int, res datefind.Result, err error) {
	if a.sessGen != sess {
		return
	}
	b := &a.broken
	b.quicking, b.quickCancel = false, nil
	rows := slices.Clone(b.rows)
	for i := range rows {
		rows[i] = withQuick(rows[i], res.Hits)
	}
	b.rows, b.msg = rows, quickMessage(res, asked, undated, err)
	a.autoAfterQuick()
}

// dropBrokenMatches forgets where rows were found when the session ends and
// stops a find by date still walking.
func (a *app) dropBrokenMatches() {
	b := &a.broken
	if b.quickCancel != nil {
		b.quickCancel()
	}
	b.quicking, b.quickCancel = false, nil
	b.seq++
	b.matching, b.stamp = false, matchStamp{}
	b.auto.phase = autoIdle
	b.report.inflight, b.report.lastFail = false, time.Time{}
	b.rows = slices.Clone(b.rows)
	for i := range b.rows {
		b.rows[i].Cands, b.rows[i].Near, b.rows[i].Quick, b.rows[i].Confirm = nil, nil, nil, ""
		b.rows[i].ReportErr = ""
	}
}

func (a *app) pushBroken() {
	cfg, problem := a.requireS3()
	b := &a.broken
	switch {
	case problem != "":
		b.msg = problem
		return
	case b.matching:
		b.msg = "Đang khớp lại danh sách, thử lại sau giây lát."
		return
	}
	b.msg = queueMessage(a.pushRows(cfg, eligibleRows(b.rows)))
}

// pushRows queues the rows at idx, each from its current source to its key,
// and remembers what was pushed. It returns the jobs as enqueueUploads does.
// UI thread only.
func (a *app) pushRows(cfg s3up.Config, idx []int) []*uploadJob {
	b := &a.broken
	specs := make([]uploadSpec, len(idx))
	for k, i := range idx {
		r := b.rows[i]
		src, _ := r.source()
		specs[k] = uploadSpec{
			Path:   src.Path,
			Key:    r.Item.Key,
			Opt:    s3up.UploadOptions{Overwrite: true, Verify: true},
			Expect: &sourceStamp{Size: src.Size, ModTime: src.ModTime},
		}
	}
	jobs := a.enqueueUploads(cfg, specs)
	if b.auto.pushed == nil {
		b.auto.pushed = map[string]pushedSource{}
	}
	for k, j := range jobs {
		if j == nil {
			continue
		}
		sp := specs[k]
		b.rows[idx[k]].Job = j
		b.auto.pushed[sp.Key] = pushedSource{sig: stampSig(sp.Path, sp.Expect.Size, sp.Expect.ModTime), job: j}
	}
	return jobs
}

// adoptUploads hands uploads queued from the search tab to the rows that name
// their keys: the server is told when they finish, and the unattended cycle
// does not send the same source again. It returns a note for the status line.
// UI thread only.
func (a *app) adoptUploads(jobs []*uploadJob) string {
	b := &a.broken
	rows, adopted := adoptJobs(b.rows, jobs)
	if len(adopted) == 0 {
		return ""
	}
	b.rows = rows
	if b.auto.pushed == nil {
		b.auto.pushed = map[string]pushedSource{}
	}
	for _, j := range adopted {
		if j.Expect != nil {
			b.auto.pushed[j.Key] = pushedSource{sig: stampSig(j.Path, j.Expect.Size, j.Expect.ModTime), job: j}
		}
	}
	return fmt.Sprintf(" %d file có trong danh sách file hỏng, đẩy xong sẽ báo server.", len(adopted))
}

// confirmSource confirms the row's current source, the newest of several exact
// copies or a look-alike, by the path the user sees.
func (a *app) confirmSource(row int) {
	b := &a.broken
	if row < 0 || row >= len(b.rows) || !b.rows[row].needsConfirm() {
		return
	}
	src, _ := b.rows[row].source()
	b.rows[row].Confirm = src.Path
}

func (a *app) brokenTab(c *ui.Context) {
	b := &a.broken
	eligible := len(eligibleRows(b.rows))
	ui.Column(c).Grow(1).Gap(10).Children(func() {
		a.brokenToolbar(c, eligible)
		if len(b.rows) == 0 {
			ui.Column(c).Gap(10).Padding(14, 16).Radius(12).Background(colCard).Border(1, colLine).Children(func() {
				ui.Text(c, "Bấm \"Lấy danh sách\" để hỏi server các file design đang hỏng trên S3.").FontSize(12).TextColor(colMuted).Wrap()
			})
		} else {
			a.brokenTable(c)
		}
		a.brokenFooter(c)
	})
	a.confirmPush(c, eligible)
	a.autoConfirm(c)
}

func (a *app) brokenToolbar(c *ui.Context, eligible int) {
	b := &a.broken
	canScan := a.svc != nil && !a.st.Scanning && missingCount(b.rows) > 0
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		if iconButton(c, icons.Refresh, "Lấy danh sách").Disabled(b.fetching).Clicked() {
			a.fetchBroken()
		}
		if iconButton(c, icons.Search, "Tìm file thiếu").Disabled(!canScan).Clicked() {
			a.scanNow()
		}
		label := "Tìm nhanh theo ngày"
		if b.quicking {
			label = "Đang tìm…"
		}
		ready := a.svc != nil && len(a.rootPaths()) > 0
		if iconButton(c, icons.Search, label).Disabled(!canQuickFind(ready, b.quicking, b.rows)).Clicked() {
			a.quickFind()
		}
		if b.fetching || b.matching || b.quicking {
			ui.Spinner(c)
		}
		ui.Text(c, "").Grow(1)
		a.autoToggle(c)
		if iconButton(c, icons.List, "Chạy thử").Disabled(len(b.rows) == 0).Clicked() {
			b.dryRun, b.msg = true, dryRunSummary(b.rows)
		}
		if iconPrimary(c, icons.CloudUpload, fmt.Sprintf("Đẩy tất cả (%d)", eligible)).Disabled(eligible == 0 || b.matching).Clicked() {
			a.askPush()
		}
	})
}

func (a *app) askPush() {
	if _, problem := a.requireS3(); problem != "" {
		a.broken.msg = problem
		return
	}
	a.broken.confirm = true
}

func (a *app) confirmPush(c *ui.Context, eligible int) {
	b := &a.broken
	ui.Modal(c, &b.confirm, func() {
		ui.Text(c, "Đẩy file hỏng lên S3").Bold()
		bucket := strings.TrimSpace(a.up.cfg.Bucket)
		ui.Text(c, confirmMessage(eligible, lookAlikeCount(b.rows), multiCount(b.rows), bucket, a.st.Scanning)).Wrap()
		ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
			if ui.Button(c, "Huỷ").Clicked() {
				b.confirm = false
			}
			if ui.PrimaryButton(c, "Đẩy").Disabled(b.matching).Clicked() {
				b.confirm = false
				a.pushBroken()
			}
		})
	})
}

func (a *app) brokenTable(c *ui.Context) {
	b := &a.broken
	cols := []ui.TableColumn{
		{Title: "File", MinWidth: 160},
		{Title: "Path trên S3", MinWidth: 200},
		{Title: "Nguồn trên ổ mạng", MinWidth: 240},
		{Title: "Trạng thái", Width: 240},
		{Title: "Đẩy lúc", Width: 130},
	}
	b.table.Selected = &b.selected
	b.table.Key = func(i int) any { return b.rows[i].Item.Key }
	calmRows(c, func() {
		ui.Table(c, &b.table, cols, len(b.rows), func(row, col int) {
			r, sel := b.rows[row], row == b.selected
			switch col {
			case 0:
				nameCell(c, r.Item.File)
			case 1:
				tint(ui.Text(c, r.Item.Key).SingleLine(), sel, colMuted)
			case 2:
				a.sourceCell(c, row, r, sel)
			case 3:
				text, clr := brokenStatus(r, b.dryRun)
				tint(ui.Text(c, text).SingleLine().Tooltip(text), sel, clr)
			case 4:
				tint(ui.Text(c, jobTime(r.Job)).SingleLine(), sel, colMuted)
			}
		}).Grow(1)
	})
}

func (a *app) brokenFooter(c *ui.Context) {
	b := &a.broken
	if b.err != "" {
		ui.Text(c, "Lỗi: "+b.err).FontSize(12).TextColor(colPink).Wrap()
	}
	if b.msg != "" {
		ui.Text(c, b.msg).FontSize(12).TextColor(colMuted).Wrap()
	}
	for _, line := range rejectedFooter(b.rejected) {
		ui.Text(c, line).FontSize(11).TextColor(colMuted).SingleLine()
	}
}

func (a *app) sourceCell(c *ui.Context, row int, r brokenRow, sel bool) {
	label := rowSourceLabel(r)
	if !r.needsConfirm() {
		tint(ui.Text(c, label).SingleLine(), sel, colMuted)
		return
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		tint(ui.Text(c, label).SingleLine().Grow(1), sel, colMuted)
		if ui.Button(c, "Dùng file này").Clicked() {
			a.confirmSource(row)
		}
	})
}
