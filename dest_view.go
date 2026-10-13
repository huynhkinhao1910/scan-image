package main

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
)

const destListHeight = 240

// destPopup is the S3 destination dialog opened by "Upload S3". It also hosts
// the overwrite confirmation as a second step, so only one dialog is up.
func (a *app) destPopup(c *ui.Context) {
	d := &a.dest
	if !d.open {
		return
	}
	if d.gen != a.sessGen {
		d.close()
		return
	}
	ui.Modal(c, &d.open, func() { a.destBody(c) })
	if !d.open {
		d.close()
	}
}

func (a *app) destBody(c *ui.Context) {
	d := &a.dest
	paths := a.selectedPaths()
	bucket := a.up.cfg.Bucket
	if d.pending != nil {
		bucket = d.pending.cfg.Bucket
	}
	ui.Column(c).Width(560).Gap(10).Children(func() {
		ui.Text(c, destTitle(len(paths), bucket)).Bold()
		if d.pending != nil {
			a.overwriteStep(c)
			return
		}
		a.destPick(c, paths)
	})
}

func (a *app) destPick(c *ui.Context, paths []string) {
	d := &a.dest
	search := ui.SearchField(c, &d.query).Label("Thư mục S3 — gõ để lọc, ví dụ \"mai\"").AutoFocus()
	changed, submitted := search.Changed(), search.Submitted()
	d.touched = d.touched || changed
	rows := destRows(d.folders, d.query, !d.scanning)
	if changed {
		d.choice = autoChoice(rows, d.query, d.choice)
	}
	d.sel = destRowIndex(rows, d.choice)
	d.list.Selected = &d.sel
	list := ui.List(c, &d.list, len(rows), func(i int) {
		ui.Text(c, rows[i].label()).SingleLine().Padding(6, 12)
	}).Height(destListHeight)
	if list.Changed() && d.sel >= 0 && d.sel < len(rows) {
		d.choice, d.touched = rows[d.sel].Folder, true
	}
	preview := destPreview(a.up.cfg, a.st.Roots, d.choice, paths)
	previewColor := colMuted
	if preview != "" && !strings.HasPrefix(preview, "→") {
		previewColor = colOrange
	}
	ui.Text(c, preview).FontSize(12).TextColor(previewColor).Wrap()
	a.destScanRow(c)
	if d.msg != "" {
		ui.Text(c, d.msg).FontSize(12).TextColor(colOrange).Wrap()
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Justify(ui.End).Children(func() {
		if d.checking {
			ui.Spinner(c)
		}
		if ui.Button(c, "Huỷ").Clicked() {
			d.close()
		}
		can := destCanPush(d.choice, len(paths), d.checking, d.scanning)
		if ui.PrimaryButton(c, "Đẩy").Disabled(!can).Clicked() || (submitted || list.Submitted()) && d.enterMayPush() {
			a.pushDest()
		}
	})
}

func (a *app) destScanRow(c *ui.Context) {
	d := &a.dest
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		if d.scanning {
			ui.Spinner(c)
		}
		color := colMuted
		if d.err != "" {
			color = colOrange
		}
		ui.Text(c, destScanLine(d)).FontSize(12).TextColor(color).Wrap().Grow(1)
		if ui.Button(c, "Làm mới").Disabled(d.scanning).Tooltip("Quét lại các thư mục có trên bucket").Clicked() {
			a.scanFolders()
		}
	})
}

// destScanLine is the state of the bucket's folder listing.
func destScanLine(d *destState) string {
	switch {
	case d.scanning:
		return "Đang quét thư mục, chờ xong rồi chọn"
	case d.err != "":
		return d.err
	case d.at.IsZero():
		return ""
	case d.truncated:
		return fmt.Sprintf("%d thư mục (danh sách bị cắt ở giới hạn, gõ tay nếu thiếu)", len(d.folders))
	}
	return fmt.Sprintf("%d thư mục", len(d.folders))
}

func confirmLabel(r headTally) string {
	if r.Unknown > 0 {
		return "Vẫn đẩy"
	}
	return "Ghi đè"
}

func (a *app) overwriteStep(c *ui.Context) {
	d := &a.dest
	if d.pending == nil {
		return
	}
	p := *d.pending
	ui.Text(c, p.message()).Wrap()
	ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
		if ui.Button(c, "Quay lại").Clicked() {
			d.pending = nil
		}
		if ui.PrimaryButton(c, confirmLabel(p.checked)).Clicked() {
			a.acceptOverwrite()
		}
	})
}
