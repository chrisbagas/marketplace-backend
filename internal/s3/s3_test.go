package s3

import (
	"strings"
	"testing"
	"time"
)

// Example from the AWS docs ("Authenticating Requests: Using Query Parameters"):
// https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html
func TestPresignMatchesAWSExample(t *testing.T) {
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	got := presignURL("GET", "https", "examplebucket.s3.amazonaws.com", "/test.txt", nil,
		"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", 86400*time.Second, now)
	const want = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.HasSuffix(got, "X-Amz-Signature="+want) {
		t.Errorf("signature mismatch:\n%s", got)
	}
}

func TestKeyURLRoundTrip(t *testing.T) {
	c, err := New(Config{Endpoint: "http://localhost:9000", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", PublicURL: "https://files.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	u := c.PublicURL("designs/x.png")
	if u != "https://files.example.com/designs/x.png" {
		t.Errorf("url %q", u)
	}
	if k, ok := c.KeyFromURL(u); !ok || k != "designs/x.png" {
		t.Errorf("KeyFromURL = %q %v", k, ok)
	}
	if _, ok := c.KeyFromURL("https://other.example.com/designs/x.png"); ok {
		t.Error("foreign URL accepted")
	}
}
