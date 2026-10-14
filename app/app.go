package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/internal/indexer"
	"github.com/isuccesscorp/scan-image/internal/netshare"
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

const (
	scanWorkers = 48
	scanTick    = 300 * time.Millisecond
	resultLimit = 2000
	rateWindow  = 10 * time.Second
)

// resultKey identifies what a.items was built from; the search reruns only
// when it changes. n is the entry count of the current index, which grows
// while a first scan fills the live index without moving gen.
type resultKey struct {
	gen    int
	query  string
	images bool
	n      int
}

func needsSearch(prev resultKey, havePrev bool, cur resultKey) bool {
	return !havePrev || prev != cur
}

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
	store    *indexer.Store
	storeErr error
	svc      *indexer.Service
	st       indexer.State
	opening  bool
	sessGen  int
	tickStop chan struct{}
	shown    resultKey
	shownOK  bool
	closing  chan struct{}

	idxSize  int64
	idxBusy  bool
	idxStamp sizeStamp

	query      string
	imagesOnly bool
	result     search.Result
	items      []search.Item
	selected   int
	table      ui.ListState
	chosen     ui.Selection[string]
	tab        int
	rates      []rateSample

	settingsOpen bool
	up           uploads
	dest         destState
	broken       brokenState
	status       string
	sessionErr   string
	trayErr      string
	problemRows  []string
	loggedRows   []string

	tray     *mygo.Tray
	quitting bool
	loginOn  bool
	loginErr string
}

func newApp() *app {
	a := &app{net: netshare.New(), imagesOnly: true, selected: -1, idxSize: -1}
	a.openStore()
	a.loadS3()
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
	if d == nil || !a.requireStore() {
		return
	}
	sess := indexer.Session{
		Mode:   indexer.ModeServer,
		Source: "smb://" + a.host,
		Host:   a.host,
		User:   d.User,
	}
	for i, s := range d.Shares {
		if a.pick[i] {
			sess.Shares = append(sess.Shares, indexer.SessionShare{Key: sess.Source + "/" + s.Name, Share: s.Name})
		}
	}
	if len(sess.Shares) == 0 {
		a.connectMsg = "Chọn ít nhất 1 share."
		return
	}
	a.openSession(sess)
}

func (a *app) connectLocal() {
	if a.localFolder == "" {
		a.connectMsg = "Chọn thư mục trước."
		return
	}
	if !a.requireStore() {
		return
	}
	a.openSession(indexer.Session{Mode: indexer.ModeLocal, Source: a.localFolder})
}

// openStore opens the index directory once; on failure the app still starts
// and the connect screen explains why nothing can be indexed.
func (a *app) openStore() {
	dir, err := indexer.DefaultDir()
	if err == nil {
		a.store, err = indexer.NewStore(dir, nil)
	}
	if err != nil {
		a.storeErr = err
		a.connectMsg = "Không mở được thư mục index (" + err.Error() + "). Chưa thể quét."
	}
}

func (a *app) requireStore() bool {
	if a.store != nil {
		return true
	}
	a.connectMsg = "Không mở được thư mục index: " + a.storeErr.Error()
	return false
}

// resume restores the saved session, if any, once at startup. It is safe to
// call from any goroutine.
func (a *app) resume() {
	a.scanFolders()
	a.startAuto()
	store := a.store
	if store == nil {
		return
	}
	go func() {
		sess, err := store.LoadSession()
		a.update(func() {
			if a.sessGen != 0 || a.screen != screenConnect {
				return
			}
			switch {
			case err == nil && sess.Source != "":
				a.restoreForm(sess)
				a.openSession(sess)
			case err != nil && !errors.Is(err, os.ErrNotExist):
				a.connectMsg = "Không đọc được phiên đã lưu: " + err.Error()
			}
		})
	}()
}

// restoreForm fills the connect form so "Đổi nguồn" starts from the old source.
func (a *app) restoreForm(sess indexer.Session) {
	if sess.Mode == indexer.ModeServer {
		a.mode, a.host, a.user = modeServer, sess.Host, sess.User
		return
	}
	a.mode, a.localFolder = modeLocal, sess.Source
}

func (a *app) disconnect() {
	a.closeService()
	a.sessionErr = ""
	if a.store != nil {
		if err := a.store.ClearSession(); err != nil {
			a.sessionErr = "Không xoá được phiên đã lưu: " + oneLine(err)
		}
	}
	a.refreshProblems()
	a.screen = screenConnect
	a.settingsOpen = false
	go a.loadSuggestions()
}

// openSession shows the browse screen and loads the saved snapshot of the
// source in the background, then starts an incremental scan. A previous
// service is closed and its notifications ignored.
func (a *app) openSession(sess indexer.Session) {
	a.closeService()
	a.screen, a.source, a.opening = screenBrowse, sess.Source, true
	a.connectMsg, a.status, a.sessionErr = "", "", ""
	gen, store, prevClosed := a.sessGen, a.store, a.closing
	opt := indexer.Options{
		Source:  source.LocalFS{SkipHidden: true, Bulk: true},
		Workers: scanWorkers,
		Notify: func() {
			a.update(func() {
				if a.sessGen == gen {
					a.syncState()
				}
			})
		},
	}
	go func() {
		if prevClosed != nil {
			<-prevClosed
		}
		svc, err := indexer.Open(store, sess, opt)
		a.update(func() {
			if a.sessGen != gen {
				if svc != nil {
					go svc.Close()
				}
				return
			}
			a.opening = false
			if err != nil {
				a.sessionErr = "Không mở được index: " + oneLine(err)
				a.refreshProblems()
				return
			}
			a.svc = svc
			if err := store.SaveSession(sess); err != nil {
				a.sessionErr = "Không lưu được phiên làm việc: " + oneLine(err)
			}
			svc.Scan(indexer.Incremental)
			a.syncState()
		})
	}()
}

// closeService drops the current service and its UI state. Close can block
// for seconds, so it runs off the UI thread. a.closing is closed once every
// service dropped so far has finished closing; openSession waits on it so a
// new service never shares a snapshot file with one still writing.
func (a *app) closeService() {
	a.sessGen++
	a.stopTicker()
	svc := a.svc
	a.svc, a.opening, a.st, a.rates = nil, false, indexer.State{}, nil
	a.items, a.result, a.selected, a.shownOK = nil, search.Result{}, -1, false
	a.idxSize, a.idxStamp = -1, sizeStamp{}
	a.chosen.Clear()
	a.dropBrokenMatches()
	if svc == nil {
		return
	}
	prev, done := a.closing, make(chan struct{})
	a.closing = done
	go func() {
		svc.Close()
		if prev != nil {
			<-prev
		}
		close(done)
	}()
}

// closeOnQuit closes the service synchronously so no snapshot write is cut
// off; call it from the quit hook.
func (a *app) closeOnQuit() {
	a.sessGen++
	a.stopTicker()
	if svc := a.svc; svc != nil {
		a.svc = nil
		svc.Close()
	}
}

// scanNow starts a user-style incremental scan; safe from any goroutine.
func (a *app) scanNow() {
	a.update(func() { a.rescan(indexer.Incremental) })
}

// kick schedules a background incremental scan after delay (wake from sleep);
// safe from any goroutine.
func (a *app) kick(delay time.Duration) {
	a.update(func() {
		if a.svc != nil {
			a.svc.Kick(delay)
		}
	})
}

func (a *app) rescan(m indexer.Mode) {
	if a.svc != nil {
		a.svc.Scan(m)
	}
}

func (a *app) stopScan() {
	if a.svc != nil {
		a.svc.Stop()
	}
}

// syncState copies the service state into the UI, restarts the first-scan
// ticker as needed and refreshes results. UI thread only.
func (a *app) syncState() {
	if a.svc == nil {
		return
	}
	prev := a.st
	a.st = a.svc.State()
	if prev.Scanning != a.st.Scanning || !prev.CheckedAt.Equal(a.st.CheckedAt) {
		a.status = ""
	}
	a.sampleRate(prev.Scanning)
	a.refreshProblems()
	a.syncTicker()
	a.refreshSearch()
	a.refreshIndexSize()
	a.rematchBroken()
}

// rateSample is the entry count the scanner had reached at one instant.
type rateSample struct {
	at time.Time
	n  int64
}

// sampleRate records the scan progress for the current-speed card; a new scan
// starts a fresh series because its counters restart at zero. UI thread only.
func (a *app) sampleRate(wasScanning bool) {
	if !a.st.Scanning {
		return
	}
	if !wasScanning {
		a.rates = nil
	}
	p := a.st.Progress
	a.rates = appendSample(a.rates, rateSample{time.Now(), p.Files + p.Dirs}, rateWindow)
}

// appendSample adds x and drops samples older than window before it.
func appendSample(s []rateSample, x rateSample, window time.Duration) []rateSample {
	s = append(s, x)
	i := 0
	for i < len(s)-1 && x.at.Sub(s[i].at) > window {
		i++
	}
	return s[i:]
}

// recentRate is entries per second between the oldest sample within window
// of the newest one and the newest. Zero without a usable span, and when the
// counter went backwards (a new scan).
func recentRate(samples []rateSample, window time.Duration) float64 {
	if len(samples) < 2 {
		return 0
	}
	last := samples[len(samples)-1]
	first := last
	for _, x := range samples {
		if last.at.Sub(x.at) <= window {
			first = x
			break
		}
	}
	span, delta := last.at.Sub(first.at).Seconds(), last.n-first.n
	if span <= 0 || delta < 0 {
		return 0
	}
	return float64(delta) / span
}

// refreshProblems rebuilds the problem rows and logs each line the first
// time it appears. UI thread only.
func (a *app) refreshProblems() {
	var sticky []string
	for _, s := range []string{a.sessionErr, a.trayErr} {
		if s != "" {
			sticky = append(sticky, s)
		}
	}
	a.problemRows = problems(a.st, sticky)
	for _, line := range newLines(a.loggedRows, a.problemRows) {
		log.Println(line)
	}
	a.loggedRows = a.problemRows
}

// problems lists what is wrong, one line per problem: errors of the index
// service first, then app-level sticky errors. Nil when all is well.
func problems(st indexer.State, sticky []string) []string {
	var out []string
	if st.LoadErr != nil {
		out = append(out, "Không nạp được index đã lưu: "+oneLine(st.LoadErr))
	}
	if st.ScanErr != nil {
		out = append(out, "Lượt quét chưa trọn vẹn: "+oneLine(st.ScanErr))
	}
	if st.SaveErr != nil {
		out = append(out, "Không lưu được index: "+oneLine(st.SaveErr))
	}
	if n := len(st.FailedDirs); n > 0 {
		out = append(out, fmt.Sprintf("Không đọc được %d thư mục, file bên trong không có trong index. Ví dụ: %s", max(int(st.Progress.Errors), n), st.FailedDirs[0]))
	}
	return append(out, sticky...)
}

// newLines returns the lines of cur that were not in prev.
func newLines(prev, cur []string) []string {
	var out []string
	for _, l := range cur {
		if !slices.Contains(prev, l) {
			out = append(out, l)
		}
	}
	return out
}

// syncTicker keeps a ticker alive only while a scan runs, because Gen does
// not move while a first scan fills the live index and progress changes
// without a notification.
func (a *app) syncTicker() {
	if !a.st.Scanning {
		a.stopTicker()
		return
	}
	if a.tickStop != nil {
		return
	}
	stop, gen := make(chan struct{}), a.sessGen
	a.tickStop = stop
	go func() {
		t := time.NewTicker(scanTick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				a.update(func() {
					if a.sessGen == gen && a.tickStop == stop {
						a.syncState()
					}
				})
			}
		}
	}()
}

func (a *app) stopTicker() {
	if a.tickStop != nil {
		close(a.tickStop)
		a.tickStop = nil
	}
}

// refreshSearch rebuilds the visible rows. It reruns only when the index
// generation or size, the query or the filter changed.
func (a *app) refreshSearch() {
	if a.svc == nil {
		a.items, a.result = nil, search.Result{}
		return
	}
	ix := a.svc.Current()
	key := resultKey{a.st.Gen, a.query, a.imagesOnly, ix.Len()}
	if !needsSearch(a.shown, a.shownOK, key) {
		return
	}
	a.shown, a.shownOK = key, true
	opt := search.Options{Limit: resultLimit, FilesOnly: true}
	if a.imagesOnly {
		opt.Extensions = imageExts
	}
	selPath := ""
	if a.selected >= 0 && a.selected < len(a.items) {
		selPath = a.items[a.selected].Path
	}
	a.result = ix.Search(a.query, opt)
	items := make([]search.Item, 0, len(a.result.IDs))
	for _, id := range a.result.IDs {
		items = append(items, ix.Item(id))
	}
	sortByPath(items)
	a.items = items
	a.selected = reselect(items, selPath)
}

// sortByPath orders items by path so rows keep their order across swaps,
// which renumber ids.
func sortByPath(items []search.Item) {
	slices.SortFunc(items, func(x, y search.Item) int { return strings.Compare(x.Path, y.Path) })
}

// reselect returns the position of path in items, or -1.
func reselect(items []search.Item, path string) int {
	if path == "" {
		return -1
	}
	return slices.IndexFunc(items, func(it search.Item) bool { return it.Path == path })
}

// sizeStamp is what the cached index file size was read for.
type sizeStamp struct {
	gen      int
	checked  time.Time
	scanning bool
}

// refreshIndexSize re-reads the size of the snapshot file off the UI thread
// when the panel is open and the index or last check changed.
func (a *app) refreshIndexSize() {
	st := sizeStamp{a.st.Gen, a.st.CheckedAt, a.st.Scanning}
	if !a.settingsOpen || a.store == nil || a.idxBusy || st == a.idxStamp {
		return
	}
	a.idxBusy, a.idxStamp = true, st
	path, gen := a.store.Path(a.source), a.sessGen
	go func() {
		fi, err := os.Stat(path)
		a.update(func() {
			a.idxBusy = false
			if a.sessGen != gen {
				return
			}
			a.idxSize = -1
			if err == nil {
				a.idxSize = fi.Size()
			}
		})
	}()
}

func (a *app) toggleSettings() {
	a.settingsOpen = !a.settingsOpen
	if a.settingsOpen {
		a.up.resetForm()
	}
	a.idxStamp = sizeStamp{}
	a.refreshIndexSize()
}

// statusLine is the scan state line; a transient action message wins until
// the next scan state change. Errors are shown by problems.
func (a *app) statusLine() string {
	if a.status != "" {
		return a.status
	}
	if a.opening {
		return "Đang nạp index đã lưu…"
	}
	if a.svc == nil {
		return ""
	}
	return scanState(a.st)
}

func scanState(st indexer.State) string {
	switch {
	case st.Scanning && !st.Complete:
		return "Index chưa đầy đủ, đang quét tiếp"
	case st.Scanning:
		return "Đang cập nhật…"
	case st.Offline:
		return "Offline, đang dùng index đã lưu"
	case !st.Complete:
		return "Index chưa đầy đủ, bấm Quét lại để quét tiếp"
	case st.CheckedAt.IsZero():
		return ""
	}
	return "Kiểm tra lúc " + st.CheckedAt.Format("15:04")
}

func oneLine(err error) string {
	return strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, strings.ReplaceAll(err.Error(), "\n", "; "))
}

func (a *app) openSelected(reveal bool) {
	if a.selected < 0 || a.selected >= len(a.items) {
		return
	}
	it := a.items[a.selected]
	args := []string{it.Path}
	if reveal {
		args = append([]string{"-R"}, args...)
	}
	go func() {
		out, err := exec.Command("open", args...).CombinedOutput()
		if err == nil {
			return
		}
		msg := "Không mở được " + it.Name + ": " + oneLine(errors.New(strings.TrimSpace(string(out)+" "+err.Error())))
		a.update(func() { a.status = msg })
	}()
}
