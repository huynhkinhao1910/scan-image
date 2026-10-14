package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"

	"github.com/egoist/mygo"
)

const trayIconSize = 32

// trayIcon draws a magnifier as a template image: black pixels whose alpha
// carries the shape, so macOS can tint it for light and dark menu bars.
func trayIcon() []byte {
	const (
		cx, cy       = 13.0, 13.0
		ringOuter    = 9.5
		ringInner    = 6.5
		handleFromXY = 19.5
		handleToXY   = 28.0
		handleHalfW  = 1.75
	)
	img := image.NewNRGBA(image.Rect(0, 0, trayIconSize, trayIconSize))
	for y := range trayIconSize {
		for x := range trayIconSize {
			px, py := float64(x)+0.5, float64(y)+0.5
			r := math.Hypot(px-cx, py-cy)
			ring := math.Min(ringOuter-r, r-ringInner)
			handle := handleHalfW - segDist(px, py, handleFromXY, handleFromXY, handleToXY, handleToXY)
			cover := math.Max(ring, handle) + 0.5
			a := uint8(math.Round(255 * math.Min(1, math.Max(0, cover))))
			img.SetNRGBA(x, y, color.NRGBA{A: a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

// segDist is the distance from (px, py) to the segment (x1, y1)-(x2, y2).
func segDist(px, py, x1, y1, x2, y2 float64) float64 {
	dx, dy := x2-x1, y2-y1
	t := ((px-x1)*dx + (py-y1)*dy) / (dx*dx + dy*dy)
	t = math.Min(1, math.Max(0, t))
	return math.Hypot(px-(x1+t*dx), py-(y1+t*dy))
}

// installTray adds the menu bar icon. The app keeps running without it, so a
// failure is logged and shown in the status line instead of aborting. UI
// thread only.
func (a *app) installTray() {
	tray, err := mygo.NewTray(mygo.TrayOptions{
		Icon:           trayIcon(),
		IconIsTemplate: true,
		ToolTip:        "Scan Image",
		Menu: mygo.NewMenu([]*mygo.MenuItem{
			{Label: "Mở Scan Image", Click: func(*mygo.MenuItem, *mygo.Window) { a.win.Show() }},
			{Label: "Quét ngay", Click: func(*mygo.MenuItem, *mygo.Window) { a.scanNow() }},
			mygo.Separator(),
			{Label: "Thoát", Click: func(*mygo.MenuItem, *mygo.Window) { mygo.App.Quit() }},
		}),
	})
	if err != nil {
		log.Println("tray:", err)
		a.trayErr = "Không tạo được biểu tượng trên thanh menu: " + oneLine(err)
		a.refreshProblems()
		return
	}
	a.tray = tray
}
