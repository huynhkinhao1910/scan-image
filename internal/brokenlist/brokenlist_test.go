package brokenlist

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestKey(t *testing.T) {
	cases := []struct {
		name, path, file, want string
		bad                    bool
	}{
		{name: "normal", path: "/Etsy/Etsy-Anh/", file: "S-121026A09.png", want: "Etsy/Etsy-Anh/S-121026A09.png"},
		{name: "root", path: "/", file: "a.png", want: "a.png"},
		{name: "unicode", path: "/Ảnh/", file: "cá.jpg", want: "Ảnh/cá.jpg"},
		{name: "empty file", path: "/a/", file: "", bad: true},
		{name: "file slash", path: "/a/", file: "b/c.png", bad: true},
		{name: "file backslash", path: "/a/", file: `b\c.png`, bad: true},
		{name: "file dotdot", path: "/a/", file: "..png", bad: true},
		{name: "file control", path: "/a/", file: "a\x01.png", bad: true},
		{name: "file del", path: "/a/", file: "a\x7f.png", bad: true},
		{name: "path no leading slash", path: "a/", file: "b.png", bad: true},
		{name: "path no trailing slash", path: "/a", file: "b.png", bad: true},
		{name: "path empty", path: "", file: "b.png", bad: true},
		{name: "path backslash", path: `/a\b/`, file: "b.png", bad: true},
		{name: "path dotdot", path: "/a/../b/", file: "b.png", bad: true},
		{name: "path double slash", path: "/a//b/", file: "b.png", bad: true},
		{name: "path control", path: "/a\n/", file: "b.png", bad: true},
		{name: "file c1 control", path: "/a/", file: "a\u0085.png", bad: true},
		{name: "file bidi override", path: "/a/", file: "a\u202egnp.exe", bad: true},
		{name: "file zero width", path: "/a/", file: "a\u200b.png", bad: true},
		{name: "file line separator", path: "/a/", file: "a\u2028.png", bad: true},
		{name: "path zero width", path: "/a\u200b/", file: "b.png", bad: true},
		{name: "key at limit", path: "/" + strings.Repeat("a", maxKeyBytes-6) + "/", file: "b.png", want: strings.Repeat("a", maxKeyBytes-6) + "/b.png"},
		{name: "key over limit", path: "/" + strings.Repeat("a", maxKeyBytes-5) + "/", file: "b.png", bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Key(tc.path, tc.file)
			if tc.bad {
				if err == nil {
					t.Fatalf("want error, got key %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	longLine := strings.Repeat("x", 500)
	cases := []struct {
		name         string
		text         string
		wantItems    []Item
		wantRejected int
	}{
		{
			name: "real sample",
			text: "S-121026A09.png\t/Etsy/Etsy-Anh/\nOGWG04-16.png\t/Etsy/Etsy-Chau/\n",
			wantItems: []Item{
				{File: "S-121026A09.png", Path: "/Etsy/Etsy-Anh/", Key: "Etsy/Etsy-Anh/S-121026A09.png"},
				{File: "OGWG04-16.png", Path: "/Etsy/Etsy-Chau/", Key: "Etsy/Etsy-Chau/OGWG04-16.png"},
			},
		},
		{
			name:      "crlf blank lines spaces",
			text:      "\r\n  a.png\t/x/  \r\n\r\n\n",
			wantItems: []Item{{File: "a.png", Path: "/x/", Key: "x/a.png"}},
		},
		{
			name:      "header skipped",
			text:      "File\tPath\na.png\t/x/\n",
			wantItems: []Item{{File: "a.png", Path: "/x/", Key: "x/a.png"}},
		},
		{name: "spaces not tab", text: "a.png /x/\n", wantRejected: 1},
		{name: "one cell", text: "a.png\n", wantRejected: 1},
		{name: "three cells", text: "a.png\t/x/\textra\n", wantRejected: 1},
		{name: "empty cell", text: "a.png\t\n", wantRejected: 1},
		{name: "bad path", text: "a.png\tx/\nb.png\t/y/\n", wantItems: []Item{{File: "b.png", Path: "/y/", Key: "y/b.png"}}, wantRejected: 1},
		{
			name:      "duplicates first wins",
			text:      "a.png\t/x/\nb.png\t/y/\na.png\t/x/\n",
			wantItems: []Item{{File: "a.png", Path: "/x/", Key: "x/a.png"}, {File: "b.png", Path: "/y/", Key: "y/b.png"}},
		},
		{name: "html page", text: "<html>\n<body>404 Not Found</body>\n</html>\n", wantRejected: 3},
		{name: "long line", text: longLine + "\n", wantRejected: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, rejected := Parse(tc.text)
			if !reflect.DeepEqual(items, tc.wantItems) && !(len(items) == 0 && len(tc.wantItems) == 0) {
				t.Errorf("items %+v, want %+v", items, tc.wantItems)
			}
			if len(rejected) != tc.wantRejected {
				t.Fatalf("rejected %q, want %d", rejected, tc.wantRejected)
			}
			for _, r := range rejected {
				if !strings.Contains(r, " — ") {
					t.Errorf("rejected %q lacks reason", r)
				}
				if n := len([]rune(r)); n > 400 {
					t.Errorf("rejected line not capped: %d runes", n)
				}
			}
		})
	}
}

func TestParseCapsRejectedText(t *testing.T) {
	_, rejected := Parse(strings.Repeat("y", 5000))
	if len(rejected) != 1 || len([]rune(rejected[0])) > 300 {
		t.Fatalf("got %d entries, first len %d", len(rejected), len([]rune(rejected[0])))
	}
}

func TestFetch(t *testing.T) {
	const sample = "a.png\t/x/\nb.png\t/y/\n"
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ok.txt", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, sample)
	})
	mux.HandleFunc("/404.txt", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	})
	mux.HandleFunc("/big.txt", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		line := "a.png\t/x/\n"
		for n := 0; n <= maxBody; n += len(line) {
			fmt.Fprint(w, line)
		}
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	client := srv.Client()

	t.Run("ok", func(t *testing.T) {
		items, rejected, err := Fetch(context.Background(), client, srv.URL+"/ok.txt")
		if err != nil || len(items) != 2 || len(rejected) != 0 {
			t.Fatalf("items %+v rejected %q err %v", items, rejected, err)
		}
	})
	t.Run("404", func(t *testing.T) {
		_, _, err := Fetch(context.Background(), client, srv.URL+"/404.txt")
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		items, _, err := Fetch(context.Background(), client, srv.URL+"/big.txt")
		if err == nil || len(items) != 0 {
			t.Fatalf("items %d err %v", len(items), err)
		}
	})
	t.Run("http refused without request", func(t *testing.T) {
		before := hits.Load()
		_, _, err := Fetch(context.Background(), client, strings.Replace(srv.URL, "https://", "http://", 1)+"/ok.txt")
		if err == nil || hits.Load() != before {
			t.Fatalf("err %v hits %d->%d", err, before, hits.Load())
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := Fetch(ctx, client, srv.URL+"/ok.txt")
		if err == nil {
			t.Fatal("want error")
		}
	})
}

func TestFetchRefusesRedirectToPlainHTTP(t *testing.T) {
	plainHits := 0
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits++
		fmt.Fprint(w, "a.png\t/x/\n")
	}))
	defer plain.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/list.txt", http.StatusFound)
	})
	mux.HandleFunc("/same", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/list.txt", http.StatusFound)
	})
	mux.HandleFunc("/list.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "a.png\t/x/\n")
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	items, _, err := Fetch(context.Background(), srv.Client(), srv.URL+"/down")
	if err == nil || len(items) != 0 || plainHits != 0 {
		t.Fatalf("redirect to http: items %v, err %v, plain server hits %d; want an error and no request", items, err, plainHits)
	}

	items, _, err = Fetch(context.Background(), srv.Client(), srv.URL+"/same")
	if err != nil || len(items) != 1 {
		t.Fatalf("redirect staying on https: items %v, err %v; want 1 item", items, err)
	}
}

func TestParseStopsAtMaxItems(t *testing.T) {
	var b strings.Builder
	for i := range maxItems + 3 {
		fmt.Fprintf(&b, "f%d.png\t/x/\n", i)
	}

	items, rejected := Parse(b.String())

	if len(items) != maxItems {
		t.Fatalf("items = %d, want %d", len(items), maxItems)
	}
	if len(rejected) != 1 || !strings.Contains(rejected[0], "3 dòng") {
		t.Fatalf("rejected = %q, want one line saying 3 lines were dropped", rejected)
	}
}

func TestParseCapsRejectedLines(t *testing.T) {
	text := strings.Repeat("x\n", maxRejected+5)

	_, rejected := Parse(text)

	if len(rejected) != maxRejected+1 {
		t.Fatalf("rejected = %d entries, want %d and one summary", len(rejected), maxRejected)
	}
	if last := rejected[maxRejected]; !strings.Contains(last, "5 dòng") {
		t.Fatalf("summary = %q, want it to count the 5 lines left out", last)
	}
}

func TestParseJSONQueue(t *testing.T) {
	cases := []struct {
		name         string
		text         string
		wantItems    []Item
		wantRejected int
	}{
		{
			name: "real sample",
			text: `[{"sku":"JDOJ06-3.png","path":"\/Etsy\/Etsy-Thuy\/"},{"sku":"N-121026A94.jpg","path":"\/Etsy\/Etsy-Huyen\/"}]`,
			wantItems: []Item{
				{File: "JDOJ06-3.png", Path: "/Etsy/Etsy-Thuy/", Key: "Etsy/Etsy-Thuy/JDOJ06-3.png"},
				{File: "N-121026A94.jpg", Path: "/Etsy/Etsy-Huyen/", Key: "Etsy/Etsy-Huyen/N-121026A94.jpg"},
			},
		},
		{name: "empty queue", text: " [] \n"},
		{
			name:      "spaces and duplicates",
			text:      `[{"sku":" a.png ","path":" /x/ "},{"sku":"a.png","path":"/x/"}]`,
			wantItems: []Item{{File: "a.png", Path: "/x/", Key: "x/a.png"}},
		},
		{
			name:         "bad rows rejected, good ones kept",
			text:         `[{"sku":"","path":"/x/"},{"sku":"a.png","path":"x/"},{"path":"/x/"},{"sku":"b.png","path":"/y/"}]`,
			wantItems:    []Item{{File: "b.png", Path: "/y/", Key: "y/b.png"}},
			wantRejected: 3,
		},
		{name: "cut off", text: `[{"sku":"a.png","path":"/x/"},{"sku":`, wantRejected: 1},
		{name: "wrong shape", text: `[1,2]`, wantRejected: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, rejected := Parse(tc.text)
			if !reflect.DeepEqual(items, tc.wantItems) && !(len(items) == 0 && len(tc.wantItems) == 0) {
				t.Errorf("items %+v, want %+v", items, tc.wantItems)
			}
			if len(rejected) != tc.wantRejected {
				t.Fatalf("rejected %q, want %d", rejected, tc.wantRejected)
			}
			for _, r := range rejected {
				if !strings.Contains(r, " — ") {
					t.Errorf("rejected %q lacks reason", r)
				}
			}
		})
	}
}

func TestDefaultURLIsTheQueueAPI(t *testing.T) {
	if !strings.HasSuffix(DefaultURL, "/api/queue-s3-upload") {
		t.Fatalf("DefaultURL = %q", DefaultURL)
	}
	if got, err := ReportURL(DefaultURL); err != nil || got != "https://production.trackingis.info"+ReportPath {
		t.Fatalf("ReportURL = %q, %v", got, err)
	}
}
