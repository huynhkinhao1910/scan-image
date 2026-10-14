package s3up

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// list answers ListObjectsV2 from f.objects, honouring prefix, delimiter,
// max-keys, continuation-token and encoding-type, and rejects a request
// whose Authorization does not match its own method, URL and headers.
func (f *fakeS3) list(w http.ResponseWriter, r *http.Request, q url.Values) {
	if !validSignature(r) {
		http.Error(w, "<Error><Code>SignatureDoesNotMatch</Code></Error>", 403)
		return
	}
	f.listQueries = append(f.listQueries, r.URL.RawQuery)
	if f.listDelay > 0 {
		select {
		case <-time.After(f.listDelay):
		case <-r.Context().Done():
			return
		}
	}
	prefix := q.Get("prefix")
	if f.listFail[prefix] {
		http.Error(w, "<Error><Code>AccessDenied</Code><Message>denied</Message></Error>", 403)
		return
	}
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	type item struct {
		name   string
		isPref bool
	}
	var items []item
	for _, k := range keys {
		rest := k[len(prefix):]
		if i := strings.Index(rest, q.Get("delimiter")); q.Get("delimiter") != "" && i >= 0 {
			name := prefix + rest[:i+1]
			if n := len(items); n > 0 && items[n-1].name == name {
				continue
			}
			items = append(items, item{name, true})
			continue
		}
		items = append(items, item{k, false})
	}
	enc := func(s string) string {
		if q.Get("encoding-type") == "url" {
			return url.QueryEscape(s)
		}
		return s
	}
	after, _ := url.QueryUnescape(q.Get("continuation-token"))
	limit, _ := strconv.Atoi(q.Get("max-keys"))
	if f.listPageMax > 0 && (limit == 0 || f.listPageMax < limit) {
		limit = f.listPageMax
	}
	var out strings.Builder
	n, next, more := 0, "", false
	for _, it := range items {
		if after != "" && it.name <= after {
			continue
		}
		if n == limit {
			more = true
			break
		}
		n++
		next = it.name
		if it.isPref {
			fmt.Fprintf(&out, "<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>", enc(it.name))
		} else {
			fmt.Fprintf(&out, "<Contents><Key>%s</Key></Contents>", enc(it.name))
		}
	}
	fmt.Fprintf(w, "<ListBucketResult><IsTruncated>%v</IsTruncated>", more)
	if more {
		fmt.Fprintf(w, "<NextContinuationToken>%s</NextContinuationToken>", url.QueryEscape(next))
	}
	fmt.Fprint(w, out.String(), "</ListBucketResult>")
}

func validSignature(r *http.Request) bool {
	got := r.Header.Get("Authorization")
	at, err := time.Parse(amzDateFormat, r.Header.Get("X-Amz-Date"))
	if err != nil {
		return false
	}
	req := r.Clone(context.Background())
	req.Header.Del("Authorization")
	signV4(req, "AK", "SK", "us-east-1", r.Header.Get("X-Amz-Content-Sha256"), at)
	return req.Header.Get("Authorization") == got
}

func listSetup(t *testing.T, keys ...string) (*fakeS3, *Client) {
	t.Helper()
	f := newFake()
	for _, k := range keys {
		f.objects[k] = []byte("x")
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, New(Config{Endpoint: srv.URL, Bucket: "bucket", AccessKey: "AK", SecretKey: "SK", PathStyle: true, Prefix: "ignored/"})
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestListPrefixesRootAndNested(t *testing.T) {
	f, c := listSetup(t, "a.jpg", "Etsy/x.jpg", "Etsy/Etsy-An/1.jpg", "Etsy/Etsy-Huyen/2.jpg", "Zed/1.jpg")
	got, trunc, err := c.ListPrefixes(context.Background(), "")
	if err != nil || trunc {
		t.Fatalf("root: %v %v", trunc, err)
	}
	equal(t, got, []string{"Etsy/", "Zed/"})
	f.mu.Lock()
	wantQ := "delimiter=%2F&encoding-type=url&list-type=2&max-keys=1000&prefix="
	if f.listQueries[0] != wantQ {
		t.Errorf("query %q, want %q", f.listQueries[0], wantQ)
	}
	f.mu.Unlock()

	got, _, err = c.ListPrefixes(context.Background(), "Etsy/")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, []string{"Etsy/Etsy-An/", "Etsy/Etsy-Huyen/"})
	f.mu.Lock()
	if !strings.HasSuffix(f.listQueries[1], "&prefix=Etsy%2F") {
		t.Errorf("query %q", f.listQueries[1])
	}
	f.mu.Unlock()
}

func TestListPrefixesPaginatesInterleavedKeys(t *testing.T) {
	f, c := listSetup(t, "a.jpg", "b/1", "c.jpg", "d/1", "e.jpg", "f/1", "g.jpg", "h/1", "i.jpg")
	f.listPageMax = 2
	got, trunc, err := c.ListPrefixes(context.Background(), "")
	if err != nil || trunc {
		t.Fatalf("%v %v", trunc, err)
	}
	equal(t, got, []string{"b/", "d/", "f/", "h/"})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.listQueries) != 5 {
		t.Errorf("%d requests, want 5", len(f.listQueries))
	}
	if !strings.Contains(f.listQueries[1], "continuation-token=") {
		t.Errorf("second query lacks token: %q", f.listQueries[1])
	}
}

func TestListPrefixesEmptyBucket(t *testing.T) {
	_, c := listSetup(t)
	got, trunc, err := c.ListPrefixes(context.Background(), "")
	if err != nil || trunc || len(got) != 0 {
		t.Fatalf("%q %v %v", got, trunc, err)
	}
}

func TestListPrefixesDecodesURLEncodedPrefixes(t *testing.T) {
	_, c := listSetup(t, "a b/1.jpg", "c+d/1.jpg", "Ảnh cá/1.jpg", "Thư mục/Đà Nẵng/1.jpg")
	got, _, err := c.ListPrefixes(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, []string{"Thư mục/", "a b/", "c+d/", "Ảnh cá/"})
	got, _, err = c.ListPrefixes(context.Background(), "Thư mục/")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, got, []string{"Thư mục/Đà Nẵng/"})
}

func TestListPrefixesPageCapReportsTruncated(t *testing.T) {
	var keys []string
	for i := range 30 {
		keys = append(keys, fmt.Sprintf("f%02d/x", i))
	}
	f, c := listSetup(t, keys...)
	f.listPageMax = 1
	got, trunc, err := c.ListPrefixes(context.Background(), "")
	if err != nil || !trunc {
		t.Fatalf("trunc %v err %v", trunc, err)
	}
	if len(got) != listMaxPages || len(f.listQueries) != listMaxPages {
		t.Errorf("%d prefixes, %d requests, want %d", len(got), len(f.listQueries), listMaxPages)
	}
}

func TestListPrefixesAccessDeniedIsError(t *testing.T) {
	f, c := listSetup(t, "a/1")
	f.listFail = map[string]bool{"": true}
	_, _, err := c.ListPrefixes(context.Background(), "")
	var e *Error
	if !errors.As(err, &e) || e.Status != 403 || e.Code != "AccessDenied" {
		t.Fatalf("err %v", err)
	}
	c.cfg.SecretKey = "other"
	_, _, err = c.ListPrefixes(context.Background(), "")
	if err == nil || strings.Contains(err.Error(), "AWS4-HMAC") || strings.Contains(err.Error(), "other") {
		t.Fatalf("err %v", err)
	}
}

func TestListPrefixesRootURLBothStyles(t *testing.T) {
	q := url.Values{"list-type": {"2"}, "prefix": {"a b/"}}
	cases := []struct {
		pathStyle bool
		want      string
	}{
		{true, "https://s3.eu-west-1.amazonaws.com/photos?list-type=2&prefix=a%20b%2F"},
		{false, "https://photos.s3.eu-west-1.amazonaws.com/?list-type=2&prefix=a%20b%2F"},
	}
	for _, tc := range cases {
		u, err := New(Config{Bucket: "photos", Region: "eu-west-1", PathStyle: tc.pathStyle}).objectURL("", q)
		if err != nil || u.String() != tc.want {
			t.Errorf("pathStyle=%v: %v %v, want %s", tc.pathStyle, u, err, tc.want)
		}
	}
}

func threeLevelTree(t *testing.T) (*fakeS3, *Client) {
	return listSetup(t, "top.jpg", "A/a1/x/f.jpg", "A/a2/f.jpg", "B/f.jpg", "C/c1/y/z/f.jpg")
}

func TestFoldersWalksTreeSortedWithinDepth(t *testing.T) {
	_, c := threeLevelTree(t)
	got, trunc, err := c.Folders(context.Background(), FolderOptions{})
	if err != nil || trunc {
		t.Fatalf("%v %v", trunc, err)
	}
	equal(t, got, []string{"A/", "A/a1/", "A/a1/x/", "A/a2/", "B/", "C/", "C/c1/", "C/c1/y/"})

	got, trunc, err = c.Folders(context.Background(), FolderOptions{MaxDepth: 1})
	if err != nil || trunc {
		t.Fatalf("%v %v", trunc, err)
	}
	equal(t, got, []string{"A/", "B/", "C/"})
}

func TestFoldersMaxFoldersTruncates(t *testing.T) {
	_, c := threeLevelTree(t)
	got, trunc, err := c.Folders(context.Background(), FolderOptions{MaxFolders: 4, Workers: 1})
	if err != nil || !trunc || len(got) != 4 {
		t.Fatalf("%q %v %v", got, trunc, err)
	}
}

func TestFoldersErrorOnDeepPrefix(t *testing.T) {
	f, c := threeLevelTree(t)
	f.listFail = map[string]bool{"A/a1/": true}
	_, _, err := c.Folders(context.Background(), FolderOptions{})
	var e *Error
	if !errors.As(err, &e) || e.Status != 403 {
		t.Fatalf("err %v", err)
	}
}

func TestFoldersErrorOnRoot(t *testing.T) {
	f, c := threeLevelTree(t)
	f.listFail = map[string]bool{"": true}
	got, _, err := c.Folders(context.Background(), FolderOptions{})
	if err == nil || got != nil {
		t.Fatalf("%q %v", got, err)
	}
}

func TestFoldersHonoursCancellation(t *testing.T) {
	f, c := threeLevelTree(t)
	f.listDelay = 5 * time.Second

	pre, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, _, err := c.Folders(pre, FolderOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled: %v", err)
	}

	ctx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	_, _, err := c.Folders(ctx, FolderOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("in flight: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestFoldersDeadlineReturnsWhatWasFound(t *testing.T) {
	f, c := threeLevelTree(t)
	f.listDelay = 300 * time.Millisecond
	ctx, stop := context.WithTimeout(context.Background(), 450*time.Millisecond)
	defer stop()

	got, trunc, err := c.Folders(ctx, FolderOptions{Workers: 1})

	if err != nil || !trunc {
		t.Fatalf("got %q, truncated %v, err %v; want the top level, truncated", got, trunc, err)
	}
	equal(t, got, []string{"A/", "B/", "C/"})
}
