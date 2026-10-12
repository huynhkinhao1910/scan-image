package s3up

import (
	"bytes"
	"context"
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
)

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	parts   map[string]map[int][]byte
	failPut int
	types   map[string]string
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
		fmt.Fprint(w, "<ListBucketResult></ListBucketResult>")
	case r.Method == "HEAD":
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
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
	in := Config{Bucket: "b", Region: "r", AccessKey: "AK", SecretKey: "topsecret"}
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
