// Package datefind finds files by the date encoded in their names, walking
// only the folders whose names can lead to that date instead of indexing the
// whole share.
package datefind

import (
	"time"
	"unicode"

	"github.com/isuccesscorp/scan-image/internal/fold"
)

// Date is a calendar day taken from a file name.
type Date struct{ Day, Month, Year int }

const (
	dateDigits     = 6
	longDateDigits = 8
)

// ParseDate finds the date in name: the first run of six to eight digits
// that starts with a valid DDMMYY (YY becomes 2000+YY). Digits after the six
// are a sequence number, as in "M15-05092610", unless the run is DDMMYYYY.
func ParseDate(name string) (Date, bool) {
	for i := 0; i < len(name); {
		if !isDigit(name[i]) {
			i++
			continue
		}
		j := i
		for j < len(name) && isDigit(name[j]) {
			j++
		}
		if d, ok := runDate(name[i:j]); ok {
			return d, true
		}
		i = j
	}
	return Date{}, false
}

func runDate(r string) (Date, bool) {
	if len(r) < dateDigits || len(r) > longDateDigits {
		return Date{}, false
	}
	two := func(i int) int { return int(r[i]-'0')*10 + int(r[i+1]-'0') }
	d := Date{Day: two(0), Month: two(2), Year: 2000 + two(4)}
	if len(r) == longDateDigits {
		if y := two(4)*100 + two(6); y >= 2000 && y <= yearHi {
			d.Year = y
		}
	}
	return d, d.valid()
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func (d Date) valid() bool {
	if d.Month < 1 || d.Month > 12 || d.Day < 1 {
		return false
	}
	return time.Date(d.Year, time.Month(d.Month), d.Day, 0, 0, 0, 0, time.UTC).Day() == d.Day
}

// folder is a folder name reduced to what the date rules look at.
type folder struct {
	nums    []int
	letters string
}

const (
	maxNumDigits = 9
	yearLo       = 1900
	yearHi       = 2099
)

// parseFolder takes the digit runs of name as integers (-1 when too long to
// be a date part) and its folded letters, without digits and separators.
func parseFolder(name string) folder {
	var f folder
	var letters []rune
	run, digits := 0, 0
	flush := func() {
		if digits == 0 {
			return
		}
		if digits > maxNumDigits {
			run = -1
		}
		f.nums = append(f.nums, run)
		run, digits = 0, 0
	}
	for _, r := range fold.String(name) {
		switch {
		case r >= '0' && r <= '9':
			run = run*10 + int(r-'0')
			digits++
		case unicode.IsLetter(r):
			flush()
			letters = append(letters, r)
		default:
			flush()
		}
	}
	flush()
	f.letters = string(letters)
	return f
}

func numsEq(n []int, want ...int) bool {
	if len(n) != len(want) {
		return false
	}
	for i, v := range want {
		if n[i] != v {
			return false
		}
	}
	return true
}

func oneOf(s string, set ...string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}

// looksYear reports whether f is a lone four-digit 19xx/20xx number.
func (f folder) looksYear() bool {
	return len(f.nums) == 1 && f.nums[0] >= yearLo && f.nums[0] <= yearHi
}

// pureYear reports whether f is nothing but a year ("2026"). Only such a
// folder marks its parent as organised by year; a labelled one, like the
// designer folder "Giang.NTH 2026", sits beside other designers' folders.
func (f folder) pureYear() bool { return f.looksYear() && f.letters == "" }

// isYear reports whether f is the year folder for y; letters are free, so
// "Giang.NTH 2026" counts.
func (f folder) isYear(y int) bool { return numsEq(f.nums, y) }

// isMonth reports whether f is the month folder for month m of year y.
func (f folder) isMonth(m, y int) bool {
	if !oneOf(f.letters, "", "thang", "t", "month", "m") {
		return false
	}
	return numsEq(f.nums, m) || numsEq(f.nums, m, y) || numsEq(f.nums, m, y%100)
}

// isDayMonth reports whether f names day d of month m, so it can sit right
// under a year folder: [D, M], [D, M, Y] or [D, M, YY], or the same written
// as one number (DDMM, DDMMYY, DDMMYYYY).
func (f folder) isDayMonth(d, m, y int) bool {
	if !oneOf(f.letters, "", "ngay", "day") {
		return false
	}
	ddmm := d*100 + m
	return numsEq(f.nums, d, m) || numsEq(f.nums, d, m, y) || numsEq(f.nums, d, m, y%100) ||
		numsEq(f.nums, ddmm) || numsEq(f.nums, ddmm*100+y%100) || numsEq(f.nums, ddmm*10000+y)
}

// isDay reports whether f is the day folder for d inside month m of year y.
func (f folder) isDay(d, m, y int) bool {
	if !oneOf(f.letters, "", "ngay", "day") {
		return false
	}
	return numsEq(f.nums, d) || f.isDayMonth(d, m, y)
}
