package brokenlist

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type reportCall struct {
	method, path, contentType, accept string
	body                              []byte
}

type reportServer struct {
	*httptest.Server
	mu    sync.Mutex
	calls []reportCall
}

func newReportServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *reportServer {
	t.Helper()
	rs := &reportServer{}
	rs.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rs.mu.Lock()
		rs.calls = append(rs.calls, reportCall{r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Accept"), b})
		rs.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *reportServer) count() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.calls)
}

func TestReportSendsExactRequest(t *testing.T) {
	rs := newReportServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	items := []Item{
		{File: "J-121026A17.png", Path: "/Etsy/Etsy-Mai/", Key: "Etsy/Etsy-Mai/J-121026A17.png"},
		{File: "b.png", Path: "/x/", Key: "x/b.png"},
	}

	if err := Report(context.Background(), rs.Client(), rs.URL+ReportPath, items); err != nil {
		t.Fatal(err)
	}

	if rs.count() != 1 {
		t.Fatalf("calls %d, want 1", rs.count())
	}
	c := rs.calls[0]
	if c.method != http.MethodPost || c.path != ReportPath {
		t.Errorf("request %s %s", c.method, c.path)
	}
	if c.contentType != "application/json" || c.accept != "application/json" {
		t.Errorf("headers Content-Type %q Accept %q", c.contentType, c.accept)
	}
	var got struct {
		Files []map[string]string `json:"files"`
	}
	if err := json.Unmarshal(c.body, &got); err != nil {
		t.Fatalf("body %q: %v", c.body, err)
	}
	want := []map[string]string{
		{"file_name": "J-121026A17.png", "path": "/Etsy/Etsy-Mai/"},
		{"file_name": "b.png", "path": "/x/"},
	}
	if !reflect.DeepEqual(got.Files, want) {
		t.Errorf("files %v, want %v", got.Files, want)
	}
}

func TestReportStatus(t *testing.T) {
	cases := []struct {
		status  int
		body    string
		wantErr bool
		want    []string
	}{
		{http.StatusOK, "", false, nil},
		{http.StatusNoContent, "", false, nil},
		{http.StatusUnprocessableEntity, "files.0.path invalid", true, []string{"422", "files.0.path invalid"}},
		{http.StatusTooManyRequests, "slow down", true, []string{"429", "slow down"}},
		{http.StatusInternalServerError, "boom", true, []string{"500", "boom"}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			rs := newReportServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})

			err := Report(context.Background(), rs.Client(), rs.URL+ReportPath, []Item{{File: "a.png", Path: "/x/"}})

			if (err != nil) != tc.wantErr {
				t.Fatalf("err %v, wantErr %v", err, tc.wantErr)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestReportErrorBodyIsShortened(t *testing.T) {
	rs := newReportServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, strings.Repeat("é", 5000))
	})

	err := Report(context.Background(), rs.Client(), rs.URL+ReportPath, []Item{{File: "a.png", Path: "/x/"}})

	if err == nil || strings.Count(err.Error(), "é") != maxEcho {
		t.Fatalf("err has %d runes of body, want %d", strings.Count(fmt.Sprint(err), "é"), maxEcho)
	}
}

func TestReportRefusesWithoutRequest(t *testing.T) {
	rs := newReportServer(t, func(w http.ResponseWriter, _ *http.Request) {})
	one := []Item{{File: "a.png", Path: "/x/"}}
	cases := []struct {
		name    string
		url     string
		items   []Item
		wantErr bool
	}{
		{"empty is a no-op", rs.URL + ReportPath, nil, false},
		{"over the cap", rs.URL + ReportPath, make([]Item, MaxReportFiles+1), true},
		{"plain http", strings.Replace(rs.URL, "https://", "http://", 1) + ReportPath, one, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Report(context.Background(), rs.Client(), tc.url, tc.items)

			if (err != nil) != tc.wantErr {
				t.Errorf("err %v, wantErr %v", err, tc.wantErr)
			}
			if rs.count() != 0 {
				t.Errorf("%d requests, want none", rs.count())
			}
		})
	}
}

func TestReportAcceptsExactlyTheCap(t *testing.T) {
	rs := newReportServer(t, func(w http.ResponseWriter, _ *http.Request) {})
	items := make([]Item, MaxReportFiles)

	if err := Report(context.Background(), rs.Client(), rs.URL+ReportPath, items); err != nil || rs.count() != 1 {
		t.Fatalf("err %v calls %d", err, rs.count())
	}
}

func TestReportRefusesRedirect(t *testing.T) {
	var target int
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) { target++ })
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	err := Report(context.Background(), srv.Client(), srv.URL+"/moved", []Item{{File: "a.png", Path: "/x/"}})

	if err == nil || target != 0 {
		t.Fatalf("err %v, redirect target hits %d; want an error and no follow", err, target)
	}
}

func TestReportCancelledContext(t *testing.T) {
	rs := newReportServer(t, func(w http.ResponseWriter, _ *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Report(ctx, rs.Client(), rs.URL+ReportPath, []Item{{File: "a.png", Path: "/x/"}})

	if err == nil {
		t.Fatal("want error")
	}
}

func TestReportURL(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{DefaultURL, "https://production.trackingis.info" + ReportPath, false},
		{"https://example.com:8443/a/b.txt?x=1", "https://example.com:8443" + ReportPath, false},
		{"http://example.com/list.txt", "", true},
		{"not a url", "", true},
		{"https://", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ReportURL(tc.in)

			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("ReportURL(%q) = %q, %v; want %q, wantErr %v", tc.in, got, err, tc.want, tc.wantErr)
			}
		})
	}
}
