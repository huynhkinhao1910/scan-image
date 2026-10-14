package main

import (
	"log"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

const wakeScanDelay = 15 * time.Second

func main() {
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}
	a := newApp()
	a.wireLifecycle()
	mygo.App.WhenReady(func() {
		a.win = mygo.NewWindow(mygo.WindowOptions{
			Title:     "Scan Image",
			Width:     1180,
			Height:    760,
			MinWidth:  860,
			MinHeight: 520,
			StateKey:  "main",
			Hidden:    mygo.App.WasOpenedAtLogin(),
			Content:   ui.View(a.view),
		})
		a.hideOnClose()
		a.loginOn = mygo.App.OpenAtLogin()
		a.installTray()
		go a.loadSuggestions()
		a.resume()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// wireLifecycle hooks the app-level events that keep the index current in the
// background: re-show on Dock click or second launch, rescan on wake, and a
// synchronous service close before the process ends.
func (a *app) wireLifecycle() {
	show := func() {
		if a.win != nil {
			a.win.Show()
		}
	}
	mygo.App.OnSecondInstance(func([]string, string) { show() })
	mygo.App.OnActivate(func(bool) { show() })
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { a.quitting = true })
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) { a.closeOnQuit() })
	mygo.Power.OnResume(func() { a.kick(wakeScanDelay) })
}

// hideOnClose turns the close button into hide so scanning and upload state
// survive; a real quit sets a.quitting first (OnBeforeQuit runs before MyGo
// closes the windows).
func (a *app) hideOnClose() {
	a.win.OnClose(func(e *mygo.CloseEvent) {
		if a.quitting {
			return
		}
		e.PreventDefault()
		a.win.Hide()
	})
}
