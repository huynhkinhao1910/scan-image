package main

import (
	"context"
	"strings"
	"time"

	"github.com/isuccesscorp/scan-image/internal/brokenlist"
)

const (
	reportTimeout  = 30 * time.Second
	reportErrRunes = 120
)

// reportState paces the calls that tell the server which files are back on
// S3. One call at a time, within the server's rate limit.
type reportState struct {
	inflight    bool
	wakePending bool
	lastOK      time.Time
	lastFail    time.Time
	badURL      string
}

// shortReportError is err as one short line for the status column.
func shortReportError(err error) string {
	s := strings.TrimPrefix(oneLine(err), "brokenlist: ")
	if r := []rune(s); len(r) > reportErrRunes {
		return string(r[:reportErrRunes]) + "…"
	}
	return s
}

// reportBroken sends the next batch of finished uploads to the server when
// the pacing allows, and otherwise arranges to come back when it does. Every
// result and every finished upload calls it, so the last batch is not left
// behind. UI thread only.
func (a *app) reportBroken() {
	b := &a.broken
	r := &b.report
	idx := rowsToReport(b.rows, brokenlist.MaxReportFiles)
	if len(idx) == 0 {
		return
	}
	start, wait := reportGate(r.inflight, r.lastOK, r.lastFail, time.Now())
	if !start {
		if wait > 0 {
			a.reportWake(wait)
		}
		return
	}
	raw := listURL(a.up.cfg.ListURL)
	url, err := brokenlist.ReportURL(raw)
	items, refs := reportBatch(b.rows, idx)
	if err != nil {
		if r.badURL != raw {
			r.badURL, b.msg = raw, oneLine(err)
		}
		b.rows = markReport(b.rows, refs, shortReportError(err))
		r.lastFail = time.Now()
		a.reportWake(reportAfterFail)
		return
	}
	r.inflight = true
	sess := a.sessGen
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		defer cancel()
		err := brokenlist.Report(ctx, nil, url, items)
		a.update(func() { a.applyReport(sess, refs, err) })
	}()
}

func (a *app) applyReport(sess int, refs []reportRef, err error) {
	if a.sessGen != sess {
		return
	}
	b := &a.broken
	b.report.inflight = false
	if err != nil {
		b.report.lastFail = time.Now()
		b.rows = markReport(b.rows, refs, shortReportError(err))
	} else {
		b.report.lastOK = time.Now()
		b.rows = markReport(b.rows, refs, "")
	}
	a.reportBroken()
}

func (a *app) reportWake(d time.Duration) {
	r := &a.broken.report
	if r.wakePending {
		return
	}
	r.wakePending = true
	time.AfterFunc(d, func() {
		a.update(func() {
			r.wakePending = false
			a.reportBroken()
		})
	})
}
