package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/isuccesscorp/scan-image/internal/s3up"
)

const appName = "ScanImage"

type uploadState int

const (
	upQueued uploadState = iota
	upRunning
	upDone
	upSkipped
	upFailed
	upCanceled
)

type uploadJob struct {
	Path  string
	Key   string
	Size  int64
	Sent  int64
	State uploadState
	Err   string
}

type uploads struct {
	cfgPath   string
	secrets   s3up.SecretStore
	cfg       s3up.Config
	secret    string
	msg       string
	checking  bool
	jobs      []*uploadJob
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
}

func (a *app) formConfig() s3up.Config {
	cfg := a.up.cfg
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.AccessKey = strings.TrimSpace(cfg.AccessKey)
	if s := strings.TrimSpace(a.up.secret); s != "" {
		cfg.SecretKey = s
	}
	return cfg
}

func (a *app) saveS3() {
	cfg := a.formConfig()
	if err := cfg.Validate(); err != nil {
		a.up.msg = "Chưa lưu: " + err.Error()
		return
	}
	if err := s3up.SaveConfig(a.up.cfgPath, cfg, a.up.secrets); err != nil {
		a.up.msg = "Lỗi lưu: " + err.Error()
		return
	}
	a.up.cfg, a.up.secret = cfg, ""
	a.up.msg = "Đã lưu. Secret key nằm trong Keychain."
}

func (a *app) testS3() {
	cfg := a.formConfig()
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

// relKey names an uploaded file after its place under the scanned root:
// "<root folder>/<subfolders>/<file>", so shares do not collide.
func (a *app) relKey(path string) string {
	for _, r := range a.roots {
		r = filepath.Clean(r)
		if strings.HasPrefix(path, r+string(filepath.Separator)) {
			return filepath.ToSlash(filepath.Join(filepath.Base(r), strings.TrimPrefix(path, r+string(filepath.Separator))))
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

func (a *app) uploadSelected() {
	cfg := a.up.cfg
	if err := cfg.Validate(); err != nil {
		a.status = "Chưa cấu hình S3 (" + err.Error() + "). Mở Settings để nhập."
		a.settingsOpen = true
		return
	}
	paths := a.selectedPaths()
	if len(paths) == 0 {
		return
	}
	queued := map[string]bool{}
	for _, j := range a.up.jobs {
		if j.State == upQueued || j.State == upRunning {
			queued[j.Path] = true
		}
	}
	a.startUploader(cfg)
	n := 0
	for _, p := range paths {
		if queued[p] {
			continue
		}
		j := &uploadJob{Path: p, Key: cfg.Key(a.relKey(p))}
		a.up.jobs = append(a.up.jobs, j)
		n++
		go func() { a.up.queue <- j }()
	}
	a.status = fmt.Sprintf("Đã thêm %d file vào hàng đợi upload.", n)
	a.uploadsOpen = true
}

func (a *app) startUploader(cfg s3up.Config) {
	a.up.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		a.up.cancel = cancel
		a.up.queue = make(chan *uploadJob, 1024)
		for w := 0; w < 3; w++ {
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
		var cfg s3up.Config
		skip := false
		done := make(chan struct{})
		a.update(func() {
			if j.State == upCanceled {
				skip = true
			} else {
				j.State = upRunning
			}
			cfg = a.up.cfg
			close(done)
		})
		<-done
		if skip {
			continue
		}
		client := s3up.New(cfg)
		var last time.Time
		res, err := client.Upload(ctx, j.Path, j.Key, func(sent, total int64) {
			if time.Since(last) < 150*time.Millisecond && sent < total {
				return
			}
			last = time.Now()
			a.update(func() { j.Sent, j.Size = sent, total })
		})
		a.update(func() {
			j.Size = res.Bytes
			switch {
			case err != nil:
				j.State, j.Err = upFailed, err.Error()
			case res.Skipped:
				j.State, j.Sent = upSkipped, res.Bytes
			default:
				j.State, j.Sent = upDone, res.Bytes
			}
		})
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
		if j.State == upQueued || j.State == upRunning {
			keep = append(keep, j)
		}
	}
	a.up.jobs = keep
}

func (a *app) retryFailed() {
	for _, j := range a.up.jobs {
		if j.State == upFailed {
			j.State, j.Err, j.Sent = upQueued, "", 0
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
