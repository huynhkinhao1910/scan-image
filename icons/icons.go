// Package icons embeds the Lucide icons the app shows (ISC license, see
// LICENSE-lucide.txt). They draw in the text color of where they sit.
package icons

import (
	"embed"

	"github.com/egoist/mygo/ui"
)

//go:embed svg/*.svg
var files embed.FS

func load(name string) *ui.SVG {
	b, err := files.ReadFile("svg/" + name + ".svg")
	if err != nil {
		panic(err)
	}
	return ui.MustParseSVG(b)
}

// Icons used across the app.
var (
	Search      = load("search")
	Folder      = load("folder")
	FolderOpen  = load("folder-open")
	HardDrive   = load("hard-drive")
	Server      = load("server")
	Refresh     = load("refresh-cw")
	Stop        = load("square")
	Settings    = load("settings")
	CloudUpload = load("cloud-upload")
	List        = load("list")
	Grid        = load("layout-grid")
	External    = load("external-link")
	Close       = load("x")
	Check       = load("circle-check")
	Alert       = load("circle-alert")
	Image       = load("image")
	Images      = load("images")
	Files       = load("files")
	Gauge       = load("gauge")
	Database    = load("database")
	ChevronDown = load("chevron-down")
	ChevronUp   = load("chevron-up")
	Plug        = load("plug")
	Retry       = load("rotate-ccw")
	Trash       = load("trash-2")
	Network     = load("network")
)
