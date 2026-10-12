package main

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/internal/netshare"
	"github.com/isuccesscorp/scan-image/internal/scanner"
	"github.com/isuccesscorp/scan-image/internal/search"
	"github.com/isuccesscorp/scan-image/internal/source"
)

var imageExts = []string{".jpg", ".jpeg", ".png", ".heic", ".tif", ".tiff", ".psd", ".webp", ".gif", ".cr2", ".cr3", ".nef", ".arw", ".dng"}

type screen int

const (
	screenConnect screen = iota
	screenBrowse
)

const (
	modeLocal = iota
	modeServer
)

type app struct {
	win    *mygo.Window
	net    *netshare.Client
	screen screen

	mode        int
	localFolder string
	host        string
	user        string
	suggestions []netshare.Mount
	busy        bool
	discovery   *netshare.Discovery
	pick        []bool
	connectMsg  string

	source   string
	roots    []string
	index    *search.Index
	scan     *scanner.Scanner
	progress scanner.Progress
	cancel   context.CancelFunc
	scanning bool
	scanGen  int

	query      string
	imagesOnly bool
	result     search.Result
	items      []search.Item
	selected   int
	table      ui.ListState
	chosen     ui.Selection[string]
	grid       ui.GridState
	viewMode   int
	th         thumbs

	settingsOpen bool
	uploadsOpen  bool
	up           uploads
	status       string
}

func newApp() *app {
	a := &app{net: netshare.New(), imagesOnly: true, selected: -1}
	a.loadS3()
	a.initThumbs()
	return a
}

func (a *app) update(fn func()) {
	if a.win != nil {
		a.win.Update(fn)
	}
}

func (a *app) loadSuggestions() {
	ss, err := a.net.Servers(context.Background())
	if err != nil {
		return
	}
	a.update(func() { a.suggestions = ss })
}

func (a *app) chooseLocalFolder() {
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:      a.win,
			Title:       "Chọn thư mục cần quét",
			Directory:   true,
			ButtonLabel: "Chọn",
		})
		if err != nil || len(paths) == 0 {
			return
		}
		a.update(func() { a.localFolder = paths[0] })
	}()
}

func (a *app) discover() {
	host := strings.TrimSpace(strings.TrimPrefix(a.host, "smb://"))
	host = strings.TrimSuffix(host, "/")
	if host == "" {
		a.connectMsg = "Nhập IP hoặc tên server NAS."
		return
	}
	a.host, a.busy, a.discovery, a.connectMsg = host, true, nil, "Đang đọc danh sách share…"
	user := strings.TrimSpace(a.user)
	go func() {
		d, err := a.net.Discover(context.Background(), host, user)
		a.update(func() {
			a.busy = false
			switch {
			case err != nil:
				a.connectMsg = "Lỗi: " + err.Error()
			case len(d.Shares) == 0 && d.ListErr != nil:
				a.connectMsg = "Không đọc được share của server (cần mật khẩu trong Keychain). Hãy mount 1 share bằng Cmd+K rồi thử lại."
			case len(d.Shares) == 0:
				a.connectMsg = "Server không có share nào account này truy cập được."
			default:
				a.discovery = &d
				a.pick = make([]bool, len(d.Shares))
				for i := range a.pick {
					a.pick[i] = true
				}
				a.connectMsg = fmt.Sprintf("Account %q thấy %d share.", d.User, len(d.Shares))
				if d.ListErr != nil {
					a.connectMsg = "Chỉ thấy các share đang mount (không đọc được danh sách đầy đủ)."
				}
			}
		})
	}()
}

func (a *app) connectServer() {
	d := a.discovery
	if d == nil {
		return
	}
	var chosen []netshare.Share
	for i, s := range d.Shares {
		if a.pick[i] {
			chosen = append(chosen, s)
		}
	}
	if len(chosen) == 0 {
		a.connectMsg = "Chọn ít nhất 1 share."
		return
	}
	a.busy = true
	host, user := a.host, d.User
	go func() {
		roots, failed := a.net.Ensure(context.Background(), host, user, chosen, func(msg string) {
			a.update(func() { a.connectMsg = msg })
		})
		a.update(func() {
			a.busy = false
			if len(roots) == 0 {
				a.connectMsg = fmt.Sprintf("Không mount được share nào (%d lỗi).", len(failed))
				return
			}
			src := "smb://" + host
			a.startScan(src, roots)
			if len(failed) > 0 {
				names := make([]string, 0, len(failed))
				for n := range failed {
					names = append(names, n)
				}
				sort.Strings(names)
				a.status = "Không mount được: " + strings.Join(names, ", ")
			}
		})
	}()
}

func (a *app) connectLocal() {
	if a.localFolder == "" {
		a.connectMsg = "Chọn thư mục trước."
		return
	}
	a.startScan(a.localFolder, []string{a.localFolder})
}

func (a *app) disconnect() {
	a.stopScan()
	a.screen = screenConnect
	a.index, a.items, a.result, a.selected = nil, nil, search.Result{}, -1
	a.chosen.Clear()
	a.settingsOpen = false
	a.connectMsg = ""
	go a.loadSuggestions()
}

func (a *app) startScan(src string, roots []string) {
	a.stopScan()
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.screen = screenBrowse
	a.source, a.roots = src, roots
	a.index = search.New()
	jobs := make([]scanner.Root, len(roots))
	for i, r := range roots {
		jobs[i] = scanner.Root{Path: r, ID: a.index.AddRoot(r)}
	}
	a.scan = &scanner.Scanner{Src: source.LocalFS{SkipHidden: true}, Workers: 48}
	a.progress = scanner.Progress{}
	a.scanning = true
	a.status = fmt.Sprintf("Đang quét %d thư mục gốc…", len(roots))
	a.refreshSearch()

	a.scanGen++
	gen, ix, sc := a.scanGen, a.index, a.scan
	go func() {
		tick := time.NewTicker(300 * time.Millisecond)
		defer tick.Stop()
		done := make(chan error, 1)
		go func() { done <- sc.Run(ctx, jobs, ix.AddChildren) }()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				a.update(func() {
					if a.scanGen == gen {
						a.progress = sc.Progress()
						a.refreshSearch()
					}
				})
			case err := <-done:
				a.update(func() {
					if a.scanGen != gen {
						return
					}
					a.progress = sc.Progress()
					a.scanning = false
					if err == nil {
						a.status = fmt.Sprintf("Quét xong trong %s.", a.progress.Elapsed.Round(time.Second))
					}
					a.refreshSearch()
				})
				return
			}
		}
	}()
}

// stopScan cancels the running scan and releases the UI at once; workers
// still inside a slow NAS listing finish in the background and their
// results are ignored.
func (a *app) stopScan() {
	if a.cancel == nil {
		return
	}
	a.cancel()
	a.cancel = nil
	a.scanGen++
	if a.scanning {
		a.scanning = false
		if a.scan != nil {
			a.progress = a.scan.Progress()
		}
		a.status = fmt.Sprintf("Đã dừng quét. Đã index %s file, vẫn search được trong phần này.", human(int64(a.index.Stats().Files)))
		a.refreshSearch()
	}
}

func (a *app) refreshSearch() {
	if a.index == nil {
		a.items, a.result = nil, search.Result{}
		return
	}
	opt := search.Options{Limit: 2000, FilesOnly: true}
	if a.imagesOnly {
		opt.Extensions = imageExts
	}
	a.result = a.index.Search(a.query, opt)
	a.items = a.items[:0]
	for _, id := range a.result.IDs {
		a.items = append(a.items, a.index.Item(id))
	}
	if a.selected >= len(a.items) {
		a.selected = -1
	}
}

func (a *app) openSelected(reveal bool) {
	if a.selected < 0 || a.selected >= len(a.items) {
		return
	}
	args := []string{a.items[a.selected].Path}
	if reveal {
		args = append([]string{"-R"}, args...)
	}
	go func() { _ = exec.Command("open", args...).Run() }()
}
