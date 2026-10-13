package datefind

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/source"
)

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeSrc is an in-memory tree that records every directory listed.
type fakeSrc struct {
	dirs  map[string][]source.Entry
	fail  map[string]bool
	block bool

	mu     sync.Mutex
	listed []string
}

func newFake(files ...string) *fakeSrc {
	f := &fakeSrc{dirs: map[string][]source.Entry{}, fail: map[string]bool{}}
	for _, p := range files {
		f.addFile(p)
	}
	return f
}

func fileSize(p string) int64     { return int64(len(p)) }
func fileTime(p string) time.Time { return baseTime.Add(time.Duration(len(p)) * time.Second) }
func (f *fakeSrc) addFile(p string) {
	dir, name := path.Split(p)
	dir = filepath.Clean(dir)
	f.mkdir(dir)
	f.dirs[dir] = append(f.dirs[dir], source.Entry{Name: name, Size: fileSize(p), ModTime: fileTime(p)})
}

// mkdir creates dir and every parent, registering each under its parent.
func (f *fakeSrc) mkdir(dir string) {
	if _, ok := f.dirs[dir]; ok {
		return
	}
	f.dirs[dir] = nil
	parent := filepath.Dir(dir)
	if parent == dir {
		return
	}
	f.mkdir(parent)
	f.dirs[parent] = append(f.dirs[parent], source.Entry{Name: filepath.Base(dir), IsDir: true})
}

func (f *fakeSrc) ReadDir(ctx context.Context, p string) ([]source.Entry, error) {
	f.mu.Lock()
	f.listed = append(f.listed, p)
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.fail[p] {
		return nil, errors.New("boom")
	}
	es, ok := f.dirs[p]
	if !ok {
		return nil, errors.New("no such dir")
	}
	return append([]source.Entry(nil), es...), nil
}

func (f *fakeSrc) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not supported")
}

func (f *fakeSrc) listedSorted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.listed...)
	sort.Strings(out)
	return out
}

// totalDirs counts the directories under roots, the cost of a full walk.
func (f *fakeSrc) totalDirs(roots ...string) int {
	n := 0
	var walk func(string)
	walk = func(d string) {
		n++
		for _, e := range f.dirs[d] {
			if e.IsDir {
				walk(filepath.Join(d, e.Name))
			}
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return n
}

func hitPaths(hs []Hit) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.Path)
	}
	sort.Strings(out)
	return out
}

func TestFindFiveLayouts(t *testing.T) {
	const (
		l1  = "/nas/Hieung/2025/Thang 11/17/Trang NTT.Etsy/S-171125A01.png"
		l1b = "/nas/Hieung/2025/Thang 11/17/Other.Etsy/S-171125A01.png"
		l2  = "/nas/Giang.NTH/2025/Tháng 3/11-3/Huyền HTN.Etsy/A-110325B02.png"
		l3  = "/nas/dungptq/2026/thang 2/9-2/Linh PT.Etsy/S-090226C03.png"
		l4  = "/nas/ha.tran/Designer ZenE/Ha_Designer/2025/7/9/An_NTT/A-090725D04.png"
		l5  = "/nas2/Giang.NTH 2026/thang 10/12-10/Mai.Etsy/S-121026E05.png"
		l5n = "/nas2/Giang.NTH 2026/thang 10/12-10/Mai.Etsy/Ảnh-121026.png"
	)
	files := []string{
		l1, l1b, l2, l3, l4, l5, l5n,
		"/nas/Hieung/2025/Thang 11/18/Trang NTT.Etsy/S-171125A01.png",
		"/nas/Hieung/2025/Thang 10/17/Trang NTT.Etsy/S-171125A01.png",
		"/nas/Hieung/2024/Thang 11/17/Trang NTT.Etsy/S-171125A01.png",
		"/nas/Hieung/2025/Mockup 12/a/x.png",
		"/nas/Hieung/2025/Mockup/b/x.png",
		"/nas/Giang.NTH/2024/Tháng 3/11-3/x.png",
		"/nas/Giang.NTH/2025/Tháng 4/11-3/x.png",
		"/nas/dungptq/2026/thang 3/9-2/x.png",
		"/nas/ha.tran/Designer ZenE/Ha_Designer/Old/x.png",
		"/nas/ha.tran/Designer ZenE/Ha_Designer/2025/7/10/An_NTT/x.png",
		"/nas/ha.tran/Designer ZenE/Ha_Designer/2025/8/9/An_NTT/x.png",
		"/nas/Archive/x1/f.png",
		"/nas/Archive/x2/f.png",
		"/nas2/Other/deep/x.png",
		"/nas2/Giang.NTH 2026/thang 9/12-10/x.png",
		"/nas2/Giang.NTH 2026/thang 10/13-10/x.png",
	}
	for i := 0; i < 25; i++ {
		files = append(files, "/nas/Hieung/2025/Mockup/m"+string(rune('a'+i))+"/f.png")
	}
	src := newFake(files...)
	names := []string{
		"S-171125A01.png", "A-110325B02.png", "S-090226C03.png", "A-090725D04.png",
		"S-121026E05.png", "Ảnh-121026.png", "OGWG04-16.png", "S-010101Z99.png",
	}

	res, err := Find(context.Background(), src, []string{"/nas", "/nas2"}, names, Options{})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string][]string{
		"S-171125A01.png": {l1, l1b},
		"A-110325B02.png": {l2},
		"S-090226C03.png": {l3},
		"A-090725D04.png": {l4},
		"S-121026E05.png": {l5},
		"Ảnh-121026.png":  {l5n},
	}
	if len(res.Hits) != len(want) {
		t.Errorf("hit keys = %d want %d: %v", len(res.Hits), len(want), res.Hits)
	}
	for name, paths := range want {
		sort.Strings(paths)
		hs := res.Hits[name]
		if got := hitPaths(hs); !reflect.DeepEqual(got, paths) {
			t.Errorf("%s: paths %q want %q", name, got, paths)
		}
		for _, h := range hs {
			if h.Size != fileSize(h.Path) || !h.ModTime.Equal(fileTime(h.Path)) {
				t.Errorf("%s: size/mtime wrong for %s: %d %v", name, h.Path, h.Size, h.ModTime)
			}
		}
	}
	if !reflect.DeepEqual(res.Skipped, []string{"OGWG04-16.png"}) {
		t.Errorf("Skipped = %q", res.Skipped)
	}
	if res.Truncated || res.Errors != 0 {
		t.Errorf("Truncated=%v Errors=%d", res.Truncated, res.Errors)
	}

	wantListed := []string{
		"/nas",
		"/nas/Hieung", "/nas/Hieung/2025", "/nas/Hieung/2025/Thang 11", "/nas/Hieung/2025/Thang 11/17",
		"/nas/Hieung/2025/Thang 11/17/Trang NTT.Etsy", "/nas/Hieung/2025/Thang 11/17/Other.Etsy",
		"/nas/Giang.NTH", "/nas/Giang.NTH/2025", "/nas/Giang.NTH/2025/Tháng 3",
		"/nas/Giang.NTH/2025/Tháng 3/11-3", "/nas/Giang.NTH/2025/Tháng 3/11-3/Huyền HTN.Etsy",
		"/nas/dungptq", "/nas/dungptq/2026", "/nas/dungptq/2026/thang 2",
		"/nas/dungptq/2026/thang 2/9-2", "/nas/dungptq/2026/thang 2/9-2/Linh PT.Etsy",
		"/nas/ha.tran", "/nas/ha.tran/Designer ZenE", "/nas/ha.tran/Designer ZenE/Ha_Designer",
		"/nas/ha.tran/Designer ZenE/Ha_Designer/2025", "/nas/ha.tran/Designer ZenE/Ha_Designer/2025/7",
		"/nas/ha.tran/Designer ZenE/Ha_Designer/2025/7/9", "/nas/ha.tran/Designer ZenE/Ha_Designer/2025/7/9/An_NTT",
		"/nas/Archive", "/nas/Archive/x1", "/nas/Archive/x2",
		"/nas2", "/nas2/Giang.NTH 2026", "/nas2/Giang.NTH 2026/thang 10",
		"/nas2/Giang.NTH 2026/thang 10/12-10", "/nas2/Giang.NTH 2026/thang 10/12-10/Mai.Etsy",
		"/nas2/Other", "/nas2/Other/deep",
	}
	sort.Strings(wantListed)
	if got := src.listedSorted(); !reflect.DeepEqual(got, wantListed) {
		t.Errorf("listed:\n got %q\nwant %q", got, wantListed)
	}
	if res.Dirs != len(wantListed) {
		t.Errorf("Dirs = %d want %d", res.Dirs, len(wantListed))
	}
	for _, p := range src.listedSorted() {
		if strings.Contains(p, "Mockup") || strings.Contains(p, "/Old") {
			t.Errorf("decoy listed: %s", p)
		}
	}
	t.Logf("guided walk listed %d dirs, full walk would list %d", res.Dirs, src.totalDirs("/nas", "/nas2"))
}

func TestFindNoDatedNamesListsNothing(t *testing.T) {
	src := newFake("/r/a/x.png")
	res, err := Find(context.Background(), src, []string{"/r"}, []string{"OGWG04-16.png", "plain.png"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(src.listedSorted()) != 0 || res.Dirs != 0 || len(res.Hits) != 0 {
		t.Errorf("listed %v, res %+v", src.listedSorted(), res)
	}
	if !reflect.DeepEqual(res.Skipped, []string{"OGWG04-16.png", "plain.png"}) {
		t.Errorf("Skipped = %q", res.Skipped)
	}
}

func TestFindCaseInsensitive(t *testing.T) {
	src := newFake("/r/2025/11/17/S-171125A01.PNG")
	res, err := Find(context.Background(), src, []string{"/r"}, []string{"s-171125a01.png"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitPaths(res.Hits["s-171125a01.png"]); !reflect.DeepEqual(got, []string{"/r/2025/11/17/S-171125A01.PNG"}) {
		t.Errorf("hits = %q", got)
	}
}

func TestFindDayUnderYear(t *testing.T) {
	src := newFake("/r/2025/17-11/f/S-171125A01.png", "/r/2025/17-12/f/S-171125A01.png")
	res, err := Find(context.Background(), src, []string{"/r"}, []string{"S-171125A01.png"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitPaths(res.Hits["S-171125A01.png"]); !reflect.DeepEqual(got, []string{"/r/2025/17-11/f/S-171125A01.png"}) {
		t.Errorf("hits = %q", got)
	}
	if res.Dirs != 4 {
		t.Errorf("Dirs = %d want 4 (/r, 2025, 17-11, f)", res.Dirs)
	}
}

func TestFindMaxDirs(t *testing.T) {
	var files []string
	for i := 'a'; i < 'a'+10; i++ {
		files = append(files, "/r/"+string(i)+"/f.png")
	}
	src := newFake(files...)
	res, err := Find(context.Background(), src, []string{"/r"}, []string{"S-171125A01.png"}, Options{MaxDirs: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.Dirs != 3 {
		t.Errorf("Truncated=%v Dirs=%d", res.Truncated, res.Dirs)
	}

	res, err = Find(context.Background(), src, []string{"/r"}, []string{"S-171125A01.png"}, Options{MaxDirs: 11})
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated || res.Dirs != 11 {
		t.Errorf("exact fit: Truncated=%v Dirs=%d", res.Truncated, res.Dirs)
	}
}

func TestFindFailedDirCounted(t *testing.T) {
	src := newFake(
		"/nas/a/2026/2/9/S-090226C03.png",
		"/nas/b/2025/11/17/S-171125A01.png",
	)
	src.fail["/nas/a/2026"] = true
	res, err := Find(context.Background(), src, []string{"/nas"}, []string{"S-090226C03.png", "S-171125A01.png"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != 1 {
		t.Errorf("Errors = %d", res.Errors)
	}
	if len(res.Hits["S-171125A01.png"]) != 1 || len(res.Hits["S-090226C03.png"]) != 0 {
		t.Errorf("hits = %v", res.Hits)
	}
}

func TestFindPreDepth(t *testing.T) {
	src := newFake("/r/a/b/2025/11/17/S-171125A01.png")
	name := []string{"S-171125A01.png"}
	for _, tc := range []struct {
		depth int
		found bool
	}{{1, false}, {2, true}, {0, true}} {
		res, err := Find(context.Background(), src, []string{"/r"}, name, Options{PreDepth: tc.depth})
		if err != nil {
			t.Fatal(err)
		}
		if got := len(res.Hits[name[0]]) == 1; got != tc.found {
			t.Errorf("PreDepth %d: found=%v want %v", tc.depth, got, tc.found)
		}
	}
}

func TestFindDayFolderDepthLimit(t *testing.T) {
	src := newFake(
		"/r/2025/11/17/a/b/c/d/S-171125A01.png",
		"/r/2025/11/17/a/b/c/d/e/S-171125B02.png",
	)
	res, err := Find(context.Background(), src, []string{"/r"}, []string{"S-171125A01.png", "S-171125B02.png"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits["S-171125A01.png"]) != 1 || len(res.Hits["S-171125B02.png"]) != 0 {
		t.Errorf("hits = %v", res.Hits)
	}
}

func TestFindCancelReturnsPromptly(t *testing.T) {
	src := newFake("/r/a/f.png")
	src.block = true
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := Find(ctx, src, []string{"/r"}, []string{"S-171125A01.png"}, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v", d)
	}
}

func TestFindLabelledYearFolderDoesNotHideItsSiblings(t *testing.T) {
	const (
		inLabelled = "/share/Giang.NTH 2026/thang 10/12-10/Mai.Etsy/S-121026E05.png"
		inSibling  = "/share/Hieung/2026/Thang 10/12/Anh.Etsy/S-121026E06.png"
	)
	src := newFake(inLabelled, inSibling,
		"/share/Giang.NTH 2025/thang 10/12-10/Mai.Etsy/old.png",
	)

	res, err := Find(context.Background(), src, []string{"/share"},
		[]string{"S-121026E05.png", "S-121026E06.png"}, Options{})

	if err != nil {
		t.Fatal(err)
	}
	if got := hitPaths(res.Hits["S-121026E05.png"]); !slices.Equal(got, []string{inLabelled}) {
		t.Errorf("file under the labelled year folder: %v, want %v", got, []string{inLabelled})
	}
	if got := hitPaths(res.Hits["S-121026E06.png"]); !slices.Equal(got, []string{inSibling}) {
		t.Errorf("file under a sibling designer folder: %v, want %v", got, []string{inSibling})
	}
	if slices.Contains(src.listedSorted(), "/share/Giang.NTH 2025") {
		t.Error("a folder labelled with another year was listed")
	}
}

func TestFindDayFolderWrittenAsOneNumber(t *testing.T) {
	const (
		short = "/share/Hieung/2026/Thang 5/120526/Mai.Etsy/S-120526A01.png"
		long  = "/share/Lan/2026/12052026/S-120526A01.png"
		ddmm  = "/share/Vy/2026/thang 5/1205/S-120526A01.png"
	)
	src := newFake(short, long, ddmm, "/share/Vy/2026/thang 5/1305/S-120526A01.png")

	res, err := Find(context.Background(), src, []string{"/share"}, []string{"S-120526A01.png"}, Options{})

	if err != nil {
		t.Fatal(err)
	}
	want := []string{short, long, ddmm}
	sort.Strings(want)
	if got := hitPaths(res.Hits["S-120526A01.png"]); !slices.Equal(got, want) {
		t.Fatalf("hits = %v, want %v", got, want)
	}
}

func TestFindStrayYearFolderAtRootDoesNotHideDesigners(t *testing.T) {
	const want = "/share/Hieung/2026/Thang 10/12/Anh.Etsy/S-121026E06.png"
	src := newFake(want, "/share/2024/old/x.png")

	res, err := Find(context.Background(), src, []string{"/share"}, []string{"S-121026E06.png"}, Options{})

	if err != nil {
		t.Fatal(err)
	}
	if got := hitPaths(res.Hits["S-121026E06.png"]); !slices.Equal(got, []string{want}) {
		t.Fatalf("hits = %v, want %v", got, []string{want})
	}
	if slices.Contains(src.listedSorted(), "/share/2024") {
		t.Error("the folder of another year was listed")
	}
}

func TestFindDoesNotExploreWideFoldersBelowTheRoot(t *testing.T) {
	const want = "/share/Hieung/2026/Thang 10/12/Anh.Etsy/S-121026E06.png"
	files := []string{want}
	for i := range maxPreFanout + 10 {
		files = append(files, fmt.Sprintf("/share/Mockup/set%03d/deep/x.png", i))
	}
	src := newFake(files...)

	res, err := Find(context.Background(), src, []string{"/share"}, []string{"S-121026E06.png"}, Options{})

	if err != nil {
		t.Fatal(err)
	}
	if got := hitPaths(res.Hits["S-121026E06.png"]); !slices.Equal(got, []string{want}) {
		t.Fatalf("hits = %v, want %v", got, []string{want})
	}
	for _, p := range src.listedSorted() {
		if strings.HasPrefix(p, "/share/Mockup/") {
			t.Fatalf("explored %s: a folder with more than %d subfolders is not a designer's date tree", p, maxPreFanout)
		}
	}
}
