package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/internal/indexer"
	"github.com/isuccesscorp/scan-image/internal/s3up"
)

const appName = "ScanImage"

// ponytail: fixed; make it a setting when one number does not suit every line.
const uploadWorkers = 8

type uploadState int

const (
	upQueued uploadState = iota
	upRunning
	upDone
	upSkipped
	upFailed
	upCanceled
)

// sourceStamp is the size and modification time (to the second, as the index
// keeps it) a source file had when it was matched.
type sourceStamp struct {
	Size    int64
	ModTime time.Time
}

var errSourceChanged = errors.New("File nguồn đã đổi từ lúc khớp, lấy lại danh sách")

// check fails when the file at path is no longer the one that was matched.
// A symbolic link never passes: the match was on a file.
func (s sourceStamp) check(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != s.Size || st.ModTime().Unix() != s.ModTime.Unix() {
		return errSourceChanged
	}
	return nil
}

// uploadJob carries the config confirmed when it was queued, so editing the
// settings later cannot redirect it.
type uploadJob struct {
	Path   string
	Key    string
	Opt    s3up.UploadOptions
	Cfg    s3up.Config
	Expect *sourceStamp
	Size   int64
	Sent   int64
	State  uploadState
	Err    string
	Done   time.Time

	// Checks counts the looks at the bucket after the push that found the
	// object intact; Repush, how often it was sent again because one did not.
	Checks   int
	Repush   int
	checking bool
}

type uploads struct {
	cfgPath   string
	secrets   s3up.SecretStore
	cfg       s3up.Config
	form      s3up.Config
	secret    string
	msg       string
	checking  bool
	jobs      []*uploadJob
	table     ui.ListState
	recheck   recheckState
	cancel    context.CancelFunc
	queue     chan *uploadJob
	startOnce sync.Once
}

func (a *app) loadS3() {
	u := &a.up
	u.secrets = s3up.NewKeychain(appName + "-S3")
	p, err := s3up.ConfigPath(appName)
	if err != nil {
		u.msg = "Không tìm được thư mục cấu hình: " + err.Error()
		return
	}
	u.cfgPath = p
	cfg, err := s3up.LoadConfig(p, u.secrets)
	if err != nil {
		u.msg = "Lỗi đọc cấu hình S3: " + err.Error()
	}
	u.cfg = cfg
	u.resetForm()
}

// resetForm makes the settings form a copy of the saved config.
func (u *uploads) resetForm() {
	u.form, u.secret = u.cfg, ""
}

// formConfig is the settings form, trimmed, with the typed secret if any.
func (u *uploads) formConfig() s3up.Config {
	cfg := u.form
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.AccessKey = strings.TrimSpace(cfg.AccessKey)
	cfg.ListURL = strings.TrimSpace(cfg.ListURL)
	if s := strings.TrimSpace(u.secret); s != "" {
		cfg.SecretKey = s
	}
	return cfg
}

func (a *app) saveS3() {
	cfg := a.up.formConfig()
	if err := cfg.Validate(); err != nil {
		a.up.msg = "Chưa lưu: " + err.Error()
		return
	}
	if err := s3up.SaveConfig(a.up.cfgPath, cfg, a.up.secrets); err != nil {
		a.up.msg = "Lỗi lưu: " + err.Error()
		return
	}
	a.up.cfg = cfg
	a.up.resetForm()
	a.up.msg = "Đã lưu. Secret key nằm trong Keychain."
	a.scanFolders()
}

func (a *app) testS3() {
	cfg := a.up.formConfig()
	if err := cfg.Validate(); err != nil {
		a.up.msg = err.Error()
		return
	}
	a.up.checking, a.up.msg = true, "Đang kiểm tra kết nối S3…"
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := s3up.New(cfg).Check(ctx)
		a.update(func() {
			a.up.checking = false
			if err != nil {
				a.up.msg = "Lỗi: " + err.Error()
			} else {
				a.up.msg = "Kết nối OK, bucket " + cfg.Bucket + " truy cập được."
			}
		})
	}()
}

// relKey names an uploaded file after its root's name (the share name for a
// server source) and its place under it: "<root>/<subfolders>/<file>". The
// root name, not the mountpoint, so a remount as "Photos-1" keeps the key.
func relKey(roots []indexer.RootInfo, path string) string {
	for _, r := range roots {
		prefix := filepath.Clean(r.Path) + string(filepath.Separator)
		if rest, ok := strings.CutPrefix(path, prefix); ok {
			return filepath.ToSlash(filepath.Join(r.Name, rest))
		}
	}
	return filepath.Base(path)
}

func (a *app) selectedPaths() []string {
	var out []string
	if a.chosen.Len() > 0 {
		for p := range a.chosen.All() {
			out = append(out, p)
		}
		slices.Sort(out)
		return out
	}
	if a.selected >= 0 && a.selected < len(a.items) {
		out = append(out, a.items[a.selected].Path)
	}
	return out
}

func (a *app) clearSelection() {
	a.chosen.Clear()
	a.selected = -1
}

// requireS3 returns the saved config, or a message and the settings panel
// when it is not usable.
func (a *app) requireS3() (cfg s3up.Config, problem string) {
	cfg = a.up.cfg
	if err := cfg.Validate(); err != nil {
		if !a.settingsOpen {
			a.up.resetForm()
		}
		a.settingsOpen = true
		return cfg, "Chưa cấu hình S3 (" + err.Error() + "). Mở Settings để nhập."
	}
	return cfg, ""
}

func (a *app) uploadSelected() {
	_, problem := a.requireS3()
	if problem != "" {
		a.status = problem
		return
	}
	paths := a.selectedPaths()
	if len(paths) == 0 {
		return
	}
	a.openDest()
}

// uploadSpec is one file to send to an exact key with explicit options.
// Expect, when set, is what the source must still look like when the job runs.
type uploadSpec struct {
	Path   string
	Key    string
	Opt    s3up.UploadOptions
	Expect *sourceStamp
}

// queueMessage reports how many of the jobs enqueueUploads returned were
// queued and how many were skipped as already queued (nil entries).
func queueMessage(jobs []*uploadJob) string {
	skipped := 0
	for _, j := range jobs {
		if j == nil {
			skipped++
		}
	}
	msg := fmt.Sprintf("Đã thêm %d file vào hàng đợi upload", len(jobs)-skipped)
	if skipped > 0 {
		msg += fmt.Sprintf(", bỏ qua %d file đã có trong hàng đợi", skipped)
	}
	return msg + "."
}

// enqueueUploads queues one job per spec, each carrying cfg, and returns them
// in the same order. A spec whose (path, key) is already queued or running
// gets a nil entry. A failed job for the same key is cancelled so a retry
// cannot overwrite the new upload with an older source. UI thread only.
func (a *app) enqueueUploads(cfg s3up.Config, specs []uploadSpec) []*uploadJob {
	type pathKey struct{ path, key string }
	queued := map[pathKey]bool{}
	failed := map[string][]*uploadJob{}
	for _, j := range a.up.jobs {
		switch j.State {
		case upQueued, upRunning:
			queued[pathKey{j.Path, j.Key}] = true
		case upFailed:
			failed[j.Key] = append(failed[j.Key], j)
		}
	}
	a.startUploader(cfg)
	jobs := make([]*uploadJob, len(specs))
	for i, sp := range specs {
		k := pathKey{sp.Path, sp.Key}
		if queued[k] {
			continue
		}
		queued[k] = true
		for _, old := range failed[sp.Key] {
			old.State = upCanceled
		}
		j := &uploadJob{Path: sp.Path, Key: sp.Key, Opt: sp.Opt, Cfg: cfg, Expect: sp.Expect}
		a.up.jobs = append(a.up.jobs, j)
		jobs[i] = j
		go func() { a.up.queue <- j }()
	}
	return jobs
}

func (a *app) startUploader(cfg s3up.Config) {
	a.up.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		a.up.cancel = cancel
		a.up.queue = make(chan *uploadJob, 1024)
		for w := 0; w < uploadWorkers; w++ {
			go a.uploadWorker(ctx)
		}
	})
}

func (a *app) uploadWorker(ctx context.Context) {
	for {
		var j *uploadJob
		select {
		case <-ctx.Done():
			return
		case j = <-a.up.queue:
		}
		skip := false
		done := make(chan struct{})
		a.update(func() {
			if j.State == upCanceled {
				skip = true
			} else {
				j.State = upRunning
			}
			close(done)
		})
		<-done
		if skip {
			continue
		}
		res, err := a.runJob(ctx, j)
		a.update(func() {
			finishJob(j, res, err, time.Now())
			a.reportBroken()
			a.recheckWake()
		})
	}
}

// finishJob records how a run of j ended, and when.
func finishJob(j *uploadJob, res s3up.Result, err error, now time.Time) {
	j.Size, j.Done = res.Bytes, now
	switch {
	case err != nil:
		j.State, j.Err = upFailed, err.Error()
	case res.Skipped:
		j.State, j.Sent = upSkipped, res.Bytes
	default:
		j.State, j.Sent = upDone, res.Bytes
	}
}

// runJob uploads one job with its own config, after checking that its source
// is still the file that was matched. Off the UI thread.
func (a *app) runJob(ctx context.Context, j *uploadJob) (s3up.Result, error) {
	if j.Expect != nil {
		if err := j.Expect.check(j.Path); err != nil {
			return s3up.Result{}, err
		}
	}
	var last time.Time
	return s3up.New(j.Cfg).UploadWith(ctx, j.Path, j.Key, j.Opt, func(sent, total int64) {
		if time.Since(last) < 150*time.Millisecond && sent < total {
			return
		}
		last = time.Now()
		a.update(func() { j.Sent, j.Size = sent, total })
	})
}

// jobWebURL is the address of the job's file on the bucket, or "" while the
// file is not there.
func jobWebURL(j *uploadJob) string {
	if j.State != upDone && j.State != upSkipped {
		return ""
	}
	u, err := s3up.New(j.Cfg).ObjectURL(j.Key)
	if err != nil {
		return ""
	}
	return u
}

// openWeb shows the job's file in the browser. UI thread only.
func (a *app) openWeb(j *uploadJob) {
	u := jobWebURL(j)
	if u == "" {
		return
	}
	if err := mygo.Shell.OpenExternal(u); err != nil {
		a.status = "Không mở được " + u + ": " + oneLine(err)
	}
}

func (a *app) cancelQueued() {
	for _, j := range a.up.jobs {
		if j.State == upQueued {
			j.State = upCanceled
		}
	}
}

func (a *app) clearFinished() {
	keep := a.up.jobs[:0]
	for _, j := range a.up.jobs {
		if j.State == upQueued || j.State == upRunning || recheckPending(j) {
			keep = append(keep, j)
		}
	}
	a.up.jobs = keep
}

func (a *app) retryFailed() {
	for _, j := range a.up.jobs {
		if j.State == upFailed {
			j.State, j.Err, j.Sent, j.Done = upQueued, "", 0, time.Time{}
			go func() { a.up.queue <- j }()
		}
	}
}

func (u *uploads) counts() (active, done, failed int) {
	for _, j := range u.jobs {
		switch j.State {
		case upQueued, upRunning:
			active++
		case upDone, upSkipped:
			done++
		case upFailed:
			failed++
		}
	}
	return
}
