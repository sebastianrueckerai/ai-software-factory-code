package console

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
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

func mMix(shares []factoryd.MixShare) string {
	if len(shares) == 0 {
		return "—"
	}
	parts := make([]string, len(shares))
	for i, s := range shares {
		parts[i] = fmt.Sprintf("%s %s %%", s.Alias, mCount(s.Share))
	}
	return strings.Join(parts, " · ")
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

func msub(a, b float64) float64 { return a - b }

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
