package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/s3up"
	"github.com/isuccesscorp/scan-image/internal/search"
)

// changedSource writes a file last modified at mod and returns a job that was
// matched on an older state of it.
func changedSource(t *testing.T, mod time.Time) *uploadJob {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(p, []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return &uploadJob{Path: p, Key: "X/a.png", Expect: &sourceStamp{Size: 3, ModTime: mod.Add(-time.Hour)}}
}

func TestCheckSource(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 30, 0, 0, time.Local)

	t.Run("a changed source fails the first time", func(t *testing.T) {
		j := changedSource(t, now.Add(-time.Hour))
		if err := checkSource(j, now); !errors.Is(err, errSourceChanged) {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("sent again, it is taken as it is now", func(t *testing.T) {
		j := changedSource(t, now.Add(-sourceSettle))
		j.Fresh = true
		if err := checkSource(j, now); err != nil {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("but not while it is being written", func(t *testing.T) {
		j := changedSource(t, now.Add(-sourceSettle+time.Second))
		j.Fresh = true
		err := checkSource(j, now)
		if err == nil || errors.Is(err, errSourceChanged) || !strings.Contains(err.Error(), "vừa được sửa") {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("and never a link", func(t *testing.T) {
		j := changedSource(t, now.Add(-time.Hour))
		link := filepath.Join(t.TempDir(), "link.png")
		if err := os.Symlink(j.Path, link); err != nil {
			t.Fatal(err)
		}
		j.Path, j.Fresh = link, true
		if err := checkSource(j, now); !errors.Is(err, errSourceChanged) {
			t.Fatalf("err %v", err)
		}
	})

	t.Run("an unchanged source passes", func(t *testing.T) {
		j := changedSource(t, now)
		j.Expect = &sourceStamp{Size: int64(len("new content")), ModTime: now}
		if err := checkSource(j, now); err != nil {
			t.Fatalf("err %v", err)
		}
	})
}

func TestRetryAfterSourceChangedTakesTheFileAsItIsNow(t *testing.T) {
	a := testUploader()
	changed := &uploadJob{Path: "/v/a.png", Key: "a"}
	finishJob(changed, s3up.Result{}, errSourceChanged, time.Now())
	other := &uploadJob{Path: "/v/b.png", Key: "b"}
	finishJob(other, s3up.Result{}, errors.New("timeout"), time.Now())
	a.up.jobs = []*uploadJob{changed, other}

	a.retryFailed()

	if changed.State != upQueued || !changed.Fresh {
		t.Errorf("changed source: state %v fresh %v", changed.State, changed.Fresh)
	}
	if other.State != upQueued || other.Fresh {
		t.Errorf("other failure: state %v fresh %v", other.State, other.Fresh)
	}
}

func TestPushAgainAfterSourceChangedIsFresh(t *testing.T) {
	a := testUploader()
	same := &uploadJob{Path: "/v/a.png", Key: "X/a.png"}
	finishJob(same, s3up.Result{}, errSourceChanged, time.Now())
	moved := &uploadJob{Path: "/v/old/b.png", Key: "X/b.png"}
	finishJob(moved, s3up.Result{}, errSourceChanged, time.Now())
	a.up.jobs = []*uploadJob{same, moved}

	jobs := a.enqueueUploads(s3up.Config{}, []uploadSpec{
		{Path: "/v/a.png", Key: "X/a.png"},
		{Path: "/v/new/b.png", Key: "X/b.png"},
		{Path: "/v/c.png", Key: "X/c.png"},
	})

	if !jobs[0].Fresh {
		t.Error("the same source pushed again after it changed is not taken as it is now")
	}
	if jobs[1].Fresh || jobs[2].Fresh {
		t.Errorf("another source, or a first push, skips the stamp: %v %v", jobs[1].Fresh, jobs[2].Fresh)
	}
}

func TestSourceChangedErrorTellsHowToSendAgain(t *testing.T) {
	if !strings.Contains(errSourceChanged.Error(), "Thử lại lỗi") {
		t.Fatalf("%q", errSourceChanged)
	}
}

func TestListedRowsArePushedAsTheSourceIsNow(t *testing.T) {
	a := testUploader()
	row := listed("a.png", "Etsy/A/")
	row.Cands = []search.Item{{Path: "/v/a.png", Name: "a.png", Size: 9}}
	a.broken.rows = []brokenRow{row}

	jobs := a.pushRows(s3up.Config{}, []int{0})

	if len(jobs) != 1 || jobs[0] == nil || !jobs[0].Fresh {
		t.Fatalf("a file the server lists as missing must go up as it is now, edited or not: %+v", jobs)
	}
}
