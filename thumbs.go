package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/egoist/mygo/ui"

	"github.com/isuccesscorp/scan-image/internal/search"
	"github.com/isuccesscorp/scan-image/internal/thumb"
)

const (
	thumbPixels  = 320
	thumbWorkers = 8
	thumbKeep    = 1500
)

// thumbs loads previews for the grid. The view asks for the items it
// builds, which are only those in view; the newest asks go first, so
// scrolling fast does not wait behind what scrolled away.
type thumbs struct {
	maker   *thumb.Maker
	bitmaps map[string]*ui.Bitmap
	order   []string
	failed  map[string]bool
	pending map[string]bool

	mu    sync.Mutex
	stack []search.Item
	wake  chan struct{}
	once  sync.Once
}

func (a *app) initThumbs() {
	t := &a.th
	t.bitmaps, t.failed, t.pending = map[string]*ui.Bitmap{}, map[string]bool{}, map[string]bool{}
	t.wake = make(chan struct{}, 1)
	m, err := thumb.New(appName, thumbPixels)
	if err == nil {
		t.maker = m
	}
}

func isImage(name string) bool {
	return slices.Contains(imageExts, strings.ToLower(filepath.Ext(name)))
}

// thumbFor returns the bitmap of it, or nil while it loads or when it has
// none, asking for it the first time.
func (a *app) thumbFor(it search.Item) *ui.Bitmap {
	t := &a.th
	if b := t.bitmaps[it.Path]; b != nil {
		return b
	}
	if t.maker == nil || t.failed[it.Path] || t.pending[it.Path] || !isImage(it.Name) {
		return nil
	}
	t.pending[it.Path] = true
	t.once.Do(func() {
		for range thumbWorkers {
			go a.thumbWorker()
		}
	})
	t.mu.Lock()
	t.stack = append(t.stack, it)
	if len(t.stack) > 400 {
		dropped := t.stack[:len(t.stack)-400]
		t.stack = slices.Clone(t.stack[len(t.stack)-400:])
		t.mu.Unlock()
		for _, d := range dropped {
			delete(t.pending, d.Path)
		}
	} else {
		t.mu.Unlock()
	}
	select {
	case t.wake <- struct{}{}:
	default:
	}
	return nil
}

func (a *app) thumbWorker() {
	t := &a.th
	for {
		t.mu.Lock()
		if len(t.stack) == 0 {
			t.mu.Unlock()
			<-t.wake
			continue
		}
		it := t.stack[len(t.stack)-1]
		t.stack = t.stack[:len(t.stack)-1]
		more := len(t.stack) > 0
		t.mu.Unlock()
		if more {
			select {
			case t.wake <- struct{}{}:
			default:
			}
		}

		img, _, err := t.maker.Load(context.Background(), it.Path, it.Size, it.ModTime)
		var bmp *ui.Bitmap
		if err == nil {
			bmp = ui.NewBitmap(img)
		}
		a.update(func() {
			delete(t.pending, it.Path)
			if err != nil {
				t.failed[it.Path] = true
				return
			}
			t.bitmaps[it.Path] = bmp
			t.order = append(t.order, it.Path)
			if len(t.order) > thumbKeep {
				for _, p := range t.order[:len(t.order)-thumbKeep] {
					delete(t.bitmaps, p)
				}
				t.order = slices.Clone(t.order[len(t.order)-thumbKeep:])
			}
		})
	}
}
