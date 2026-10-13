// Package s3up uploads files to S3 or an S3-compatible store (MinIO, R2,
// Wasabi…) with only the standard library: SigV4 signing, single PUT for
// small files and multipart for large ones, retrying parts on failure.
package s3up

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Config describes the target bucket. SecretKey is never written to the
// config file; it lives in the Keychain.
type Config struct {
	Endpoint     string `json:"endpoint"`
	Region       string `json:"region"`
	Bucket       string `json:"bucket"`
	Prefix       string `json:"prefix"`
	AccessKey    string `json:"accessKey"`
	SecretKey    string `json:"-"`
	PathStyle    bool   `json:"pathStyle"`
	SkipExisting bool   `json:"skipExisting"`
	// ListURL is the broken-file list address; empty means the caller's default.
	ListURL string `json:"listUrl"`
	// AutoPush keeps the broken-file tab fetching, matching and pushing on
	// its own.
	AutoPush bool `json:"autoPush"`
}

// Validate reports the first missing field.
func (c Config) Validate() error {
	switch {
	case c.Bucket == "":
		return errors.New("thiếu bucket")
	case c.AccessKey == "" || c.SecretKey == "":
		return errors.New("thiếu access key / secret key")
	case c.Endpoint == "" && c.Region == "":
		return errors.New("thiếu region (AWS) hoặc endpoint (S3-compatible)")
	}
	return nil
}

// Key joins the prefix and a relative path into an object key.
func (c Config) Key(rel string) string {
	rel = strings.TrimLeft(strings.ReplaceAll(rel, "\\", "/"), "/")
	p := strings.Trim(c.Prefix, "/")
	if p == "" {
		return rel
	}
	return p + "/" + rel
}

// Client talks to one bucket.
type Client struct {
	cfg  Config
	http *http.Client
	now  func() time.Time
	// PartSize is the multipart part size; files larger than
	// MultipartThreshold are sent in parts.
	PartSize           int64
	MultipartThreshold int64
	Retries            int
}

// responseHeaderTimeout bounds the wait for a reply once a request is fully
// sent, so a server that stops answering cannot hold an upload worker forever.
const responseHeaderTimeout = 2 * time.Minute

func transportWith(headerTimeout time.Duration) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = headerTimeout
	return t
}

// sharedTransport keeps one connection pool for every Client.
var sharedTransport = transportWith(responseHeaderTimeout)

// New returns a client for cfg.
func New(cfg Config) *Client {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return &Client{
		cfg:                cfg,
		http:               &http.Client{Transport: sharedTransport},
		now:                time.Now,
		PartSize:           16 << 20,
		MultipartThreshold: 64 << 20,
		Retries:            3,
	}
}

// Config returns the client's configuration.
func (c *Client) Config() Config { return c.cfg }

// ObjectURL is the address of key on the bucket, as a browser opens it. It
// carries no signature, so it shows the object only when the bucket allows
// public reads.
func (c *Client) ObjectURL(key string) (string, error) {
	u, err := c.objectURL(key, nil)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (c *Client) objectURL(key string, q url.Values) (*url.URL, error) {
	base := c.cfg.Endpoint
	if base == "" {
		base = "https://s3." + c.cfg.Region + ".amazonaws.com"
	}
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return nil, fmt.Errorf("endpoint: %w", err)
	}
	p := ""
	if key != "" {
		p = "/" + strings.TrimLeft(key, "/")
	}
	if c.cfg.PathStyle {
		p = "/" + c.cfg.Bucket + p
	} else {
		u.Host = c.cfg.Bucket + "." + u.Host
	}
	if p == "" {
		p = "/"
	}
	u.Path = p
	u.RawPath = encodePath(p)
	u.RawQuery = canonicalQuery(q)
	return u, nil
}

func (c *Client) do(ctx context.Context, method, key string, q url.Values, body io.Reader, size int64, payloadHash string, hdr http.Header) (*http.Response, error) {
	u, err := c.objectURL(key, q)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.URL = u
	req.ContentLength = size
	if body == nil || size == 0 {
		req.Body = http.NoBody
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	signV4(req, c.cfg.AccessKey, c.cfg.SecretKey, c.cfg.Region, payloadHash, c.now())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return resp, nil
}

// Error is an S3 error response.
type Error struct {
	Status  int
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("s3: HTTP %d", e.Status)
	}
	return fmt.Sprintf("s3: %s: %s (HTTP %d)", e.Code, e.Message, e.Status)
}

func readError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Status: resp.StatusCode}
	_ = xml.Unmarshal(b, e)
	return e
}

func retryable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Status >= 500 || e.Status == 429 || e.Code == "RequestTimeout" || e.Code == "SlowDown"
	}
	return !errors.Is(err, context.Canceled)
}

func (c *Client) retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
			}
		}
		if err = fn(); err == nil || !retryable(err) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// Check verifies the credentials and the bucket by listing one key.
func (c *Client) Check(ctx context.Context) error {
	q := url.Values{"list-type": {"2"}, "max-keys": {"1"}}
	resp, err := c.do(ctx, http.MethodGet, "", q, nil, 0, emptySHA256, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Head returns the size of an object, with ok false when it is missing.
func (c *Client) Head(ctx context.Context, key string) (size int64, ok bool, err error) {
	resp, err := c.do(ctx, http.MethodHead, key, nil, nil, 0, emptySHA256, nil)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Status == http.StatusNotFound {
			return 0, false, nil
		}
		return 0, false, err
	}
	resp.Body.Close()
	return resp.ContentLength, true, nil
}

// Result tells what Upload did.
type Result struct {
	Key     string
	Skipped bool
	Bytes   int64
}

// UploadOptions tune one upload.
type UploadOptions struct {
	// Overwrite uploads even when Config.SkipExisting is on and an object of
	// the same size exists.
	Overwrite bool
	// Verify heads the object after the upload and fails unless it exists with
	// the local file's size.
	Verify bool
}

// Upload sends the file at local to key, calling progress with the bytes
// sent so far.
func (c *Client) Upload(ctx context.Context, local, key string, progress func(sent, total int64)) (Result, error) {
	return c.UploadWith(ctx, local, key, UploadOptions{}, progress)
}

// UploadWith is Upload with options.
func (c *Client) UploadWith(ctx context.Context, local, key string, opt UploadOptions, progress func(sent, total int64)) (Result, error) {
	f, st, err := openRegular(local)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	size := st.Size()
	if size == 0 {
		return Result{}, fmt.Errorf("s3: %s rỗng (0 byte), không đẩy", local)
	}
	res := Result{Key: key, Bytes: size}
	if c.cfg.SkipExisting && !opt.Overwrite {
		if n, ok, err := c.Head(ctx, key); err == nil && ok && n == size {
			res.Skipped = true
			if progress != nil {
				progress(size, size)
			}
			return res, nil
		}
	}
	ct := mime.TypeByExtension(strings.ToLower(path.Ext(local)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	counter := &progressCounter{total: size, fn: progress}
	if size <= c.MultipartThreshold {
		err = c.retry(ctx, func() error {
			counter.reset(0)
			body := io.NewSectionReader(f, 0, size)
			hdr := http.Header{"Content-Type": {ct}}
			resp, err := c.do(ctx, http.MethodPut, key, nil, counter.wrap(body), size, unsignedPayload, hdr)
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		})
	} else {
		err = c.multipart(ctx, f, key, size, ct, counter)
	}
	if err == nil && opt.Verify {
		err = c.verify(ctx, key, size)
	}
	return res, err
}

// openRegular opens the regular file at local. A symbolic link is refused,
// also one swapped in after the check: a link stored on a share resolves on
// this machine, so following it could send a local file.
func openRegular(local string) (*os.File, os.FileInfo, error) {
	lst, err := os.Lstat(local)
	if err != nil {
		return nil, nil, err
	}
	if !lst.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("s3: %s is not a regular file (%s)", local, lst.Mode().Type())
	}
	f, err := os.Open(local)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err == nil && !os.SameFile(lst, st) {
		err = fmt.Errorf("s3: %s changed while it was opened", local)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, st, nil
}

func (c *Client) verify(ctx context.Context, key string, size int64) error {
	n, ok, err := c.Head(ctx, key)
	switch {
	case err != nil:
		return fmt.Errorf("verify %q: %w", key, err)
	case !ok:
		return fmt.Errorf("verify %q: object không tồn tại sau khi upload (local %d bytes)", key, size)
	case n != size:
		return fmt.Errorf("verify %q: size trên S3 %d bytes, file local %d bytes", key, n, size)
	}
	return nil
}

func (c *Client) multipart(ctx context.Context, f *os.File, key string, size int64, ct string, counter *progressCounter) error {
	partSize := max(c.PartSize, (size+9999)/10000)
	var uploadID string
	err := c.retry(ctx, func() error {
		resp, err := c.do(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, 0, emptySHA256, http.Header{"Content-Type": {ct}})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var r struct {
			UploadID string `xml:"UploadId"`
		}
		if err := xml.NewDecoder(resp.Body).Decode(&r); err != nil {
			return err
		}
		uploadID = r.UploadID
		return nil
	})
	if err != nil {
		return fmt.Errorf("create multipart: %w", err)
	}

	type part struct {
		XMLName    xml.Name `xml:"Part"`
		PartNumber int      `xml:"PartNumber"`
		ETag       string   `xml:"ETag"`
	}
	var parts []part
	for n, off := 1, int64(0); off < size; n, off = n+1, off+partSize {
		length := min(partSize, size-off)
		q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {uploadID}}
		var etag string
		err := c.retry(ctx, func() error {
			counter.reset(off)
			body := io.NewSectionReader(f, off, length)
			resp, err := c.do(ctx, http.MethodPut, key, q, counter.wrap(body), length, unsignedPayload, nil)
			if err != nil {
				return err
			}
			resp.Body.Close()
			etag = resp.Header.Get("ETag")
			return nil
		})
		if err != nil {
			c.abort(key, uploadID)
			return fmt.Errorf("part %d: %w", n, err)
		}
		parts = append(parts, part{PartNumber: n, ETag: etag})
	}

	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part
	}{Parts: parts})
	if err != nil {
		return err
	}
	err = c.retry(ctx, func() error {
		resp, err := c.do(ctx, http.MethodPost, key, url.Values{"uploadId": {uploadID}}, bytes.NewReader(body), int64(len(body)), hexSHA256(body), http.Header{"Content-Type": {"application/xml"}})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if bytes.Contains(b, []byte("<Error>")) {
			e := &Error{Status: 200}
			_ = xml.Unmarshal(b, e)
			return e
		}
		return nil
	})
	if err != nil {
		c.abort(key, uploadID)
		return fmt.Errorf("complete multipart: %w", err)
	}
	return nil
}

func (c *Client) abort(key, uploadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if resp, err := c.do(ctx, http.MethodDelete, key, url.Values{"uploadId": {uploadID}}, nil, 0, emptySHA256, nil); err == nil {
		resp.Body.Close()
	}
}

type progressCounter struct {
	total int64
	base  int64
	sent  int64
	fn    func(sent, total int64)
}

func (p *progressCounter) reset(base int64) { p.base, p.sent = base, 0 }

func (p *progressCounter) wrap(r io.Reader) io.Reader {
	return readerFunc(func(b []byte) (int, error) {
		n, err := r.Read(b)
		p.sent += int64(n)
		if p.fn != nil && n > 0 {
			p.fn(p.base+p.sent, p.total)
		}
		return n, err
	})
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(b []byte) (int, error) { return f(b) }
