package console

import (
	"cmp"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// metricsDays is the metrics page's default window of UTC days, today
// included.
const metricsDays = 28

// projectMetrics answers GET /projects/{name}/metrics: a project's
// figures over the window and the one before it, as the page, and as
// JSON for the seam (format=json). Query params from and to are
// YYYY-MM-DD dates; each defaults as before.
func (c *Console) projectMetrics(w http.ResponseWriter, r *http.Request) {
	p, ok := c.pathProject(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -(metricsDays - 1))
	q := r.URL.Query()
	if v := q.Get("from"); v != "" {
		d, _, ok := parseCostsDay(v)
		if !ok {
			http.Error(w, "from: want YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		from = d
	}
	if v := q.Get("to"); v != "" {
		d, _, ok := parseCostsDay(v)
		if !ok {
			http.Error(w, "to: want YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		to = d
	}
	if from.After(to) {
		http.Error(w, "from is after to", http.StatusBadRequest)
		return
	}
	report, err := c.cfg.Figures.ProjectMetrics(r.Context(), p.Repository, from, to)
	if err != nil {
		log.Printf("console: the figures of %s: %v", p.Repository, err)
		http.Error(w, "cannot read the figures", http.StatusInternalServerError)
		return
	}
	if q.Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(report); err != nil { // #nosec G104 -- best-effort response write
			log.Printf("console: the figures of %s: %v", p.Repository, err)
		}
		return
	}
	s := current(r)
	days := int(to.Sub(from).Hours()/24) + 1
	wins := make([]metricsWindowLink, 0, 3)
	for _, d := range []int{7, 28, 90} {
		wins = append(wins, metricsWindowLink{
			Days:   d,
			Href:   fmt.Sprintf("/projects/%s/metrics?from=%s&to=%s", p.Name, to.AddDate(0, 0, -(d-1)).Format("2006-01-02"), to.Format("2006-01-02")),
			Active: days == d,
		})
	}
	c.render(w, "metrics.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Factory": true,
		"Project": p.Name, "Report": report, "Windows": wins})
}

// metricsWindowLink is one word of the window control: the days it
// names and the href that asks for them, keeping the window's end.
type metricsWindowLink struct {
	Days   int
	Href   string
	Active bool
}

// msign is the sign a move carries: U+2212 for a fall, + for a rise.
func msign(v float64) string {
	if v < 0 {
		return "−"
	}
	return "+"
}

func mCount(n float64) string     { return strconv.FormatInt(int64(math.Round(n)), 10) }
func mCountMove(d float64) string { return msign(d) + mCount(math.Abs(d)) }
func mDec(v float64) string       { return fmt.Sprintf("%.1f", v) }
func mDecMove(d float64) string   { return msign(d) + mDec(math.Abs(d)) }
func mPct(ratio float64) string   { return mCount(ratio*100) + " %" }
func mPctMove(d float64) string   { return msign(d) + mCount(math.Abs(d)*100) }
func mEur(v float64) string       { return fmt.Sprintf("€%.2f", v) }
func mEurMove(d float64) string   { return msign(d) + mEur(math.Abs(d)) }

// mK reads token counts: whole thousands as k, whole millions as M,
// fractions to one digit.
func mK(n float64) string {
	unit, div := " M", 1e6
	switch {
	case n < 1000:
		return mCount(n)
	case n < 1e6:
		unit, div = " k", 1e3
	}
	if k := n / div; k == math.Trunc(k) {
		return mCount(k) + unit
	}
	return mDec(n/div) + unit
}

// mDur reads milliseconds as the deck's durations: hours and padded
// minutes, bare minutes, bare seconds under a minute.
func mDur(ms float64) string {
	min := math.Round(ms / 60000)
	switch {
	case min < 1:
		return mCount(math.Round(ms/1000)) + " s"
	case min < 60:
		return mCount(min) + " m"
	default:
		total := int64(min)
		h, m := total/60, total%60
		if m == 0 {
			return strconv.FormatInt(h, 10) + " h"
		}
		return fmt.Sprintf("%d h %02d", h, m)
	}
}

func mDurMove(d float64) string { return msign(d) + mDur(math.Abs(d)) }

// mShare reads a share of 0..100 as whole percent; one that would round
// to nothing reads as under one.
func mShare(v float64) string {
	if v > 0 && v < 0.5 {
		return "< 1 %"
	}
	return mCount(v) + " %"
}

func mDiff(d factoryd.DiffSize) string {
	if !d.Has {
		return "—"
	}
	files := "files"
	if d.Files == 1 {
		files = "file"
	}
	return fmt.Sprintf("%s %s · +%s −%s", mCount(d.Files), files, mCount(d.Insertions), mCount(d.Deletions))
}

// mChat reads the chat's tool work: calls and minutes, seconds under a
// minute.
func mChat(c factoryd.ChatWork) string {
	if c.Minutes < 1 {
		return fmt.Sprintf("%s calls · %s s", mCount(c.Calls), mCount(c.Minutes*60))
	}
	return fmt.Sprintf("%s calls · %s min", mCount(c.Calls), mCount(c.Minutes))
}

// mSessionTime reads where the window's session time went: the wall over
// its sessions, the hours by class and the three biggest tools, most
// first, the full check's median run and the longest command; a dash
// without sessions.
func mSessionTime(st factoryd.SessionTime) string {
	if st.Sessions == 0 {
		return "—"
	}
	sessions := "sessions"
	if st.Sessions == 1 {
		sessions = "session"
	}
	parts := []string{fmt.Sprintf("%s in %d %s", mDur(st.WallHours*3.6e6), st.Sessions, sessions)}
	mostFirst := func(hours map[string]float64) []string {
		names := slices.Collect(maps.Keys(hours))
		slices.SortFunc(names, func(a, b string) int {
			return cmp.Or(cmp.Compare(hours[b], hours[a]), cmp.Compare(a, b))
		})
		return names
	}
	for _, c := range mostFirst(st.ClassHours) {
		parts = append(parts, c+" "+mDur(st.ClassHours[c]*3.6e6))
	}
	if tools := mostFirst(st.ToolHours); len(tools) > 0 {
		var spent []string
		for _, name := range tools[:min(3, len(tools))] {
			spent = append(spent, name+" "+mDur(st.ToolHours[name]*3.6e6))
		}
		parts = append(parts, strings.Join(spent, ", "))
	}
	if st.CheckRuns > 0 {
		parts = append(parts, fmt.Sprintf("full check median %s over %d runs", mDur(st.CheckMedianS*1000), st.CheckRuns))
	}
	if st.Longest.Command != "" {
		parts = append(parts, fmt.Sprintf("longest %s %s: %s", st.Longest.Class, mDur(st.Longest.Seconds*1000), st.Longest.Command))
	}
	return strings.Join(parts, " · ")
}

func msub(a, b float64) float64 { return a - b }

// mInt is an integer count as the coverage notes read it.
func mInt(n int) string { return strconv.FormatInt(int64(n), 10) }

// mEur4 is money to four digits, the EUR-per-line panel's own precision.
func mEur4(v float64) string { return "€" + fmt.Sprintf("%.4f", v) }

func mEur4Move(d float64) string { return msign(d) + mEur4(math.Abs(d)) }

// mHours reads the divide panels' precise milliseconds as hours: two
// digits kept, one never dropped — 6.0, 3.75 — the page's own words
// over the milliseconds the report answers, beside the legacy hour
// fields the JSON keeps unchanged.
func mHours(ms float64) string {
	h := fmt.Sprintf("%.2f", ms/3.6e6)
	if strings.HasSuffix(h, "0") {
		h = h[:len(h)-1]
	}
	return h + " hours"
}

func mHoursMove(d float64) string { return msign(d) + mHours(math.Abs(d)) }

// mSizes lists the sessions panel's per-size averages: small 2.0 ·
// medium 2.0; a size the window measured none of reads its dash.
func mSizes(sizes []factoryd.SizeFigure) string {
	if len(sizes) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(sizes))
	for _, s := range sizes {
		if s.Figure.Has {
			parts = append(parts, s.Size+" "+mDec(s.Figure.Value))
		} else {
			parts = append(parts, s.Size+" —")
		}
	}
	return strings.Join(parts, " · ")
}

// mNamed lists a typed distribution's counts: pushed 2 · wall_clock 1.
func mNamed(counts []factoryd.NamedCount) string {
	if len(counts) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(counts))
	for _, nc := range counts {
		parts = append(parts, nc.Name+" "+mCount(nc.Count))
	}
	return strings.Join(parts, " · ")
}

// mIntercepts lists the fixed-EUR-per-card fits, a size's euros to the
// cent — small 2.00 / medium 3.00 — and unavailable, never zero, where
// the cohort cannot hold the fit.
func mIntercepts(fits []factoryd.SizeIntercept) string {
	if len(fits) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(fits))
	for _, f := range fits {
		if f.Available {
			parts = append(parts, f.Size+" "+fmt.Sprintf("%.2f", f.EUR))
		} else {
			parts = append(parts, f.Size+" unavailable")
		}
	}
	return strings.Join(parts, " / ")
}

// mRoundName words one round bucket: 1 round, 2 rounds; a name that is
// no number — the unknown bucket — its own words.
func mRoundName(name string) string {
	if n, err := strconv.Atoi(name); err == nil {
		if n == 1 {
			return "1 round"
		}
		return fmt.Sprintf("%d rounds", n)
	}
	return name
}

// mRoundSpans lists a named round comparison's window counts: 1
// round:1 / 2 rounds:1, the unknown bucket last.
func mRoundSpans(figs []factoryd.NamedFigure) string {
	return mNamedSpans(figs, mRoundName)
}

// mOriginSpans lists the replans panel's origins: person:1 / cut:1 /
// round:1, the unknown bucket last.
func mOriginSpans(figs []factoryd.NamedFigure) string {
	return mNamedSpans(figs, func(name string) string { return name })
}

// mNamedSpans lists a named comparison's window counts, name by name —
// the explicit names with slashes, the unknown bucket apart at the end —
// never collapsing one name into another; a count the window does not
// hold reads its dash, never a measured zero.
func mNamedSpans(figs []factoryd.NamedFigure, name func(string) string) string {
	if len(figs) == 0 {
		return "—"
	}
	count := func(f factoryd.Figure) string {
		if f.Has {
			return mCount(f.Value)
		}
		return "—"
	}
	var parts []string
	for _, nf := range figs {
		if nf.Name != "unknown" {
			parts = append(parts, name(nf.Name)+":"+count(nf.Figure))
		}
	}
	for _, nf := range figs {
		if nf.Name == "unknown" {
			parts = append(parts, name(nf.Name)+":"+count(nf.Figure))
		}
	}
	return strings.Join(parts, " / ")
}

// mGateCov words the size-bound panel's evidence: its readings, the
// measured rest it stands on, and every hole by its own name.
func mGateCov(c factoryd.GateBoundCoverage) string {
	parts := []string{mInt(c.Readings) + " readings", mInt(c.Measured) + " measured"}
	for _, hole := range []struct {
		n    int
		word string
	}{
		{c.MissingBound, "no bound"},
		{c.MissingLines, "no lines"},
		{c.UnknownSize, "unknown size"},
		{c.UnsizedSize, "unsized"},
		{c.Identityless, "no identity"},
	} {
		if hole.n > 0 {
			parts = append(parts, mInt(hole.n)+" "+hole.word)
		}
	}
	return strings.Join(parts, " · ")
}

// mSessCov words the sessions panel's cohort and the holes its averages
// read around.
func mSessCov(c factoryd.SessionsCoverage) string {
	parts := []string{mInt(c.Cards) + " cards"}
	for _, hole := range []struct {
		n    int
		word string
	}{
		{c.UnknownSize, "unknown size"},
		{c.Unsized, "unsized"},
		{c.MissingStarts, "missing starts"},
		{c.PartialStarts, "partial starts"},
	} {
		if hole.n > 0 {
			parts = append(parts, mInt(hole.n)+" "+hole.word)
		}
	}
	return strings.Join(parts, " · ")
}

// mEconCov words the economics panels' cohort and its holes: a missing
// cost is never a free card.
func mEconCov(c factoryd.EconomicsCoverage) string {
	parts := []string{mInt(c.Cards) + " cards"}
	for _, hole := range []struct {
		n    int
		word string
	}{
		{c.MissingCost, "missing cost"},
		{c.MissingLines, "missing lines"},
		{c.MissingSize, "missing size"},
	} {
		if hole.n > 0 {
			parts = append(parts, mInt(hole.n)+" "+hole.word)
		}
	}
	return strings.Join(parts, " · ")
}

// mGroupCov words the group-open panel's cohort and the groups its mean
// never reads: the open and the startless, each its own label.
func mGroupCov(c factoryd.GroupHoursCoverage) string {
	parts := []string{"cohort " + mInt(c.Groups)}
	if c.Open > 0 {
		parts = append(parts, "open "+mInt(c.Open))
	}
	if c.MissingStart > 0 {
		parts = append(parts, "missing start "+mInt(c.MissingStart))
	}
	return strings.Join(parts, " · ")
}

// mHoldCov words the hold panel's evidence: the ended holds, the one
// still running, and the rows no interval covers.
func mHoldCov(c factoryd.HoldWaitCoverage) string {
	parts := []string{"ended " + mInt(c.Ended)}
	if c.Open > 0 {
		parts = append(parts, "open "+mInt(c.Open))
	}
	if c.Missing > 0 {
		parts = append(parts, "missing "+mInt(c.Missing))
	}
	return strings.Join(parts, " · ")
}

// mReviewCov words the review panel's holes: the rows no usable
// evidence covers, unknown and never an inferred success.
func mReviewCov(e factoryd.ReviewEvidence) string {
	if !e.Unknown.Has || e.Unknown.Value == 0 {
		return ""
	}
	return mCount(e.Unknown.Value) + " unknown"
}

// sparkSVG renders one figure's weeks as a hairline trace with an
// endpoint dot (C30). A range narrower than 15% of the weeks' middle
// widens to 30% so a near-constant figure reads as flat, not swinging.
func sparkSVG(weeks [7]float64) template.HTML {
	lo, hi := weeks[0], weeks[0]
	for _, v := range weeks {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	switch {
	case hi == lo:
		lo, hi = lo-1, hi+1
	case hi-lo < 0.15*math.Abs((hi+lo)/2):
		span := 0.30 * math.Abs((hi+lo)/2)
		lo, hi = (hi+lo)/2-span/2, (hi+lo)/2+span/2
	}
	var b strings.Builder
	b.WriteString(`<svg class="mspark" viewBox="0 0 240 30" aria-hidden="true"><path d="`)
	x, y := 0.0, 0.0
	for i, v := range weeks {
		x = 1 + float64(i)*238/float64(len(weeks)-1)
		y = 25 - (v-lo)/(hi-lo)*20
		if i == 0 {
			fmt.Fprintf(&b, "M%.1f %.1f", x, y)
		} else {
			fmt.Fprintf(&b, "L%.1f %.1f", x, y)
		}
	}
	fmt.Fprintf(&b, `"/><circle cx="%.1f" cy="%.1f" r="2"/></svg>`, x, y)
	return template.HTML(b.String()) // #nosec G203 -- the console's own spark; every number is its own formatting, no input
}
