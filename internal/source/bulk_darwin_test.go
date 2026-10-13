//go:build darwin

package source

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const bulkBigDirEntries = 4000

func mustWrite(tb testing.TB, path string, size int) {
	tb.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		tb.Fatal(err)
	}
}

func mustMkdir(tb testing.TB, path string) {
	tb.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		tb.Fatal(err)
	}
}

// makeBigDir fills dir with n files with long names, so that listing it takes
// several getattrlistbulk calls.
func makeBigDir(tb testing.TB, dir string, n int) {
	tb.Helper()
	pad := strings.Repeat("x", 90)
	for i := 0; i < n; i++ {
		mustWrite(tb, filepath.Join(dir, fmt.Sprintf("IMG_%06d_%s.jpg", i, pad)), i%7)
	}
}

func sortedByName(es []Entry) []Entry {
	out := append([]Entry(nil), es...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func assertSameListing(t *testing.T, dir string, skipHidden bool) []Entry {
	t.Helper()
	ctx := context.Background()
	want, err := LocalFS{SkipHidden: skipHidden}.ReadDir(ctx, dir)
	if err != nil {
		t.Fatalf("readdir lister: %v", err)
	}
	got, err := LocalFS{SkipHidden: skipHidden, Bulk: true}.ReadDir(ctx, dir)
	if err != nil {
		t.Fatalf("bulk lister: %v", err)
	}
	want, got = sortedByName(want), sortedByName(got)
	if len(got) != len(want) {
		t.Fatalf("bulk returned %d entries, readdir %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.Name != g.Name {
			t.Fatalf("entry %d name %q (% x), want %q (% x)", i, g.Name, []byte(g.Name), w.Name, []byte(w.Name))
		}
		if g.IsDir != w.IsDir {
			t.Errorf("%q IsDir %v, want %v", w.Name, g.IsDir, w.IsDir)
		}
		if g.Size != w.Size {
			t.Errorf("%q Size %d, want %d", w.Name, g.Size, w.Size)
		}
		if g.ModTime.Unix() != w.ModTime.Unix() {
			t.Errorf("%q ModTime %v, want %v", w.Name, g.ModTime, w.ModTime)
		}
	}
	return got
}

func TestBulkListsSameEntriesAsReaddirOnMixedTree(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "empty.jpg"), 0)
	mustWrite(t, filepath.Join(dir, "small.jpg"), 17)
	mustWrite(t, filepath.Join(dir, "large.jpg"), 300_000)
	mustMkdir(t, filepath.Join(dir, "sub", "deeper"))
	mustMkdir(t, filepath.Join(dir, "emptydir"))
	mustWrite(t, filepath.Join(dir, "sub", "inner.jpg"), 5)

	nfc := "Ảnh Việt Nam đẹp.jpg"
	nfd := "Ảnh Việt Nam dđẹp.jpg"
	mustWrite(t, filepath.Join(dir, nfc), 11)
	mustWrite(t, filepath.Join(dir, nfd), 13)
	mustWrite(t, filepath.Join(dir, "Tiếng Việt.JPG"), 1)

	if err := os.Symlink("small.jpg", filepath.Join(dir, "link-to-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(dir, "link-to-dir")); err != nil {
		t.Fatal(err)
	}

	got := assertSameListing(t, dir, false)

	byName := map[string]Entry{}
	for _, e := range got {
		byName[e.Name] = e
	}
	for _, n := range []string{"link-to-file", "link-to-dir"} {
		e, ok := byName[n]
		if !ok {
			t.Fatalf("symlink %q missing from bulk listing", n)
		}
		if e.IsDir {
			t.Errorf("symlink %q reported as directory", n)
		}
	}
	if !byName["sub"].IsDir || !byName["emptydir"].IsDir {
		t.Errorf("real directories not flagged IsDir: sub=%v emptydir=%v", byName["sub"].IsDir, byName["emptydir"].IsDir)
	}
	if byName["large.jpg"].Size != 300_000 {
		t.Errorf("large.jpg size %d, want 300000", byName["large.jpg"].Size)
	}
}

func TestBulkKeepsNFDAndNFCNamesByteForByte(t *testing.T) {
	dir := t.TempDir()
	nfd := "Ảnh Việt.jpg"
	mustWrite(t, filepath.Join(dir, nfd), 1)

	got, err := LocalFS{Bulk: true}.ReadDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want, err := LocalFS{}.ReadDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(want) != 1 {
		t.Fatalf("got %d / want %d entries, need 1", len(got), len(want))
	}
	if got[0].Name != want[0].Name {
		t.Fatalf("name bytes % x, readdir gives % x", []byte(got[0].Name), []byte(want[0].Name))
	}
}

func TestBulkReturnsZeroEntriesForEmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	got, err := LocalFS{Bulk: true}.ReadDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("ReadDir empty dir: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d entries, want 0: %+v", len(got), got)
	}
}

func TestBulkListsDirectoryNeedingSeveralSyscalls(t *testing.T) {
	dir := t.TempDir()
	makeBigDir(t, dir, bulkBigDirEntries)

	got := assertSameListing(t, dir, false)

	if len(got) != bulkBigDirEntries {
		t.Fatalf("got %d entries, want %d", len(got), bulkBigDirEntries)
	}
}

func TestBulkSkipHiddenMatchesReaddirLister(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.jpg"), 1)
	mustWrite(t, filepath.Join(dir, ".hidden.jpg"), 2)
	mustWrite(t, filepath.Join(dir, ".DS_Store"), 3)
	mustWrite(t, filepath.Join(dir, "._a.jpg"), 4)
	mustMkdir(t, filepath.Join(dir, ".Trashes"))
	mustMkdir(t, filepath.Join(dir, "visible"))

	got := assertSameListing(t, dir, true)

	for _, e := range got {
		if strings.HasPrefix(e.Name, ".") {
			t.Errorf("hidden entry %q survived SkipHidden", e.Name)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want a.jpg and visible: %+v", len(got), got)
	}
}

func TestBulkKeepsHiddenWhenSkipHiddenIsOff(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.jpg"), 1)
	mustWrite(t, filepath.Join(dir, ".hidden.jpg"), 2)

	got, err := LocalFS{Bulk: true}.ReadDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
}

func TestBulkReturnsErrorWhenPathDoesNotExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")

	got, err := LocalFS{Bulk: true}.ReadDir(context.Background(), missing)

	if err == nil {
		t.Fatalf("ReadDir of missing path succeeded with %d entries", len(got))
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want errors.Is os.ErrNotExist", err)
	}
	if got != nil {
		t.Errorf("entries returned alongside error: %+v", got)
	}
}

func TestBulkReturnsErrorWhenPathIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f.jpg")
	mustWrite(t, file, 1)

	got, err := LocalFS{Bulk: true}.ReadDir(context.Background(), file)

	if err == nil {
		t.Fatalf("ReadDir of a file succeeded with %d entries", len(got))
	}
}

func TestBulkReturnsContextErrorWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	makeBigDir(t, dir, 50)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := LocalFS{Bulk: true}.ReadDir(ctx, dir)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got != nil {
		t.Errorf("entries returned alongside error: %d", len(got))
	}
}

func TestBulkReportsExplicitMtimeOfFileAndDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "photo.jpg")
	sub := filepath.Join(dir, "album")
	mustWrite(t, file, 9)
	mustMkdir(t, sub)
	fileTime := time.Unix(1_600_000_123, 0)
	dirTime := time.Unix(1_500_000_456, 0)
	if err := os.Chtimes(file, fileTime, fileTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sub, dirTime, dirTime); err != nil {
		t.Fatal(err)
	}

	got, err := LocalFS{Bulk: true}.ReadDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]Entry{}
	for _, e := range got {
		byName[e.Name] = e
	}
	if g := byName["photo.jpg"].ModTime.Unix(); g != fileTime.Unix() {
		t.Errorf("file mtime %d, want %d", g, fileTime.Unix())
	}
	if g := byName["album"].ModTime.Unix(); g != dirTime.Unix() {
		t.Errorf("dir mtime %d, want %d", g, dirTime.Unix())
	}
	if byName["photo.jpg"].Size != 9 {
		t.Errorf("file size %d, want 9", byName["photo.jpg"].Size)
	}
}

func benchmarkReadDir(b *testing.B, l LocalFS) {
	dir := b.TempDir()
	makeBigDir(b, dir, bulkBigDirEntries)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		es, err := l.ReadDir(ctx, dir)
		if err != nil {
			b.Fatal(err)
		}
		if len(es) != bulkBigDirEntries {
			b.Fatalf("got %d entries, want %d", len(es), bulkBigDirEntries)
		}
	}
}

func BenchmarkReadDirBulk(b *testing.B)    { benchmarkReadDir(b, LocalFS{Bulk: true}) }
func BenchmarkReadDirReaddir(b *testing.B) { benchmarkReadDir(b, LocalFS{}) }

// bulkRecord hand-builds one getattrlistbulk record whose returned bitmap is
// common (attrCmnName, attrCmnObjType and attrCmnModTime payloads are written
// for the bits set).
func bulkRecord(common uint32, name string, objType uint32, mtime int64) []byte {
	le := binary.LittleEndian
	rec := make([]byte, recordHeaderLen)
	le.PutUint32(rec[4:], common)
	ref := len(rec)
	if common&attrCmnName != 0 {
		rec = append(rec, make([]byte, 8)...)
	}
	if common&attrCmnObjType != 0 {
		rec = le.AppendUint32(rec, objType)
	}
	if common&attrCmnModTime != 0 {
		rec = le.AppendUint64(rec, uint64(mtime))
		rec = le.AppendUint64(rec, 0)
	}
	if common&attrCmnName != 0 {
		le.PutUint32(rec[ref:], uint32(len(rec)-ref))
		le.PutUint32(rec[ref+4:], uint32(len(name)+1))
		rec = append(rec, name...)
		rec = append(rec, 0)
	}
	le.PutUint32(rec, uint32(len(rec)))
	return rec
}

func TestParseBulkDecodesCompleteRecord(t *testing.T) {
	all := uint32(attrCmnName | attrCmnObjType | attrCmnModTime)
	buf := bulkRecord(all, "pics", vDir, 1_700_000_000)

	got, err := parseBulk(nil, buf, 1, false)

	if err != nil {
		t.Fatalf("parseBulk: %v", err)
	}
	if len(got) != 1 || got[0].Name != "pics" || !got[0].IsDir || got[0].ModTime.Unix() != 1_700_000_000 {
		t.Fatalf("entries %+v", got)
	}
}

func TestParseBulkFailsWhenReturnedBitmapLacksNameTypeOrMtime(t *testing.T) {
	all := uint32(attrCmnName | attrCmnObjType | attrCmnModTime)
	for name, missing := range map[string]uint32{
		"name": attrCmnName, "object type": attrCmnObjType, "modification time": attrCmnModTime,
	} {
		t.Run(name, func(t *testing.T) {
			buf := bulkRecord(all&^missing, "pics", vDir, 1_700_000_000)

			got, err := parseBulk(nil, buf, 1, false)

			if err == nil {
				t.Fatalf("parseBulk invented a value for the missing %s: %+v", name, got)
			}
		})
	}
}
