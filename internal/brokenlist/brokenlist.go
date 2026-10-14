// Package brokenlist fetches and validates the server's list of design files
// that are missing or broken on S3. The list is untrusted input.
package brokenlist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// DefaultURL is where the server publishes the list.
const DefaultURL = "https://production.trackingis.info/api/queue-s3-upload"

const (
	maxBody      = 8 << 20
	maxEcho      = 200
	maxItems     = 10_000
	maxRejected  = 1_000
	maxKeyBytes  = 1024
	maxRedirects = 10
	fetchTimeout = 20 * time.Second
)

// Item is one file to re-upload. Key is the S3 object key built from Path and File.
type Item struct{ File, Path, Key string }

// hasControl reports characters that make a key display differently from the
// bytes written: controls, formatting marks (bidi, zero width) and the line
// and paragraph separators.
func hasControl(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return true
		}
	}
	return false
}

// Key builds the S3 object key for file under path. Both come from outside the
// app, so both are validated.
func Key(path, file string) (string, error) {
	switch {
	case file == "":
		return "", errors.New("Tên file trống")
	case strings.ContainsAny(file, `/\`) || strings.Contains(file, ".."):
		return "", errors.New("Tên file không được chứa / \\ hoặc ..")
	case hasControl(file):
		return "", errors.New("Tên file chứa ký tự điều khiển")
	case !strings.HasPrefix(path, "/") || !strings.HasSuffix(path, "/"):
		return "", errors.New("Path phải bắt đầu và kết thúc bằng dấu /")
	case strings.Contains(path, `\`) || strings.Contains(path, "..") || strings.Contains(path, "//"):
		return "", errors.New("Path không được chứa \\ .. hoặc //")
	case hasControl(path):
		return "", errors.New("Path chứa ký tự điều khiển")
	case len(path)-1+len(file) > maxKeyBytes:
		return "", fmt.Errorf("Path và tên file dài hơn %d byte", maxKeyBytes)
	}
	return path[1:] + file, nil
}

func echo(s string) string {
	if r := []rune(s); len(r) > maxEcho {
		return string(r[:maxEcho]) + "…"
	}
	return s
}

// queueRow is one entry of the JSON form of the list.
type queueRow struct {
	SKU  string `json:"sku"`
	Path string `json:"path"`
}

// collector validates rows as they come and keeps the caps: at most maxItems
// items, so a runaway list cannot flood the table and the upload queue, and at
// most maxRejected reasons.
type collector struct {
	seen              map[string]bool
	items             []Item
	rejected          []string
	dropped, unlisted int
}

func (c *collector) reject(raw, why string) {
	if len(c.rejected) == maxRejected {
		c.unlisted++
		return
	}
	c.rejected = append(c.rejected, echo(raw)+" — "+why)
}

func (c *collector) add(file, path, raw string) {
	if len(c.items) == maxItems {
		c.dropped++
		return
	}
	file, path = strings.TrimSpace(file), strings.TrimSpace(path)
	if file == "" || path == "" {
		c.reject(raw, "thiếu File hoặc Path")
		return
	}
	key, err := Key(path, file)
	if err != nil {
		c.reject(raw, err.Error())
		return
	}
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.items = append(c.items, Item{File: file, Path: path, Key: key})
}

func (c *collector) result() ([]Item, []string) {
	if c.unlisted > 0 {
		c.rejected = append(c.rejected, fmt.Sprintf("… và %d dòng lỗi nữa", c.unlisted))
	}
	if c.dropped > 0 {
		c.rejected = append(c.rejected, fmt.Sprintf("Danh sách dài hơn %d file: bỏ %d dòng cuối", maxItems, c.dropped))
	}
	return c.items, c.rejected
}

// Parse reads the list in either form the server publishes: a JSON array of
// {"sku", "path"}, or text with one "File<TAB>Path" per line. Entries that
// fail validation go to rejected, each with the reason.
func Parse(text string) (items []Item, rejected []string) {
	c := &collector{seen: map[string]bool{}}
	if t := strings.TrimSpace(text); strings.HasPrefix(t, "[") {
		parseQueue(c, t)
	} else {
		parseLines(c, text)
	}
	return c.result()
}

func parseQueue(c *collector, text string) {
	var rows []queueRow
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		c.reject(text, "JSON không đọc được: "+echo(err.Error()))
		return
	}
	for _, r := range rows {
		c.add(r.SKU, r.Path, r.SKU+" "+r.Path)
	}
}

func parseLines(c *collector, text string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cells := strings.Split(line, "\t")
		if len(cells) != 2 {
			if len(c.items) < maxItems {
				c.reject(line, "cần đúng 2 cột File và Path ngăn bằng TAB")
			} else {
				c.dropped++
			}
			continue
		}
		if strings.EqualFold(strings.TrimSpace(cells[0]), "File") && strings.EqualFold(strings.TrimSpace(cells[1]), "Path") {
			continue
		}
		c.add(cells[0], cells[1], line)
	}
}

// httpsOnly returns a copy of client that refuses to follow a redirect away
// from https: the list decides which S3 objects get overwritten, so it must
// not be readable or replaceable on the way.
func httpsOnly(client *http.Client) *http.Client {
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("chuyển hướng sang %s không dùng https", req.URL.Scheme)
		}
		if len(via) >= maxRedirects {
			return errors.New("quá nhiều lần chuyển hướng")
		}
		return nil
	}
	return &c
}

// Fetch downloads and parses the list. Redirects are followed only to https.
func Fetch(ctx context.Context, client *http.Client, url string) (items []Item, rejected []string, err error) {
	if !strings.HasPrefix(strings.ToLower(url), "https://") {
		return nil, nil, fmt.Errorf("brokenlist: URL phải dùng https: %q", echo(url))
	}
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("brokenlist: tạo request: %w", err)
	}
	resp, err := httpsOnly(client).Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("brokenlist: tải danh sách: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("brokenlist: server trả về %s", echo(resp.Status))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, nil, fmt.Errorf("brokenlist: đọc danh sách: %w", err)
	}
	if len(b) > maxBody {
		return nil, nil, fmt.Errorf("brokenlist: danh sách lớn hơn %d MiB", maxBody>>20)
	}
	items, rejected = Parse(string(b))
	return items, rejected, nil
}
