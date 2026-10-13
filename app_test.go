package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/indexer"
	"github.com/isuccesscorp/scan-image/internal/s3up"
	"github.com/isuccesscorp/scan-image/internal/scanner"
	"github.com/isuccesscorp/scan-image/internal/search"
)

func TestRelKey(t *testing.T) {
	photos1 := indexer.RootInfo{Name: "Photos", Path: "/Volumes/Photos-1"}
	photos := indexer.RootInfo{Name: "Photos", Path: "/Volumes/Photos"}
	docs := indexer.RootInfo{Name: "Docs", Path: "/Volumes/Docs"}
	pictures := indexer.RootInfo{Name: "Pictures", Path: "/Users/x/Pictures"}
	siblingA := indexer.RootInfo{Name: "Photos", Path: "/Volumes/Photos"}
	siblingB := indexer.RootInfo{Name: "Archive", Path: "/Volumes/Photos-1"}

	tests := []struct {
		name  string
		roots []indexer.RootInfo
		path  string
		want  string
	}{
		{"uses root name not remounted mountpoint base", []indexer.RootInfo{photos1}, "/Volumes/Photos-1/a/b.jpg", "Photos/a/b.jpg"},
		{"same file via original mountpoint gives same key", []indexer.RootInfo{photos}, "/Volumes/Photos/a/b.jpg", "Photos/a/b.jpg"},
		{"prefix match respects path separator boundary", []indexer.RootInfo{siblingA, siblingB}, "/Volumes/Photos-1/x.jpg", "Archive/x.jpg"},
		{"picks the right root among several", []indexer.RootInfo{photos, docs}, "/Volumes/Docs/r/s.pdf", "Docs/r/s.pdf"},
		{"nested folders keep forward slashes", []indexer.RootInfo{docs}, "/Volumes/Docs/a/b/c/d.txt", "Docs/a/b/c/d.txt"},
		{"path under no root falls back to base name", []indexer.RootInfo{photos, docs}, "/elsewhere/dir/z.jpg", "z.jpg"},
		{"local source root", []indexer.RootInfo{pictures}, "/Users/x/Pictures/2024/a.jpg", "Pictures/2024/a.jpg"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := relKey(tc.roots, tc.path)
			if got != tc.want {
				t.Fatalf("relKey(%v, %q) = %q, want %q", tc.roots, tc.path, got, tc.want)
			}
		})
	}
}

func TestReselect(t *testing.T) {
	items := []search.Item{
		{ID: 7, Path: "/r/a.jpg"},
		{ID: 3, Path: "/r/b.jpg"},
		{ID: 9, Path: "/r/c.jpg"},
	}
	tests := []struct {
		name  string
		items []search.Item
		path  string
		want  int
	}{
		{"finds item by path", items, "/r/b.jpg", 1},
		{"finds last item", items, "/r/c.jpg", 2},
		{"returns -1 when item is gone", items, "/r/zzz.jpg", -1},
		{"returns -1 for empty path", items, "", -1},
		{"returns -1 for empty items", nil, "/r/a.jpg", -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reselect(tc.items, tc.path); got != tc.want {
				t.Fatalf("reselect(%q) = %d, want %d", tc.path, got, tc.want)
			}
		})
	}
}

func TestSortByPath(t *testing.T) {
	paths := func(items []search.Item) []string {
		out := make([]string, len(items))
		for i, it := range items {
			out[i] = it.Path
		}
		return out
	}
	want := []string{"/r/a", "/r/b", "/r/c"}
	tests := []struct {
		name  string
		items []search.Item
	}{
		{"sorts ascending by path", []search.Item{{ID: 1, Path: "/r/c"}, {ID: 2, Path: "/r/a"}, {ID: 3, Path: "/r/b"}}},
		{"different id order sorts to same sequence", []search.Item{{ID: 30, Path: "/r/b"}, {ID: 10, Path: "/r/c"}, {ID: 20, Path: "/r/a"}}},
		{"already sorted stays sorted", []search.Item{{ID: 1, Path: "/r/a"}, {ID: 2, Path: "/r/b"}, {ID: 3, Path: "/r/c"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sortByPath(tc.items)
			got := paths(tc.items)
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("order = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestProblems(t *testing.T) {
	e := func(s string) error { return errors.New(s) }
	tests := []struct {
		name   string
		st     indexer.State
		sticky []string
		want   []string
	}{
		{"empty state is nil", indexer.State{}, nil, nil},
		{"load error", indexer.State{LoadErr: e("bad")}, nil, []string{"Không nạp được index đã lưu: bad"}},
		{"scan error", indexer.State{ScanErr: e("mount")}, nil, []string{"Lượt quét chưa trọn vẹn: mount"}},
		{"save error", indexer.State{SaveErr: e("disk")}, nil, []string{"Không lưu được index: disk"}},
		{"unreadable directories", indexer.State{FailedDirs: []string{"/v/a: no such file or directory", "/v/b: x"}, Progress: scanner.Progress{Errors: 7}}, nil,
			[]string{"Không đọc được 7 thư mục, file bên trong không có trong index. Ví dụ: /v/a: no such file or directory"}},
		{"multiline error is one line", indexer.State{ScanErr: e("a\nb")}, nil, []string{"Lượt quét chưa trọn vẹn: a; b"}},
		{
			"all kinds in order then sticky",
			indexer.State{LoadErr: e("l"), ScanErr: e("s"), SaveErr: e("v")},
			[]string{"sess", "tray"},
			[]string{"Không nạp được index đã lưu: l", "Lượt quét chưa trọn vẹn: s", "Không lưu được index: v", "sess", "tray"},
		},
		{"sticky alone is kept", indexer.State{}, []string{"tray"}, []string{"tray"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := problems(tc.st, tc.sticky); !slices.Equal(got, tc.want) {
				t.Fatalf("problems = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewLines(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur []string
		want      []string
	}{
		{"nothing new", []string{"a"}, []string{"a"}, nil},
		{"first appearance", nil, []string{"a"}, []string{"a"}},
		{"changed text", []string{"a: 1"}, []string{"a: 2"}, []string{"a: 2"}},
		{"cleared", []string{"a"}, nil, nil},
		{"only the added one", []string{"a"}, []string{"a", "b"}, []string{"b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := newLines(tc.prev, tc.cur); !slices.Equal(got, tc.want) {
				t.Fatalf("newLines = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNeedsSearch(t *testing.T) {
	base := resultKey{gen: 3, query: "cat", images: true, n: 100}
	tests := []struct {
		name     string
		havePrev bool
		cur      resultKey
		want     bool
	}{
		{"no previous result", false, base, true},
		{"same key", true, base, false},
		{"index grew", true, resultKey{3, "cat", true, 101}, true},
		{"generation moved", true, resultKey{4, "cat", true, 100}, true},
		{"query changed", true, resultKey{3, "dog", true, 100}, true},
		{"filter changed", true, resultKey{3, "cat", false, 100}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsSearch(base, tc.havePrev, tc.cur); got != tc.want {
				t.Fatalf("needsSearch = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRecentRate(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := func(sec int, n int64) rateSample { return rateSample{t0.Add(time.Duration(sec) * time.Second), n} }
	tests := []struct {
		name    string
		samples []rateSample
		want    float64
	}{
		{"empty", nil, 0},
		{"one sample", []rateSample{at(0, 100)}, 0},
		{"same instant", []rateSample{at(0, 100), at(0, 200)}, 0},
		{"steady rate", []rateSample{at(0, 0), at(5, 500), at(10, 1000)}, 100},
		{"old samples outside window ignored", []rateSample{at(0, 0), at(20, 1000), at(25, 1500), at(30, 2500)}, 150},
		{"counter reset never negative", []rateSample{at(0, 5000), at(5, 10)}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := recentRate(tc.samples, 10*time.Second); got != tc.want {
				t.Fatalf("recentRate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAppendSample(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var s []rateSample
	for i := range 100 {
		s = appendSample(s, rateSample{t0.Add(time.Duration(i) * time.Second), int64(i)}, 10*time.Second)
	}
	if len(s) != 11 {
		t.Fatalf("len = %d, want 11 (samples within window)", len(s))
	}
	if s[len(s)-1].n != 99 {
		t.Fatalf("newest sample dropped")
	}
}

func testUploader() *app {
	a := &app{}
	a.up.startOnce.Do(func() {})
	a.up.queue = make(chan *uploadJob, 16)
	return a
}

func TestEnqueueSnapshotsConfigIntoJob(t *testing.T) {
	a := testUploader()
	a.up.cfg = s3up.Config{Bucket: "prod"}
	confirmed := a.up.cfg
	jobs := a.enqueueUploads(confirmed, []uploadSpec{{Path: "/v/a.png", Key: "X/a.png"}})
	a.up.cfg.Bucket = "staging"
	if jobs[0].Cfg.Bucket != "prod" {
		t.Errorf("job bucket = %q, want prod", jobs[0].Cfg.Bucket)
	}
}

func TestEnqueueDedupesOnPathAndKey(t *testing.T) {
	a := testUploader()
	specs := []uploadSpec{
		{Path: "/v/a.png", Key: "X/a.png"},
		{Path: "/v/a.png", Key: "Y/a.png"},
		{Path: "/v/a.png", Key: "X/a.png"},
	}
	jobs := a.enqueueUploads(s3up.Config{}, specs)
	if jobs[0] == nil || jobs[1] == nil || jobs[2] != nil {
		t.Fatalf("jobs = %v", jobs)
	}
	again := a.enqueueUploads(s3up.Config{}, specs[:1])
	if again[0] != nil {
		t.Error("a queued (path, key) was queued twice")
	}
}

func TestEnqueueCancelsSupersededFailedJob(t *testing.T) {
	a := testUploader()
	old := &uploadJob{Path: "/v/old.png", Key: "X/a.png", State: upFailed}
	other := &uploadJob{Path: "/v/o.png", Key: "X/o.png", State: upFailed}
	done := &uploadJob{Path: "/v/d.png", Key: "X/a.png", State: upDone}
	a.up.jobs = []*uploadJob{old, other, done}
	a.enqueueUploads(s3up.Config{}, []uploadSpec{{Path: "/v/new.png", Key: "X/a.png"}})
	if old.State != upCanceled || other.State != upFailed || done.State != upDone {
		t.Errorf("states = %d, %d, %d", old.State, other.State, done.State)
	}
	a.retryFailed()
	if old.State != upCanceled {
		t.Error("retryFailed requeued a superseded job")
	}
}

func TestQueueMessage(t *testing.T) {
	j := &uploadJob{}
	tests := []struct {
		name string
		jobs []*uploadJob
		want string
	}{
		{"all added", []*uploadJob{j, j}, "Đã thêm 2 file vào hàng đợi upload."},
		{"some skipped", []*uploadJob{j, nil, nil}, "Đã thêm 1 file vào hàng đợi upload, bỏ qua 2 file đã có trong hàng đợi."},
	}
	for _, tc := range tests {
		if got := queueMessage(tc.jobs); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSourceStampCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	mod := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		stamp sourceStamp
		bad   bool
	}{
		{"unchanged", sourceStamp{Size: 5, ModTime: mod}, false},
		{"size differs", sourceStamp{Size: 6, ModTime: mod}, true},
		{"mtime differs", sourceStamp{Size: 5, ModTime: mod.Add(time.Minute)}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.stamp.check(path)
			if (err != nil) != tc.bad {
				t.Fatalf("err = %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "File nguồn đã đổi từ lúc khớp") {
				t.Errorf("err = %q", err)
			}
		})
	}
	if err := (sourceStamp{Size: 5, ModTime: mod}).check(path + ".gone"); err == nil {
		t.Error("missing source passed")
	}
	link := filepath.Join(t.TempDir(), "link.png")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := (sourceStamp{Size: 5, ModTime: mod}).check(link); err == nil {
		t.Error("a symbolic link to a matching file passed")
	}
}

func TestSettingsFormIsSeparateFromSavedConfig(t *testing.T) {
	u := uploads{cfg: s3up.Config{Bucket: "prod", AccessKey: "k", SecretKey: "s"}}
	u.resetForm()
	u.form.Bucket = "  staging "
	u.secret = "new"
	if u.cfg.Bucket != "prod" {
		t.Fatalf("saved bucket = %q", u.cfg.Bucket)
	}
	got := u.formConfig()
	if got.Bucket != "staging" || got.SecretKey != "new" || got.AccessKey != "k" {
		t.Errorf("formConfig = %+v", got)
	}
	u.resetForm()
	if u.form.Bucket != "prod" || u.secret != "" {
		t.Errorf("after reset: %q, %q", u.form.Bucket, u.secret)
	}
}

func TestOneLineStripsControlAndFormatCharacters(t *testing.T) {
	got := oneLine(errors.New("a\nb\x1b[31m\u202ec\u2028d\x00"))

	if want := "a; b[31mcd"; got != want {
		t.Fatalf("oneLine = %q, want %q", got, want)
	}
}

func TestJobWebURLOnlyForFilesThatAreOnTheBucket(t *testing.T) {
	cfg := s3up.Config{Bucket: "thanhcong", Region: "us-east-1"}
	const want = "https://thanhcong.s3.us-east-1.amazonaws.com/Etsy/Etsy-Chau/A-091026A13.png"
	for state, wantURL := range map[uploadState]string{
		upQueued: "", upRunning: "", upFailed: "", upCanceled: "", upDone: want, upSkipped: want,
	} {
		j := &uploadJob{Key: "Etsy/Etsy-Chau/A-091026A13.png", Cfg: cfg, State: state}
		if got := jobWebURL(j); got != wantURL {
			t.Errorf("state %d: jobWebURL = %q, want %q", state, got, wantURL)
		}
	}
}
