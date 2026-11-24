package console

import (
	"html/template"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // the zone database travels in the binary, whatever the image holds

	"golang.org/x/net/html"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// screenZone is the zone a screen's times read in until the browser reads
// them in its own: time.js writes every <time data-layout> again.
var screenZone = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		panic(err) // time/tzdata holds it
	}
	return loc
}()

// The layouts a screen's moments take, in Go's reference time with no
// token but those time.js fills in: 2006 01 02 2 January Jan Mon 15 04 05
// MST.
const (
	momentWords  = "2 January at 15:04 MST"
	momentClock  = "15:04 MST"
	momentDay    = "2 Jan 15:04 MST"
	momentShort  = "01-02 15:04 MST"
	momentStamp  = "2006-01-02 15:04 MST"
	momentSecond = "2006-01-02 15:04:05 MST"
)

// moment is t as a screen shows it: a <time> of the instant in UTC whose
// text reads t in layout in screenZone.
func moment(t time.Time, layout string) template.HTML {
	return momentTag(t, layout, "", t.In(screenZone).Format(layout))
}

// momentTitle is moment with text in its place and t read in its title,
// for a cell whose text is t's age.
func momentTitle(t time.Time, layout, text string) template.HTML {
	return momentTag(t, layout, t.In(screenZone).Format(layout), text)
}

func momentTag(t time.Time, layout, title, text string) template.HTML {
	tag := `<time datetime="` + t.UTC().Format(time.RFC3339) + `" data-layout="` + template.HTMLEscapeString(layout) + `"`
	if title != "" {
		tag += ` title="` + template.HTMLEscapeString(title) + `"`
	}
	return template.HTML(tag + ">" + template.HTMLEscapeString(text) + "</time>") // #nosec G203 -- layout, title and text are escaped, the stamp is RFC 3339
}

// escaped is text as markup that a moment may stand beside.
func escaped(text string) template.HTML {
	return template.HTML(template.HTMLEscapeString(text)) // #nosec G203 -- escaped
}

// screenLayout is the layout a screen reads m in: m's own with the zone
// named, an RFC 3339 stamp's as a stamp to the second.
func screenLayout(m factoryd.Moment) string {
	if m.Layout == factoryd.LayoutRFC3339 {
		return momentSecond
	}
	return m.Layout + " MST"
}

// A screenTime is a moment as a page's script shows it in a text it writes
// (time.js timed): its words, its instant and the layout it reads in.
type screenTime struct {
	Words  string    `json:"words"`
	At     time.Time `json:"at"`
	Layout string    `json:"layout"`
}

// screenTimes are ms as a page's script shows them.
func screenTimes(ms factoryd.Moments) []screenTime {
	var out []screenTime
	for _, m := range ms {
		out = append(out, screenTime{m.Words(), m.At.UTC(), screenLayout(m)})
	}
	return out
}

// timed is h with the words of each of ms in its text shown as its moment,
// the longest words first; words in code stay as they are written.
func timed(h template.HTML, ms factoryd.Moments) template.HTML {
	if len(ms) == 0 {
		return h
	}
	pairs := make([]string, 0, 2*len(ms))
	for _, m := range slices.SortedStableFunc(slices.Values(ms), func(a, b factoryd.Moment) int { return len(b.Words()) - len(a.Words()) }) {
		pairs = append(pairs, m.Words(), string(moment(m.At, screenLayout(m))))
	}
	said := strings.NewReplacer(pairs...)
	var b strings.Builder
	code := 0
	z := html.NewTokenizer(strings.NewReader(string(h)))
	for {
		tt := z.Next()
		name, _ := z.TagName()
		inCode := string(name) == "code" || string(name) == "pre"
		switch {
		case tt == html.ErrorToken:
			return template.HTML(b.String()) // #nosec G203 -- h's own markup; each moment escapes its text
		case tt == html.TextToken && code == 0:
			b.WriteString(said.Replace(string(z.Raw())))
			continue
		case tt == html.StartTagToken && inCode:
			code++
		case tt == html.EndTagToken && inCode && code > 0:
			code--
		}
		b.Write(z.Raw())
	}
}
