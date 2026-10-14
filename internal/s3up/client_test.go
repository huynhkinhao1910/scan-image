package s3up

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	parts   map[string]map[int][]byte
	failPut int
	types   map[string]string
	// headSize, when set, makes HEAD report this size; headMissing makes it 404.
	headSize    int
	headMissing bool
	// list* drive the ListObjectsV2 handler: raw queries received, a cap on
	// keys per page, prefixes that answer 403, and a per-request delay.
	listQueries []string
	listPageMax int
	listFail    map[string]bool
	listDelay   time.Duration
}

func newFake() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}, types: map[string]string{}}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", 403)
		return
	}
	key, _ := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/bucket/"))
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == "GET" && q.Get("list-type") == "2":
		f.list(w, r, q)
	case r.Method == "HEAD":
		b, ok := f.objects[key]
		if f.headMissing {
			ok = false
		}
		if ok && f.headSize > 0 {
			b = make([]byte, f.headSize)
		}
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set("ETag", fmt.Sprintf(`"%x"`, md5.Sum(b)))
	case r.Method == "PUT" && q.Get("uploadId") != "":
		if f.failPut > 0 {
			f.failPut--
			http.Error(w, "<Error><Code>SlowDown</Code></Error>", 503)
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.parts[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
	case r.Method == "PUT":
		if f.failPut > 0 {
			f.failPut--
			http.Error(w, "<Error><Code>InternalError</Code></Error>", 500)
			return
		}
		if int64(len(body)) != r.ContentLength {
			http.Error(w, "short body", 400)
			return
		}
		f.objects[key] = body
		f.types[key] = r.Header.Get("Content-Type")
	case r.Method == "POST" && q.Has("uploads"):
		id := fmt.Sprintf("up-%d", len(f.parts)+1)
		f.parts[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == "POST" && q.Get("uploadId") != "":
		var req struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		_ = xml.Unmarshal(body, &req)
		ps := f.parts[q.Get("uploadId")]
		var nums []int
		for n := range ps {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		if len(nums) != len(req.Parts) {
			http.Error(w, "<Error><Code>InvalidPart</Code></Error>", 400)
			return
		}
		var all []byte
		for _, n := range nums {
			all = append(all, ps[n]...)
		}
		f.objects[key] = all
		fmt.Fprint(w, "<CompleteMultipartUploadResult/>")
	default:
		http.Error(w, "unexpected", 400)
	}
}

func setup(t *testing.T, size int) (*fakeS3, *Client, string, []byte) {
	t.Helper()
	f := newFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	data := make([]byte, size)
	_, _ = rand.Read(data)
	local := filepath.Join(t.TempDir(), "Ảnh cá.jpg")
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{Endpoint: srv.URL, Bucket: "bucket", AccessKey: "AK", SecretKey: "SK", PathStyle: true, SkipExisting: true})
	return f, c, local, data
}

func TestUploadSmall(t *testing.T) {
	f, c, local, data := setup(t, 300_000)
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.failPut = 1
	var last int64
	res, err := c.Upload(context.Background(), local, "img/Ảnh cá.jpg", func(sent, total int64) { last = sent })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.objects["img/Ảnh cá.jpg"], data) || last != int64(len(data)) || res.Skipped {
		t.Fatalf("stored %d bytes, progress %d, %+v", len(f.objects["img/Ảnh cá.jpg"]), last, res)
	}
	if f.types["img/Ảnh cá.jpg"] != "image/jpeg" {
		t.Errorf("content type %q", f.types["img/Ảnh cá.jpg"])
	}
	res, err = c.Upload(context.Background(), local, "img/Ảnh cá.jpg", nil)
	if err != nil || !res.Skipped {
		t.Errorf("second upload %+v %v, want skipped", res, err)
	}
}

func TestUploadMultipart(t *testing.T) {
	f, c, local, data := setup(t, 5<<20+123)
	c.PartSize, c.MultipartThreshold = 1<<20, 2<<20
	f.failPut = 1
	if _, err := c.Upload(context.Background(), local, "big.jpg", nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.objects["big.jpg"], data) {
		t.Fatalf("stored %d bytes, want %d", len(f.objects["big.jpg"]), len(data))
	}
}

func TestBadCredentials(t *testing.T) {
	_, c, _, _ := setup(t, 1)
	c.cfg.AccessKey = "WRONG"
	if err := c.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err %v", err)
	}
}

func TestKeyAndVirtualHostURL(t *testing.T) {
	cfg := Config{Bucket: "photos", Region: "ap-southeast-1", Prefix: "/nas/"}
	if k := cfg.Key("/Design Team/a.jpg"); k != "nas/Design Team/a.jpg" {
		t.Errorf("key %q", k)
	}
	u, _ := New(cfg).objectURL("nas/a b.jpg", nil)
	if u.String() != "https://photos.s3.ap-southeast-1.amazonaws.com/nas/a%20b.jpg" {
		t.Errorf("url %s", u)
	}
}

type memSecrets map[string]string

func (m memSecrets) Get(a string) (string, error) { return m[a], nil }
func (m memSecrets) Set(a, s string) error        { m[a] = s; return nil }

func TestConfigRoundTripKeepsSecretOutOfFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x", "s3.json")
	sec := memSecrets{}
	in := Config{Bucket: "b", Region: "r", AccessKey: "AK", SecretKey: "topsecret", ListURL: "https://example.com/list.txt"}
	if err := SaveConfig(p, in, sec); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if bytes.Contains(raw, []byte("topsecret")) {
		t.Fatal("secret written to file")
	}
	out, err := LoadConfig(p, sec)
	if err != nil || out != in {
		t.Errorf("loaded %+v, %v", out, err)
	}
}

func TestUploadWithOverwriteReplacesExisting(t *testing.T) {
	f, c, local, data := setup(t, 1000)
	f.objects["k.jpg"] = make([]byte, len(data))
	res, err := c.UploadWith(context.Background(), local, "k.jpg", UploadOptions{}, nil)
	if err != nil || !res.Skipped {
		t.Fatalf("without overwrite: %+v %v, want skipped", res, err)
	}
	res, err = c.UploadWith(context.Background(), local, "k.jpg", UploadOptions{Overwrite: true}, nil)
	if err != nil || res.Skipped || !bytes.Equal(f.objects["k.jpg"], data) {
		t.Fatalf("overwrite: %+v %v, stored equal=%v", res, err, bytes.Equal(f.objects["k.jpg"], data))
	}
}

func TestUploadWithVerify(t *testing.T) {
	opt := UploadOptions{Verify: true}
	t.Run("passes", func(t *testing.T) {
		_, c, local, _ := setup(t, 1000)
		if _, err := c.UploadWith(context.Background(), local, "k.jpg", opt, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("size mismatch", func(t *testing.T) {
		f, c, local, _ := setup(t, 1000)
		f.headSize = 7
		_, err := c.UploadWith(context.Background(), local, "k.jpg", opt, nil)
		if err == nil || !strings.Contains(err.Error(), "k.jpg") || !strings.Contains(err.Error(), "1000") || !strings.Contains(err.Error(), "7") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		f, c, local, _ := setup(t, 1000)
		f.headMissing = true
		_, err := c.UploadWith(context.Background(), local, "k.jpg", opt, nil)
		if err == nil || !strings.Contains(err.Error(), "k.jpg") {
			t.Fatalf("err %v", err)
		}
	})
}

func TestUploadRefusesSymlinkAndNonRegularSource(t *testing.T) {
	f, c, local, _ := setup(t, 1000)
	link := filepath.Join(t.TempDir(), "link.jpg")
	if err := os.Symlink(local, link); err != nil {
		t.Fatal(err)
	}

	_, err := c.UploadWith(context.Background(), link, "k.jpg", UploadOptions{Overwrite: true}, nil)

	if err == nil || !strings.Contains(err.Error(), "link.jpg") {
		t.Fatalf("symlink: err %v, want a refusal naming the file", err)
	}
	if _, err := c.UploadWith(context.Background(), t.TempDir(), "k.jpg", UploadOptions{Overwrite: true}, nil); err == nil {
		t.Fatal("directory: want a refusal")
	}
	if len(f.objects) != 0 {
		t.Fatalf("stored %d objects, want none", len(f.objects))
	}
}

func TestStalledServerFailsInsteadOfPinningAWorker(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })
	c := New(Config{Endpoint: srv.URL, Bucket: "bucket", AccessKey: "AK", SecretKey: "SK", PathStyle: true})
	if tr, ok := c.http.Transport.(*http.Transport); !ok || tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Fatalf("New: transport %T does not carry the response header timeout", c.http.Transport)
	}
	c.http = &http.Client{Transport: transportWith(50 * time.Millisecond)}
	c.Retries = 0

	start := time.Now()
	_, _, err := c.Head(context.Background(), "k.jpg")

	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %v, want a timeout error", err, time.Since(start))
	}
}

func TestObjectURLIsTheAddressABrowserOpens(t *testing.T) {
	cfg := Config{Bucket: "thanhcong", Region: "us-east-1"}
	got, err := New(cfg).ObjectURL("Etsy/Etsy-Chau/A 09.png")
	if want := "https://thanhcong.s3.us-east-1.amazonaws.com/Etsy/Etsy-Chau/A%2009.png"; err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
	cfg.Endpoint, cfg.PathStyle = "https://minio.local:9000", true
	got, err = New(cfg).ObjectURL("a.png")
	if want := "https://minio.local:9000/thanhcong/a.png"; err != nil || got != want {
		t.Fatalf("path style: got %q, %v; want %q", got, err, want)
	}
}

func TestUploadRefusesEmptyFile(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	c := New(Config{Endpoint: srv.URL, Bucket: "bucket", AccessKey: "AK", SecretKey: "SK", PathStyle: true})
	empty := filepath.Join(t.TempDir(), "blank.png")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := c.UploadWith(context.Background(), empty, "Etsy/A/blank.png", UploadOptions{Overwrite: true, Verify: true}, nil)

	if err == nil || !strings.Contains(err.Error(), "0 byte") {
		t.Fatalf("err = %v", err)
	}
	if calls != 0 {
		t.Fatalf("an empty file reached the bucket: %d requests", calls)
	}
}

func TestStatTellsSizeAndETag(t *testing.T) {
	f, c, _, _ := setup(t, 10)
	f.objects["k.jpg"] = []byte("hello")

	got, ok, err := c.Stat(context.Background(), "k.jpg")

	want := Object{Size: 5, ETag: `"5d41402abc4b2a76b9719d911017c592"`}
	if err != nil || !ok || got != want {
		t.Fatalf("Stat = %+v, %v, %v; want %+v", got, ok, err, want)
	}
	if _, ok, err := c.Stat(context.Background(), "gone.jpg"); err != nil || ok {
		t.Fatalf("missing object: ok %v err %v", ok, err)
	}
}

func TestUploadWithVerifyReturnsTheETagOnTheBucket(t *testing.T) {
	_, c, local, data := setup(t, 1000)

	res, err := c.UploadWith(context.Background(), local, "k.jpg", UploadOptions{Verify: true}, nil)

	if want := fmt.Sprintf(`"%x"`, md5.Sum(data)); err != nil || res.ETag != want {
		t.Fatalf("ETag %q err %v, want %q", res.ETag, err, want)
	}
}
