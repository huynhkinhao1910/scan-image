package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/s3up"
	"github.com/isuccesscorp/scan-image/internal/search"
)

func doneJob(key string, size int64, done time.Time) *uploadJob {
	return &uploadJob{Path: "/v/" + key, Key: key, State: upDone, Size: size, Sent: size, Done: done}
}

func TestRecheckDue(t *testing.T) {
	now := time.Date(2026, 10, 13, 16, 0, 0, 0, time.Local)
	first, second := recheckDelays[0], recheckDelays[1]
	cases := []struct {
		name string
		job  *uploadJob
		want bool
	}{
		{"just pushed", doneJob("a", 5, now.Add(-first+time.Second)), false},
		{"first check due", doneJob("a", 5, now.Add(-first)), true},
		{"second check not yet", &uploadJob{State: upDone, Size: 5, Done: now.Add(-second + time.Second), Checks: 1}, false},
		{"second check due", &uploadJob{State: upDone, Size: 5, Done: now.Add(-second), Checks: 1}, true},
		{"all checks done", &uploadJob{State: upDone, Size: 5, Done: now.Add(-24 * time.Hour), Checks: len(recheckDelays)}, false},
		{"being checked", &uploadJob{State: upDone, Size: 5, Done: now.Add(-first), checking: true}, false},
		{"failed", &uploadJob{State: upFailed, Done: now.Add(-first)}, false},
		{"skipped", &uploadJob{State: upSkipped, Done: now.Add(-first)}, false},
		{"queued", &uploadJob{State: upQueued}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recheckDue(tc.job, now); got != tc.want {
				t.Fatalf("recheckDue = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSettleRecheck(t *testing.T) {
	done := time.Date(2026, 10, 13, 15, 59, 20, 0, time.Local)

	t.Run("same size counts one check", func(t *testing.T) {
		j := doneJob("a", 5, done)
		j.checking = true
		if again := settleRecheck(j, recheckResult{job: j, size: 5, found: true}); again {
			t.Fatal("asked to push an intact object again")
		}
		if j.State != upDone || j.Checks != 1 || j.checking {
			t.Fatalf("state %v checks %d checking %v", j.State, j.Checks, j.checking)
		}
	})

	t.Run("emptied object is pushed again", func(t *testing.T) {
		j := doneJob("a", 5, done)
		j.Checks, j.checking = 1, true
		if again := settleRecheck(j, recheckResult{job: j, size: 0, found: true}); !again {
			t.Fatal("an emptied object was left alone")
		}
		if j.State != upQueued || j.Repush != 1 || j.Checks != 0 || j.Sent != 0 || !j.Done.IsZero() || j.checking {
			t.Fatalf("%+v", j)
		}
	})

	t.Run("missing object is pushed again", func(t *testing.T) {
		j := doneJob("a", 5, done)
		if again := settleRecheck(j, recheckResult{job: j}); !again || j.State != upQueued {
			t.Fatalf("again %v state %v", again, j.State)
		}
	})

	t.Run("gives up after maxRepush", func(t *testing.T) {
		j := doneJob("a", 5, done)
		j.Repush = maxRepush
		if again := settleRecheck(j, recheckResult{job: j, size: 0, found: true}); again {
			t.Fatal("pushed again past the limit")
		}
		if j.State != upFailed || !strings.Contains(j.Err, "ghi đè") || !strings.Contains(j.Err, "0 B") {
			t.Fatalf("state %v err %q", j.State, j.Err)
		}
	})

	t.Run("a failed check changes nothing but frees the job", func(t *testing.T) {
		j := doneJob("a", 5, done)
		j.checking = true
		if again := settleRecheck(j, recheckResult{job: j, err: errors.New("timeout")}); again {
			t.Fatal("pushed again on a failed check")
		}
		if j.State != upDone || j.Checks != 0 || j.Repush != 0 || j.checking {
			t.Fatalf("%+v", j)
		}
	})

	t.Run("a job that moved on is left alone", func(t *testing.T) {
		j := &uploadJob{State: upRunning, Size: 5}
		if again := settleRecheck(j, recheckResult{job: j, size: 0, found: true}); again || j.State != upRunning {
			t.Fatalf("again %v state %v", again, j.State)
		}
	})
}

func TestHeadJobsReadsWhatIsOnTheBucket(t *testing.T) {
	sizes := map[string]string{"/bucket/Etsy/A/ok.png": "5", "/bucket/Etsy/A/empty.png": "0"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("recheck sent %s %s", r.Method, r.URL.Path)
		}
		n, ok := sizes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", n)
		w.Header().Set("ETag", `"tag-`+n+`"`)
	}))
	defer srv.Close()
	cfg := s3up.Config{Endpoint: srv.URL, Bucket: "bucket", AccessKey: "AK", SecretKey: "SK", PathStyle: true}
	jobs := []*uploadJob{
		{Key: "Etsy/A/ok.png", Cfg: cfg},
		{Key: "Etsy/A/empty.png", Cfg: cfg},
		{Key: "Etsy/A/gone.png", Cfg: cfg},
	}

	got := headJobs(context.Background(), jobs)

	if len(got) != 3 {
		t.Fatalf("results %d", len(got))
	}
	if r := got[0]; r.job != jobs[0] || r.err != nil || !r.found || r.size != 5 || r.etag != `"tag-5"` {
		t.Errorf("ok: %+v", r)
	}
	if r := got[1]; r.err != nil || !r.found || r.size != 0 {
		t.Errorf("empty: %+v", r)
	}
	if r := got[2]; r.err != nil || r.found {
		t.Errorf("gone: %+v", r)
	}
}

func TestApplyRechecksQueuesTheEmptiedJobAgain(t *testing.T) {
	a := testUploader()
	ok := doneJob("Etsy/A/ok.png", 5, time.Now().Add(-2*time.Minute))
	lost := doneJob("Etsy/A/lost.png", 5, time.Now().Add(-2*time.Minute))
	a.up.jobs = []*uploadJob{ok, lost}

	a.applyRechecks([]recheckResult{{job: ok, size: 5, found: true}, {job: lost, size: 0, found: true}})

	if ok.State != upDone || ok.Checks != 1 {
		t.Errorf("intact job: state %v checks %d", ok.State, ok.Checks)
	}
	select {
	case j := <-a.up.queue:
		if j != lost || j.State != upQueued || j.Repush != 1 {
			t.Fatalf("queued %+v", j)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the emptied job was not queued again")
	}
}

func TestJobLabelTellsAboutTheRecheck(t *testing.T) {
	cases := []struct {
		job  *uploadJob
		want string
	}{
		{&uploadJob{State: upDone, Size: 5 << 20}, "Xong · 5.0 MB · chờ kiểm lại"},
		{&uploadJob{State: upDone, Size: 5 << 20, Checks: len(recheckDelays)}, "Xong · 5.0 MB · đã kiểm lại"},
		{&uploadJob{State: upDone, Size: 5 << 20, Checks: len(recheckDelays), Repush: 2}, "Xong · 5.0 MB · đã kiểm lại · đã đẩy lại 2 lần"},
		{&uploadJob{State: upQueued, Repush: 1}, "Đang chờ · đẩy lại vì S3 bị ghi đè"},
		{&uploadJob{State: upQueued}, "Đang chờ"},
	}
	for _, tc := range cases {
		if got := jobLabel(tc.job); got != tc.want {
			t.Errorf("jobLabel = %q, want %q", got, tc.want)
		}
	}
}

func TestClearFinishedKeepsJobsStillToBeChecked(t *testing.T) {
	a := testUploader()
	waiting := doneJob("a", 5, time.Now())
	checked := doneJob("b", 5, time.Now())
	checked.Checks = len(recheckDelays)
	failed := &uploadJob{State: upFailed}
	a.up.jobs = []*uploadJob{waiting, checked, failed}

	a.clearFinished()

	if len(a.up.jobs) != 1 || a.up.jobs[0] != waiting {
		t.Fatalf("kept %d jobs", len(a.up.jobs))
	}
}

func TestEmptySourceIsNeverPushed(t *testing.T) {
	row := listed("a.png", "Etsy/A/")
	row.Cands = []search.Item{{Path: "/v/a.png", Name: "a.png", Size: 0}}
	full := listed("b.png", "Etsy/B/")
	full.Cands = []search.Item{{Path: "/v/b.png", Name: "b.png", Size: 9}}
	rows := []brokenRow{row, full}

	if got := eligibleRows(rows); len(got) != 1 || got[0] != 1 {
		t.Fatalf("eligible %v, want only the non-empty source", got)
	}
	if text, _ := brokenStatus(row, false); !strings.Contains(text, "0 byte") {
		t.Fatalf("status %q", text)
	}
	if text, _ := brokenStatus(full, false); text != "Sẵn sàng" {
		t.Fatalf("status %q", text)
	}
}

func TestSettleRecheckComparesTheETag(t *testing.T) {
	done := time.Date(2026, 10, 14, 9, 0, 0, 0, time.Local)

	t.Run("same size with other content is pushed again", func(t *testing.T) {
		j := doneJob("a", 5, done)
		j.ETag = `"aa"`
		if again := settleRecheck(j, recheckResult{job: j, size: 5, etag: `"bb"`, found: true}); !again {
			t.Fatal("an object replaced by one of the same size was left alone")
		}
		if j.State != upQueued || j.Repush != 1 {
			t.Fatalf("state %v repush %d", j.State, j.Repush)
		}
	})

	t.Run("same ETag counts one check", func(t *testing.T) {
		j := doneJob("a", 5, done)
		j.ETag = `"aa"`
		if again := settleRecheck(j, recheckResult{job: j, size: 5, etag: `"aa"`, found: true}); again || j.Checks != 1 {
			t.Fatalf("again %v checks %d", again, j.Checks)
		}
	})
}

func TestFinishJobKeepsTheETag(t *testing.T) {
	j := &uploadJob{}

	finishJob(j, s3up.Result{Bytes: 5, ETag: `"aa"`}, nil, time.Now())

	if j.State != upDone || j.ETag != `"aa"` {
		t.Fatalf("state %v etag %q", j.State, j.ETag)
	}
}

func TestWatchLastsADay(t *testing.T) {
	if last := recheckDelays[len(recheckDelays)-1]; last < 24*time.Hour {
		t.Fatalf("last look after %v, want a day", last)
	}
}

func TestEnqueueEndsTheWatchOfAnOlderPushOfTheKey(t *testing.T) {
	a := testUploader()
	old := doneJob("X/a.png", 5, time.Now())
	other := doneJob("X/b.png", 5, time.Now())
	a.up.jobs = []*uploadJob{old, other}

	a.enqueueUploads(s3up.Config{}, []uploadSpec{{Path: "/v/new/a.png", Key: "X/a.png"}})

	if recheckPending(old) {
		t.Error("the older push is still watched: it would be sent again over the new one")
	}
	if !recheckPending(other) {
		t.Error("a push of another key lost its watch")
	}
}

func TestNewUploadSupersedesEveryOlderJobOfTheKey(t *testing.T) {
	a := testUploader()
	checking := doneJob("X/a.png", 5, time.Now())
	checking.checking = true
	resend := &uploadJob{Path: "/v/old/a.png", Key: "X/a.png", State: upQueued, Repush: 1}
	running := &uploadJob{Path: "/v/older/a.png", Key: "X/a.png", State: upRunning, Repush: 1}
	a.up.jobs = []*uploadJob{checking, resend, running}

	a.enqueueUploads(s3up.Config{}, []uploadSpec{{Path: "/v/new/a.png", Key: "X/a.png"}})

	if again := settleRecheck(checking, recheckResult{job: checking, size: 0, found: true}); again || checking.State != upDone {
		t.Errorf("a look under way sent the superseded job again: again %v state %v", again, checking.State)
	}
	if resend.State != upCanceled {
		t.Errorf("a superseded job waiting to be sent again: state %v, want cancelled", resend.State)
	}
	finishJob(running, s3up.Result{Bytes: 5}, nil, time.Now())
	if recheckPending(running) || watched(running) {
		t.Error("a superseded job that finished later is watched")
	}
	if got := jobLabel(running); !strings.Contains(got, "mới hơn") {
		t.Errorf("label %q does not tell about the newer push", got)
	}
}

func TestFailedLookIsShownUntilOneSucceeds(t *testing.T) {
	j := doneJob("a", 5, time.Now())

	settleRecheck(j, recheckResult{job: j, err: errors.New("403 Forbidden")})

	if !strings.Contains(j.Err, "403 Forbidden") || !strings.Contains(jobLabel(j), "chưa kiểm lại được") {
		t.Fatalf("err %q label %q", j.Err, jobLabel(j))
	}
	settleRecheck(j, recheckResult{job: j, size: 5, found: true})
	if j.Err != "" || strings.Contains(jobLabel(j), "chưa kiểm lại được") {
		t.Fatalf("after a good look: err %q label %q", j.Err, jobLabel(j))
	}
}
