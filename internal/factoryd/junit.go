package factoryd

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// proofOutcome is what one proof's run showed: an outcome and its evidence.
type proofOutcome struct{ Outcome, Message string }

const (
	proofPassed   = "passed"
	proofFailed   = "failed"
	proofMissing  = "missing"
	proofSkipped  = "skipped"
	proofNoReport = "no report"
)

// maxReportXML caps a report's unzipped size.
const maxReportXML = 32 << 20

// note is a test case's failure, error or skipped element.
type note struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

// evidence is the note's message and text, each trimmed, one per line.
func (n *note) evidence() string {
	var parts []string
	for _, s := range []string{n.Message, n.Text} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

type testCase struct {
	Name    string `xml:"name,attr"`
	Failure *note  `xml:"failure"`
	Error   *note  `xml:"error"`
	Skipped *note  `xml:"skipped"`
}

// failed is the case's failure or error, nil when it has neither.
func (c testCase) failed() *note {
	if c.Failure != nil {
		return c.Failure
	}
	return c.Error
}

// readJUnit reads a JUnit XML report: name the one test the run asked for, "" a suite run.
func readJUnit(report []byte, name string) proofOutcome {
	if len(bytes.TrimSpace(report)) == 0 {
		return proofOutcome{proofNoReport, "check test wrote an empty report"}
	}
	cases, err := testCases(report)
	if err != nil {
		return proofOutcome{proofNoReport, "the report does not parse: " + err.Error()}
	}
	if name == "" {
		return suiteOutcome(cases)
	}
	var named []testCase
	for _, c := range cases {
		if c.Name == name {
			named = append(named, c)
		}
	}
	if len(named) == 0 {
		for _, c := range cases {
			if f := c.failed(); f != nil {
				return proofOutcome{proofMissing, f.evidence()}
			}
		}
		return proofOutcome{proofMissing, ""}
	}
	for _, c := range named {
		if f := c.failed(); f != nil {
			return proofOutcome{proofFailed, f.evidence()}
		}
	}
	for _, c := range named {
		if c.Skipped == nil {
			return proofOutcome{proofPassed, ""}
		}
	}
	return proofOutcome{proofSkipped, named[0].Skipped.evidence()}
}

func suiteOutcome(cases []testCase) proofOutcome {
	if len(cases) == 0 {
		return proofOutcome{proofMissing, ""}
	}
	skipped := 0
	for _, c := range cases {
		if f := c.failed(); f != nil {
			return proofOutcome{proofFailed, f.evidence()}
		}
		if c.Skipped != nil {
			skipped++
		}
	}
	if skipped == len(cases) {
		return proofOutcome{proofSkipped, cases[0].Skipped.evidence()}
	}
	return proofOutcome{proofPassed, fmt.Sprintf("%d passed, %d skipped", len(cases)-skipped, skipped)}
}

// testCases reads the report's test cases up to its root element's end.
func testCases(report []byte) ([]testCase, error) {
	dec := xml.NewDecoder(bytes.NewReader(report))
	var cases []testCase
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, errors.New("no root element")
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != "testcase" {
				depth++
				continue
			}
			var c testCase
			if err := dec.DecodeElement(&c, &t); err != nil {
				return nil, err
			}
			cases = append(cases, c)
		case xml.EndElement:
			depth--
		default:
			continue
		}
		if depth == 0 {
			return cases, nil
		}
	}
}

// readReport unzips a worker's gzipped report, at most maxReportXML, then readJUnit.
func readReport(gz []byte, name string) proofOutcome {
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return proofOutcome{proofNoReport, "the report does not unzip: " + err.Error()}
	}
	report, err := io.ReadAll(io.LimitReader(r, maxReportXML+1))
	if err != nil {
		return proofOutcome{proofNoReport, "the report does not unzip: " + err.Error()}
	}
	if len(report) > maxReportXML {
		return proofOutcome{proofNoReport, fmt.Sprintf("the report passes %d MiB unzipped", maxReportXML>>20)}
	}
	return readJUnit(report, name)
}
