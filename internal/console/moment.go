package console

import (
	"html/template"
	"time"
	_ "time/tzdata" // the zone database travels in the binary, whatever the image holds
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
// token but those time.js fills in: 2006 01 02 2 January Jan 15 04 05 MST.
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
