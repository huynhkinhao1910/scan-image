package s3up

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	exAccess = "AKIAIOSFODNN7EXAMPLE"
	exSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var exDate = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

func sigOf(h string) string {
	i := strings.Index(h, "Signature=")
	return h[i+len("Signature="):]
}

// Vectors from the AWS S3 documentation, "Signature Calculations for the
// Authorization Header".
func TestSignGetObjectVector(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	signV4(req, exAccess, exSecret, "us-east-1", emptySHA256, exDate)
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date") {
		t.Errorf("signed headers: %s", auth)
	}
	if got, want := sigOf(auth), "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"; got != want {
		t.Errorf("signature %s, want %s", got, want)
	}
}

func TestSignListObjectsVector(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", nil)
	signV4(req, exAccess, exSecret, "us-east-1", emptySHA256, exDate)
	if got, want := sigOf(req.Header.Get("Authorization")), "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"; got != want {
		t.Errorf("signature %s, want %s", got, want)
	}
}

func TestEncodePath(t *testing.T) {
	if got := encodePath("/Ảnh Cá/a+b(1).jpg"); got != "/%E1%BA%A2nh%20C%C3%A1/a%2Bb%281%29.jpg" {
		t.Errorf("got %s", got)
	}
}
