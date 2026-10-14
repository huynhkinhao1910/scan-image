package main

import (
	"fmt"
	"strconv"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/icons"
	"github.com/isuccesscorp/scan-image/internal/brokenlist"
	"github.com/isuccesscorp/scan-image/internal/indexer"
	"github.com/isuccesscorp/scan-image/internal/search"
)

var (
	colBg     = ui.Hex("#0f1012")
	colCard   = ui.Hex("#18191c")
	colCardHi = ui.Hex("#1f2024")
	colLine   = ui.Hex("#26282d")
	colMuted  = ui.Hex("#8a8f98")
	colBlue   = ui.Hex("#4c8dff")
	colGreen  = ui.Hex("#3ecf8e")
	colOrange = ui.Hex("#ff9f43")
	colPink   = ui.Hex("#ff5c7a")
	colSelect = ui.Hex("#2d5bb9")
)

func (a *app) view(c *ui.Context) {
	t := *ui.DarkTheme()
	t.Background, t.Surface, t.Border = colBg, colCardHi, colLine
	t.Accent, t.Radius = colBlue, 8
	c.SetTheme(&t)

	if a.screen == screenConnect {
		a.connectView(c)
		return
	}
	if a.tab == tabSearch && c.Shortcut(0, ui.KeyEscape) && (a.chosen.Len() > 0 || a.selected >= 0) {
		a.clearSelection()
	}
	ui.Column(c).Fill().Background(colBg).Padding(20).Gap(16).Children(func() {
		a.header(c)
		a.problemList(c)
		if a.settingsOpen {
			a.settingsPanel(c)
		}
		ui.Tabs(c, &a.tab, "Tìm ảnh", brokenTabLabel(a.broken.rows), queueTabLabel(a.up.jobs))
		switch a.tab {
		case tabBroken:
			a.brokenTab(c)
		case tabQueue:
			a.queueTab(c)
		default:
			a.cards(c)
			a.searchBar(c)
			a.results(c)
			a.destPopup(c)
		}
	})
}

const (
	tabSearch = iota
	tabBroken
	tabQueue
)

func (a *app) header(c *ui.Context) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Grow(1).Gap(2).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, icons.Search).FontSize(22).TextColor(colBlue)
				ui.Text(c, "Scan Image").FontSize(22).Bold()
			})
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, icons.Server).FontSize(12).TextColor(colMuted)
				ui.Text(c, fmt.Sprintf("%s · %d thư mục gốc", a.source, len(a.st.Roots))).FontSize(12).TextColor(colMuted).SingleLine()
			})
		})
		if a.st.Scanning {
			ui.Spinner(c)
			if iconButton(c, icons.Stop, "Dừng").Clicked() {
				a.stopScan()
			}
		} else if iconButton(c, icons.Refresh, "Quét lại").Disabled(a.svc == nil).Clicked() {
			a.rescan(indexer.Incremental)
		}
		chevron := icons.ChevronDown
		if a.settingsOpen {
			chevron = icons.ChevronUp
		}
		if ui.Button(c, "").Label("Settings").Children(func() {
			ui.Icon(c, icons.Settings)
			ui.Text(c, "Settings").SingleLine()
			ui.Icon(c, chevron).TextColor(colMuted)
		}).Clicked() {
			a.toggleSettings()
		}
	})
}

// problemList shows one warning line per problem; nothing when all is well.
func (a *app) problemList(c *ui.Context) {
	if len(a.problemRows) == 0 {
		return
	}
	ui.Column(c).Gap(2).Children(func() {
		for _, p := range a.problemRows {
			ui.Text(c, p).FontSize(12).TextColor(colOrange).Wrap()
		}
	})
}

func (a *app) connectView(c *ui.Context) {
	ui.Column(c).Fill().Background(colBg).Center().Padding(24).Children(func() {
		ui.Column(c).Width(520).Gap(16).Padding(24).Radius(16).Background(colCard).Border(1, colLine).Children(func() {
			ui.Column(c).Gap(4).Children(func() {
				ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, icons.Images).FontSize(24).TextColor(colBlue)
					ui.Text(c, "Kết nối nguồn ảnh").FontSize(22).Bold()
				})
				ui.Text(c, "Chọn thư mục trên máy, hoặc server NAS (qua LAN / Kerio VPN).").FontSize(12).TextColor(colMuted).Wrap()
			})
			if ui.Segmented(c, &a.mode, "Thư mục local", "Server NAS").Changed() {
				a.connectMsg = ""
			}
			if a.mode == modeLocal {
				a.localForm(c)
			} else {
				a.serverForm(c)
			}
			if a.connectMsg != "" {
				ui.Text(c, a.connectMsg).FontSize(12).TextColor(colMuted).Wrap()
			}
			a.problemList(c)
		})
	})
}

func (a *app) localForm(c *ui.Context) {
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		path := a.localFolder
		if path == "" {
			path = "Chưa chọn thư mục"
		}
		ui.Text(c, path).Grow(1).SingleLine().TextColor(colMuted)
		if iconButton(c, icons.FolderOpen, "Chọn…").Clicked() {
			a.chooseLocalFolder()
		}
	})
	if iconPrimary(c, icons.Search, "Quét").Disabled(a.localFolder == "").Clicked() {
		a.connectLocal()
	}
}

func (a *app) serverForm(c *ui.Context) {
	ui.Row(c).Gap(10).Children(func() {
		if ui.TextInput(c, &a.host).Label("Server").Placeholder("IP hoặc tên, vd 192.168.x.x").Grow(1).Submitted() {
			a.discover()
		}
		ui.TextInput(c, &a.user).Label("User").Placeholder("tự lấy theo Keychain").Width(170)
	})
	if len(a.suggestions) > 0 {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "Đang mount:").FontSize(12).TextColor(colMuted)
			for _, s := range a.suggestions {
				if iconButton(c, icons.Server, s.Host).Clicked() {
					a.host, a.user = s.Host, s.User
					a.discover()
				}
			}
		})
	}
	if iconButton(c, icons.Network, "Xem các share").Disabled(a.busy).Clicked() {
		a.discover()
	}
	if d := a.discovery; d != nil {
		ui.Column(c).Gap(6).Padding(12).Radius(10).Background(colCardHi).Children(func() {
			for i, s := range d.Shares {
				label := s.Name
				if s.Path != "" {
					label += "  ·  đã mount"
				}
				ui.Checkbox(c, &a.pick[i], label)
			}
		})
		if iconPrimary(c, icons.Plug, "Kết nối và quét").Disabled(a.busy).Clicked() {
			a.connectServer()
		}
	}
	if a.busy {
		ui.Spinner(c)
	}
}

func (a *app) cards(c *ui.Context) {
	var files, dirs int
	var bytes int64
	if a.svc != nil {
		st := a.svc.Current().Stats()
		files, dirs, bytes = st.Files, st.Dirs, st.Bytes
	}
	ui.Row(c).Gap(12).Children(func() {
		card(c, icons.Files, "Files", colBlue, human(int64(files)), fmt.Sprintf("%s thư mục", human(int64(dirs))))
		card(c, icons.Database, "Dung lượng", colOrange, humanBytes(bytes), "tổng các file đã index")
		card(c, icons.Gauge, "Tốc độ quét", colGreen, fmt.Sprintf("%.0f", a.scanSpeed()), fmt.Sprintf("TB %.0f mục/giây · %d lỗi · bỏ qua %s thư mục", a.st.Progress.FilesPerSec(), a.st.Progress.Errors, human(int64(a.st.Stats.ReusedDirs))))
		card(c, icons.Search, "Search", colPink, fmt.Sprintf("%.1f ms", float64(a.result.Took.Microseconds())/1000), fmt.Sprintf("%s kết quả", human(int64(a.result.Total))))
	})
}

// scanSpeed is the rate over the last few seconds while scanning, and the
// average of the finished scan otherwise.
func (a *app) scanSpeed() float64 {
	if a.st.Scanning {
		return recentRate(a.rates, rateWindow)
	}
	return a.st.Progress.FilesPerSec()
}

func card(c *ui.Context, icon *ui.SVG, title string, accent ui.Color, value, sub string) {
	ui.Column(c).Grow(1).Gap(6).Padding(14, 16).Radius(12).Background(colCard).Border(1, colLine).Children(func() {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icon).FontSize(13).TextColor(accent)
			ui.Text(c, title).FontSize(12).Bold().TextColor(accent)
		})
		ui.Text(c, value).FontSize(26).Bold()
		ui.Text(c, sub).FontSize(11).TextColor(colMuted).SingleLine()
	})
}

func (a *app) searchBar(c *ui.Context) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		if ui.SearchField(c, &a.query).Label("Tìm theo tên file").Grow(1).Changed() {
			a.refreshSearch()
		}
		if ui.Checkbox(c, &a.imagesOnly, "Chỉ ảnh").Changed() {
			a.refreshSearch()
		}
		all := a.allChosen()
		if ui.Checkbox(c, &all, "Chọn tất cả").Disabled(len(a.items) == 0).Changed() {
			a.chooseAll(all)
		}
	})
}

func (a *app) results(c *ui.Context) {
	cols := []ui.TableColumn{
		{ID: "chọn", Width: 40, Fixed: true},
		numberColumn,
		{Title: "Tên", MinWidth: 200},
		{Title: "Thư mục", MinWidth: 200},
		{Title: "Size", Width: 90, Align: ui.End},
		{Title: "Sửa lúc", Width: 140},
	}
	a.table.Selected = &a.selected
	a.table.Selection = &a.chosen
	a.table.Key = func(i int) any { return a.items[i].Path }
	ui.Column(c).Grow(1).Gap(8).Children(func() {
		var table ui.Element
		calmRows(c, func() {
			table = ui.Table(c, &a.table, cols, len(a.items), func(row, col int) {
				it := a.items[row]
				sel := row == a.selected || a.chosen.Has(it.Path)
				switch col {
				case 0:
					a.chooseBox(c, it)
				case 1:
					rowNumber(c, row, sel)
				case 2:
					ui.Text(c, it.Name).SingleLine()
				case 3:
					tint(ui.Text(c, it.Dir).SingleLine(), sel, colMuted)
				case 4:
					tint(ui.Text(c, humanBytes(it.Size)), sel, colMuted)
				case 5:
					tint(ui.Text(c, it.ModTime.Format("02/01/2006 15:04")), sel, colMuted)
				}
			}).Grow(1)
		})
		if table.Submitted() {
			a.openSelected(false)
		}
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, a.statusLine()).FontSize(12).TextColor(colMuted).Grow(1).SingleLine()
			has := a.selected >= 0
			n := len(a.selectedPaths())
			if a.chosen.Len() > 1 && iconButton(c, icons.Close, "Bỏ chọn (Esc)").Clicked() {
				a.clearSelection()
			}
			if iconButton(c, icons.FolderOpen, "Show in Finder").Disabled(!has).Clicked() {
				a.openSelected(true)
			}
			if iconButton(c, icons.External, "Mở file").Disabled(!has).Clicked() {
				a.openSelected(false)
			}
			if iconPrimary(c, icons.CloudUpload, fmt.Sprintf("Upload S3 (%d)", n)).Disabled(n == 0).Tooltip("Cmd-click / Shift-click để chọn nhiều file").Clicked() {
				a.uploadSelected()
			}
		})
	})
}

var numberColumn = ui.TableColumn{Title: "#", Width: 52, Align: ui.End, Fixed: true}

// rowNumber is the cell of numberColumn: the row's place in the table as shown.
func rowNumber(c *ui.Context, row int, selected bool) {
	tint(ui.Text(c, strconv.Itoa(row+1)), selected, colMuted)
}

// chooseBox ticks a result row in or out of the selection the upload uses.
func (a *app) chooseBox(c *ui.Context, it search.Item) {
	on := a.chosen.Has(it.Path)
	if !ui.Checkbox(c, &on, "").Label("Chọn " + it.Name).Changed() {
		return
	}
	if on {
		a.chosen.Add(it.Path)
	} else {
		a.chosen.Remove(it.Path)
	}
}

// allChosen reports whether every shown result is selected.
func (a *app) allChosen() bool {
	for _, it := range a.items {
		if !a.chosen.Has(it.Path) {
			return false
		}
	}
	return len(a.items) > 0
}

// chooseAll selects every shown result, or none.
func (a *app) chooseAll(on bool) {
	a.clearSelection()
	if !on {
		return
	}
	for _, it := range a.items {
		a.chosen.Add(it.Path)
	}
}

// calmRows builds fn with a darker accent. A selected table row takes the
// accent as its background, and white text on the bright one is hard to read.
func calmRows(c *ui.Context, fn func()) {
	base := c.Theme()
	t := *base
	t.Accent = colSelect
	c.SetTheme(&t)
	fn()
	c.SetTheme(base)
}

// tint colors e, except on a selected row: the accent behind it drowns the
// muted and status colors, so the row's own text color is kept there.
func tint(e ui.Element, selected bool, clr ui.Color) ui.Element {
	if selected {
		return e
	}
	return e.TextColor(clr)
}

func iconButton(c *ui.Context, icon *ui.SVG, label string) ui.Element {
	return ui.Button(c, "").Label(label).Children(func() {
		ui.Icon(c, icon)
		if label != "" {
			ui.Text(c, label).SingleLine()
		}
	})
}

func iconPrimary(c *ui.Context, icon *ui.SVG, label string) ui.Element {
	return ui.PrimaryButton(c, "").Label(label).Children(func() {
		ui.Icon(c, icon)
		ui.Text(c, label).SingleLine()
	})
}

func section(c *ui.Context, title string, fn func()) {
	ui.Column(c).Grow(1).Gap(10).Padding(14, 16).Radius(12).Background(colCard).Border(1, colLine).Children(func() {
		ui.Text(c, title).FontSize(12).Bold().TextColor(colMuted)
		fn()
	})
}

func (a *app) settingsPanel(c *ui.Context) {
	ui.Column(c).Gap(12).Children(func() {
		ui.Row(c).Gap(12).Children(func() {
			a.sourceSection(c)
			a.s3Section(c)
		})
		ui.Row(c).Gap(12).Children(func() {
			a.indexSection(c)
		})
	})
}

func (a *app) sourceSection(c *ui.Context) {
	section(c, "NGUỒN ĐANG KẾT NỐI", func() {
		ui.Text(c, a.source).Bold().SingleLine()
		for _, r := range a.st.Roots {
			ui.Text(c, r.Path).FontSize(12).TextColor(colMuted).SingleLine()
		}
		if iconButton(c, icons.Plug, "Đổi nguồn / kết nối lại…").Clicked() {
			a.disconnect()
		}
	})
}

func (a *app) indexSection(c *ui.Context) {
	section(c, "INDEX", func() {
		size := "chưa có file index"
		if a.idxSize >= 0 {
			size = humanBytes(a.idxSize)
		}
		if a.store != nil {
			ui.Text(c, a.store.Path(a.source)).FontSize(12).TextColor(colMuted).SingleLine()
		}
		ui.Text(c, "Dung lượng file index: "+size).FontSize(12).TextColor(colMuted).SingleLine()
		if iconButton(c, icons.Refresh, "Quét lại toàn bộ").Disabled(a.svc == nil || a.st.Scanning).Clicked() {
			a.rescan(indexer.Full)
		}
		a.loginToggle(c)
	})
}

// loginToggle is the open-at-login checkbox. The value is cached in a.loginOn
// because OpenAtLogin queries the system. A login item needs a packaged app
// bundle on macOS 13+, so under `mygo dev` the error is shown and the box
// stays unchecked.
func (a *app) loginToggle(c *ui.Context) {
	on := a.loginOn
	if ui.Checkbox(c, &on, "Mở cùng máy và chạy nền").Changed() {
		if err := mygo.App.SetOpenAtLogin(on); err != nil {
			a.loginOn = false
			a.loginErr = "Chỉ bật được trên bản đã đóng gói (go tool mygo build): " + oneLine(err)
		} else {
			a.loginOn, a.loginErr = on, ""
		}
	}
	if a.loginErr != "" {
		ui.Text(c, a.loginErr).FontSize(12).TextColor(colMuted).Wrap()
	}
}

func (a *app) s3Section(c *ui.Context) {
	section(c, "UPLOAD S3", func() {
		cfg := &a.up.form
		ui.Row(c).Gap(8).Children(func() {
			ui.TextInput(c, &cfg.Endpoint).Label("Endpoint").Placeholder("trống = AWS · https://minio.x:9000").Grow(1)
			ui.TextInput(c, &cfg.Region).Label("Region").Placeholder("ap-southeast-1").Width(150)
		})
		ui.Row(c).Gap(8).Children(func() {
			ui.TextInput(c, &cfg.Bucket).Label("Bucket").Placeholder("bucket").Grow(1)
			ui.TextInput(c, &cfg.Prefix).Label("Prefix").Placeholder("thư mục trong bucket (tuỳ chọn)").Grow(1)
		})
		ui.Row(c).Gap(8).Children(func() {
			ui.TextInput(c, &cfg.AccessKey).Label("Access key").Placeholder("access key").Grow(1)
			ph := "secret key"
			if cfg.SecretKey != "" {
				ph = "đã lưu trong Keychain (nhập để đổi)"
			}
			ui.TextInput(c, &a.up.secret).Label("Secret key").Placeholder(ph).Grow(1)
		})
		ui.TextInput(c, &cfg.ListURL).Label("URL danh sách file hỏng").Placeholder(brokenlist.DefaultURL)
		ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
			ui.Checkbox(c, &cfg.PathStyle, "Path-style (MinIO)")
			ui.Checkbox(c, &cfg.SkipExisting, "Bỏ qua file đã có (cùng size)")
		})
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if iconButton(c, icons.Plug, "Test kết nối").Disabled(a.up.checking).Clicked() {
				a.testS3()
			}
			if iconPrimary(c, icons.Check, "Lưu").Clicked() {
				a.saveS3()
			}
			if a.up.checking {
				ui.Spinner(c)
			}
		})
		if a.up.msg != "" {
			ui.Text(c, a.up.msg).FontSize(12).TextColor(colMuted).Wrap()
		}
	})
}

func jobLabel(j *uploadJob) string {
	switch j.State {
	case upQueued:
		if j.Repush > 0 {
			return "Đang chờ · đẩy lại vì S3 bị ghi đè"
		}
		return "Đang chờ"
	case upRunning:
		return fmt.Sprintf("%s / %s", humanBytes(j.Sent), humanBytes(j.Size))
	case upDone:
		return doneLabel(j)
	case upSkipped:
		return "Đã có, bỏ qua"
	case upCanceled:
		return "Đã huỷ"
	default:
		return "Lỗi: " + j.Err
	}
}

// doneLabel is the status of a pushed job, with what the later looks at the
// bucket found.
func doneLabel(j *uploadJob) string {
	label := "Xong · " + humanBytes(j.Size)
	if recheckPending(j) {
		label += " · chờ kiểm lại"
	} else {
		label += " · đã kiểm lại"
	}
	if j.Repush > 0 {
		label += fmt.Sprintf(" · đã đẩy lại %d lần", j.Repush)
	}
	return label
}

func jobIcon(s uploadState) *ui.SVG {
	switch s {
	case upDone, upSkipped:
		return icons.Check
	case upFailed:
		return icons.Alert
	case upRunning:
		return icons.CloudUpload
	default:
		return icons.Image
	}
}

func jobColor(s uploadState) ui.Color {
	switch s {
	case upDone, upSkipped:
		return colGreen
	case upFailed:
		return colPink
	case upRunning:
		return colBlue
	default:
		return colMuted
	}
}

func human(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return fmt.Sprint(n)
	}
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
