package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/isuccesscorp/scan-image/internal/brokenlist"
	"github.com/isuccesscorp/scan-image/internal/search"
)

func TestMatchRowKeepsQuickHitsWhenTheIndexHasNone(t *testing.T) {
	quick := search.Item{Name: "S-121026A09.png", Path: "/v/a/S-121026A09.png", ModTime: time.Unix(10, 0)}
	r := brokenRow{Item: brokenlist.Item{File: "S-121026A09.png"}, Quick: []search.Item{quick}}

	got := matchRow(r, nil)

	if len(got.Cands) != 1 || got.Cands[0].Path != quick.Path {
		t.Fatalf("Cands = %+v, want the quick hit: a re-match must not drop it", got.Cands)
	}
}

func TestMatchRowCountsOnePathOnceAndKeepsTheIndexEntry(t *testing.T) {
	quick := search.Item{Name: "a.png", Path: "/v/a.png", Size: 1}
	index := search.Item{ID: 7, Name: "a.png", Path: "/v/a.png", Size: 2}
	newer := search.Item{ID: 8, Name: "a.png", Path: "/v/b/a.png", ModTime: time.Unix(99, 0)}
	r := brokenRow{Item: brokenlist.Item{File: "a.png"}, Quick: []search.Item{quick}}

	got := matchRow(r, []search.Item{index, newer})

	if len(got.Cands) != 2 {
		t.Fatalf("Cands = %+v, want 2: the same path found twice counts once", got.Cands)
	}
	if got.Cands[0].Path != newer.Path {
		t.Errorf("first candidate = %s, want the newest (%s)", got.Cands[0].Path, newer.Path)
	}
	if got.Cands[1].ID != index.ID {
		t.Errorf("entry for %s has ID %d, want the index entry (ID %d)", index.Path, got.Cands[1].ID, index.ID)
	}
}

func TestMatchRowKeepsTheNewestCandidatesOnly(t *testing.T) {
	var index []search.Item
	for i := range maxCandidates * 3 {
		index = append(index, search.Item{ID: uint32(i), Name: "a.png", Path: fmt.Sprintf("/v/%03d/a.png", i), ModTime: time.Unix(int64(i), 0)})
	}
	r := brokenRow{Item: brokenlist.Item{File: "a.png"}}

	got := matchRow(r, index)

	if len(got.Cands) != maxCandidates {
		t.Fatalf("Cands = %d, want %d", len(got.Cands), maxCandidates)
	}
	if want := fmt.Sprintf("/v/%03d/a.png", maxCandidates*3-1); got.Cands[0].Path != want {
		t.Errorf("first candidate = %s, want the newest (%s)", got.Cands[0].Path, want)
	}
}

func TestMatchRowLookAlikesDoNotCrowdOutAnExactHit(t *testing.T) {
	index := []search.Item{{Name: "a.png", Path: "/v/old/a.png", ModTime: time.Unix(1, 0)}}
	for i := range maxCandidates + 10 {
		index = append(index, search.Item{Name: "á.png", Path: fmt.Sprintf("/v/%03d/á.png", i), ModTime: time.Unix(int64(100+i), 0)})
	}
	r := brokenRow{Item: brokenlist.Item{File: "a.png"}}

	got := matchRow(r, index)

	if len(got.Cands) != 1 || got.Cands[0].Path != "/v/old/a.png" {
		t.Errorf("Cands = %+v, want the exact hit", got.Cands)
	}
	if len(got.Near) != maxCandidates {
		t.Errorf("Near = %d, want %d", len(got.Near), maxCandidates)
	}
}
