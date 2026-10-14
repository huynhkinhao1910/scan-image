//go:build darwin

package source

import (
	"context"
	"encoding/binary"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

const bulkSupported = true

const (
	bulkInitialBuf = 256 << 10
	bulkMaxBuf     = 16 << 20

	attrBitMapCount = 5

	attrCmnName          = 0x00000001
	attrCmnObjType       = 0x00000008
	attrCmnModTime       = 0x00000400
	attrCmnError         = 0x20000000
	attrCmnReturnedAttrs = 0x80000000
	attrFileDataLength   = 0x00000200

	fsoptNoFollow = 0x1

	vDir = 2

	// bulkRequired must all be present in a record's returned bitmap: a
	// missing one would decode as a zero value (a directory read as a file,
	// an mtime of 0) and look valid.
	bulkRequired = attrCmnName | attrCmnObjType | attrCmnModTime

	returnedAttrsLen = 20
	recordHeaderLen  = 4 + returnedAttrsLen
	timespecLen      = 16
)

// attrList mirrors struct attrlist; it is a package variable so its address
// stays valid for the raw call.
var attrList = struct {
	bitmapCount uint16
	reserved    uint16
	common      uint32
	vol         uint32
	dir         uint32
	file        uint32
	fork        uint32
}{
	bitmapCount: attrBitMapCount,
	common:      attrCmnReturnedAttrs | attrCmnName | attrCmnError | attrCmnObjType | attrCmnModTime,
	file:        attrFileDataLength,
}

var bulkFn = sync.OnceValues(func() (uintptr, error) {
	return purego.Dlsym(purego.RTLD_DEFAULT, "getattrlistbulk")
})

// listBulk lists path with getattrlistbulk(2): name, type, mtime and size come
// back in batches, without a stat per entry. Entries are sorted by name, as
// os.ReadDir sorts them, so an unchanged directory lists identically each time.
func listBulk(ctx context.Context, path string, skipHidden bool) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fn, err := bulkFn()
	if err != nil {
		return nil, &os.PathError{Op: "getattrlistbulk", Path: path, Err: err}
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer syscall.Close(fd)

	var out []Entry
	buf := make([]byte, bulkInitialBuf)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r1, _, errno := purego.SyscallN(fn, uintptr(fd), uintptr(unsafe.Pointer(&attrList)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), fsoptNoFollow)
		if int32(r1) == -1 {
			if syscall.Errno(errno) == syscall.ERANGE && len(buf) < bulkMaxBuf {
				buf = make([]byte, len(buf)*2)
				continue
			}
			return nil, &os.PathError{Op: "getattrlistbulk", Path: path, Err: syscall.Errno(errno)}
		}
		count := int(int32(r1))
		if count == 0 {
			slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
			return out, nil
		}
		out, err = parseBulk(out, buf, count, skipHidden)
		if err != nil {
			return nil, &os.PathError{Op: "getattrlistbulk", Path: path, Err: err}
		}
	}
}

// parseBulk appends the count records in buf to out. Fields are decoded by
// offset from the returned bitmap; each record is skipped by its own length.
func parseBulk(out []Entry, buf []byte, count int, skipHidden bool) ([]Entry, error) {
	le := binary.LittleEndian
	pos := 0
	for i := 0; i < count; i++ {
		if pos+4 > len(buf) {
			return nil, syscall.EINVAL
		}
		length := int(le.Uint32(buf[pos:]))
		if length < recordHeaderLen || pos+length > len(buf) {
			return nil, syscall.EINVAL
		}
		rec := buf[pos : pos+length]
		pos += length

		common, file := le.Uint32(rec[4:]), le.Uint32(rec[16:])
		off := recordHeaderLen
		need := func(n int) bool { return off+n <= len(rec) }

		if common&attrCmnError != 0 {
			if !need(4) {
				return nil, syscall.EINVAL
			}
			if e := le.Uint32(rec[off:]); e != 0 {
				return nil, syscall.Errno(e)
			}
			off += 4
		}

		if common&bulkRequired != bulkRequired {
			return nil, syscall.EINVAL
		}

		var e Entry
		if common&attrCmnName != 0 {
			if !need(8) {
				return nil, syscall.EINVAL
			}
			ref := off
			nameOff := int(int32(le.Uint32(rec[ref:])))
			nameLen := int(le.Uint32(rec[ref+4:]))
			start := ref + nameOff
			if nameOff < 0 || nameLen < 1 || start < 0 || start+nameLen > len(rec) {
				return nil, syscall.EINVAL
			}
			e.Name = strings.TrimRight(string(rec[start:start+nameLen]), "\x00")
			off += 8
		}
		if common&attrCmnObjType != 0 {
			if !need(4) {
				return nil, syscall.EINVAL
			}
			e.IsDir = le.Uint32(rec[off:]) == vDir
			off += 4
		}
		if common&attrCmnModTime != 0 {
			if !need(timespecLen) {
				return nil, syscall.EINVAL
			}
			e.ModTime = time.Unix(int64(le.Uint64(rec[off:])), int64(le.Uint64(rec[off+8:])))
			off += timespecLen
		}
		if file&attrFileDataLength != 0 {
			if !need(8) {
				return nil, syscall.EINVAL
			}
			e.Size = int64(le.Uint64(rec[off:]))
		}

		if e.Name == "" || (skipHidden && strings.HasPrefix(e.Name, ".")) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
