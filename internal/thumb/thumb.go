// Package thumb makes small previews of images and caches them on local
// disk. It tries, fastest first: the cache, the JPEG preview embedded in
// the file's first bytes (most JPEGs and camera raws), then macOS Quick
// Look (qlmanage), which reads PNG, HEIC, PSD and more but reads the whole
// file and starts a process.
package thumb

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Runner executes a command; a seam for tests.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Source says where a thumbnail came from.
type Source int

// Sources of a thumbnail.
const (
	FromCache Source = iota
	FromEmbedded
	FromQuickLook
)

// Maker produces thumbnails into Dir.
type Maker struct {
	Dir     string
	Size    int
	Timeout time.Duration
	Run     Runner
	// MinEmbedded is the smallest embedded preview width accepted;
	// smaller ones fall through to Quick Look.
	MinEmbedded int
}

// New returns a Maker caching under the user's cache folder.
func New(app string, size int) (*Maker, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, app, "thumbs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Maker{
		Dir:         dir,
		Size:        size,
		Timeout:     30 * time.Second,
		MinEmbedded: 120,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}, nil
}

// CachePath is where the thumbnail of a file version lives; a changed
// size or mtime makes a new one.
func (m *Maker) CachePath(path string, size int64, mtime time.Time) string {
	h := sha1.Sum([]byte(path + "\x00" + strconv.FormatInt(size, 10) + "\x00" + strconv.FormatInt(mtime.Unix(), 10) + "\x00" + strconv.Itoa(m.Size)))
	name := hex.EncodeToString(h[:])
	return filepath.Join(m.Dir, name[:2], name+".jpg")
}

// Load returns the thumbnail of the file, upright.
func (m *Maker) Load(ctx context.Context, path string, size int64, mtime time.Time) (image.Image, Source, error) {
	dst := m.CachePath(path, size, mtime)
	if b, err := os.ReadFile(dst); err == nil {
		if img, err := jpeg.Decode(bytes.NewReader(b)); err == nil {
			return img, FromCache, nil
		}
	}
	if img, err := m.embedded(path, size); err == nil {
		m.store(dst, img)
		return img, FromEmbedded, nil
	}
	img, err := m.quickLook(ctx, path)
	if err != nil {
		return nil, FromQuickLook, err
	}
	m.store(dst, img)
	return img, FromQuickLook, nil
}

func (m *Maker) embedded(path string, size int64) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, orient, err := Embedded(f, size)
	if err != nil {
		return nil, err
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if max(b.Dx(), b.Dy()) < m.MinEmbedded {
		return nil, ErrNoEmbedded
	}
	return Orient(img, orient), nil
}

func (m *Maker) quickLook(ctx context.Context, path string) (image.Image, error) {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(m.Dir, "ql-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(ctx, m.Timeout)
	defer cancel()
	out, err := m.Run(ctx, "qlmanage", "-t", "-s", strconv.Itoa(m.Size), "-o", tmp, path)
	if err != nil {
		return nil, fmt.Errorf("qlmanage: %w: %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(tmp, filepath.Base(path)+".png"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("không tạo được thumbnail")
	}
	if err != nil {
		return nil, err
	}
	return png.Decode(bytes.NewReader(b))
}

func (m *Maker) store(dst string, img image.Image) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		return
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err == nil {
		_ = os.Rename(tmp, dst)
	}
}
