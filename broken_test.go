package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/brokenlist"
	"github.com/isuccesscorp/scan-image/internal/datefind"
	"github.com/isuccesscorp/scan-image/internal/search"
	"github.com/isuccesscorp/scan-image/internal/source"
)

func cand(path string, mod int64) search.Item {
	return search.Item{Path: path, Name: filepath.Base(path), Size: 2048, ModTime: time.Unix(mod, 0)}
}

func TestNewestFirstSortsByModTimeThenPathWithoutTouchingInput(t *testing.T) {
	in := []search.Item{cand("/b", 10), cand("/c", 30), cand("/a", 10)}
	got := newestFirst(in)
	var paths []string
	for _, it := range got {
		paths = append(paths, it.Path)
	}
	if want := []string{"/c", "/a", "/b"}; !slices.Equal(paths, want) {
		t.Errorf("order = %v, want %v", paths, want)
	}
	if in[0].Path != "/b" {
		t.Errorf("input was reordered: %v", in)
	}
}

func TestSourceLabel(t *testing.T) {
	tests := []struct {
		name  string
		cands []search.Item
		want  string
	}{
		{"none", nil, ""},
		{"one", []search.Item{cand("/v/a.png", 5)}, "/v/a.png"},
		{"several", []search.Item{cand("/v/new.png", 9), cand("/v/old.png", 1), cand("/w/x.png", 2)}, "/v/new.png (+2 bản khác, lấy bản mới nhất)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceLabel(tc.cands); got != tc.want {
				t.Errorf("sourceLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBrokenStatusPrecedence(t *testing.T) {
	item := brokenlist.Item{File: "a.png", Path: "/X/", Key: "X/a.png"}
	found := []search.Item{cand("/v/a.png", 1)}
	doneJob := &uploadJob{State: upDone, Size: 2048}
	tests := []struct {
		name string
		row  brokenRow
		dry  bool
		want string
	}{
		{"missing", brokenRow{Item: item}, false, "Chưa tìm thấy trên ổ mạng"},
		{"missing stays missing in dry run", brokenRow{Item: item}, true, "Chưa tìm thấy trên ổ mạng"},
		{"ready", brokenRow{Item: item, Cands: found}, false, "Sẵn sàng"},
		{"dry run", brokenRow{Item: item, Cands: found}, true, "sẽ đẩy → X/a.png · 2.0 KB"},
		{"queued beats dry run", brokenRow{Item: item, Cands: found, Job: &uploadJob{State: upQueued}}, true, "Đang chờ"},
		{"running shows percent", brokenRow{Item: item, Cands: found, Job: &uploadJob{State: upRunning, Sent: 50, Size: 200}}, false, "Đang đẩy 25%"},
		{"running without size", brokenRow{Item: item, Cands: found, Job: &uploadJob{State: upRunning}}, false, "Đang đẩy 0%"},
		{"done waits for the report", brokenRow{Item: item, Cands: found, Job: &uploadJob{State: upDone, Size: 2048}}, false, "Đã đẩy · 2.0 KB · đang báo server…"},
		{"done and reported", brokenRow{Item: item, Cands: found, Job: doneJob, Reported: doneJob}, false, "Đã đẩy · 2.0 KB · đã báo server"},
		{"done, report failed", brokenRow{Item: item, Cands: found, Job: &uploadJob{State: upDone, Size: 2048}, ReportErr: "server trả về 500"}, false, "Đã đẩy · 2.0 KB · chưa báo được server: server trả về 500"},
		{"report mark of an older job is not this job's", brokenRow{Item: item, Cands: found, Job: &uploadJob{State: upDone, Size: 2048}, Reported: doneJob}, false, "Đã đẩy · 2.0 KB · đang báo server…"},
		{"skipped is not reported", brokenRow{Item: item, Cands: found, Job: &uploadJob{State: upSkipped}}, false, "Đã có, bỏ qua"},
		{"failed", brokenRow{Item: item, Cands: found, Job: &uploadJob{State: upFailed, Err: "boom"}}, false, "Lỗi: boom"},
		{"canceled", brokenRow{Item: item, Cands: found, Job: &uploadJob{State: upCanceled}}, false, "Đã huỷ"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := brokenStatus(tc.row, tc.dry); got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEligibleRows(t *testing.T) {
	found := []search.Item{cand("/v/a.png", 1)}
	rows := []brokenRow{
		{Cands: found},
		{},
		{Cands: found, Job: &uploadJob{State: upQueued}},
		{Cands: found, Job: &uploadJob{State: upRunning}},
		{Cands: found, Job: &uploadJob{State: upDone}},
		{Cands: found, Job: &uploadJob{State: upSkipped}},
		{Cands: found, Job: &uploadJob{State: upFailed}},
		{Cands: found, Job: &uploadJob{State: upCanceled}},
	}
	if got, want := eligibleRows(rows), []int{0, 6, 7}; !slices.Equal(got, want) {
		t.Errorf("eligible = %v, want %v", got, want)
	}
}

func TestDryRunSummary(t *testing.T) {
	found := []search.Item{cand("/v/a.png", 1)}
	rows := []brokenRow{{Cands: found}, {}, {Cands: found}, {}, {}}
	if got, want := dryRunSummary(rows), "2 file sẽ đẩy, 3 chưa tìm thấy"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestRejectedFooter(t *testing.T) {
	lines := []string{"1", "2", "3", "4", "5", "6", "7"}
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"none", nil, nil},
		{"five fit", lines[:5], lines[:5]},
		{"more are counted", lines, []string{"1", "2", "3", "4", "5", "… và 2 dòng nữa"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rejectedFooter(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("footer = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", brokenlist.DefaultURL},
		{"   \t", brokenlist.DefaultURL},
		{"  https://x.test/list.txt \n", "https://x.test/list.txt"},
	}
	for _, tc := range tests {
		if got := listURL(tc.in); got != tc.want {
			t.Errorf("listURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMergeRowsKeepsJobAndMatchesByKey(t *testing.T) {
	job := &uploadJob{State: upRunning}
	found := []search.Item{cand("/v/a.png", 1)}
	old := []brokenRow{
		{Item: brokenlist.Item{Key: "X/a.png"}, Cands: found, Job: job},
		{Item: brokenlist.Item{Key: "X/gone.png"}, Cands: found},
	}
	items := []brokenlist.Item{{File: "a.png", Key: "X/a.png"}, {File: "n.png", Key: "X/n.png"}}
	got := mergeRows(old, items)
	if len(got) != 2 || got[0].Job != job || len(got[0].Cands) != 1 || got[1].Job != nil || got[1].Cands != nil {
		t.Errorf("mergeRows = %+v", got)
	}
}

func TestMergeRowsKeepsOnlyLiveJobs(t *testing.T) {
	tests := []struct {
		state uploadState
		keep  bool
	}{
		{upQueued, true}, {upRunning, true},
		{upDone, false}, {upSkipped, false}, {upFailed, false}, {upCanceled, false},
	}
	for _, tc := range tests {
		old := []brokenRow{{Item: brokenlist.Item{Key: "X/a.png"}, Job: &uploadJob{State: tc.state}}}
		got := mergeRows(old, []brokenlist.Item{{File: "a.png", Key: "X/a.png"}})
		if kept := got[0].Job != nil; kept != tc.keep {
			t.Errorf("state %d: job kept = %v, want %v", tc.state, kept, tc.keep)
		}
	}
}

func TestMergeRowsKeepsConfirmationOnlyForSameKey(t *testing.T) {
	near := []search.Item{cand("/v/Mau A.psd", 1)}
	old := []brokenRow{{Item: brokenlist.Item{Key: "X/a.psd"}, Near: near, Confirm: "/v/Mau A.psd"}}
	got := mergeRows(old, []brokenlist.Item{{File: "a.psd", Key: "X/a.psd"}, {File: "a.psd", Key: "Y/a.psd"}})
	if got[0].Confirm != "/v/Mau A.psd" || got[1].Confirm != "" {
		t.Errorf("confirm = %q, %q", got[0].Confirm, got[1].Confirm)
	}
}

func TestSplitHitsSeparatesExactFromLookAlike(t *testing.T) {
	hits := []search.Item{cand("/v/old/Mẫu A.PSD", 1), cand("/v/new/Mau A.psd", 9), cand("/v/mid/mẫu a.psd", 5)}
	tests := []struct {
		name      string
		file      string
		exact     []string
		lookAlike []string
	}{
		{"case differs only", "Mẫu A.psd", []string{"/v/mid/mẫu a.psd", "/v/old/Mẫu A.PSD"}, []string{"/v/new/Mau A.psd"}},
		{"only look-alikes", "Mãu A.psd", nil, []string{"/v/new/Mau A.psd", "/v/mid/mẫu a.psd", "/v/old/Mẫu A.PSD"}},
		{"no hits", "x.psd", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := hits
			if tc.name == "no hits" {
				in = nil
			}
			exact, near := splitHits(tc.file, newestFirst(in))
			if got := paths(exact); !slices.Equal(got, tc.exact) {
				t.Errorf("exact = %v, want %v", got, tc.exact)
			}
			if got := paths(near); !slices.Equal(got, tc.lookAlike) {
				t.Errorf("look-alike = %v, want %v", got, tc.lookAlike)
			}
		})
	}
}

func paths(items []search.Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Path)
	}
	return out
}

func TestRowSourceAndEligibility(t *testing.T) {
	exact := []search.Item{cand("/v/a.psd", 5), cand("/v/b/a.psd", 1)}
	near := []search.Item{cand("/v/Mau a.psd", 9)}
	tests := []struct {
		name        string
		row         brokenRow
		source      string
		needConfirm bool
		eligible    bool
	}{
		{"missing", brokenRow{}, "", false, false},
		{"exact beats look-alike", brokenRow{Cands: exact[:1], Near: near}, "/v/a.psd", false, true},
		{"several exact unconfirmed", brokenRow{Cands: exact, Near: near}, "/v/a.psd", true, false},
		{"several exact confirmed", brokenRow{Cands: exact, Confirm: "/v/a.psd"}, "/v/a.psd", false, true},
		{"several exact, confirmation of an older one", brokenRow{Cands: exact, Confirm: "/v/b/a.psd"}, "/v/a.psd", true, false},
		{"look-alike unconfirmed", brokenRow{Near: near}, "/v/Mau a.psd", true, false},
		{"look-alike confirmed", brokenRow{Near: near, Confirm: "/v/Mau a.psd"}, "/v/Mau a.psd", false, true},
		{"confirmation of another path", brokenRow{Near: near, Confirm: "/v/other.psd"}, "/v/Mau a.psd", true, false},
		{"confirmed but job running", brokenRow{Near: near, Confirm: "/v/Mau a.psd", Job: &uploadJob{State: upRunning}}, "/v/Mau a.psd", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src, _ := tc.row.source()
			if src.Path != tc.source {
				t.Errorf("source = %q, want %q", src.Path, tc.source)
			}
			if got := tc.row.needsConfirm(); got != tc.needConfirm {
				t.Errorf("needsConfirm = %v, want %v", got, tc.needConfirm)
			}
			if got := len(eligibleRows([]brokenRow{tc.row})) == 1; got != tc.eligible {
				t.Errorf("eligible = %v, want %v", got, tc.eligible)
			}
		})
	}
}

func TestSettleConfirmDropsStaleConfirmation(t *testing.T) {
	near := []search.Item{cand("/v/Mau a.psd", 9)}
	tests := []struct {
		name string
		row  brokenRow
		want string
	}{
		{"same source stays", brokenRow{Near: near, Confirm: "/v/Mau a.psd"}, "/v/Mau a.psd"},
		{"source changed", brokenRow{Near: []search.Item{cand("/v/Mau a2.psd", 9)}, Confirm: "/v/Mau a.psd"}, ""},
		{"exact now found", brokenRow{Cands: []search.Item{cand("/v/a.psd", 1)}, Near: near, Confirm: "/v/Mau a.psd"}, ""},
		{"nothing found", brokenRow{Confirm: "/v/Mau a.psd"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := settleConfirm(tc.row).Confirm; got != tc.want {
				t.Errorf("confirm = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLookAlikeStatusAndSourceLabel(t *testing.T) {
	r := brokenRow{Item: brokenlist.Item{Key: "X/a.psd"}, Near: []search.Item{cand("/v/Mau a.psd", 9), cand("/v/old/Mau a.psd", 1)}}
	if got, _ := brokenStatus(r, false); got != "Tên gần giống, cần xác nhận" {
		t.Errorf("status = %q", got)
	}
	if got, _ := brokenStatus(r, true); got != "Tên gần giống, cần xác nhận" {
		t.Errorf("dry status = %q", got)
	}
	if got := rowSourceLabel(r); got != "/v/Mau a.psd" {
		t.Errorf("label = %q", got)
	}
	r.Cands = []search.Item{cand("/v/a.psd", 3), cand("/w/a.psd", 2)}
	if got := rowSourceLabel(r); got != "/v/a.psd (+1 bản khác, lấy bản mới nhất)" {
		t.Errorf("exact label = %q", got)
	}
}

func TestDryRunSummaryCountsLookAlikesSeparately(t *testing.T) {
	found := []search.Item{cand("/v/a.png", 1)}
	near := []search.Item{cand("/v/A.png", 1)}
	rows := []brokenRow{{Cands: found}, {}, {Near: near}, {Near: near, Confirm: "/v/A.png"}}
	want := "2 file sẽ đẩy, 1 chưa tìm thấy, 1 file tên gần giống cần xác nhận"
	if got := dryRunSummary(rows); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestMultiCandidateRowsAreCountedAndSummarisedSeparately(t *testing.T) {
	one := []search.Item{cand("/v/a.png", 1)}
	two := []search.Item{cand("/v/new/a.png", 9), cand("/v/a.png", 1)}
	near := []search.Item{cand("/v/A.png", 1)}
	rows := []brokenRow{
		{Cands: one}, {}, {Near: near}, {Cands: two}, {Cands: two},
		{Cands: two, Confirm: "/v/new/a.png"},
		{Cands: two, Job: &uploadJob{State: upQueued}},
	}
	if got := multiCount(rows); got != 2 {
		t.Errorf("multiCount = %d, want 2", got)
	}
	if got := lookAlikeCount(rows); got != 1 {
		t.Errorf("lookAlikeCount = %d, want 1", got)
	}
	want := "2 file sẽ đẩy, 1 chưa tìm thấy, 1 file tên gần giống cần xác nhận, 3 file có nhiều bản cần xác nhận"
	if got := dryRunSummary(rows); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestMultiCandidateStatusNamesTheCopyCount(t *testing.T) {
	r := fileRow("a.png", cand("/v/new/a.png", 9), cand("/v/a.png", 1), cand("/w/a.png", 2))
	for _, dry := range []bool{false, true} {
		if got, _ := brokenStatus(r, dry); got != "Có 3 bản, cần xác nhận" {
			t.Errorf("status(dry=%v) = %q", dry, got)
		}
	}
	r.Confirm = "/v/new/a.png"
	if got, _ := brokenStatus(r, false); got != "Sẵn sàng" {
		t.Errorf("confirmed status = %q", got)
	}
}

func TestSettleConfirmForSeveralExactCandidates(t *testing.T) {
	old := cand("/v/a.png", 1)
	cur := cand("/v/b/a.png", 5)
	planted := cand("/v/planted/a.png", 99)
	tests := []struct {
		name string
		row  brokenRow
		want string
	}{
		{"same newest stays", fileRow("a.png", cur, old), "/v/b/a.png"},
		{"a newer file appeared", fileRow("a.png", planted, cur, old), ""},
		{"back to a single exact file", fileRow("a.png", cur), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.row
			r.Confirm = "/v/b/a.png"
			if got := settleConfirm(r).Confirm; got != tc.want {
				t.Errorf("confirm = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfirmSourceSetsThePathOfTheCurrentSource(t *testing.T) {
	a := &app{}
	a.broken.rows = []brokenRow{
		fileRow("a.png", cand("/v/new/a.png", 9), cand("/v/a.png", 1)),
		{Item: brokenlist.Item{File: "b.png"}, Near: []search.Item{cand("/v/B.png", 1)}},
		fileRow("c.png", cand("/v/c.png", 1)),
	}
	for i := range a.broken.rows {
		a.confirmSource(i)
	}
	a.confirmSource(-1)
	a.confirmSource(9)
	got := []string{a.broken.rows[0].Confirm, a.broken.rows[1].Confirm, a.broken.rows[2].Confirm}
	if want := []string{"/v/new/a.png", "/v/B.png", ""}; !slices.Equal(got, want) {
		t.Errorf("confirms = %q, want %q", got, want)
	}
	if len(eligibleRows(a.broken.rows)) != 3 {
		t.Errorf("eligible = %v, want all three", eligibleRows(a.broken.rows))
	}
}

func TestConfirmMessage(t *testing.T) {
	tests := []struct {
		name                string
		eligible, lookAlike int
		multi               int
		scanning            bool
		has, lacks          []string
	}{
		{"plain", 3, 0, 0, false, []string{"Ghi đè 3 file trên bucket prod"}, []string{"gần giống", "nhiều bản", "quét dở"}},
		{"look-alikes", 3, 2, 0, false, []string{"2 file tên gần giống cần xác nhận"}, []string{"quét dở", "nhiều bản"}},
		{"several copies", 3, 0, 4, false, []string{"4 file có nhiều bản cần xác nhận"}, []string{"gần giống", "quét dở"}},
		{"scanning", 3, 0, 0, true, []string{"Index đang quét dở: bản mới hơn của file có thể chưa được tìm thấy."}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := confirmMessage(tc.eligible, tc.lookAlike, tc.multi, "prod", tc.scanning)
			for _, s := range tc.has {
				if !strings.Contains(got, s) {
					t.Errorf("%q lacks %q", got, s)
				}
			}
			for _, s := range tc.lacks {
				if strings.Contains(got, s) {
					t.Errorf("%q has %q", got, s)
				}
			}
		})
	}
}

func TestShouldMatch(t *testing.T) {
	now := time.Unix(1000, 0)
	base := matchStamp{sess: 1, gen: 4, n: 100}
	tests := []struct {
		name     string
		cur      matchStamp
		scanning bool
		last     time.Time
		want     bool
	}{
		{"unchanged", base, false, now.Add(-time.Hour), false},
		{"grew while scanning, matched just now", matchStamp{1, 4, 200}, true, now.Add(-time.Second), false},
		{"grew while scanning, interval passed", matchStamp{1, 4, 200}, true, now.Add(-brokenMatchEvery), true},
		{"grew after the scan ended", matchStamp{1, 4, 200}, false, now.Add(-time.Second), true},
		{"index swapped during scan", matchStamp{1, 5, 200}, true, now.Add(-time.Second), true},
		{"session changed during scan", matchStamp{2, 4, 200}, true, now.Add(-time.Second), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldMatch(tc.cur, base, tc.scanning, tc.last, now); got != tc.want {
				t.Errorf("shouldMatch = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFindCandidatesResolvesAndSortsNewestFirst(t *testing.T) {
	ix := search.New()
	root := ix.AddRoot("/Volumes/NAS", "/Volumes/NAS")
	dirs := ix.AddChildren(root, []source.Entry{{Name: "old", IsDir: true}, {Name: "new", IsDir: true}})
	ix.AddChildren(dirs[0], []source.Entry{{Name: "A.png", ModTime: time.Unix(10, 0)}})
	ix.AddChildren(dirs[1], []source.Entry{{Name: "a.PNG", ModTime: time.Unix(20, 0)}})

	got := findCandidates(ix, []string{"a.png", "missing.png"})
	if len(got) != 2 || len(got[0]) != 2 || got[1] != nil {
		t.Fatalf("candidates = %+v", got)
	}
	if got[0][0].Path != "/Volumes/NAS/new/a.PNG" || got[0][1].Path != "/Volumes/NAS/old/A.png" {
		t.Errorf("order = %q, %q", got[0][0].Path, got[0][1].Path)
	}
}

func TestFindCandidatesLooksEachNameUpOnceAndKeepsTheNewest(t *testing.T) {
	ix := search.New()
	root := ix.AddRoot("/Volumes/NAS", "/Volumes/NAS")
	total := maxCandidates + 20
	for i := range total {
		dir := ix.AddChildren(root, []source.Entry{{Name: fmt.Sprintf("d%03d", i), IsDir: true}})
		ix.AddChildren(dir[0], []source.Entry{{Name: "a.png", ModTime: time.Unix(int64(i), 0)}})
	}

	got := findCandidates(ix, []string{"a.png", "a.png", "a.png"})

	for i, hits := range got {
		if len(hits) != maxCandidates {
			t.Fatalf("row %d has %d hits, want %d", i, len(hits), maxCandidates)
		}
	}
	if want := fmt.Sprintf("/Volumes/NAS/d%03d/a.png", total-1); got[0][0].Path != want {
		t.Errorf("newest = %q, want %q", got[0][0].Path, want)
	}
	if &got[0][0] != &got[2][0] {
		t.Error("rows sharing a name should share one resolved slice")
	}
}

func fileRow(name string, cands ...search.Item) brokenRow {
	return brokenRow{Item: brokenlist.Item{File: name, Key: "X/" + name}, Cands: cands}
}

func TestQuickNamesTakesDatedRowsWithoutSourceOnce(t *testing.T) {
	jobbed := fileRow("S-121026A02.png")
	jobbed.Job = &uploadJob{State: upQueued}
	rows := []brokenRow{
		fileRow("S-121026A09.png"),
		fileRow("S-121026A01.png", cand("/v/S-121026A01.png", 1)),
		fileRow("no-date.png"),
		fileRow("S-121026A09.png"),
		jobbed,
		fileRow("S-131026B01.png"),
		{Item: brokenlist.Item{File: "S-141026C01.png"}, Near: []search.Item{cand("/v/s-141026c01.PNG", 1)}},
	}
	want := []string{"S-121026A09.png", "S-131026B01.png", "S-141026C01.png"}
	if got := quickNames(rows); !slices.Equal(got, want) {
		t.Errorf("quickNames = %v, want %v", got, want)
	}
	if got := undatedCount(rows); got != 1 {
		t.Errorf("undatedCount = %d, want 1", got)
	}
}

func TestMergeHitsDedupesByPathIndexWinsNewestFirst(t *testing.T) {
	idx := search.Item{ID: 7, Path: "/v/a.png", Name: "a.png", ModTime: time.Unix(5, 0)}
	quickDup := search.Item{Path: "/v/a.png", Name: "a.png", ModTime: time.Unix(99, 0)}
	quickNew := cand("/w/a.png", 50)
	got := mergeHits([]search.Item{idx, cand("/x/a.png", 1)}, []search.Item{quickDup, quickNew})
	if p := paths(got); !slices.Equal(p, []string{"/w/a.png", "/v/a.png", "/x/a.png"}) {
		t.Fatalf("order = %v", p)
	}
	if got[1].ID != 7 || !got[1].ModTime.Equal(time.Unix(5, 0)) {
		t.Errorf("index entry did not win: %+v", got[1])
	}
}

func TestWithQuickClassifiesByNameRule(t *testing.T) {
	hits := map[string][]datefind.Hit{
		"S-121026A09.png": {{Path: "/Volumes/N/2026/Thang 10/12/s-121026a09.PNG", Size: 10, ModTime: time.Unix(3, 0)}},
		"S-131026B01.png": {{Path: "/Volumes/N/2026/Thang 10/13/S-131026B01 copy.png", Size: 10, ModTime: time.Unix(3, 0)}},
	}
	exact := withQuick(fileRow("S-121026A09.png"), hits)
	if src, ok := exact.source(); !ok || src.Name != "s-121026a09.PNG" || src.Dir != "/Volumes/N/2026/Thang 10/12" || exact.needsConfirm() {
		t.Errorf("exact = %+v", exact)
	}
	if len(exact.Quick) != 1 {
		t.Errorf("quick = %v", exact.Quick)
	}
	look := withQuick(fileRow("S-131026B01.png"), hits)
	if len(look.Cands) != 0 || !look.needsConfirm() {
		t.Errorf("look-alike = %+v", look)
	}
	if none := withQuick(fileRow("other.png"), hits); len(none.Quick) != 0 || len(none.Cands) != 0 {
		t.Errorf("none = %+v", none)
	}
}

func TestWithQuickKeepsIndexCandidates(t *testing.T) {
	r := fileRow("a.png", cand("/idx/a.png", 1))
	got := withQuick(r, map[string][]datefind.Hit{"a.png": {{Path: "/q/a.png", ModTime: time.Unix(9, 0)}, {Path: "/idx/a.png", ModTime: time.Unix(9, 0)}}})
	if p := paths(got.Cands); !slices.Equal(p, []string{"/q/a.png", "/idx/a.png"}) {
		t.Errorf("cands = %v", p)
	}
	if got.Cands[1].ModTime.Unix() != 1 {
		t.Errorf("index entry lost: %+v", got.Cands[1])
	}
}

func TestMergeRowsKeepsQuickHitsForSameKey(t *testing.T) {
	old := []brokenRow{{Item: brokenlist.Item{Key: "X/a.png"}, Quick: []search.Item{cand("/q/a.png", 1)}}}
	got := mergeRows(old, []brokenlist.Item{{File: "a.png", Key: "X/a.png"}, {File: "a.png", Key: "Y/a.png"}})
	if len(got[0].Quick) != 1 || got[1].Quick != nil {
		t.Errorf("quick = %v, %v", got[0].Quick, got[1].Quick)
	}
}

func TestQuickMessage(t *testing.T) {
	res := datefind.Result{Hits: map[string][]datefind.Hit{"a": {{Path: "/a"}}, "b": nil, "c": {{Path: "/c"}}}, Dirs: 42}
	tests := []struct {
		name    string
		res     datefind.Result
		asked   int
		undated int
		err     error
		want    string
	}{
		{"plain", res, 3, 0, nil, "Tìm theo ngày: thấy 2/3 file, đã liệt kê 42 thư mục"},
		{"truncated", datefind.Result{Dirs: 5000, Truncated: true}, 4, 0, nil,
			"Tìm theo ngày: thấy 0/4 file, đã liệt kê 5000 thư mục, dừng ở giới hạn 5000 thư mục"},
		{"errors", datefind.Result{Dirs: 9, Errors: 2}, 1, 0, nil,
			"Tìm theo ngày: thấy 0/1 file, đã liệt kê 9 thư mục, 2 thư mục không đọc được"},
		{"undated", res, 3, 5, nil,
			"Tìm theo ngày: thấy 2/3 file, đã liệt kê 42 thư mục. 5 tên không có ngày, chờ index quét tới"},
		{"all", datefind.Result{Dirs: 5000, Truncated: true, Errors: 1}, 2, 1, nil,
			"Tìm theo ngày: thấy 0/2 file, đã liệt kê 5000 thư mục, dừng ở giới hạn 5000 thư mục, 1 thư mục không đọc được. 1 tên không có ngày, chờ index quét tới"},
		{"timeout", res, 3, 0, context.DeadlineExceeded, "Tìm theo ngày bị dừng: quá thời gian cho phép (3 phút)"},
		{"other error", res, 3, 0, errors.New("boom"), "Tìm theo ngày bị dừng: boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := quickMessage(tc.res, tc.asked, tc.undated, tc.err); got != tc.want {
				t.Errorf("quickMessage =\n %q\nwant\n %q", got, tc.want)
			}
		})
	}
}

func TestCanQuickFind(t *testing.T) {
	dated := []brokenRow{fileRow("S-121026A09.png")}
	found := []brokenRow{fileRow("S-121026A09.png", cand("/v/S-121026A09.png", 1))}
	tests := []struct {
		name           string
		ready, running bool
		rows           []brokenRow
		want           bool
	}{
		{"ready", true, false, dated, true},
		{"not ready", false, false, dated, false},
		{"running", true, true, dated, false},
		{"no rows", true, false, nil, false},
		{"all found", true, false, found, false},
		{"no dates", true, false, []brokenRow{fileRow("x.png")}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := canQuickFind(tc.ready, tc.running, tc.rows); got != tc.want {
				t.Errorf("canQuickFind = %v, want %v", got, tc.want)
			}
		})
	}
}

func jobRow(key string, state uploadState) brokenRow {
	return brokenRow{Item: brokenlist.Item{File: key, Path: "/X/", Key: "X/" + key}, Job: &uploadJob{State: state}}
}

func TestRowsToReport(t *testing.T) {
	reported := jobRow("reported", upDone)
	reported.Reported = reported.Job
	oldMark := jobRow("rerun", upDone)
	oldMark.Reported = &uploadJob{State: upDone}
	failedBefore := jobRow("retry", upDone)
	failedBefore.ReportErr = "boom"
	rows := []brokenRow{
		jobRow("a", upDone),
		jobRow("queued", upQueued),
		jobRow("running", upRunning),
		jobRow("skipped", upSkipped),
		jobRow("failed", upFailed),
		jobRow("canceled", upCanceled),
		{Item: brokenlist.Item{Key: "X/nojob"}},
		reported,
		oldMark,
		failedBefore,
		jobRow("b", upDone),
	}
	tests := []struct {
		name  string
		limit int
		want  []int
	}{
		{"only unreported done jobs, in row order", 100, []int{0, 8, 9, 10}},
		{"capped, earliest first", 2, []int{0, 8}},
		{"no room", 0, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowsToReport(rows, tc.limit); !slices.Equal(got, tc.want) {
				t.Errorf("rowsToReport = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRowsToReportCapsAtServerLimit(t *testing.T) {
	var rows []brokenRow
	for i := 0; i < brokenlist.MaxReportFiles+30; i++ {
		rows = append(rows, jobRow(strings.Repeat("x", i+1), upDone))
	}
	if got := rowsToReport(rows, brokenlist.MaxReportFiles); len(got) != brokenlist.MaxReportFiles || got[0] != 0 || got[len(got)-1] != brokenlist.MaxReportFiles-1 {
		t.Errorf("batch of %d, want the first %d rows", len(got), brokenlist.MaxReportFiles)
	}
}

func TestReportBatchSendsFileAndPathNotKey(t *testing.T) {
	rows := []brokenRow{jobRow("a.png", upDone), jobRow("b.png", upDone)}

	items, refs := reportBatch(rows, []int{1, 0})

	if len(items) != 2 || items[0].File != "b.png" || items[0].Path != "/X/" || items[1].File != "a.png" {
		t.Errorf("items = %+v", items)
	}
	if len(refs) != 2 || refs[0].key != "X/b.png" || refs[0].job != rows[1].Job {
		t.Errorf("refs = %+v", refs)
	}
}

func TestReportGate(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	tests := []struct {
		name     string
		inflight bool
		ok, fail time.Time
		start    bool
		wait     time.Duration
	}{
		{"never reported", false, time.Time{}, time.Time{}, true, 0},
		{"in flight", true, time.Time{}, time.Time{}, false, 0},
		{"success 400ms ago", false, ago(400 * time.Millisecond), time.Time{}, false, 600 * time.Millisecond},
		{"success 1s ago", false, ago(time.Second), time.Time{}, true, 0},
		{"failure 2s ago", false, time.Time{}, ago(2 * time.Second), false, 3 * time.Second},
		{"failure 5s ago", false, time.Time{}, ago(5 * time.Second), true, 0},
		{"failure 1s ago, success long ago", false, ago(time.Minute), ago(time.Second), false, 4 * time.Second},
		{"both recent: the longer wait", false, ago(100 * time.Millisecond), ago(time.Second), false, 4 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, wait := reportGate(tc.inflight, tc.ok, tc.fail, now)
			if start != tc.start || wait != tc.wait {
				t.Errorf("reportGate = %v, %v; want %v, %v", start, wait, tc.start, tc.wait)
			}
		})
	}
}

func TestMarkReport(t *testing.T) {
	a, b, c := jobRow("a", upDone), jobRow("b", upDone), jobRow("c", upDone)
	rows := []brokenRow{a, b, c}
	sent := []reportRef{{a.Item.Key, a.Job}, {b.Item.Key, b.Job}}

	t.Run("success marks the sent rows only", func(t *testing.T) {
		got := markReport(rows, sent, "")

		if got[0].Reported != a.Job || got[1].Reported != b.Job || got[2].Reported != nil {
			t.Errorf("reported = %v %v %v", got[0].Reported, got[1].Reported, got[2].Reported)
		}
		if rows[0].Reported != nil {
			t.Error("input rows were modified")
		}
	})
	t.Run("failure keeps them unreported with the error", func(t *testing.T) {
		got := markReport(rows, sent, "server trả về 500")

		if got[0].Reported != nil || got[0].ReportErr != "server trả về 500" || got[1].ReportErr == "" || got[2].ReportErr != "" {
			t.Errorf("rows = %+v", got)
		}
	})
	t.Run("success clears an earlier error", func(t *testing.T) {
		failed := []brokenRow{a}
		failed[0].ReportErr = "boom"

		got := markReport(failed, sent[:1], "")

		if got[0].ReportErr != "" || got[0].Reported != a.Job {
			t.Errorf("row = %+v", got[0])
		}
	})
	t.Run("a row now holding another job is left alone", func(t *testing.T) {
		replaced := []brokenRow{a}
		replaced[0].Job = &uploadJob{State: upDone}

		got := markReport(replaced, sent[:1], "")

		if got[0].Reported != nil {
			t.Error("marked a job that was not reported")
		}
	})
	t.Run("a row that left the list is ignored", func(t *testing.T) {
		got := markReport([]brokenRow{c}, sent, "")

		if len(got) != 1 || got[0].Reported != nil {
			t.Errorf("rows = %+v", got)
		}
	})
}

func TestMergeRowsDropsReportMarkWithItsJob(t *testing.T) {
	done := jobRow("a", upDone)
	done.Reported = done.Job
	failed := jobRow("b", upDone)
	failed.ReportErr = "boom"

	got := mergeRows([]brokenRow{done, failed}, []brokenlist.Item{done.Item, failed.Item})

	for i, r := range got {
		if r.Job != nil || r.Reported != nil || r.ReportErr != "" {
			t.Errorf("row %d kept report state: %+v", i, r)
		}
	}
}

func TestDropBrokenMatchesClearsReportErrorsKeepsReportedMarks(t *testing.T) {
	done := jobRow("a", upDone)
	done.Reported = done.Job
	failed := jobRow("b", upDone)
	failed.ReportErr = "boom"
	a := &app{}
	a.broken.rows = []brokenRow{done, failed}
	a.broken.report.inflight = true

	a.dropBrokenMatches()

	r := a.broken.rows
	if r[0].Reported != done.Job || r[1].ReportErr != "" || a.broken.report.inflight {
		t.Errorf("rows %+v, inflight %v", r, a.broken.report.inflight)
	}
}

func TestShortReportError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"drops package prefix", errors.New("brokenlist: server trả về 500 Internal Server Error: boom"), "server trả về 500 Internal Server Error: boom"},
		{"one line", errors.New("a\nb"), "a; b"},
		{"capped", errors.New(strings.Repeat("é", 500)), strings.Repeat("é", reportErrRunes) + "…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortReportError(tc.err); got != tc.want {
				t.Errorf("shortReportError = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReportBrokenWithUnusableListURLMarksRowsAndSaysSoOnce(t *testing.T) {
	a := &app{}
	a.up.cfg.ListURL = "http://example.com/list.txt"
	a.broken.rows = []brokenRow{jobRow("a", upDone)}

	a.reportBroken()
	first := a.broken.msg
	a.broken.msg = ""
	a.reportBroken()

	if a.broken.report.inflight {
		t.Fatal("started a report without a usable URL")
	}
	if first == "" || a.broken.msg != "" {
		t.Errorf("message first %q then %q, want it once", first, a.broken.msg)
	}
	if got, _ := brokenStatus(a.broken.rows[0], false); !strings.Contains(got, "chưa báo được server") {
		t.Errorf("status %q", got)
	}
}
