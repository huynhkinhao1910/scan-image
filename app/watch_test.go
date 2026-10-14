package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/s3up"
)

func TestWatchSurvivesARestart(t *testing.T) {
	cfg := s3up.Config{Bucket: "prod", AccessKey: "AK", SecretKey: "s3cr3t"}
	done := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	waiting := doneJob("Etsy/A/a.png", 5, done)
	waiting.Cfg, waiting.ETag, waiting.Checks, waiting.Repush = cfg, `"aa"`, 2, 1
	waiting.Opt = s3up.UploadOptions{Overwrite: true, Verify: true}
	waiting.Expect = &sourceStamp{Size: 5, ModTime: done}
	resend := &uploadJob{Path: "/v/b.png", Key: "Etsy/A/b.png", Cfg: cfg, State: upRunning, Size: 7, Sent: 3, Repush: 2}
	checked := doneJob("c", 5, done)
	checked.Checks = len(recheckDelays)
	jobs := []*uploadJob{waiting, resend, checked, {Key: "d", State: upQueued}, {Key: "e", State: upFailed, Err: "x"}}

	raw, err := encodeWatch(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cr3t") {
		t.Fatal("the secret key was written to the watch file")
	}
	got, err := decodeWatch(raw, func(accessKey string) string { return "secret of " + accessKey })
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("restored %d jobs, want the two still watched", len(got))
	}
	a := got[0]
	if a.Path != waiting.Path || a.Key != waiting.Key || a.ETag != `"aa"` || a.Size != 5 || a.Checks != 2 || a.Repush != 1 ||
		a.State != upDone || !a.Done.Equal(done) || a.Opt != waiting.Opt || a.Expect == nil || !a.Expect.ModTime.Equal(done) {
		t.Errorf("watched job: %+v", a)
	}
	if a.Cfg.Bucket != "prod" || a.Cfg.SecretKey != "secret of AK" {
		t.Errorf("config: %+v", a.Cfg)
	}
	b := got[1]
	if b.Key != resend.Key || b.State != upDone || b.Repush != 2 || !recheckDue(b, time.Now()) {
		t.Errorf("a job cut off while being sent again must be looked at first: %+v", b)
	}
}

func TestDecodeWatchRejectsGarbage(t *testing.T) {
	if _, err := decodeWatch([]byte("{"), func(string) string { return "" }); err == nil {
		t.Fatal("no error for a broken watch file")
	}
}

func TestSaveAndRestoreWatchAcrossRuns(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "ScanImage", "s3.json")
	cfg := s3up.Config{Bucket: "prod", AccessKey: "AK", SecretKey: "SK"}
	first := testUploader()
	first.up.cfgPath, first.up.cfg = cfgPath, cfg
	j := doneJob("Etsy/A/a.png", 5, time.Now())
	j.Cfg, j.ETag = cfg, `"aa"`
	first.up.jobs = []*uploadJob{j}
	first.saveWatch()
	if first.status != "" {
		t.Fatal(first.status)
	}
	second := testUploader()
	second.up.cfgPath, second.up.cfg = cfgPath, cfg

	second.restoreWatch()

	if second.status != "" || len(second.up.jobs) != 1 {
		t.Fatalf("status %q, %d jobs", second.status, len(second.up.jobs))
	}
	if got := second.up.jobs[0]; got.Key != j.Key || got.ETag != `"aa"` || got.Cfg.SecretKey != "SK" || !recheckPending(got) {
		t.Errorf("restored %+v", got)
	}
	if !second.up.recheck.wakePending {
		t.Error("the watch was not taken up after the restart")
	}
}

func TestRestoreWatchWithoutAFile(t *testing.T) {
	a := testUploader()
	a.up.cfgPath = filepath.Join(t.TempDir(), "s3.json")

	a.restoreWatch()

	if a.status != "" || len(a.up.jobs) != 0 {
		t.Fatalf("status %q, %d jobs", a.status, len(a.up.jobs))
	}
}

func TestDecodeWatchClampsCounters(t *testing.T) {
	raw := []byte(`[{"Path":"/v/a.png","Key":"a","State":2,"Size":5,"Checks":-3,"Repush":-1},{"Path":"/v/b.png","Key":"b","State":2,"Size":5,"Checks":99}]`)

	got, err := decodeWatch(raw, func(string) string { return "" })

	if err != nil || len(got) != 2 {
		t.Fatalf("%d jobs, err %v", len(got), err)
	}
	if got[0].Checks != 0 || got[0].Repush != 0 || got[1].Checks != len(recheckDelays) {
		t.Fatalf("checks %d repush %d, checks %d", got[0].Checks, got[0].Repush, got[1].Checks)
	}
	recheckDue(got[0], time.Now())
}

func TestCancelAndRetryRewriteTheWatchFile(t *testing.T) {
	a := testUploader()
	a.up.cfgPath = filepath.Join(t.TempDir(), "s3.json")
	resend := &uploadJob{Path: "/v/a.png", Key: "a", State: upQueued, Repush: 1}
	lost := &uploadJob{Path: "/v/b.png", Key: "b", State: upFailed, Repush: maxRepush}
	a.up.jobs = []*uploadJob{resend, lost}
	a.saveWatch()
	read := func() string {
		b, err := os.ReadFile(a.up.watchPath())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	a.cancelQueued()
	if got := read(); strings.Contains(got, `"Key":"a"`) {
		t.Errorf("a cancelled job is still in the watch file: %s", got)
	}
	a.retryFailed()
	if got := read(); !strings.Contains(got, `"Key":"b"`) {
		t.Errorf("a job sent again by hand is not in the watch file: %s", got)
	}
}

func TestUnreadableWatchFileIsSetAside(t *testing.T) {
	a := testUploader()
	a.up.cfgPath = filepath.Join(t.TempDir(), "s3.json")
	if err := os.WriteFile(a.up.watchPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	a.restoreWatch()
	a.saveWatch()

	if a.status == "" {
		t.Error("no message about the unreadable watch file")
	}
	if b, err := os.ReadFile(a.up.watchPath() + ".bad"); err != nil || string(b) != "{" {
		t.Errorf("the unreadable file was not kept: %q, %v", b, err)
	}
}
