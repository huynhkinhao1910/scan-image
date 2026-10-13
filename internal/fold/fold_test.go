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

var appendInputs = []string{
	"img_1234.jpg",
	"IMG_1234.JPG",
	"MiXeD Case.Png",
	"",
	"Ảnh Cá Hồi.JPG",
	"ĐÀ NẴNG",
	"Nguyễn Thị Ngọc Ánh",
	"Ảnh Cá Hồi.JPG",
	"ế.txt",
	"bad\xffbyte.JPG",
}

func TestAppendMatchesString(t *testing.T) {
	for _, in := range appendInputs {
		name := []byte(in)
		got := Append(nil, name)
		if want := String(in); string(got) != want {
			t.Errorf("Append(%q) = %q, want %q", in, got, want)
		}
		if string(name) != in {
			t.Errorf("Append modified its input: %q -> %q", in, name)
		}
	}
}

func TestAppendKeepsExistingPrefix(t *testing.T) {
	for _, in := range appendInputs {
		dst := []byte("prefix|")
		got := Append(dst, []byte(in))
		if want := "prefix|" + String(in); string(got) != want {
			t.Errorf("Append(prefix, %q) = %q, want %q", in, got, want)
		}
	}
}

var appendSink []byte

func TestAppendASCIIDoesNotAllocate(t *testing.T) {
	for _, in := range []string{"IMG_1234.JPG", "img_1234.jpg", "Mixed Case 01.Png"} {
		name := []byte(in)
		dst := make([]byte, 0, 64)
		allocs := testing.AllocsPerRun(100, func() {
			appendSink = Append(dst[:0], name)
		})
		if allocs != 0 {
			t.Errorf("Append(%q) allocates %v times, want 0", in, allocs)
		}
	}
}
