package brokenlist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ReportPath is the endpoint, relative to the origin that serves the list.
const ReportPath = "/api/v2/s3/missing-design-files/uploaded"

// MaxReportFiles is the most files the server accepts in one report.
const MaxReportFiles = 100

const maxReportReply = 64 << 10

type reportFile struct {
	FileName string `json:"file_name"`
	Path     string `json:"path"`
}

// ReportURL returns the report endpoint on the origin (scheme and host) of
// listURL.
func ReportURL(listURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(listURL))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("brokenlist: không suy ra được địa chỉ báo server từ %q (cần https và tên máy)", echo(listURL))
	}
	return "https://" + u.Host + ReportPath, nil
}

// Report tells the server that items are back on S3, in one POST of at most
// MaxReportFiles items. Any 2xx is success; a redirect is an error, because a
// redirected POST may be replayed as a GET.
func Report(ctx context.Context, client *http.Client, url string, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	if len(items) > MaxReportFiles {
		return fmt.Errorf("brokenlist: báo tối đa %d file một lần, có %d", MaxReportFiles, len(items))
	}
	if !strings.HasPrefix(strings.ToLower(url), "https://") {
		return fmt.Errorf("brokenlist: URL phải dùng https: %q", echo(url))
	}
	body, err := json.Marshal(struct {
		Files []reportFile `json:"files"`
	}{reportFiles(items)})
	if err != nil {
		return fmt.Errorf("brokenlist: mã hoá yêu cầu: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("brokenlist: tạo request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := noRedirect(client).Do(req)
	if err != nil {
		return fmt.Errorf("brokenlist: báo server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxReportReply))
	text := echo(strings.Join(strings.Fields(string(reply)), " "))
	if text == "" {
		return fmt.Errorf("brokenlist: server trả về %s", echo(resp.Status))
	}
	return fmt.Errorf("brokenlist: server trả về %s: %s", echo(resp.Status), text)
}

func reportFiles(items []Item) []reportFile {
	out := make([]reportFile, len(items))
	for i, it := range items {
		out[i] = reportFile{FileName: it.File, Path: it.Path}
	}
	return out
}

func noRedirect(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}
