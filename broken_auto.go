package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/internal/s3up"
	"github.com/isuccesscorp/scan-image/internal/search"
)

const (
	// ponytail: fixed pace; make it a setting when someone needs another one.
	autoEvery      = 5 * time.Minute
	autoStartDelay = 30 * time.Second
)

type autoPhase int

const (
	autoIdle autoPhase = iota
	autoFetching
	autoMatching
	autoFinding
)

// pushedSource is the source a key was last pushed from, and the job that
// pushed it.
type pushedSource struct {
	sig string
	job *uploadJob
}

// autoState drives the unattended cycle of the broken-file tab: fetch the
// list, match it, find the missing rows by date, push, report. pushed keeps a
// row from being sent again and again while the server still lists it.
type autoState struct {
	on     bool
	ask    bool
	phase  autoPhase
	waking bool
	pushed map[string]pushedSource
}

func stampSig(path string, size int64, mod time.Time) string {
	return fmt.Sprintf("%s|%d|%d", path, size, mod.Unix())
}

func sourceSig(it search.Item) string { return stampSig(it.Path, it.Size, it.ModTime) }

// autoRows lists the rows an unattended push may send: the eligible ones,
// minus those already pushed from the very same source. repeated counts the
// latter.
func autoRows(rows []brokenRow, pushed map[string]pushedSource) (idx []int, repeated int) {
	for _, i := range eligibleRows(rows) {
		src, _ := rows[i].source()
		p, ok := pushed[rows[i].Item.Key]
		if ok && p.sig == sourceSig(src) && p.job.State != upFailed && p.job.State != upCanceled {
			repeated++
			continue
		}
		idx = append(idx, i)
	}
	return idx, repeated
}

// rowsBusy reports uploads still queued or running, or finished ones the
// server has not been told about. A new list would drop them.
func rowsBusy(rows []brokenRow) bool {
	for _, r := range rows {
		j := r.Job
		if j == nil {
			continue
		}
		if j.State == upQueued || j.State == upRunning || (j.State == upDone && r.Reported != j) {
			return true
		}
	}
	return false
}

func autoPrefix(at time.Time) string { return "Tự động " + at.Format("15:04") + ": " }

func autoMessage(at time.Time, queued, repeated int, rows []brokenRow) string {
	var parts []string
	if queued > 0 {
		parts = append(parts, fmt.Sprintf("đẩy %d file", queued))
	}
	if n := missingCount(rows); n > 0 {
		parts = append(parts, fmt.Sprintf("%d chưa tìm thấy", n))
	}
	if n := lookAlikeCount(rows) + multiCount(rows); n > 0 {
		parts = append(parts, fmt.Sprintf("%d cần xác nhận", n))
	}
	if repeated > 0 {
		parts = append(parts, fmt.Sprintf("bỏ qua %d file đã đẩy mà server vẫn liệt kê", repeated))
	}
	if len(parts) == 0 {
		return autoPrefix(at) + "không có file nào cần đẩy"
	}
	return autoPrefix(at) + strings.Join(parts, ", ")
}

func autoConfirmText(bucket string) string {
	return fmt.Sprintf("Mỗi %d phút app sẽ tự lấy danh sách, tìm file trên ổ mạng và GHI ĐÈ lên bucket %s mà không hỏi lại. "+
		"Chỉ đẩy dòng có đúng một file trùng tên chính xác; dòng tên gần giống hoặc có nhiều bản vẫn chờ bạn xác nhận.",
		int(autoEvery.Minutes()), strings.TrimSpace(bucket))
}

// startAuto resumes the unattended cycle saved in the settings. UI thread only.
func (a *app) startAuto() {
	if a.up.cfg.AutoPush {
		a.broken.auto.on = true
		a.autoWake(autoStartDelay)
	}
}

// setAuto turns the unattended cycle on or off and saves the choice. The
// secret is not written again. UI thread only.
func (a *app) setAuto(on bool) {
	b := &a.broken
	b.auto.on, b.auto.ask = on, false
	a.up.cfg.AutoPush, a.up.form.AutoPush = on, on
	if a.up.cfgPath != "" {
		if err := s3up.SaveConfig(a.up.cfgPath, a.up.cfg, nil); err != nil {
			b.msg = "Không lưu được lựa chọn tự động: " + oneLine(err)
		}
	}
	if on {
		a.autoTick()
	}
}

func (a *app) autoWake(d time.Duration) {
	s := &a.broken.auto
	if s.waking {
		return
	}
	s.waking = true
	time.AfterFunc(d, func() {
		a.update(func() {
			s.waking = false
			a.autoTick()
		})
	})
}

// autoTick starts a cycle unless one is under way or uploads of the last one
// are not finished and reported. UI thread only.
func (a *app) autoTick() {
	b := &a.broken
	s := &b.auto
	if !s.on {
		return
	}
	a.autoWake(autoEvery)
	if s.phase != autoIdle || b.fetching || b.matching || b.quicking || rowsBusy(b.rows) {
		return
	}
	s.phase = autoFetching
	a.fetchBroken()
}

// autoAfterFetch moves the cycle on when the new list is being matched, and
// ends it otherwise: the fetch failed, or there is nothing to match.
func (a *app) autoAfterFetch(matching bool) {
	s := &a.broken.auto
	if s.phase != autoFetching {
		return
	}
	s.phase = autoIdle
	if matching {
		s.phase = autoMatching
	}
}

func (a *app) autoAfterMatch() {
	b := &a.broken
	if b.auto.phase != autoMatching {
		return
	}
	a.quickFind()
	if b.quicking {
		b.auto.phase = autoFinding
		return
	}
	a.autoPush()
}

func (a *app) autoAfterQuick() {
	if a.broken.auto.phase == autoFinding {
		a.autoPush()
	}
}

// autoPush ends a cycle by queueing what may be sent without a person looking.
// It waits for a complete index: a partial one can hide a second copy of a
// file.
func (a *app) autoPush() {
	b := &a.broken
	b.auto.phase = autoIdle
	now, cfg := time.Now(), a.up.cfg
	switch {
	case !b.auto.on:
	case cfg.Validate() != nil:
		b.msg = autoPrefix(now) + "chưa cấu hình S3, không đẩy"
	case !a.st.Complete:
		b.msg = autoPrefix(now) + "index chưa quét xong lần đầu, chưa đẩy"
	default:
		idx, repeated := autoRows(b.rows, b.auto.pushed)
		queued := 0
		for _, j := range a.pushRows(cfg, idx) {
			if j != nil {
				queued++
			}
		}
		b.msg = autoMessage(now, queued, repeated, b.rows)
	}
}

func (a *app) autoToggle(c *ui.Context) {
	b := &a.broken
	on := b.auto.on
	label := fmt.Sprintf("Tự động (%d phút)", int(autoEvery.Minutes()))
	if !ui.Checkbox(c, &on, label).Tooltip("Tự lấy danh sách, tìm file và đẩy lên S3 theo chu kỳ").Changed() {
		return
	}
	if !on {
		a.setAuto(false)
		return
	}
	if _, problem := a.requireS3(); problem != "" {
		b.msg = problem
		return
	}
	b.auto.ask = true
}

func (a *app) autoConfirm(c *ui.Context) {
	b := &a.broken
	ui.Modal(c, &b.auto.ask, func() {
		ui.Column(c).Width(480).Gap(10).Children(func() {
			ui.Text(c, "Bật tự động đẩy file hỏng").Bold()
			ui.Text(c, autoConfirmText(a.up.cfg.Bucket)).Wrap()
			ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
				if ui.Button(c, "Huỷ").Clicked() {
					b.auto.ask = false
				}
				if ui.PrimaryButton(c, "Bật").Clicked() {
					a.setAuto(true)
				}
			})
		})
	})
}
