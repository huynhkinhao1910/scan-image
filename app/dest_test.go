package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/indexer"
	"github.com/isuccesscorp/scan-image/internal/s3up"
	"github.com/isuccesscorp/scan-image/internal/search"
)

func TestParseFolder(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty is the default", "", "", false},
		{"blanks are the default", "   ", "", false},
		{"adds trailing slash", "Etsy/Etsy-Mai", "Etsy/Etsy-Mai/", false},
		{"keeps single trailing slash", "Etsy/Etsy-Mai/", "Etsy/Etsy-Mai/", false},
		{"strips leading slashes and spaces", "  //Etsy/ ", "Etsy/", false},
		{"collapses repeated trailing slashes", "Etsy///", "Etsy/", false},
		{"only slashes is the default", "///", "", false},
		{"rejects parent segments", "Etsy/../x", "", true},
		{"rejects backslash", `Etsy\x`, "", true},
		{"rejects empty segment", "Etsy//x", "", true},
		{"rejects control characters", "Etsy/\tx", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFolder(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseFolder(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("parseFolder(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDestKey(t *testing.T) {
	roots := []indexer.RootInfo{{Name: "Photos", Path: "/Volumes/Photos"}}
	withPrefix := s3up.Config{Prefix: "backup"}
	tests := []struct {
		name    string
		cfg     s3up.Config
		folder  string
		path    string
		want    string
		wantErr bool
	}{
		{"default follows the share layout", s3up.Config{}, "", "/Volumes/Photos/a/b.jpg", "Photos/a/b.jpg", false},
		{"default applies the prefix", withPrefix, "", "/Volumes/Photos/a/b.jpg", "backup/Photos/a/b.jpg", false},
		{"folder is verbatim, no prefix", withPrefix, "Etsy/Etsy-Mai/", "/Volumes/Photos/x/C-071026A26.png", "Etsy/Etsy-Mai/C-071026A26.png", false},
		{"folder uses base name only", s3up.Config{}, "A/", "/elsewhere/deep/dir/z.jpg", "A/z.jpg", false},
		{"bad file name", s3up.Config{}, "A/", "/x/..", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := destKey(tc.cfg, roots, tc.folder, tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("destKey err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("destKey = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDuplicateNames(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		want []string
	}{
		{"none", []string{"A/a.png", "A/b.png"}, nil},
		{"one pair", []string{"A/a.png", "A/b.png", "A/a.png"}, []string{"a.png"}},
		{"three of a kind reported once", []string{"A/a.png", "A/a.png", "A/a.png"}, []string{"a.png"}},
		{"sorted", []string{"A/z", "A/b", "A/z", "A/b"}, []string{"b", "z"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := duplicateNames(tc.keys); !slices.Equal(got, tc.want) {
				t.Fatalf("duplicateNames(%v) = %v, want %v", tc.keys, got, tc.want)
			}
		})
	}
}

func TestDestSpecs(t *testing.T) {
	mod := time.Unix(1_700_000_000, 0)
	roots := []indexer.RootInfo{{Name: "Photos", Path: "/Volumes/Photos"}}
	items := []search.Item{
		{Path: "/Volumes/Photos/a/one.png", Size: 10, ModTime: mod},
		{Path: "/Volumes/Photos/b/two.png", Size: 20, ModTime: mod},
		{Path: "/Volumes/Photos/c/one.png", Size: 30, ModTime: mod},
	}
	cfg := s3up.Config{Prefix: "p"}

	t.Run("default keeps the share layout and the settings' options, with the source stamp", func(t *testing.T) {
		got, err := destSpecs(cfg, roots, items, []string{items[0].Path}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Path != items[0].Path || got[0].Key != "p/Photos/a/one.png" {
			t.Fatalf("got %+v", got)
		}
		if got[0].Opt != (s3up.UploadOptions{}) {
			t.Fatalf("options = %+v, want none: the default layout follows Settings", got[0].Opt)
		}
		if e := got[0].Expect; e == nil || e.Size != 10 || !e.ModTime.Equal(mod) {
			t.Fatalf("expect = %+v, want the stamp of the matched file", e)
		}
	})
	t.Run("file missing from the results is refused in the default layout", func(t *testing.T) {
		if _, err := destSpecs(cfg, roots, items, []string{"/Volumes/Photos/gone.png"}, ""); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("folder sets overwrite, verify and the source stamp", func(t *testing.T) {
		got, err := destSpecs(cfg, roots, items, []string{items[1].Path}, "Etsy/")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Key != "Etsy/two.png" {
			t.Fatalf("got %+v", got)
		}
		if !got[0].Opt.Overwrite || !got[0].Opt.Verify {
			t.Fatalf("options = %+v", got[0].Opt)
		}
		if e := got[0].Expect; e == nil || e.Size != 20 || !e.ModTime.Equal(mod) {
			t.Fatalf("expect = %+v", e)
		}
	})
	t.Run("same base name collides", func(t *testing.T) {
		_, err := destSpecs(cfg, roots, items, []string{items[0].Path, items[2].Path}, "Etsy/")
		if err == nil || !strings.Contains(err.Error(), "one.png") {
			t.Fatalf("err = %v, want a collision naming one.png", err)
		}
	})
	t.Run("same base name is fine in the default layout", func(t *testing.T) {
		if _, err := destSpecs(cfg, roots, items, []string{items[0].Path, items[2].Path}, ""); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("file missing from the results is refused in folder mode", func(t *testing.T) {
		if _, err := destSpecs(cfg, roots, items, []string{"/gone/x.png"}, "Etsy/"); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestMessages(t *testing.T) {
	if got, want := previewLine("Etsy/Etsy-Mai/C-071026A26.png"), "→ Etsy/Etsy-Mai/C-071026A26.png"; got != want {
		t.Fatalf("previewLine = %q, want %q", got, want)
	}
	if got, want := overwriteMessage(2, 5, "thanhcong"), "2 trong 5 file đã có trên bucket thanhcong. Ghi đè?"; got != want {
		t.Fatalf("overwriteMessage = %q, want %q", got, want)
	}
	got := collisionMessage([]string{"a", "b", "c", "d", "e"})
	if !strings.Contains(got, "a, b, c") || strings.Contains(got, "d") || !strings.Contains(got, "5") {
		t.Fatalf("collisionMessage = %q", got)
	}
}

func TestFolderScanMessage(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"403", &s3up.Error{Status: 403, Code: "AccessDenied"}, "Khoá S3 không có quyền liệt kê bucket (403 AccessDenied) — vẫn gõ tay thư mục được"},
		{"403 wrapped", fmt.Errorf("list: %w", &s3up.Error{Status: 403, Code: "AccessDenied"}), "Khoá S3 không có quyền liệt kê bucket (403 AccessDenied) — vẫn gõ tay thư mục được"},
		{"404", &s3up.Error{Status: 404, Code: "NoSuchBucket"}, "Không tìm thấy bucket (404 NoSuchBucket) — kiểm tra Bucket và Endpoint trong Settings"},
		{"other status", &s3up.Error{Status: 500, Code: "InternalError"}, "Không liệt kê được thư mục: S3 trả lỗi 500 InternalError"},
		{"timeout", context.DeadlineExceeded, "Không liệt kê được thư mục: quá thời gian chờ"},
		{"plain", errors.New("dial tcp: no route\nto host"), "Không liệt kê được thư mục: dial tcp: no route; to host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := folderScanMessage(tc.err); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFinishScan(t *testing.T) {
	now := time.Unix(100, 0)
	var d destState
	first := d.startScan()
	second := d.startScan()
	if d.finishScan(first, []string{"old/"}, false, nil, now) {
		t.Fatal("stale scan was applied")
	}
	if !d.scanning {
		t.Fatal("stale result stopped the running scan")
	}
	if !d.finishScan(second, []string{"A/", "B/"}, true, nil, now) {
		t.Fatal("current scan dropped")
	}
	if d.scanning || !d.truncated || !d.at.Equal(now) || !slices.Equal(d.folders, []string{"A/", "B/"}) {
		t.Fatalf("state = %+v", d)
	}
	third := d.startScan()
	if len(d.folders) != 0 || d.err != "" {
		t.Fatalf("startScan kept old results: %+v", d)
	}
	d.finishScan(third, nil, false, &s3up.Error{Status: 403, Code: "AccessDenied"}, now)
	if d.err == "" || d.scanning || len(d.folders) != 0 {
		t.Fatalf("error state = %+v", d)
	}
}

type fakeHead struct {
	mu     sync.Mutex
	exists map[string]bool
	fail   string
	errs   map[string]error
	calls  []string
}

func (f *fakeHead) Head(ctx context.Context, key string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)
	if key == f.fail {
		return 0, false, errors.New("boom")
	}
	if err := f.errs[key]; err != nil {
		return 0, false, err
	}
	return 1, f.exists[key], nil
}

func TestHeadBatch(t *testing.T) {
	keys := []string{"A/1", "A/2", "A/3", "A/4"}
	denied := &s3up.Error{Status: 403, Code: "AccessDenied"}
	tests := []struct {
		name    string
		head    *fakeHead
		want    headTally
		wantErr bool
	}{
		{"all missing", &fakeHead{}, headTally{}, false},
		{"some exist", &fakeHead{exists: map[string]bool{"A/2": true, "A/4": true}}, headTally{Existing: 2}, false},
		{"some unknown", &fakeHead{errs: map[string]error{"A/1": denied, "A/3": fmt.Errorf("head: %w", denied)}}, headTally{Unknown: 2}, false},
		{"exist and unknown", &fakeHead{exists: map[string]bool{"A/2": true}, errs: map[string]error{"A/4": denied}}, headTally{Existing: 1, Unknown: 1}, false},
		{"a 500 aborts", &fakeHead{errs: map[string]error{"A/3": &s3up.Error{Status: 500}}}, headTally{}, true},
		{"a 500 beside a 403 aborts", &fakeHead{errs: map[string]error{"A/1": denied, "A/3": &s3up.Error{Status: 500}}}, headTally{}, true},
		{"a plain error aborts", &fakeHead{errs: map[string]error{"A/3": errors.New("boom")}}, headTally{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := headBatch(context.Background(), tc.head, keys, 2)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %+v err=%v, want %+v wantErr=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
	t.Run("canceled context aborts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := headBatch(ctx, &fakeHead{}, keys, 2); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestPushConfirmMessage(t *testing.T) {
	const unknownLine = "Không kiểm tra được 2 trong 5 file đã có trên bucket hay chưa (khoá S3 không có quyền, 403). Đẩy sẽ ghi đè nếu file đã tồn tại."
	tests := []struct {
		name string
		res  headTally
		want string
		ask  bool
	}{
		{"nothing to confirm", headTally{}, "", false},
		{"existing only", headTally{Existing: 2}, "2 trong 5 file đã có trên bucket thanhcong. Ghi đè?", true},
		{"unknown only", headTally{Unknown: 2}, unknownLine, true},
		{"both", headTally{Existing: 1, Unknown: 2}, "1 trong 5 file đã có trên bucket thanhcong. " + unknownLine, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.res.needsConfirm(); got != tc.ask {
				t.Fatalf("needsConfirm = %v, want %v", got, tc.ask)
			}
			if !tc.ask {
				return
			}
			if got := pushConfirmMessage(tc.res, 5, "thanhcong"); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFilterFolders(t *testing.T) {
	all := []string{"Etsy/Etsy-Mai/", "Etsy/Etsy-An/", "Archive/Maialen/", "Etsy/Etsy-Chau/", "Ảnh/Mai Linh/"}
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"empty query returns all sorted", "", []string{"Archive/Maialen/", "Etsy/Etsy-An/", "Etsy/Etsy-Chau/", "Etsy/Etsy-Mai/", "Ảnh/Mai Linh/"}},
		{"blank query returns all", "  ", []string{"Archive/Maialen/", "Etsy/Etsy-An/", "Etsy/Etsy-Chau/", "Etsy/Etsy-Mai/", "Ảnh/Mai Linh/"}},
		{"contains is case-insensitive", "CHAU", []string{"Etsy/Etsy-Chau/"}},
		{"segment start first", "mai", []string{"Archive/Maialen/", "Ảnh/Mai Linh/", "Etsy/Etsy-Mai/"}},
		{"no match", "zzz", nil},
		{"diacritics folded both ways", "anh", []string{"Ảnh/Mai Linh/"}},
		{"query with diacritics", "Ảnh", []string{"Ảnh/Mai Linh/"}},
		{"path query", "etsy/etsy-an", []string{"Etsy/Etsy-An/"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(all)
			got := filterFolders(all, tc.query)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("filterFolders(%q) = %q, want %q", tc.query, got, tc.want)
			}
			if !slices.Equal(all, before) {
				t.Fatal("input was reordered")
			}
		})
	}
}

func TestFilterFoldersRanking(t *testing.T) {
	tests := []struct {
		name    string
		folders []string
		query   string
		want    []string
	}{
		{"last segment equal beats a deeper path that ends the same", []string{"Giang.NTH/mai/", "Mai/"}, "mai", []string{"Mai/", "Giang.NTH/mai/"}},
		{"equal beats prefix beats contains", []string{"Etsy/Etsy-Mai-Old/", "Etsy/Etsy-Mai/", "Old/Mai/"}, "etsy-mai", []string{"Etsy/Etsy-Mai/", "Etsy/Etsy-Mai-Old/"}},
		{"exact last segment before longer last segment", []string{"Etsy/Etsy-Mai-Old/", "Etsy/Mai/", "Mai-Old/"}, "mai", []string{"Etsy/Mai/", "Mai-Old/", "Etsy/Etsy-Mai-Old/"}},
		{"whole path equal beats last segment equal", []string{"A/Etsy/Mai/", "Etsy/Mai/"}, "etsy/mai", []string{"Etsy/Mai/", "A/Etsy/Mai/"}},
		{"leading and trailing slashes ignored", []string{"A/Mai/", "Mai/"}, "/Mai/", []string{"Mai/", "A/Mai/"}},
		{"any segment prefix beats contains", []string{"Etsy/Etsy-Mai/", "Mai/Etsy/"}, "mai", []string{"Mai/Etsy/", "Etsy/Etsy-Mai/"}},
		{"last segment prefix beats other segment prefix", []string{"Mai/Etsy/", "Etsy/Mai-Linh/"}, "mai", []string{"Etsy/Mai-Linh/", "Mai/Etsy/"}},
		{"fewer segments first inside a class", []string{"A/B/Mai-X/", "Mai-Y/", "A/Mai-Z/"}, "mai", []string{"Mai-Y/", "A/Mai-Z/", "A/B/Mai-X/"}},
		{"alphabetical inside a class and depth", []string{"B/Mai/", "A/Mai/"}, "mai", []string{"A/Mai/", "B/Mai/"}},
		{"vietnamese folded equal first", []string{"Ảnh/Mai Linh/", "Ảnh Mới/", "Ảnh/"}, "anh", []string{"Ảnh/", "Ảnh Mới/", "Ảnh/Mai Linh/"}},
		{"vietnamese query with diacritics", []string{"Anh/Chị Mai/", "Ảnh Cũ/", "Ảnh/"}, "Ảnh", []string{"Ảnh/", "Ảnh Cũ/", "Anh/Chị Mai/"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := filterFolders(tc.folders, tc.query); !slices.Equal(got, tc.want) {
				t.Fatalf("filterFolders(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

func TestDestRows(t *testing.T) {
	def := destRow{Kind: rowDefault}
	folder := func(f string) destRow { return destRow{Kind: rowFolder, Folder: f} }
	fresh := func(f string) destRow { return destRow{Kind: rowNew, Folder: f} }
	folders := []string{"Etsy/Etsy-An/", "Etsy/Etsy-Mai/"}
	tests := []struct {
		name  string
		query string
		want  []destRow
	}{
		{"empty query: default then all", "", []destRow{def, folder("Etsy/Etsy-An/"), folder("Etsy/Etsy-Mai/")}},
		{"default stays first while filtering", "mai", []destRow{def, fresh("mai/"), folder("Etsy/Etsy-Mai/")}},
		{"existing folder typed exactly is not offered as new", "Etsy/Etsy-Mai", []destRow{def, folder("Etsy/Etsy-Mai/")}},
		{"trailing slash typed exactly", "Etsy/Etsy-Mai/", []destRow{def, folder("Etsy/Etsy-Mai/")}},
		{"valid new path", "Etsy/Etsy-Lan", []destRow{def, fresh("Etsy/Etsy-Lan/")}},
		{"invalid path gives no new row", "Etsy/../x", []destRow{def}},
		{"slashes only gives no new row", "///", []destRow{def, folder("Etsy/Etsy-An/"), folder("Etsy/Etsy-Mai/")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := destRows(folders, tc.query, true)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("destRows(%q) = %+v, want %+v", tc.query, got, tc.want)
			}
		})
	}
	t.Run("no new-folder row while scanning", func(t *testing.T) {
		got := destRows(nil, "mai", false)
		if want := []destRow{def}; !slices.Equal(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}

func TestAutoChoice(t *testing.T) {
	folders := []string{"Etsy/Etsy-An/", "Etsy/Etsy-Mai/"}
	tests := []struct {
		name, query, current, want string
	}{
		{"top match wins over the new-folder row", "mai", "", "Etsy/Etsy-Mai/"},
		{"new folder when nothing matches", "Etsy/Etsy-Lan", "", "Etsy/Etsy-Lan/"},
		{"invalid text keeps the choice", "Etsy/../x", "Etsy/Etsy-An/", "Etsy/Etsy-An/"},
		{"empty query keeps the choice", "", "Etsy/Etsy-An/", "Etsy/Etsy-An/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := autoChoice(destRows(folders, tc.query, true), tc.query, tc.current)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDestPreview(t *testing.T) {
	roots := []indexer.RootInfo{{Name: "Photos", Path: "/Volumes/Photos"}}
	cfg := s3up.Config{Prefix: "backup"}
	tests := []struct {
		name   string
		folder string
		paths  []string
		want   string
		isErr  bool
	}{
		{"folder", "Etsy/Etsy-Mai/", []string{"/Volumes/Photos/x/C-071026A26.png"}, "→ Etsy/Etsy-Mai/C-071026A26.png", false},
		{"default share layout with prefix", "", []string{"/Volumes/Photos/a/b.jpg"}, "→ backup/Photos/a/b.jpg", false},
		{"first file only", "A/", []string{"/x/1.png", "/x/2.png"}, "→ A/1.png", false},
		{"no files", "A/", nil, "", false},
		{"bad file name shows the error", "A/", []string{"/x/.."}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := destPreview(cfg, roots, tc.folder, tc.paths)
			if tc.isErr {
				if got == "" || strings.HasPrefix(got, "→") {
					t.Fatalf("want an error line, got %q", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDestCanPush(t *testing.T) {
	tests := []struct {
		name     string
		folder   string
		files    int
		checking bool
		scanning bool
		want     bool
	}{
		{"folder chosen", "Etsy/Etsy-Mai/", 3, false, false, true},
		{"default layout", "", 1, false, false, true},
		{"no files", "A/", 0, false, false, false},
		{"check running", "A/", 2, true, false, false},
		{"folder scan running", "A/", 2, false, true, false},
		{"unnormalised folder", "A", 2, false, false, false},
		{"invalid folder", "A/../B/", 2, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := destCanPush(tc.folder, tc.files, tc.checking, tc.scanning); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEnterMayPush(t *testing.T) {
	tests := []struct {
		name     string
		touched  bool
		scanning bool
		want     bool
	}{
		{"untouched popup", false, false, false},
		{"typed or clicked", true, false, true},
		{"touched but scanning", true, true, false},
		{"untouched and scanning", false, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := destState{touched: tc.touched, scanning: tc.scanning}
			if got := d.enterMayPush(); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSelectedPathsAreSorted(t *testing.T) {
	a := &app{}
	for _, p := range []string{"/v/c.png", "/v/a.png", "/v/d.png", "/v/b.png", "/v/e.png"} {
		a.chosen.Add(p)
	}
	want := []string{"/v/a.png", "/v/b.png", "/v/c.png", "/v/d.png", "/v/e.png"}
	for range 20 {
		if got := a.selectedPaths(); !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestDestTitleAlwaysNamesTheBucket(t *testing.T) {
	if got, want := destTitle(3, " thanhcong "), "Đẩy 3 file lên bucket thanhcong"; got != want {
		t.Fatalf("destTitle = %q, want %q", got, want)
	}
}

func TestPendingPushMessageUsesItsOwnConfig(t *testing.T) {
	specs := make([]uploadSpec, 5)
	p := pendingPush{cfg: s3up.Config{Bucket: "snap"}, specs: specs, checked: headTally{Existing: 2}}

	if got, want := p.message(), "2 trong 5 file đã có trên bucket snap. Ghi đè?"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}

	p.keepSame = true
	got := p.message()
	if !strings.Contains(got, "bucket snap") || !strings.Contains(got, "cùng dung lượng") {
		t.Fatalf("message = %q, want it to say that same-size files are skipped", got)
	}
}
