package fold

import "testing"

func TestString(t *testing.T) {
	cases := map[string]string{
		"Ảnh Cá Hồi.JPG":      "anh ca hoi.jpg",
		"ĐÀ NẴNG":             "da nang",
		"plain.png":           "plain.png",
		"Ánh":                "anh",
		"Nguyễn Thị Ngọc Ánh": "nguyen thi ngoc anh",
	}
	for in, want := range cases {
		if got := String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}
