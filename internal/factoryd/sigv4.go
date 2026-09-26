package factoryd

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// signer produces AWS Signature Version 4 Authorization headers for S3
// requests, stdlib only. Verified against the vectors in the AWS S3 SigV4
// walkthrough (see sigv4_test.go).
type signer struct {
	AccessKey string
	SecretKey string
	Region    string
}

type putRequest struct {
	Method  string
	Path    string      // absolute path, '/' not encoded
	Headers [][2]string // request headers; Host required
	Body    []byte      // signed payload
	AmzDate time.Time   // request time, truncated to the second
}

// sign returns the Authorization header value for req. The payload hash is
// signed (not UNSIGNED-PAYLOAD) so the stub and the object store can both
// verify the body against x-amz-content-sha256.
func (s signer) sign(req putRequest) string {
	payloadHash := sha256Hex(req.Body)
	amzDate := req.AmzDate.UTC().Format("20060102T150405Z")
	scope := req.AmzDate.UTC().Format("20060102") + "/" + s.Region + "/s3/aws4_request"

	headers := make([][2]string, 0, len(req.Headers)+2)
	headers = append(headers, req.Headers...)
	headers = append(headers, [2]string{"x-amz-content-sha256", payloadHash}, [2]string{"x-amz-date", amzDate})
	names := make([]string, len(headers))
	canonical := make([]string, len(headers))
	for i, h := range headers {
		names[i] = strings.ToLower(h[0])
		canonical[i] = names[i] + ":" + strings.TrimSpace(h[1])
	}
	sort.Strings(names)
	sort.Strings(canonical)

	canonicalRequest := strings.Join([]string{
		req.Method,
		uriEncode(req.Path),
		"",                                   // no query string on object PUTs
		strings.Join(canonical, "\n") + "\n", // each canonical header ends with \n
		strings.Join(names, ";"),
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	date := req.AmzDate.UTC().Format("20060102")
	key := hmacSHA256([]byte("AWS4"+s.SecretKey), date)
	key = hmacSHA256(key, s.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s,SignedHeaders=%s,Signature=%s",
		s.AccessKey, scope, strings.Join(names, ";"), signature)
}

// uriEncode encodes every byte except unreserved characters and '/', per the
// SigV4 canonicalization rules (space is %20, hex digits uppercase).
func uriEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xf])
		}
	}
	return b.String()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
