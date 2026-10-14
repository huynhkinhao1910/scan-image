package datefind

import "testing"

func TestParseDate(t *testing.T) {
	tests := []struct {
		name string
		want Date
		ok   bool
	}{
		{"S-121026A09.png", Date{12, 10, 2026}, true},
		{"A-091026A02.png", Date{9, 10, 2026}, true},
		{"OGWG04-16.png", Date{}, false},
		{"D1JM07-01.png", Date{}, false},
		{"X-320126.png", Date{}, false},
		{"X-121326.png", Date{}, false},
		{"X-1210262.png", Date{12, 10, 2026}, true},
		{"X-12102026.png", Date{12, 10, 2026}, true},
		{"M15-05092610.png", Date{5, 9, 2026}, true},
		{"A13-31072651.png", Date{31, 7, 2026}, true},
		{"A24-17082607.png", Date{17, 8, 2026}, true},
		{"X-123456789.png", Date{}, false},
		{"X-32012610.png", Date{}, false},
		{"X-300226.png", Date{}, false},
		{"X-290224.png", Date{29, 2, 2024}, true},
		{"X-320126-050326.png", Date{5, 3, 2026}, true},
		{"X-121026-050326.png", Date{12, 10, 2026}, true},
		{"no digits.png", Date{}, false},
	}
	for _, tc := range tests {
		got, ok := ParseDate(tc.name)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseDate(%q) = %v,%v want %v,%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFolderRules(t *testing.T) {
	tests := []struct {
		kind string
		name string
		d    Date
		want bool
	}{
		{"year", "2025", Date{1, 1, 2025}, true},
		{"year", "Giang.NTH 2026", Date{1, 1, 2026}, true},
		{"year", "2025", Date{1, 1, 2026}, false},
		{"year", "2025-26", Date{1, 1, 2025}, false},
		{"year", "25", Date{1, 1, 2025}, false},

		{"month", "Thang 11", Date{17, 11, 2025}, true},
		{"month", "Tháng 3", Date{11, 3, 2025}, true},
		{"month", "Tháng 3", Date{11, 3, 2025}, true},
		{"month", "thang 2", Date{9, 2, 2026}, true},
		{"month", "7", Date{9, 7, 2025}, true},
		{"month", "07", Date{9, 7, 2025}, true},
		{"month", "11-2025", Date{17, 11, 2025}, true},
		{"month", "11-25", Date{17, 11, 2025}, true},
		{"month", "T11", Date{17, 11, 2025}, true},
		{"month", "Thang 11", Date{17, 10, 2025}, false},
		{"month", "11-2024", Date{17, 11, 2025}, false},
		{"month", "Mockup 11", Date{17, 11, 2025}, false},
		{"month", "11-3", Date{11, 3, 2025}, false},

		{"day", "17", Date{17, 11, 2025}, true},
		{"day", "11-3", Date{11, 3, 2025}, true},
		{"day", "9-2", Date{9, 2, 2026}, true},
		{"day", "17-11-2025", Date{17, 11, 2025}, true},
		{"day", "17-11-25", Date{17, 11, 2025}, true},
		{"day", "Ngay 17", Date{17, 11, 2025}, true},
		{"day", "17", Date{18, 11, 2025}, false},
		{"day", "17-12", Date{17, 11, 2025}, false},
		{"day", "17-11-2024", Date{17, 11, 2025}, false},
		{"day", "Mockup 12", Date{12, 11, 2025}, false},

		{"daymonth", "11-3", Date{11, 3, 2025}, true},
		{"daymonth", "11-3-25", Date{11, 3, 2025}, true},
		{"daymonth", "11", Date{11, 3, 2025}, false},
		{"daymonth", "Mockup 11-3", Date{11, 3, 2025}, false},
	}
	for _, tc := range tests {
		f := parseFolder(tc.name)
		var got bool
		switch tc.kind {
		case "year":
			got = f.isYear(tc.d.Year)
		case "month":
			got = f.isMonth(tc.d.Month, tc.d.Year)
		case "day":
			got = f.isDay(tc.d.Day, tc.d.Month, tc.d.Year)
		case "daymonth":
			got = f.isDayMonth(tc.d.Day, tc.d.Month, tc.d.Year)
		}
		if got != tc.want {
			t.Errorf("%s(%q, %v) = %v want %v", tc.kind, tc.name, tc.d, got, tc.want)
		}
	}
}

func TestParseFolder(t *testing.T) {
	f := parseFolder("Tháng 3")
	if f.letters != "thang" || !numsEq(f.nums, 3) {
		t.Fatalf("got %+v", f)
	}
	f = parseFolder("11-3")
	if f.letters != "" || !numsEq(f.nums, 11, 3) {
		t.Fatalf("got %+v", f)
	}
	if !parseFolder("1900").looksYear() || parseFolder("2100").looksYear() || parseFolder("12").looksYear() {
		t.Fatal("looksYear")
	}
}
