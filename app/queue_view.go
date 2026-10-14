package main

import (
	"fmt"
	"path"

	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/icons"
)

const (
	copyLabel = "Copy tên file"
	timeShort = "02/01 15:04:05"
)

func queueTabLabel(jobs []*uploadJob) string {
	active := 0
	for _, j := range jobs {
		if j.State == upQueued || j.State == upRunning {
			active++
		}
	}
	if active == 0 {
		return "Hàng đợi S3"
	}
	return fmt.Sprintf("Hàng đợi S3 (%d)", active)
}

// jobTime is when the job ended, or "" while it has not.
func jobTime(j *uploadJob) string {
	if j == nil || j.Done.IsZero() {
		return ""
	}
	return j.Done.Format(timeShort)
}

// nameCell shows a file name with a copy button that appears while the
// pointer is over the cell. The button keeps its room when hidden, so rows do
// not jump.
func nameCell(c *ui.Context, name string) {
	cell := ui.Row(c).Gap(6).AlignItems(ui.Center)
	hovered := cell.Hovered()
	cell.Children(func() {
		ui.Text(c, name).SingleLine().Grow(1)
		btn := ui.Button(c, "").Label(copyLabel).Tooltip(copyLabel).Padding(2, 4).Children(func() {
			ui.Icon(c, icons.Copy).FontSize(12)
		})
		if !hovered {
			btn.Invisible()
		}
		if btn.Clicked() {
			c.WriteClipboard(name)
			c.Toast("Đã copy " + name)
		}
	})
}

func (a *app) queueTab(c *ui.Context) {
	ui.Column(c).Grow(1).Gap(10).Children(func() {
		a.queueToolbar(c)
		if len(a.up.jobs) == 0 {
			ui.Column(c).Gap(10).Padding(14, 16).Radius(12).Background(colCard).Border(1, colLine).Children(func() {
				ui.Text(c, "Chưa có file nào được đẩy lên S3 từ lúc mở app.").FontSize(12).TextColor(colMuted).Wrap()
			})
			return
		}
		a.queueTable(c)
	})
}

func (a *app) queueToolbar(c *ui.Context) {
	active, done, failed := a.up.counts()
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, icons.CloudUpload).TextColor(colBlue)
		ui.Text(c, fmt.Sprintf("%d đang chạy · %d xong · %d lỗi", active, done, failed)).SingleLine().Grow(1)
		if active > 0 {
			ui.Spinner(c)
			if iconButton(c, icons.Close, "Huỷ hàng đợi").Clicked() {
				a.cancelQueued()
			}
		}
		if failed > 0 && iconButton(c, icons.Retry, "Thử lại lỗi").Clicked() {
			a.retryFailed()
		}
		if iconButton(c, icons.Trash, "Xoá đã xong").Disabled(len(a.up.jobs) == 0).Clicked() {
			a.clearFinished()
		}
	})
}

func (a *app) queueTable(c *ui.Context) {
	cols := []ui.TableColumn{
		numberColumn,
		{Title: "File", MinWidth: 180},
		{Title: "Path trên S3", MinWidth: 220},
		{Title: "Nguồn trên ổ mạng", MinWidth: 240},
		{Title: "Trạng thái", Width: 320},
		{Title: "Đẩy lúc", Width: 130},
	}
	jobs := a.up.jobs
	newest := func(row int) *uploadJob { return jobs[len(jobs)-1-row] }
	a.up.table.Key = func(i int) any { return newest(i) }
	ui.Table(c, &a.up.table, cols, len(jobs), func(row, col int) {
		j := newest(row)
		switch col {
		case 0:
			rowNumber(c, row, false)
		case 1:
			nameCell(c, path.Base(j.Key))
		case 2:
			ui.Text(c, j.Key).SingleLine().TextColor(colMuted)
		case 3:
			ui.Text(c, j.Path).SingleLine().TextColor(colMuted)
		case 4:
			a.jobStatusCell(c, j)
		case 5:
			ui.Text(c, jobTime(j)).SingleLine().TextColor(colMuted)
		}
	}).Grow(1)
}

func (a *app) jobStatusCell(c *ui.Context, j *uploadJob) {
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, jobIcon(j.State)).FontSize(13).TextColor(jobColor(j.State))
		ui.Text(c, jobLabel(j)).SingleLine().TextColor(jobColor(j.State)).Grow(1).Tooltip(j.Err)
		if j.State != upDone && j.State != upSkipped {
			return
		}
		open := ui.Button(c, "").Label("Mở trên web").Tooltip("Mở file này trên web").Padding(2, 4).Children(func() {
			ui.Icon(c, icons.External).FontSize(12)
		})
		if open.Clicked() {
			a.openWeb(j)
		}
	})
}
