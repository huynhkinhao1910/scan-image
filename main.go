package main

import (
	"log"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

func main() {
	a := newApp()
	mygo.App.WhenReady(func() {
		a.win = mygo.NewWindow(mygo.WindowOptions{
			Title:     "Scan Image",
			Width:     1180,
			Height:    760,
			MinWidth:  860,
			MinHeight: 520,
			StateKey:  "main",
			Content:   ui.View(a.view),
		})
		go a.loadSuggestions()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
