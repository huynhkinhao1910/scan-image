package main

import (
	"slices"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/brokenlist"
	"github.com/isuccesscorp/scan-image/internal/search"
)

func autoRow(key string, cands ...search.Item) brokenRow {
	return brokenRow{Item: brokenlist.Item{File: "a.png", Key: key}, Cands: cands}
}

func TestAutoRowsPushesExactMatchesNotPushedBefore(t *testing.T) {
	one := cand("/v/a.png", 10)
	rows := []brokenRow{
		autoRow("ready"),
		autoRow("ready", one),
		autoRow("several", one, cand("/v/b/a.png", 5)),
		{Item: brokenlist.Item{Key: "lookalike"}, Near: []search.Item{one}},
		autoRow("pushed", one),
		autoRow("pushed-failed", one),
		autoRow("pushed-older-source", cand("/v/a.png", 99)),
	}
	rows[0].Item.Key = "missing"
	pushed := map[string]pushedSource{
		"pushed":              {sig: sourceSig(one), job: &uploadJob{State: upDone}},
		"pushed-failed":       {sig: sourceSig(one), job: &uploadJob{State: upFailed}},
		"pushed-older-source": {sig: sourceSig(one), job: &uploadJob{State: upDone}},
	}

	idx, repeated := autoRows(rows, pushed)

	if want := []int{1, 2, 5, 6}; !slices.Equal(idx, want) {
		t.Errorf("rows to push = %v, want %v: exact names (the newest of several), and not the same source twice", idx, want)
	}
	if repeated != 1 {
		t.Errorf("repeated = %d, want 1 (the row the server still lists after its push)", repeated)
	}
}

func TestRowsBusyWhileUploadsRunOrWaitForTheReport(t *testing.T) {
	done := &uploadJob{State: upDone}
	tests := []struct {
		name string
		row  brokenRow
		busy bool
	}{
		{"no job", brokenRow{}, false},
		{"queued", brokenRow{Job: &uploadJob{State: upQueued}}, true},
		{"running", brokenRow{Job: &uploadJob{State: upRunning}}, true},
		{"done, not reported", brokenRow{Job: done}, true},
		{"done and reported", brokenRow{Job: done, Reported: done}, false},
		{"failed", brokenRow{Job: &uploadJob{State: upFailed}}, false},
	}
	for _, tc := range tests {
		if got := rowsBusy([]brokenRow{{}, tc.row}); got != tc.busy {
			t.Errorf("%s: rowsBusy = %v, want %v", tc.name, got, tc.busy)
		}
	}
}

func TestAutoMessage(t *testing.T) {
	at := time.Date(2026, 10, 13, 14, 5, 0, 0, time.Local)
	rows := []brokenRow{
		autoRow("missing"),
		autoRow("several", cand("/v/a.png", 1), cand("/v/b/a.png", 2)),
		{Item: brokenlist.Item{Key: "lookalike"}, Near: []search.Item{cand("/v/A.png", 1)}},
	}
	got := autoMessage(at, 3, 1, rows)
	want := "Tự động 14:05: đẩy 3 file, 1 chưa tìm thấy, 1 cần xác nhận, bỏ qua 1 file đã đẩy mà server vẫn liệt kê"
	if got != want {
		t.Errorf("autoMessage = %q\nwant        %q", got, want)
	}
	if got, want := autoMessage(at, 0, 0, nil), "Tự động 14:05: không có file nào cần đẩy"; got != want {
		t.Errorf("autoMessage = %q, want %q", got, want)
	}
}

func TestAutoMinutesBounds(t *testing.T) {
	for in, want := range map[int]int{0: 1, -3: 1, 1: 1, 7: 7, 60: 60, 500: 60} {
		if got := autoMinutes(in); got != want {
			t.Errorf("autoMinutes(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestNeedWalkSkipsNamesLookedForRecently(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	walked := map[string]time.Time{"a.png": now.Add(-time.Minute), "b.png": now.Add(-autoRewalk)}
	if needWalk([]string{"a.png"}, walked, now) {
		t.Error("a name walked a minute ago must not walk again")
	}
	if !needWalk([]string{"a.png", "b.png"}, walked, now) {
		t.Error("a name walked autoRewalk ago must walk again")
	}
	if !needWalk([]string{"a.png", "new.png"}, walked, now) {
		t.Error("a new name must walk at once")
	}
	if needWalk(nil, walked, now) {
		t.Error("no names, no walk")
	}
}
