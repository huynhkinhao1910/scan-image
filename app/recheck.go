package main

import (
	"context"
	"fmt"
	"time"

	"github.com/isuccesscorp/scan-image/internal/s3up"
)

// recheckDelays is how long after a push each look at the bucket happens.
// Another writer was seen replacing a pushed object with an empty one 16
// seconds after the push.
// ponytail: the watch ends after a day; a later overwrite is not seen.
var recheckDelays = []time.Duration{time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}

const (
	// maxRepush is how often a job is sent again before it is given up as
	// failed: past that, the other writer keeps winning.
	maxRepush      = 3
	recheckTick    = 20 * time.Second
	recheckBatch   = 50
	recheckTimeout = 15 * time.Second
)

type recheckState struct {
	wakePending bool
	// saved is the watch file as last written or read.
	saved []byte
}

// recheckResult is what the bucket holds for a job's key.
type recheckResult struct {
	job   *uploadJob
	size  int64
	etag  string
	found bool
	err   error
}

func recheckPending(j *uploadJob) bool {
	return j.State == upDone && !j.superseded && j.Checks < len(recheckDelays)
}

func recheckDue(j *uploadJob, now time.Time) bool {
	return recheckPending(j) && !j.checking && now.Sub(j.Done) >= recheckDelays[j.Checks]
}

// headJobs asks the bucket for the size and ETag of each job's object, with
// the config the job was pushed with. Off the UI thread: it reads only Key and
// Cfg, which do not change after a job is queued.
func headJobs(ctx context.Context, jobs []*uploadJob) []recheckResult {
	out := make([]recheckResult, len(jobs))
	for i, j := range jobs {
		hctx, cancel := context.WithTimeout(ctx, recheckTimeout)
		obj, found, err := s3up.New(j.Cfg).Stat(hctx, j.Key)
		cancel()
		out[i] = recheckResult{job: j, size: obj.Size, etag: obj.ETag, found: found, err: err}
	}
	return out
}

func onBucket(j *uploadJob, r recheckResult) string {
	switch {
	case !r.found:
		return "không còn file"
	case r.size != j.Size:
		return "chỉ còn " + humanBytes(r.size)
	}
	return "là nội dung khác"
}

// intact reports whether the bucket still holds what the job pushed. A job
// pushed without an ETag is judged by size alone.
func intact(j *uploadJob, r recheckResult) bool {
	return r.found && r.size == j.Size && (j.ETag == "" || r.etag == j.ETag)
}

// settleRecheck applies one result to its job and reports whether the job has
// to be sent again. A check that failed leaves the job to the next round and
// shows why.
func settleRecheck(j *uploadJob, r recheckResult) (again bool) {
	j.checking = false
	switch {
	case !recheckPending(j):
		return false
	case r.err != nil:
		j.Err = "Chưa kiểm lại được trên S3: " + oneLine(r.err)
		return false
	case intact(j, r):
		j.Checks++
		j.Err = ""
		return false
	case j.Repush >= maxRepush:
		j.State = upFailed
		j.Err = fmt.Sprintf("Trên S3 %s sau %d lần đẩy lại: có tiến trình khác ghi đè key này", onBucket(j, r), j.Repush)
		return false
	}
	j.Repush++
	j.State, j.Err, j.Checks, j.Sent, j.Done = upQueued, "", 0, 0, time.Time{}
	return true
}

// recheck looks again at the objects whose time has come, and keeps coming
// back while any job still has a look ahead. UI thread only.
func (a *app) recheck() {
	now := time.Now()
	var due []*uploadJob
	pending := false
	for _, j := range a.up.jobs {
		pending = pending || recheckPending(j)
		if len(due) < recheckBatch && recheckDue(j, now) {
			j.checking = true
			due = append(due, j)
		}
	}
	if pending {
		a.recheckWake()
	}
	if len(due) == 0 {
		return
	}
	go func() {
		results := headJobs(context.Background(), due)
		a.update(func() { a.applyRechecks(results) })
	}()
}

// applyRechecks settles the results and queues again the jobs whose object
// was replaced. UI thread only.
func (a *app) applyRechecks(results []recheckResult) {
	for _, r := range results {
		if settleRecheck(r.job, r) {
			go func() { a.up.queue <- r.job }()
		}
	}
	a.saveWatch()
}

func (a *app) recheckWake() {
	r := &a.up.recheck
	if r.wakePending {
		return
	}
	r.wakePending = true
	time.AfterFunc(recheckTick, func() {
		a.update(func() {
			r.wakePending = false
			a.recheck()
		})
	})
}
