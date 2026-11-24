package bump

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Result is what a bump hands back from its pod: the files it changed, by
// path, and its report.
type Result struct {
	Files  map[string]string `json:"files"`
	Report Report            `json:"report"`
}

// The lines around a result in the pod's log.
const (
	resultBegin = "upstream-bump: result"
	resultEnd   = "upstream-bump: end"
)

// maxResult bounds a result's JSON: the ten files are about 80 KB.
const maxResult = 8 << 20

// Encode is r as the pod prints it last: base64 of its gzipped JSON in
// lines of 76 between two marker lines.
func (r Result) Encode() (string, error) {
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	if err := json.NewEncoder(w).Encode(r); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	s := base64.StdEncoding.EncodeToString(z.Bytes())
	var b strings.Builder
	b.WriteString(resultBegin + "\n")
	for len(s) > 76 {
		b.WriteString(s[:76] + "\n")
		s = s[76:]
	}
	b.WriteString(s + "\n" + resultEnd + "\n")
	return b.String(), nil
}

// DecodeResult reads the result a pod's log ends with.
func DecodeResult(log string) (Result, error) {
	_, rest, ok := strings.Cut(log, resultBegin+"\n")
	body, _, done := strings.Cut(rest, resultEnd+"\n")
	if !ok || !done {
		return Result{}, errors.New("the log holds no result between its marker lines")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body, "\n", ""))
	if err != nil {
		return Result{}, fmt.Errorf("the result: %w", err)
	}
	z, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("the result: %w", err)
	}
	var r Result
	if err := json.NewDecoder(io.LimitReader(z, maxResult)).Decode(&r); err != nil {
		return Result{}, fmt.Errorf("the result: %w", err)
	}
	return r, nil
}
