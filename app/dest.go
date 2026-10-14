package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/internal/brokenlist"
	"github.com/isuccesscorp/scan-image/internal/indexer"
	"github.com/isuccesscorp/scan-image/internal/s3up"
	"github.com/isuccesscorp/scan-image/internal/search"
)

const (
	folderScanTimeout = 60 * time.Second
	headTimeout       = 30 * time.Second
	headPerKey        = 100 * time.Millisecond
	headWorkers       = 4
	maxNamesShown     = 3
)

// pendingPush is an upload waiting for the user to accept overwriting the
// keys that already exist. keepSame says the jobs skip a key that already
// holds a file of the same size.
type pendingPush struct {
	cfg      s3up.Config
	specs    []uploadSpec
	gen      int
	checked  headTally
	keepSame bool
}

// message is the confirmation text, naming the bucket the jobs will use.
func (p pendingPush) message() string {
	msg := pushConfirmMessage(p.checked, len(p.specs), strings.TrimSpace(p.cfg.Bucket))
	if p.keepSame {
		msg += " Settings đang bật bỏ qua file đã có: file cùng dung lượng được bỏ qua, file khác dung lượng bị ghi đè."
	}
	return msg
}

func destTitle(files int, bucket string) string {
	return fmt.Sprintf("Đẩy %d file lên bucket %s", files, strings.TrimSpace(bucket))
}

// destState is the S3 destination popup: the bucket's folders, the folder
// chosen, and the check that runs before an upload to a folder.
type destState struct {
	open   bool
	gen    int
	query  string
	choice string
	last   string
	msg    string
	list   ui.ListState
	sel    int

	folders   []string
	scanning  bool
	err       string
	truncated bool
	at        time.Time
	scanSeq   int

	touched  bool
	checking bool
	pushSeq  int
	pending  *pendingPush
}

// startScan marks a scan as running, drops the old result and returns the
// sequence number its result must present to be applied.
func (d *destState) startScan() int {
	d.scanSeq++
	d.scanning, d.err, d.truncated = true, "", false
	d.folders = nil
	return d.scanSeq
}

// finishScan applies a scan result unless a newer scan was started.
func (d *destState) finishScan(seq int, folders []string, truncated bool, err error, now time.Time) bool {
	if seq != d.scanSeq {
		return false
	}
	d.scanning, d.at = false, now
	if err != nil {
		d.err = folderScanMessage(err)
		return true
	}
	d.folders, d.truncated = folders, truncated
	return true
}

// folderScanMessage explains a failed folder listing in plain Vietnamese.
func folderScanMessage(err error) string {
	var se *s3up.Error
	switch {
	case errors.As(err, &se) && se.Status == 403:
		return fmt.Sprintf("Khoá S3 không có quyền liệt kê bucket (403 %s) — vẫn gõ tay thư mục được", codeOr(se, "AccessDenied"))
	case errors.As(err, &se) && se.Status == 404:
		return fmt.Sprintf("Không tìm thấy bucket (404 %s) — kiểm tra Bucket và Endpoint trong Settings", codeOr(se, "NoSuchBucket"))
	case errors.As(err, &se):
		return strings.TrimSpace(fmt.Sprintf("Không liệt kê được thư mục: S3 trả lỗi %d %s", se.Status, se.Code))
	case errors.Is(err, context.DeadlineExceeded):
		return "Không liệt kê được thư mục: quá thời gian chờ"
	}
	return "Không liệt kê được thư mục: " + oneLine(err)
}

func codeOr(e *s3up.Error, fallback string) string {
	if e.Code != "" {
		return e.Code
	}
	return fallback
}

// parseFolder normalises a typed folder to "a/b/" and validates it. An empty
// result means the default layout.
func parseFolder(in string) (string, error) {
	f := strings.Trim(strings.TrimSpace(in), "/")
	if f == "" {
		return "", nil
	}
	f += "/"
	if _, err := brokenlist.Key("/"+f, "x"); err != nil {
		return "", errors.New("Thư mục không hợp lệ: " + err.Error())
	}
	return f, nil
}

// destKey is the S3 key of path: the share layout under the configured prefix
// when folder is empty, else folder + file name, verbatim.
func destKey(cfg s3up.Config, roots []indexer.RootInfo, folder, p string) (string, error) {
	if folder == "" {
		return cfg.Key(relKey(roots, p)), nil
	}
	return brokenlist.Key("/"+folder, filepath.Base(p))
}

// duplicateNames lists, sorted, the file names that more than one key ends in.
func duplicateNames(keys []string) []string {
	seen := map[string]int{}
	for _, k := range keys {
		seen[path.Base(k)]++
	}
	var out []string
	for name, n := range seen {
		if n > 1 {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// destSpecs builds the upload specs for paths. Every file must still be in
// items, whose size and mtime become the source stamp. In folder mode keys
// are checked for collisions and the upload overwrites and verifies; the
// default layout follows the settings.
func destSpecs(cfg s3up.Config, roots []indexer.RootInfo, items []search.Item, paths []string, folder string) ([]uploadSpec, error) {
	byPath := make(map[string]search.Item, len(items))
	for _, it := range items {
		byPath[it.Path] = it
	}
	specs := make([]uploadSpec, len(paths))
	keys := make([]string, len(paths))
	for i, p := range paths {
		key, err := destKey(cfg, roots, folder, p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		it, ok := byPath[p]
		if !ok {
			return nil, fmt.Errorf("%s không còn trong danh sách kết quả, chọn lại file", filepath.Base(p))
		}
		keys[i], specs[i] = key, uploadSpec{Path: p, Key: key, Expect: &sourceStamp{Size: it.Size, ModTime: it.ModTime}}
		if folder != "" {
			specs[i].Opt = s3up.UploadOptions{Overwrite: true, Verify: true}
		}
	}
	if dups := duplicateNames(keys); folder != "" && len(dups) > 0 {
		return nil, errors.New(collisionMessage(dups))
	}
	return specs, nil
}

func collisionMessage(names []string) string {
	shown := names
	more := ""
	if len(shown) > maxNamesShown {
		shown, more = shown[:maxNamesShown], "…"
	}
	return fmt.Sprintf("Có %d tên file trùng nhau, sẽ ghi đè lẫn nhau: %s%s", len(names), strings.Join(shown, ", "), more)
}

func previewLine(key string) string { return "→ " + key }

func overwriteMessage(existing, total int, bucket string) string {
	return fmt.Sprintf("%d trong %d file đã có trên bucket %s. Ghi đè?", existing, total, bucket)
}

type headClient interface {
	Head(ctx context.Context, key string) (size int64, ok bool, err error)
}

// headTally classifies the keys of a batch: Existing were found, Unknown
// could not be checked because the key may not read them (403).
type headTally struct {
	Existing, Unknown int
}

func (r headTally) needsConfirm() bool { return r.Existing > 0 || r.Unknown > 0 }

// pushConfirmMessage is the second step's text for a batch of total files.
func pushConfirmMessage(r headTally, total int, bucket string) string {
	if r.Unknown == 0 {
		return overwriteMessage(r.Existing, total, bucket)
	}
	unknown := fmt.Sprintf("Không kiểm tra được %d trong %d file đã có trên bucket hay chưa (khoá S3 không có quyền, 403). Đẩy sẽ ghi đè nếu file đã tồn tại.", r.Unknown, total)
	if r.Existing == 0 {
		return unknown
	}
	return fmt.Sprintf("%d trong %d file đã có trên bucket %s. %s", r.Existing, total, bucket, unknown)
}

func isForbidden(err error) bool {
	var se *s3up.Error
	return errors.As(err, &se) && se.Status == 403
}

// headBatch heads every key with a few requests in flight and tallies the
// results. The first error other than 403 aborts the rest.
func headBatch(ctx context.Context, c headClient, keys []string, workers int) (headTally, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
		found    atomic.Int64
		unknown  atomic.Int64
		next     atomic.Int64
	)
	for range min(workers, len(keys)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(keys) {
					return
				}
				_, ok, err := headOnce(ctx, c, keys[i])
				switch {
				case isForbidden(err):
					unknown.Add(1)
				case err != nil:
					once.Do(func() { firstErr = err; cancel() })
					return
				case ok:
					found.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return headTally{}, firstErr
	}
	if err := ctx.Err(); err != nil {
		return headTally{}, err
	}
	return headTally{Existing: int(found.Load()), Unknown: int(unknown.Load())}, nil
}

func headOnce(ctx context.Context, c headClient, key string) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	return c.Head(ctx, key)
}

// scanFolders lists the bucket's folders with the saved config, never the
// form copy. UI thread only.
func (a *app) scanFolders() {
	cfg := a.up.cfg
	if cfg.Validate() != nil {
		return
	}
	d := &a.dest
	seq := d.startScan()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), folderScanTimeout)
		defer cancel()
		folders, truncated, err := s3up.New(cfg).Folders(ctx, s3up.FolderOptions{})
		a.update(func() { d.finishScan(seq, folders, truncated, err, time.Now()) })
	}()
}

// pushChecked queues paths under folder ("" = the default layout) after
// checking, off the UI thread, which destination keys already exist. UI
// thread only.
func (a *app) pushChecked(cfg s3up.Config, paths []string, folder string) {
	specs, err := destSpecs(cfg, a.st.Roots, a.items, paths, folder)
	if err != nil {
		a.dest.msg = err.Error()
		return
	}
	d := &a.dest
	d.pushSeq++
	seq, gen := d.pushSeq, a.sessGen
	d.checking = true
	keys := make([]string, len(specs))
	for i, sp := range specs {
		keys[i] = sp.Key
	}
	keepSame := folder == "" && cfg.SkipExisting
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), headTimeout+time.Duration(len(keys))*headPerKey)
		defer cancel()
		res, err := headBatch(ctx, s3up.New(cfg), keys, headWorkers)
		p := pendingPush{cfg: cfg, specs: specs, gen: gen, checked: res, keepSame: keepSame}
		a.update(func() { a.afterHeadCheck(seq, gen, p, err) })
	}()
}

func (a *app) afterHeadCheck(seq, gen int, p pendingPush, err error) {
	d := &a.dest
	if seq != d.pushSeq {
		return
	}
	d.checking = false
	switch {
	case a.sessGen != gen:
		d.close()
		a.status = "Nguồn đã đổi trong lúc kiểm tra, chọn lại file."
	case err != nil:
		d.msg = "Không kiểm tra được file đã có trên bucket, chưa upload: " + oneLine(err)
	case !p.checked.needsConfirm():
		a.enqueueDest(p)
	default:
		d.pending = &p
	}
}

// enqueueDest queues the checked specs and closes the popup.
func (a *app) enqueueDest(p pendingPush) {
	a.dest.close()
	jobs := a.enqueueUploads(p.cfg, p.specs)
	a.status = queueMessage(jobs) + a.adoptUploads(jobs)
	a.tab = tabQueue
}

func (a *app) acceptOverwrite() {
	d := &a.dest
	p := d.pending
	d.pending = nil
	if p == nil {
		return
	}
	if p.gen != a.sessGen {
		d.close()
		a.status = "Nguồn đã đổi, chọn lại file."
		return
	}
	a.enqueueDest(*p)
}

// openDest shows the popup for the selected files. UI thread only.
func (a *app) openDest() {
	d := &a.dest
	d.open, d.gen, d.query, d.choice, d.msg, d.pending, d.touched = true, a.sessGen, "", d.last, "", nil, false
	if d.at.IsZero() && !d.scanning {
		a.scanFolders()
	}
}

// enterMayPush reports whether Enter may start the upload: only after the
// user typed or clicked a row, and not while the folder list is loading.
func (d *destState) enterMayPush() bool { return d.touched && !d.scanning }

// close hides the popup and drops a check still in flight.
func (d *destState) close() {
	d.open, d.pending, d.msg, d.checking = false, nil, "", false
	d.pushSeq++
}

// pushDest sends the selected files to the chosen destination. UI thread only.
func (a *app) pushDest() {
	d := &a.dest
	cfg, problem := a.requireS3()
	if problem != "" {
		d.msg = problem
		return
	}
	paths := a.selectedPaths()
	if !destCanPush(d.choice, len(paths), d.checking, d.scanning) {
		return
	}
	d.last, d.msg = d.choice, ""
	a.pushChecked(cfg, paths, d.choice)
}
