package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/internal/brokenlist"
	"github.com/isuccesscorp/scan-image/internal/s3up"
	"github.com/isuccesscorp/scan-image/internal/search"
)

func browseApp(tab int) *app {
	a := &app{screen: screenBrowse, selected: -1, idxSize: -1, source: "test", tab: tab}
	a.broken.selected = -1
	return a
}

func hover(t *testing.T, tt *ui.Tester, text string) {
	t.Helper()
	box, ok := tt.Find(text)
	if !ok {
		t.Fatalf("%q not shown, texts %q", text, tt.Texts())
	}
	tt.Move(box.X+box.W/2, box.Y+box.H/2)
}

func TestBrokenRowCopiesFileNameOnHover(t *testing.T) {
	a := browseApp(tabBroken)
	a.broken.rows = []brokenRow{
		{Item: brokenlist.Item{File: "U-121026A26.png", Path: "/Etsy/Etsy-An/", Key: "Etsy/Etsy-An/U-121026A26.png"}},
	}
	tt := ui.NewTester(a.view, 1400, 800)

	hover(t, tt, "U-121026A26.png")
	if err := tt.Click(copyLabel); err != nil {
		t.Fatalf("copy button: %v", err)
	}

	if got := tt.Clipboard(); got != "U-121026A26.png" {
		t.Fatalf("clipboard %q", got)
	}
}

func TestQueueTabListsJobsNewestFirstWithPushTime(t *testing.T) {
	at := time.Date(2026, 10, 13, 15, 4, 5, 0, time.Local)
	a := browseApp(tabQueue)
	a.up.jobs = []*uploadJob{
		{Path: "/Volumes/X/old.png", Key: "Etsy/Etsy-An/old.png", State: upDone, Size: 2048, Sent: 2048, Done: at.Add(-time.Hour)},
		{Path: "/Volumes/X/new.png", Key: "Etsy/Etsy-An/new.png", State: upDone, Size: 1024, Sent: 1024, Done: at},
		{Path: "/Volumes/X/wait.png", Key: "Etsy/Etsy-An/wait.png", State: upQueued},
	}
	tt := ui.NewTester(a.view, 1600, 800)

	for _, want := range []string{"Đẩy lúc", "Etsy/Etsy-An/new.png", "/Volumes/X/new.png", "13/10 15:04:05", "13/10 14:04:05", "Đang chờ"} {
		if !tt.HasText(want) {
			t.Errorf("%q not shown, texts %q", want, tt.Texts())
		}
	}
	newer, _ := tt.Find("new.png")
	older, _ := tt.Find("old.png")
	waiting, _ := tt.Find("wait.png")
	if !(waiting.Y < newer.Y && newer.Y < older.Y) {
		t.Errorf("rows not newest first: wait %v new %v old %v", waiting.Y, newer.Y, older.Y)
	}
}

func TestQueueTabCopiesFileNameOnHover(t *testing.T) {
	a := browseApp(tabQueue)
	a.up.jobs = []*uploadJob{{Path: "/Volumes/X/a.png", Key: "Etsy/Etsy-An/a.png", State: upQueued}}
	tt := ui.NewTester(a.view, 1600, 800)

	hover(t, tt, "a.png")
	if err := tt.Click(copyLabel); err != nil {
		t.Fatalf("copy button: %v", err)
	}

	if got := tt.Clipboard(); got != "a.png" {
		t.Fatalf("clipboard %q", got)
	}
}

func TestQueueIsATabNotAPanel(t *testing.T) {
	a := browseApp(tabBroken)
	a.up.jobs = []*uploadJob{{Path: "/Volumes/X/a.png", Key: "Etsy/Etsy-An/a.png", State: upQueued}}
	tt := ui.NewTester(a.view, 1400, 800)

	if tt.HasText("/Volumes/X/a.png") {
		t.Fatalf("queue rows show outside their tab: %q", tt.Texts())
	}
	if err := tt.Click(queueTabLabel(a.up.jobs)); err != nil {
		t.Fatalf("queue tab: %v, texts %q", err, tt.Texts())
	}
	if a.tab != tabQueue || !tt.HasText("/Volumes/X/a.png") {
		t.Fatalf("tab %d, texts %q", a.tab, tt.Texts())
	}
}

func TestQueueTabLabelCountsActiveJobs(t *testing.T) {
	jobs := []*uploadJob{{State: upQueued}, {State: upRunning}, {State: upDone}, {State: upFailed}}
	if got := queueTabLabel(jobs); got != "Hàng đợi S3 (2)" {
		t.Errorf("label %q", got)
	}
	if got := queueTabLabel(jobs[2:]); got != "Hàng đợi S3" {
		t.Errorf("idle label %q", got)
	}
}

func TestFinishJobStampsTheTime(t *testing.T) {
	now := time.Date(2026, 10, 13, 15, 4, 5, 0, time.Local)
	cases := []struct {
		name      string
		res       s3up.Result
		err       error
		wantState uploadState
		wantSent  int64
	}{
		{"done", s3up.Result{Bytes: 10}, nil, upDone, 10},
		{"skipped", s3up.Result{Bytes: 10, Skipped: true}, nil, upSkipped, 10},
		{"failed", s3up.Result{}, errors.New("boom"), upFailed, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := &uploadJob{State: upRunning, Sent: 3}
			finishJob(j, tc.res, tc.err, now)
			if j.State != tc.wantState || j.Sent != tc.wantSent || !j.Done.Equal(now) {
				t.Fatalf("state %v sent %d done %v", j.State, j.Sent, j.Done)
			}
			if tc.err != nil && j.Err != "boom" {
				t.Fatalf("err %q", j.Err)
			}
		})
	}
}

func TestJobTime(t *testing.T) {
	at := time.Date(2026, 10, 13, 15, 4, 5, 0, time.Local)
	if got := jobTime(&uploadJob{State: upDone, Done: at}); got != "13/10 15:04:05" {
		t.Errorf("done: %q", got)
	}
	if got := jobTime(&uploadJob{State: upQueued}); got != "" {
		t.Errorf("queued: %q", got)
	}
	if got := jobTime(nil); got != "" {
		t.Errorf("nil: %q", got)
	}
}

func TestBrokenTableShowsPushTime(t *testing.T) {
	at := time.Date(2026, 10, 13, 15, 4, 5, 0, time.Local)
	a := browseApp(tabBroken)
	j := &uploadJob{Key: "Etsy/Etsy-An/a.png", State: upDone, Size: 10, Done: at}
	a.broken.rows = []brokenRow{{Item: brokenlist.Item{File: "a.png", Path: "/Etsy/Etsy-An/", Key: "Etsy/Etsy-An/a.png"}, Job: j, Reported: j}}
	tt := ui.NewTester(a.view, 1600, 800)

	if !tt.HasText("Đẩy lúc") || !tt.HasText("13/10 15:04:05") {
		t.Fatalf("texts %q", tt.Texts())
	}
}

func listed(file, dir string) brokenRow {
	return brokenRow{Item: brokenlist.Item{File: file, Path: "/" + dir, Key: dir + file}}
}

func TestAdoptJobs(t *testing.T) {
	failed := &uploadJob{Key: "Etsy/B/b.png", State: upFailed}
	rows := []brokenRow{listed("a.png", "Etsy/A/"), {Item: listed("b.png", "Etsy/B/").Item, Job: failed}, listed("c.png", "Etsy/C/")}
	a := &uploadJob{Path: "/v/a.png", Key: "Etsy/A/a.png"}
	b := &uploadJob{Path: "/v/b.png", Key: "Etsy/B/b.png"}
	other := &uploadJob{Path: "/v/x.png", Key: "Etsy/Other/x.png"}

	got, adopted := adoptJobs(rows, []*uploadJob{nil, a, b, other})

	if len(adopted) != 2 || got[0].Job != a || got[1].Job != b || got[2].Job != nil {
		t.Fatalf("adopted %d, jobs %v %v %v", len(adopted), got[0].Job, got[1].Job, got[2].Job)
	}
	if rows[0].Job != nil || rows[1].Job != failed {
		t.Fatal("adoptJobs changed the rows it was given")
	}
}

func TestUploadFromSearchReportsListedKeys(t *testing.T) {
	a := testUploader()
	a.broken.rows = []brokenRow{listed("J-131026A06.png", "Etsy/Etsy-An/")}
	stamp := &sourceStamp{Size: 11, ModTime: time.Unix(1_800_000_000, 0)}
	specs := []uploadSpec{
		{Path: "/v/J-131026A06.png", Key: "Etsy/Etsy-An/J-131026A06.png", Expect: stamp},
		{Path: "/v/other.png", Key: "Etsy/Etsy-An/other.png", Expect: stamp},
	}

	a.enqueueDest(pendingPush{specs: specs})

	row := a.broken.rows[0]
	if row.Job == nil || row.Job.Path != "/v/J-131026A06.png" {
		t.Fatalf("listed row has no job: %+v", row)
	}
	if want := "1 file có trong danh sách file hỏng"; !strings.Contains(a.status, want) {
		t.Errorf("status %q lacks %q", a.status, want)
	}
	if len(rowsToReport(a.broken.rows, 10)) != 0 {
		t.Fatal("reported before the upload finished")
	}
	finishJob(row.Job, s3up.Result{Bytes: 11}, nil, time.Now())
	if got := rowsToReport(a.broken.rows, 10); len(got) != 1 {
		t.Fatalf("finished upload not reported: %v", got)
	}
	refetched := mergeRows(withSource(a.broken.rows, "/v/J-131026A06.png", stamp), []brokenlist.Item{row.Item})
	if idx, repeated := autoRows(refetched, a.broken.auto.pushed); len(idx) != 0 || repeated != 1 {
		t.Errorf("auto would push the same source again: idx %v repeated %d", idx, repeated)
	}
}

func TestUploadFromSearchWithoutListedKeySaysNothingAboutServer(t *testing.T) {
	a := testUploader()
	a.broken.rows = []brokenRow{listed("a.png", "Etsy/A/")}

	a.enqueueDest(pendingPush{specs: []uploadSpec{{Path: "/v/z.png", Key: "Etsy/Z/z.png"}}})

	if a.broken.rows[0].Job != nil || strings.Contains(a.status, "danh sách file hỏng") {
		t.Fatalf("row %+v, status %q", a.broken.rows[0], a.status)
	}
}

func withSource(rows []brokenRow, path string, st *sourceStamp) []brokenRow {
	out := make([]brokenRow, len(rows))
	for i, r := range rows {
		r.Cands = []search.Item{{Path: path, Size: st.Size, ModTime: st.ModTime}}
		out[i] = r
	}
	return out
}
