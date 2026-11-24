package console

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A moment reads in Europe/Berlin until the browser reads it in its own
// zone: CET in winter, CEST in summer, its datetime the instant in UTC,
// its layout the one time.js fills in again; the title form keeps its
// text and carries the moment in its title.
func TestMoment(t *testing.T) {
	turn := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	summer := time.Date(2026, 7, 1, 10, 30, 5, 0, time.UTC)
	for _, c := range []struct {
		got  string
		want string
	}{
		{string(moment(turn, momentWords)), `<time datetime="2026-11-25T00:00:00Z" data-layout="2 January at 15:04 MST">25 November at 01:00 CET</time>`},
		{string(moment(turn.Add(-time.Hour), momentClock)), `<time datetime="2026-11-24T23:00:00Z" data-layout="15:04 MST">00:00 CET</time>`},
		{string(moment(summer, momentWords)), `<time datetime="2026-07-01T10:30:05Z" data-layout="2 January at 15:04 MST">1 July at 12:30 CEST</time>`},
		{string(moment(summer, momentStamp)), `<time datetime="2026-07-01T10:30:05Z" data-layout="2006-01-02 15:04 MST">2026-07-01 12:30 CEST</time>`},
		{string(moment(summer, momentDay)), `<time datetime="2026-07-01T10:30:05Z" data-layout="2 Jan 15:04 MST">1 Jul 12:30 CEST</time>`},
		{string(moment(time.Date(2026, 12, 31, 23, 30, 0, 0, time.UTC), momentStamp)), `<time datetime="2026-12-31T23:30:00Z" data-layout="2006-01-02 15:04 MST">2027-01-01 00:30 CET</time>`},
		{string(momentTitle(summer, momentStamp, "3 h <ago>")), `<time datetime="2026-07-01T10:30:05Z" data-layout="2006-01-02 15:04 MST" title="2026-07-01 12:30 CEST">3 h &lt;ago&gt;</time>`},
	} {
		if c.got != c.want {
			t.Errorf("got  %s\nwant %s", c.got, c.want)
		}
	}
}

// time.js reads a moment again in the browser's zone, in the layout the
// server wrote it in: under Europe/Berlin as the server wrote it, under
// America/New_York 00:00 UTC on 25 November is 19:00 EST the day before
// and 10:30:05 UTC on 1 July 06:30:05 EDT, and a zone with no short name
// reads by its GMT offset. Run under node, which the test needs.
func TestTimeJSReadsTheBrowsersZone(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	const driver = `var c = {};
require("vm").runInNewContext(require("fs").readFileSync("static/time.js", "utf8"), c);
var turn = new Date("2026-11-25T00:00:00Z"), summer = new Date("2026-07-01T10:30:05Z");
console.log([c.localTime(turn, "2 January at 15:04 MST"), c.localTime(turn, "15:04 MST"), c.localTime(summer, "2006-01-02 15:04:05 MST"),
  c.localTime(summer, "2 Jan 15:04"), c.localTime(summer, "01-02 15:04 MST"), c.timeHTML(turn, "15:04 MST")].join("|"));`
	for _, c := range []struct{ zone, want string }{
		{"Europe/Berlin", `25 November at 01:00 CET|01:00 CET|2026-07-01 12:30:05 CEST|1 Jul 12:30|07-01 12:30 CEST|` +
			`<time datetime="2026-11-25T00:00:00.000Z" data-layout="15:04 MST">01:00 CET</time>`},
		{"America/New_York", `24 November at 19:00 EST|19:00 EST|2026-07-01 06:30:05 EDT|1 Jul 06:30|07-01 06:30 EDT|` +
			`<time datetime="2026-11-25T00:00:00.000Z" data-layout="15:04 MST">19:00 EST</time>`},
		{"Asia/Kolkata", `25 November at 05:30 GMT+5:30|05:30 GMT+5:30|2026-07-01 16:00:05 GMT+5:30|1 Jul 16:00|07-01 16:00 GMT+5:30|` +
			`<time datetime="2026-11-25T00:00:00.000Z" data-layout="15:04 MST">05:30 GMT+5:30</time>`},
	} {
		cmd := exec.CommandContext(t.Context(), node, "-e", driver)
		cmd.Env = append(os.Environ(), "TZ="+c.zone)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("node under %s: %v", c.zone, err)
		}
		if got := strings.TrimSpace(string(out)); got != c.want {
			t.Errorf("under %s:\n%s\nwant\n%s", c.zone, got, c.want)
		}
	}
}
