package factoryd

import (
	"testing"
	"time"
)

// The signer is verified against the PUT Object vector published in the AWS
// S3 SigV4 walkthrough (docs.aws.amazon.com, "Example: PUT Object"): the
// signature must match byte for byte.
func TestSignAWSPutObjectVector(t *testing.T) {
	s := signer{
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:    "us-east-1",
	}
	body := []byte("Welcome to Amazon S3.")
	got := s.sign(putRequest{
		Method: "PUT",
		Path:   "/test$file.text", // the signer UriEncodes everything but '/'
		Headers: [][2]string{
			{"Host", "examplebucket.s3.amazonaws.com"},
			{"Date", "Fri, 24 May 2013 00:00:00 GMT"},
			{"x-amz-storage-class", "REDUCED_REDUNDANCY"},
		},
		Body:    body,
		AmzDate: time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC),
	})
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
		"SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class," +
		"Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	if got != want {
		t.Fatalf("authorization =\n%s\nwant\n%s", got, want)
	}
}

func TestSignEncodesObjectKeyButNotSlashes(t *testing.T) {
	if got, want := uriEncode("sessions/t1/x y$.jsonl"), "sessions/t1/x%20y%24.jsonl"; got != want {
		t.Fatalf("uriEncode = %q, want %q", got, want)
	}
}
