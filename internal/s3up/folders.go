package s3up

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
)

const (
	listPageKeys = 1000
	// listMaxPages bounds the requests of one ListPrefixes call: a prefix that
	// directly holds many objects returns them interleaved with its folders.
	listMaxPages = 20
	listMaxBody  = 8 << 20

	defaultFolderDepth   = 3
	defaultFolderCount   = 2000
	defaultFolderWorkers = 6
)

type listPage struct {
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Prefixes              []string `xml:"CommonPrefixes>Prefix"`
}

// ListPrefixes returns the folders directly under prefix ("" = bucket root):
// the common prefixes of a delimiter-"/" listing, each ending in "/", sorted.
// truncated reports that the listing was cut at the page limit.
func (c *Client) ListPrefixes(ctx context.Context, prefix string) (prefixes []string, truncated bool, err error) {
	token := ""
	for range listMaxPages {
		page, err := c.listPage(ctx, prefix, token)
		if err != nil {
			return nil, false, err
		}
		for _, p := range page.Prefixes {
			d, err := url.QueryUnescape(p)
			if err != nil {
				return nil, false, fmt.Errorf("s3: list %q: bad prefix %q: %w", prefix, p, err)
			}
			prefixes = append(prefixes, d)
		}
		if !page.IsTruncated {
			sort.Strings(prefixes)
			return prefixes, false, nil
		}
		if page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	sort.Strings(prefixes)
	return prefixes, true, nil
}

func (c *Client) listPage(ctx context.Context, prefix, token string) (listPage, error) {
	q := url.Values{
		"list-type":     {"2"},
		"delimiter":     {"/"},
		"prefix":        {prefix},
		"max-keys":      {fmt.Sprint(listPageKeys)},
		"encoding-type": {"url"},
	}
	if token != "" {
		q.Set("continuation-token", token)
	}
	var body []byte
	err := c.retry(ctx, func() error {
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, 0, emptySHA256, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err = io.ReadAll(io.LimitReader(resp.Body, listMaxBody+1))
		return err
	})
	var page listPage
	if err != nil {
		return page, err
	}
	if len(body) > listMaxBody {
		return page, fmt.Errorf("s3: list %q: response over %d bytes", prefix, listMaxBody)
	}
	if err := xml.Unmarshal(body, &page); err != nil {
		return page, fmt.Errorf("s3: list %q: %w", prefix, err)
	}
	return page, nil
}

// FolderOptions bound a walk of the bucket's folder tree.
type FolderOptions struct {
	MaxDepth   int // levels below the root; 0 → 3
	MaxFolders int // stop after this many folders; 0 → 2000
	Workers    int // listings in flight; 0 → 6
}

// Folders walks the folder tree breadth-first from the bucket root and
// returns every folder found, sorted, each ending in "/". truncated reports
// that a limit (depth excluded), a page limit or the context's deadline cut
// the result: a deadline that passes after some folders were found returns
// those instead of an error, so a slow bucket still offers its top levels.
func (c *Client) Folders(ctx context.Context, opt FolderOptions) (folders []string, truncated bool, err error) {
	if opt.MaxDepth <= 0 {
		opt.MaxDepth = defaultFolderDepth
	}
	if opt.MaxFolders <= 0 {
		opt.MaxFolders = defaultFolderCount
	}
	if opt.Workers <= 0 {
		opt.Workers = defaultFolderWorkers
	}
	walk, cancel := context.WithCancel(ctx)
	defer cancel()

	var found atomic.Int64
	frontier := []string{""}
	for depth := 1; depth <= opt.MaxDepth && len(frontier) > 0; depth++ {
		results := make([][]string, len(frontier))
		var (
			wg       sync.WaitGroup
			once     sync.Once
			firstErr error
			cut      atomic.Bool
		)
		sem := make(chan struct{}, opt.Workers)
	launch:
		for i, p := range frontier {
			if found.Load() >= int64(opt.MaxFolders) {
				cut.Store(true)
				break
			}
			select {
			case sem <- struct{}{}:
			case <-walk.Done():
				break launch
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				r, tr, err := c.ListPrefixes(walk, p)
				if err != nil {
					once.Do(func() { firstErr = err; cancel() })
					return
				}
				if tr {
					cut.Store(true)
				}
				found.Add(int64(len(r)))
				results[i] = r
			}()
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			for _, r := range results {
				folders = append(folders, r...)
			}
			if !errors.Is(err, context.DeadlineExceeded) || len(folders) == 0 {
				return nil, false, err
			}
			truncated = true
			break
		}
		if firstErr != nil {
			return nil, false, firstErr
		}
		truncated = truncated || cut.Load()
		frontier = frontier[:0:0]
		for _, r := range results {
			frontier = append(frontier, r...)
		}
		folders = append(folders, frontier...)
	}
	if len(folders) > opt.MaxFolders {
		folders, truncated = folders[:opt.MaxFolders], true
	}
	sort.Strings(folders)
	return folders, truncated, nil
}
