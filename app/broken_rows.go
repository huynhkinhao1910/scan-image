package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/internal/brokenlist"
	"github.com/isuccesscorp/scan-image/internal/datefind"
	"github.com/isuccesscorp/scan-image/internal/search"
)

// brokenRow is one line of the server's list with where it was found locally
// and the upload job pushing it, if any. Cands are the files whose name is
// the row's name up to case, newest first. Near are files whose name only
// matches after folding diacritics; they are never pushed until the user sets
// Confirm to the path to use. Cands and Near hold the index hits merged with
// Quick, the hits of the last find by date, which an index match keeps.
type brokenRow struct {
	Item    brokenlist.Item
	Cands   []search.Item
	Near    []search.Item
	Quick   []search.Item
	Confirm string
	Job     *uploadJob

	Reported  *uploadJob
	ReportErr string
}

// splitHits separates hits (newest first) into exact name matches, ignoring
// case only, and look-alikes.
func splitHits(file string, hits []search.Item) (exact, near []search.Item) {
	for _, h := range hits {
		if strings.EqualFold(h.Name, file) {
			exact = append(exact, h)
		} else {
			near = append(near, h)
		}
	}
	return exact, near
}

// capHits keeps the newest maxCandidates of hits, which are newest first.
func capHits(hits []search.Item) []search.Item {
	return hits[:min(len(hits), maxCandidates)]
}

// splitCapped is splitHits with each side held to the newest maxCandidates, so
// a name that hits thousands of files stays cheap and look-alikes cannot push
// an exact hit out.
func splitCapped(file string, hits []search.Item) (exact, near []search.Item) {
	exact, near = splitHits(file, hits)
	return capHits(exact), capHits(near)
}

// source is the file a push would send: the newest exact match, else the
// newest look-alike.
func (r brokenRow) source() (search.Item, bool) {
	switch {
	case len(r.Cands) > 0:
		return r.Cands[0], true
	case len(r.Near) > 0:
		return r.Near[0], true
	}
	return search.Item{}, false
}

// hasSeveral reports that more than one file has the row's exact name. The
// newest of them is pushed without asking: the user chose that on 2026-10-13,
// knowing that whoever can write a newer file of that name to the share then
// decides what goes to the bucket.
func (r brokenRow) hasSeveral() bool { return len(r.Cands) > 1 }

// wantsConfirm is whether the row's kind is pushed only after a confirmation:
// look-alikes alone, whose name is not the listed one.
func (r brokenRow) wantsConfirm() bool {
	return len(r.Cands) == 0 && len(r.Near) > 0
}

func (r brokenRow) needsConfirm() bool {
	src, ok := r.source()
	return ok && r.wantsConfirm() && r.Confirm != src.Path
}

// settleConfirm drops a confirmation that no longer names the row's source.
func settleConfirm(r brokenRow) brokenRow {
	if src, ok := r.source(); !ok || !r.wantsConfirm() || src.Path != r.Confirm {
		r.Confirm = ""
	}
	return r
}

// newestFirst returns a copy of items ordered by modification time, newest
// first, then by path so the choice is stable.
func newestFirst(items []search.Item) []search.Item {
	out := slices.Clone(items)
	slices.SortStableFunc(out, func(x, y search.Item) int {
		if c := y.ModTime.Compare(x.ModTime); c != 0 {
			return c
		}
		return cmp.Compare(x.Path, y.Path)
	})
	return out
}

func sourceLabel(cands []search.Item) string {
	if len(cands) == 0 {
		return ""
	}
	if len(cands) == 1 {
		return cands[0].Path
	}
	return fmt.Sprintf("%s (+%d bản khác, lấy bản mới nhất)", cands[0].Path, len(cands)-1)
}

func rowSourceLabel(r brokenRow) string {
	if len(r.Cands) == 0 && len(r.Near) > 0 {
		return r.Near[0].Path
	}
	return sourceLabel(r.Cands)
}

// brokenStatus is the status text and colour of a row: its upload job first,
// then whether a source exists and is trusted, then the dry run.
func brokenStatus(r brokenRow, dry bool) (string, ui.Color) {
	if j := r.Job; j != nil {
		if j.State == upRunning {
			return fmt.Sprintf("Đang đẩy %d%%", percent(j.Sent, j.Size)), jobColor(j.State)
		}
		if j.State == upDone {
			return doneText(r), jobColor(j.State)
		}
		return brokenJobText(j), jobColor(j.State)
	}
	src, ok := r.source()
	switch {
	case !ok:
		return "Chưa tìm thấy trên ổ mạng", colOrange
	case src.Size == 0:
		return "File nguồn rỗng (0 byte), không đẩy", colPink
	case r.needsConfirm():
		return "Tên gần giống, cần xác nhận", colOrange
	case dry:
		return fmt.Sprintf("sẽ đẩy → %s · %s", r.Item.Key, humanBytes(src.Size)), colMuted
	case r.hasSeveral():
		return fmt.Sprintf("Sẵn sàng · lấy bản mới nhất trong %d bản", len(r.Cands)), colMuted
	}
	return "Sẵn sàng", colMuted
}

func brokenJobText(j *uploadJob) string {
	switch j.State {
	case upQueued:
		return "Đang chờ"
	case upSkipped:
		return "Đã có, bỏ qua"
	case upCanceled:
		return "Đã huỷ"
	}
	return "Lỗi: " + j.Err
}

func percent(sent, total int64) int {
	if total <= 0 {
		return 0
	}
	return int(sent * 100 / total)
}

func jobBlocks(j *uploadJob) bool {
	return j != nil && j.State != upFailed && j.State != upCanceled
}

// eligibleRows lists the rows that have a trusted source with content and no
// job that is queued, running or finished. An empty source would put an empty
// object on the bucket.
func eligibleRows(rows []brokenRow) []int {
	var out []int
	for i, r := range rows {
		if src, ok := r.source(); !ok || src.Size == 0 || r.needsConfirm() || jobBlocks(r.Job) {
			continue
		}
		out = append(out, i)
	}
	return out
}

func missingCount(rows []brokenRow) int {
	n := 0
	for _, r := range rows {
		if _, ok := r.source(); !ok {
			n++
		}
	}
	return n
}

// maxCandidates bounds the exact and the look-alike hits kept per row.
const maxCandidates = 50

// lookAlikeCount is the rows waiting for the user to confirm a look-alike.
func lookAlikeCount(rows []brokenRow) int {
	n := 0
	for _, r := range rows {
		if r.needsConfirm() && !jobBlocks(r.Job) {
			n++
		}
	}
	return n
}

func dryRunSummary(rows []brokenRow) string {
	missing, look := missingCount(rows), 0
	for _, r := range rows {
		if r.needsConfirm() {
			look++
		}
	}
	s := fmt.Sprintf("%d file sẽ đẩy, %d chưa tìm thấy", len(rows)-missing-look, missing)
	if look > 0 {
		s += fmt.Sprintf(", %d file tên gần giống cần xác nhận", look)
	}
	return s
}

func confirmMessage(eligible, lookAlike int, bucket string, scanning bool) string {
	msg := fmt.Sprintf("Ghi đè %d file trên bucket %s?", eligible, bucket)
	if lookAlike > 0 {
		msg += fmt.Sprintf(" %d file tên gần giống cần xác nhận, không nằm trong lượt này.", lookAlike)
	}
	if scanning {
		msg += " Index đang quét dở: bản mới hơn của file có thể chưa được tìm thấy."
	}
	return msg
}

func rejectedFooter(rejected []string) []string {
	if len(rejected) <= rejectedShown {
		return rejected
	}
	out := slices.Clone(rejected[:rejectedShown])
	return append(out, fmt.Sprintf("… và %d dòng nữa", len(rejected)-rejectedShown))
}

// mergeRows builds rows for a fresh list, keeping the matches and the
// confirmation of rows whose key is unchanged, and their job only while it is
// queued or running.
func mergeRows(old []brokenRow, items []brokenlist.Item) []brokenRow {
	byKey := make(map[string]brokenRow, len(old))
	for _, r := range old {
		byKey[r.Item.Key] = r
	}
	rows := make([]brokenRow, len(items))
	for i, it := range items {
		prev := byKey[it.Key]
		rows[i] = brokenRow{Item: it, Cands: prev.Cands, Near: prev.Near, Quick: prev.Quick, Confirm: prev.Confirm}
		if j := prev.Job; j != nil && (j.State == upQueued || j.State == upRunning) {
			rows[i].Job = j
		}
	}
	return rows
}

// adoptJobs returns rows with each job attached to the row that names its
// key, and the jobs that found one. An upload queued outside the list is then
// reported to the server like a push from it.
func adoptJobs(rows []brokenRow, jobs []*uploadJob) ([]brokenRow, []*uploadJob) {
	byKey := make(map[string]*uploadJob, len(jobs))
	for _, j := range jobs {
		if j != nil {
			byKey[j.Key] = j
		}
	}
	out := slices.Clone(rows)
	var adopted []*uploadJob
	for i, r := range out {
		if j, ok := byKey[r.Item.Key]; ok {
			out[i].Job = j
			adopted = append(adopted, j)
		}
	}
	return out, adopted
}

// mergeHits unites index hits with quick hits by path, the index entry
// winning, newest first.
func mergeHits(index, quick []search.Item) []search.Item {
	seen := make(map[string]bool, len(index)+len(quick))
	out := make([]search.Item, 0, len(index)+len(quick))
	for _, it := range slices.Concat(index, quick) {
		if seen[it.Path] {
			continue
		}
		seen[it.Path] = true
		out = append(out, it)
	}
	return newestFirst(out)
}

// withQuick returns the row with the hits found for its file name added to
// its candidates.
func withQuick(r brokenRow, hits map[string][]datefind.Hit) brokenRow {
	found := hits[r.Item.File]
	if len(found) == 0 {
		return r
	}
	quick := make([]search.Item, len(found))
	for i, h := range found {
		quick[i] = search.Item{Name: filepath.Base(h.Path), Dir: filepath.Dir(h.Path), Path: h.Path, Size: h.Size, ModTime: h.ModTime}
	}
	r.Quick = quick
	r.Cands, r.Near = splitCapped(r.Item.File, mergeHits(slices.Concat(r.Cands, r.Near), quick))
	return settleConfirm(r)
}

func (r brokenRow) wantsQuickFind() bool {
	if len(r.Cands) > 0 || jobBlocks(r.Job) {
		return false
	}
	_, ok := datefind.ParseDate(r.Item.File)
	return ok
}

// matchRow gives r the candidates of a fresh index match. The quick-find hits
// stay in: the index match runs again and again while a scan fills the index,
// and must not send a row found by date back to "not found".
func matchRow(r brokenRow, index []search.Item) brokenRow {
	r.Cands, r.Near = splitCapped(r.Item.File, mergeHits(index, r.Quick))
	return settleConfirm(r)
}

// quickNames lists, once each in row order, the file names of the rows that
// have no exact source yet and carry a date.
func quickNames(rows []brokenRow) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range rows {
		if name := r.Item.File; r.wantsQuickFind() && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// undatedCount is the rows without a source whose name carries no date.
func undatedCount(rows []brokenRow) int {
	n := 0
	for _, r := range rows {
		if _, ok := datefind.ParseDate(r.Item.File); !ok && len(r.Cands) == 0 && !jobBlocks(r.Job) {
			n++
		}
	}
	return n
}

func canQuickFind(ready, running bool, rows []brokenRow) bool {
	return ready && !running && slices.ContainsFunc(rows, brokenRow.wantsQuickFind)
}

// quickMessage reports a find by date. Hits found before an error still
// count, so the report stays useful when the walk stopped early.
func quickMessage(res datefind.Result, asked, undated int, err error) string {
	if err != nil {
		reason := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			reason = fmt.Sprintf("quá thời gian cho phép (%d phút)", int(quickFindTimeout.Minutes()))
		}
		return "Tìm theo ngày bị dừng: " + reason
	}
	found := 0
	for _, h := range res.Hits {
		if len(h) > 0 {
			found++
		}
	}
	msg := fmt.Sprintf("Tìm theo ngày: thấy %d/%d file, đã liệt kê %d thư mục", found, asked, res.Dirs)
	if res.Truncated {
		msg += fmt.Sprintf(", dừng ở giới hạn %d thư mục", quickMaxDirs)
	}
	if res.Errors > 0 {
		msg += fmt.Sprintf(", %d thư mục không đọc được", res.Errors)
	}
	if undated > 0 {
		msg += fmt.Sprintf(". %d tên không có ngày, chờ index quét tới", undated)
	}
	return msg
}

const (
	reportEvery     = time.Second
	reportAfterFail = 5 * time.Second
)

// reportRef names a row's report by its key and the job it reported, so a
// result still applies after the list was fetched again.
type reportRef struct {
	key string
	job *uploadJob
}

// rowsToReport lists, in row order and at most limit of them, the rows whose
// finished upload the server has not been told about.
func rowsToReport(rows []brokenRow, limit int) []int {
	var out []int
	for i, r := range rows {
		if len(out) == limit {
			break
		}
		if j := r.Job; j != nil && j.State == upDone && r.Reported != j {
			out = append(out, i)
		}
	}
	return out
}

// reportBatch builds the report for the rows at idx and the refs to apply its
// result by.
func reportBatch(rows []brokenRow, idx []int) ([]brokenlist.Item, []reportRef) {
	items := make([]brokenlist.Item, len(idx))
	refs := make([]reportRef, len(idx))
	for k, i := range idx {
		items[k] = rows[i].Item
		refs[k] = reportRef{key: rows[i].Item.Key, job: rows[i].Job}
	}
	return items, refs
}

// reportGate says whether a report may start now: not while one is in flight,
// not within reportEvery of the last success (the server's rate limit) nor
// within reportAfterFail of the last failure. wait is how long until it may.
func reportGate(inflight bool, lastOK, lastFail, now time.Time) (start bool, wait time.Duration) {
	if inflight {
		return false, 0
	}
	wait = max(reportEvery-now.Sub(lastOK), reportAfterFail-now.Sub(lastFail), 0)
	return wait == 0, wait
}

// markReport returns rows with the sent ones marked reported, or, when short
// is not empty, carrying it as the reason the report failed. A row that no
// longer holds the reported job is left alone.
func markReport(rows []brokenRow, sent []reportRef, short string) []brokenRow {
	byKey := make(map[string]*uploadJob, len(sent))
	for _, s := range sent {
		byKey[s.key] = s.job
	}
	out := slices.Clone(rows)
	for i, r := range out {
		if j, ok := byKey[r.Item.Key]; !ok || r.Job != j {
			continue
		}
		if short != "" {
			out[i].ReportErr = short
			continue
		}
		out[i].Reported, out[i].ReportErr = r.Job, ""
	}
	return out
}

// doneText is the status of a finished upload, with what the server has been
// told about it.
func doneText(r brokenRow) string {
	base := "Đã đẩy · " + humanBytes(r.Job.Size)
	switch {
	case r.Reported == r.Job:
		return base + " · đã báo server"
	case r.ReportErr != "":
		return base + " · chưa báo được server: " + r.ReportErr
	}
	return base + " · đang báo server…"
}
