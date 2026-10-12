package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/icons"
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
	if c.Shortcut(0, ui.KeyEscape) && (a.chosen.Len() > 0 || a.selected >= 0) {
		a.clearSelection()
	}
	ui.Column(c).Fill().Background(colBg).Padding(20).Gap(16).Children(func() {
		a.header(c)
		if a.settingsOpen {
			a.settingsPanel(c)
		}
		a.cards(c)
		a.searchBar(c)
		a.results(c)
	})
}

func (a *app) header(c *ui.Context) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Grow(1).Gap(2).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, icons.Search).FontSize(22).TextColor(colBlue)
				ui.Text(c, "Scan Image").FontSize(22).Bold()
			})
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, icons.Server).FontSize(12).TextColor(colMuted)
				ui.Text(c, fmt.Sprintf("%s · %d thư mục gốc", a.source, len(a.roots))).FontSize(12).TextColor(colMuted).SingleLine()
			})
		})
		if a.scanning {
			ui.Spinner(c)
			if iconButton(c, icons.Stop, "Dừng").Clicked() {
				a.stopScan()
			}
		} else if iconButton(c, icons.Refresh, "Quét lại").Clicked() {
			a.startScan(a.source, a.roots)
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
			a.settingsOpen = !a.settingsOpen
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
	if a.index != nil {
		st := a.index.Stats()
		files, dirs, bytes = st.Files, st.Dirs, st.Bytes
	}
	ui.Row(c).Gap(12).Children(func() {
		card(c, icons.Files, "Files", colBlue, human(int64(files)), fmt.Sprintf("%s thư mục", human(int64(dirs))))
		card(c, icons.Database, "Dung lượng", colOrange, humanBytes(bytes), "tổng các file đã index")
		card(c, icons.Gauge, "Tốc độ quét", colGreen, fmt.Sprintf("%.0f", a.progress.FilesPerSec()), fmt.Sprintf("mục/giây · %d lỗi", a.progress.Errors))
		card(c, icons.Search, "Search", colPink, fmt.Sprintf("%.1f ms", float64(a.result.Took.Microseconds())/1000), fmt.Sprintf("%s kết quả", human(int64(a.result.Total))))
	})
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
		seg := ui.SegmentedBase(c, &a.viewMode, 2)
		seg.Track.Padding(3).Radius(8).Background(colCardHi).Border(1, colLine).Children(func() {
			for i, ic := range []*ui.SVG{icons.List, icons.Grid} {
				s := seg.Segment(i).Padding(5, 12).Radius(6).Label([]string{"Danh sách", "Lưới"}[i]).Tooltip([]string{"Danh sách", "Lưới"}[i])
				if i == a.viewMode {
					s.Background(colBlue)
				}
				s.Children(func() { ui.Icon(c, ic) })
			}
		})
	})
}

func (a *app) results(c *ui.Context) {
	cols := []ui.TableColumn{
		{Title: "Tên", MinWidth: 200},
		{Title: "Thư mục", MinWidth: 200},
		{Title: "Size", Width: 90, Align: ui.End},
		{Title: "Sửa lúc", Width: 140},
	}
	a.table.Selected = &a.selected
	a.table.Selection = &a.chosen
	a.table.Key = func(i int) any { return a.items[i].Path }
	ui.Column(c).Grow(1).Gap(8).Children(func() {
		if a.viewMode == viewGrid {
			a.gridView(c)
		} else if ui.Table(c, &a.table, cols, len(a.items), func(row, col int) {
			it := a.items[row]
			switch col {
			case 0:
				ui.Text(c, it.Name).SingleLine()
			case 1:
				ui.Text(c, it.Dir).SingleLine().TextColor(colMuted)
			case 2:
				ui.Text(c, humanBytes(it.Size)).TextColor(colMuted)
			case 3:
				ui.Text(c, it.ModTime.Format("02/01/2006 15:04")).TextColor(colMuted)
			}
		}).Grow(1).Submitted() {
			a.openSelected(false)
		}
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, a.status).FontSize(12).TextColor(colMuted).Grow(1).SingleLine()
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
		a.uploadPanel(c)
	})
}

const (
	viewList = iota
	viewGrid
)

func (a *app) gridView(c *ui.Context) {
	a.grid.Selected = &a.selected
	a.grid.Selection = &a.chosen
	a.grid.Key = func(i int) any { return a.items[i].Path }
	a.grid.Label = func(i int) string { return a.items[i].Name }
	if ui.GridView(c, &a.grid, len(a.items), 170, 200, func(i int) {
		it := a.items[i]
		ui.Column(c).Grow(1).Gap(6).Padding(6).Children(func() {
			box := ui.Box(c).Height(150).Radius(8).Background(colCardHi).Clip().Center()
			box.Children(func() {
				if b := a.thumbFor(it); b != nil {
					ui.Image(c, b).Fit(ui.Contain).Grow(1)
				} else {
					ext := strings.ToUpper(strings.TrimPrefix(filepath.Ext(it.Name), "."))
					ui.Column(c).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Icon(c, icons.Image).FontSize(28).TextColor(colMuted)
						ui.Text(c, ext).FontSize(11).Bold().TextColor(colMuted)
					})
				}
			})
			ui.Text(c, it.Name).FontSize(12).SingleLine()
			ui.Text(c, humanBytes(it.Size)).FontSize(11).TextColor(colMuted).SingleLine()
		})
	}).Grow(1).Submitted() {
		a.openSelected(false)
	}
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
	ui.Row(c).Gap(12).Children(func() {
		section(c, "NGUỒN ĐANG KẾT NỐI", func() {
			ui.Text(c, a.source).Bold().SingleLine()
			for _, r := range a.roots {
				ui.Text(c, r).FontSize(12).TextColor(colMuted).SingleLine()
			}
			if iconButton(c, icons.Plug, "Đổi nguồn / kết nối lại…").Clicked() {
				a.disconnect()
			}
		})
		section(c, "UPLOAD S3", func() {
			cfg := &a.up.cfg
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
	})
}

func (a *app) uploadPanel(c *ui.Context) {
	if len(a.up.jobs) == 0 {
		return
	}
	active, done, failed := a.up.counts()
	ui.Column(c).Gap(8).Padding(12, 14).Radius(12).Background(colCard).Border(1, colLine).Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			chevron := icons.ChevronDown
			if a.uploadsOpen {
				chevron = icons.ChevronUp
			}
			if ui.Button(c, "").Label("Upload S3").Children(func() {
				ui.Icon(c, icons.CloudUpload).TextColor(colBlue)
				ui.Text(c, fmt.Sprintf("Upload S3 · %d đang chạy · %d xong · %d lỗi", active, done, failed)).SingleLine()
				ui.Icon(c, chevron).TextColor(colMuted)
			}).Clicked() {
				a.uploadsOpen = !a.uploadsOpen
			}
			ui.Text(c, "").Grow(1)
			if active > 0 {
				ui.Spinner(c)
				if iconButton(c, icons.Close, "Huỷ hàng đợi").Clicked() {
					a.cancelQueued()
				}
			}
			if failed > 0 && iconButton(c, icons.Retry, "Thử lại lỗi").Clicked() {
				a.retryFailed()
			}
			if iconButton(c, icons.Trash, "Xoá đã xong").Clicked() {
				a.clearFinished()
			}
		})
		if !a.uploadsOpen {
			return
		}
		ui.Column(c).Gap(6).Children(func() {
			shown := 0
			for i := len(a.up.jobs) - 1; i >= 0 && shown < 8; i-- {
				j := a.up.jobs[i]
				shown++
				ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
					ui.Icon(c, jobIcon(j.State)).FontSize(13).TextColor(jobColor(j.State))
					ui.Text(c, j.Key).FontSize(12).SingleLine().Grow(1)
					frac := 0.0
					if j.Size > 0 {
						frac = float64(j.Sent) / float64(j.Size)
					}
					ui.Progress(c, frac).Width(140)
					ui.Text(c, jobLabel(j)).FontSize(12).TextColor(jobColor(j.State)).Width(150).SingleLine().Tooltip(j.Err)
				})
			}
		})
	})
}

func jobLabel(j *uploadJob) string {
	switch j.State {
	case upQueued:
		return "Đang chờ"
	case upRunning:
		return fmt.Sprintf("%s / %s", humanBytes(j.Sent), humanBytes(j.Size))
	case upDone:
		return "Xong · " + humanBytes(j.Size)
	case upSkipped:
		return "Đã có, bỏ qua"
	case upCanceled:
		return "Đã huỷ"
	default:
		return "Lỗi: " + j.Err
	}
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
