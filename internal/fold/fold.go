// Package fold normalises text for search: lowercase and Vietnamese
// diacritics removed, so "Ảnh Cá" and "anh ca" match.
package fold

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

var table = func() map[rune]rune {
	groups := map[rune]string{
		'a': "àáảãạăằắẳẵặâầấẩẫậ",
		'e': "èéẻẽẹêềếểễệ",
		'i': "ìíỉĩị",
		'o': "òóỏõọôồốổỗộơờớởỡợ",
		'u': "ùúủũụưừứửữự",
		'y': "ỳýỷỹỵ",
		'd': "đ",
	}
	m := make(map[rune]rune, 80)
	for base, chars := range groups {
		for _, r := range chars {
			m[r] = base
		}
	}
	return m
}()

// combining marks used when text arrives decomposed (NFD), as macOS file
// names on some volumes do.
func isCombining(r rune) bool {
	return r >= 0x0300 && r <= 0x036F
}

// String folds s.
func String(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf || ('A' <= s[i] && s[i] <= 'Z') {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isCombining(r) {
			continue
		}
		r = unicode.ToLower(r)
		if m, ok := table[r]; ok {
			r = m
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Bytes appends the folded form of s to dst.
func Bytes(dst []byte, s string) []byte {
	return append(dst, String(s)...)
}
