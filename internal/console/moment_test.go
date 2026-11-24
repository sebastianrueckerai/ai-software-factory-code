package console

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
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
		{string(moment(turn, "Mon 15:04 MST")), `<time datetime="2026-11-25T00:00:00Z" data-layout="Mon 15:04 MST">Wed 01:00 CET</time>`},
		{string(moment(summer, runDue)), `<time datetime="2026-07-01T10:30:05Z" data-layout="Mon 2 Jan 15:04 MST">Wed 1 Jul 12:30 CEST</time>`},
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
  c.localTime(summer, "2 Jan 15:04"), c.localTime(summer, "01-02 15:04 MST"), c.timeHTML(turn, "15:04 MST"),
  c.localTime(turn, "Mon 15:04 MST"), c.localTime(summer, "Mon 2 Jan 15:04 MST")].join("|"));`
	for _, c := range []struct{ zone, want string }{
		{"Europe/Berlin", `25 November at 01:00 CET|01:00 CET|2026-07-01 12:30:05 CEST|1 Jul 12:30|07-01 12:30 CEST|` +
			`<time datetime="2026-11-25T00:00:00.000Z" data-layout="15:04 MST">01:00 CET</time>|Wed 01:00 CET|Wed 1 Jul 12:30 CEST`},
		{"America/New_York", `24 November at 19:00 EST|19:00 EST|2026-07-01 06:30:05 EDT|1 Jul 06:30|07-01 06:30 EDT|` +
			`<time datetime="2026-11-25T00:00:00.000Z" data-layout="15:04 MST">19:00 EST</time>|Tue 19:00 EST|Wed 1 Jul 06:30 EDT`},
		{"Asia/Kolkata", `25 November at 05:30 GMT+5:30|05:30 GMT+5:30|2026-07-01 16:00:05 GMT+5:30|1 Jul 16:00|07-01 16:00 GMT+5:30|` +
			`<time datetime="2026-11-25T00:00:00.000Z" data-layout="15:04 MST">05:30 GMT+5:30</time>|Wed 05:30 GMT+5:30|Wed 1 Jul 16:00 GMT+5:30`},
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

// time.js's timed writes the words of each screen time in a text as its
// <time> in the browser's zone, as timed does on the server: under
// America/New_York the winter turn, 00:00 UTC, reads 19:00 EST the day
// before and the summer start, 10:30 UTC, 06:30 EDT. The longer words win
// where one holds the other, words in a tag or in code stay, and a text
// without times stays as it was.
func TestTimeJSTimed(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	times, err := json.Marshal(screenTimes(factoryd.Moments{{At: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), Layout: factoryd.LayoutClock},
		{At: time.Date(2026, 7, 1, 10, 30, 0, 0, time.UTC), Layout: factoryd.LayoutWords}, {At: time.Date(2026, 7, 1, 10, 30, 0, 0, time.UTC), Layout: factoryd.LayoutClock}}))
	if err != nil {
		t.Fatal(err)
	}
	driver := `var c = {};
require("vm").runInNewContext(require("fs").readFileSync("static/time.js", "utf8"), c);
var text = '<p title="00:00 UTC">Back at 00:00 UTC, since 1 July at 10:30 UTC. <code>00:00 UTC</code> 10:30 UTC</p>';
console.log(c.timed(text, ` + string(times) + `));
console.log(c.timed(text, null) === text);`
	cmd := exec.CommandContext(t.Context(), node, "-e", driver)
	cmd.Env = append(os.Environ(), "TZ=America/New_York")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	want := `<p title="00:00 UTC">Back at <time datetime="2026-11-25T00:00:00.000Z" data-layout="15:04 MST">19:00 EST</time>, ` +
		`since <time datetime="2026-07-01T10:30:00.000Z" data-layout="2 January at 15:04 MST">1 July at 06:30 EDT</time>. <code>00:00 UTC</code> ` +
		`<time datetime="2026-07-01T10:30:00.000Z" data-layout="15:04 MST">06:30 EDT</time></p>` + "\ntrue"
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// timed shows each moment's UTC words in a text as the moment, in
// Europe/Berlin until the browser reads it: a winter turn at 01:00 CET, a
// summer start at 12:30 CEST. The longer words win where one holds the
// other, words in an attribute or in code stay, the text around them stays
// escaped, and a text without moments stays as it was. An RFC 3339 stamp
// reads to the second.
func TestTimed(t *testing.T) {
	turn := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	summer := time.Date(2026, 7, 1, 10, 30, 0, 0, time.UTC)
	ms := factoryd.Moments{{At: turn, Layout: factoryd.LayoutClock}, {At: turn, Layout: factoryd.LayoutWords}, {At: summer, Layout: factoryd.LayoutWords}}
	in := template.HTML(`<p title="1 December at 00:00 UTC">Resets on 1 December at 00:00 UTC, &lt;or&gt; at 00:00 UTC; fired since 1 July at 10:30 UTC.</p>` +
		`<pre><code>1 July at 10:30 UTC</code></pre>`)
	want := `<p title="1 December at 00:00 UTC">Resets on <time datetime="2026-12-01T00:00:00Z" data-layout="2 January at 15:04 MST">1 December at 01:00 CET</time>, ` +
		`&lt;or&gt; at <time datetime="2026-12-01T00:00:00Z" data-layout="15:04 MST">01:00 CET</time>; ` +
		`fired since <time datetime="2026-07-01T10:30:00Z" data-layout="2 January at 15:04 MST">1 July at 12:30 CEST</time>.</p>` +
		`<pre><code>1 July at 10:30 UTC</code></pre>`
	if got := string(timed(in, ms)); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := timed(in, nil); got != in {
		t.Errorf("without moments: %s", got)
	}
	// an RFC 3339 stamp reads as a stamp to the second
	stamp := factoryd.Moments{{At: turn.Add(-90 * time.Second), Layout: factoryd.LayoutRFC3339}, {At: summer, Layout: factoryd.LayoutRFC3339}}
	in = template.HTML(`<p>Failed at 2026-11-30T23:58:30Z, reset 2026-07-01T10:30:00Z.</p>`)
	want = `<p>Failed at <time datetime="2026-11-30T23:58:30Z" data-layout="2006-01-02 15:04:05 MST">2026-12-01 00:58:30 CET</time>, ` +
		`reset <time datetime="2026-07-01T10:30:00Z" data-layout="2006-01-02 15:04:05 MST">2026-07-01 12:30:00 CEST</time>.</p>`
	if got := string(timed(in, stamp)); got != want {
		t.Errorf("stamps: got  %s\nwant %s", got, want)
	}
}

// A card's ask shows each time it names in the browser's zone, in
// Europe/Berlin as the server writes it: a card at the month cap opened in
// winter goes on by itself on 1 December at 01:00 CET, its Not now says;
// one at the day cap opened in summer resumes at 02:00 CEST, its why line
// on the small card and the opened card says, and on 16 July at 02:00
// CEST; an alert's card that fires since a summer morning says so at 12:30
// CEST. The words in UTC are the agents' and the issue's (factoryd's
// tests).
func TestAnAsksTimesReadInTheBrowsersZone(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	agentCard := func(id string, issue int64) factoryd.BoardTask {
		return factoryd.BoardTask{Task: factoryd.Task{ID: id, Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: issue, Objective: "Count the skipped rows"},
			Escalation: "the spending limit cannot fund a call"}
	}
	alert := factoryd.AlertQuestion(factoryd.SigNozAlert{Fingerprint: "fp", Labels: map[string]string{"alertname": "disk full", "service": "db"},
		StartsAt: time.Date(2026, 7, 1, 10, 30, 0, 0, time.UTC)}, "code", "prod", "the project runs no agents", "The disk of db is full.")
	fires := factoryd.BoardTask{Task: factoryd.Task{ID: "t-alert", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 23, Objective: alert.Ask},
		Escalation: alert.Ask, Question: &alert}
	r.tasks.tasks = []factoryd.BoardTask{agentCard("t-month", 21), agentCard("t-day", 22), fires}
	r.openCases = map[string][]factoryd.Case{
		"t-month": {{ID: 1, TaskID: "t-month", Kind: factoryd.CaseMonthCap, Status: factoryd.CaseAtHuman, Problem: "the month cap is reached",
			OpenedAt: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}},
		"t-day": {{ID: 2, TaskID: "t-day", Kind: factoryd.CaseDayCap, Status: factoryd.CaseAtHuman, Problem: "the day cap is reached",
			OpenedAt: time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)}},
		"t-alert": {{ID: 3, TaskID: "t-alert", Kind: factoryd.CaseHumanCard, Status: factoryd.CaseAtHuman, Problem: alert.Ask,
			Facts: factoryd.HumanAsk{Q: alert, Asker: factoryd.SourceAlert, Via: factoryd.ViaFactoryd}.Event(fires.Task).Detail}},
	}
	resumes := `or it resumes at <time datetime="2026-07-16T00:00:00Z" data-layout="15:04 MST">02:00 CEST</time>.`
	for id, wants := range map[string][]string{
		"t-month": {`goes on by itself when the limit resets on <time datetime="2026-12-01T00:00:00Z" data-layout="2 January at 15:04 MST">1 December at 01:00 CET</time>.`},
		"t-day": {resumes,
			`goes on by itself when the limit resets on <time datetime="2026-07-16T00:00:00Z" data-layout="2 January at 15:04 MST">16 July at 02:00 CEST</time>.`},
		"t-alert": {`It has fired since <time datetime="2026-07-01T10:30:00Z" data-layout="2 January at 15:04 MST">1 July at 12:30 CEST</time>.`},
	} {
		body := drawer(t, r, ck, id, "overview")
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("the opened card of %s lacks %s:\n%s", id, want, askBlockOf(t, body))
			}
		}
		for _, utc := range []string{"00:00 UTC", "10:30 UTC"} {
			if strings.Contains(askBlockOf(t, body), utc) {
				t.Errorf("the ask of %s still reads %q", id, utc)
			}
		}
	}
	board := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	if face := faceOf(t, board, "t-day"); !strings.Contains(face, resumes) {
		t.Errorf("the small card of the day cap lacks %s:\n%s", resumes, face)
	}
}

// A card's brief names its instants in the browser's zone, in the font of
// its words: the deadline of a winter red main's fix reads 13:00:00 CET
// and a summer alert's start 12:30:00 CEST, while the agent's view keeps
// the UTC words its agent reads.
func TestABriefsTimesReadInTheBrowsersZone(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	fixBrief, said := factoryd.MainFixCard(3380, "7eaf7c73121dd332c52b95bb5b024a08b3757146", "test", "https://git.example/sf/code/actions/runs/3380",
		time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC))
	start := time.Date(2026, 7, 1, 10, 30, 0, 0, time.UTC)
	alert := factoryd.AlertQuestion(factoryd.SigNozAlert{Fingerprint: "fp", Labels: map[string]string{"alertname": "disk full", "service": "db"}, StartsAt: start},
		"code", "prod", "the project runs no agents", factoryd.AlertIssue(factoryd.SigNozAlert{Fingerprint: "fp", Labels: map[string]string{"alertname": "disk full"}, StartsAt: start},
			"code", "prod", nil).Body)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-fix", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 31, Source: factoryd.SourceMainFix,
			Objective: factoryd.IssueObjective(fixBrief.Title, fixBrief.Body), Moments: said}},
		{Task: factoryd.Task{ID: "t-alert", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 32, Source: factoryd.SourceAlert,
			Objective: factoryd.IssueObjective(alert.Ask, alert.Info), Moments: alert.Moments}},
	}
	for id, want := range map[string]string{
		"t-fix":   `does not land by <time datetime="2026-11-24T12:00:00Z" data-layout="2006-01-02 15:04:05 MST">2026-11-24 13:00:00 CET</time> has factoryd`,
		"t-alert": `Firing since <time datetime="2026-07-01T10:30:00Z" data-layout="2006-01-02 15:04:05 MST">2026-07-01 12:30:00 CEST</time>.`,
	} {
		if body := drawer(t, r, ck, id, "overview"); !strings.Contains(body, want) {
			t.Errorf("the brief of %s lacks %s:\n%s", id, want, body)
		}
	}
	if view := drawer(t, r, ck, "t-fix", "overview"); !strings.Contains(view, "does not land by 2026-11-24T12:00:00Z has factoryd") {
		t.Errorf("the agent's view of t-fix lost its UTC words")
	}
}
