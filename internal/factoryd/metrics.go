package factoryd

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// The metrics page's figures (roadmap 147): every number is a query over
// the hard data the factory already stores — no LLM output, no stored
// summaries, one row per event. Every figure reads the window, the
// equal-length window before it, and seven weekly spark buckets that end
// where the window does; a fact before the spark start belongs to none.

// Figure is one deck number: the window value, the previous window's,
// and the spark. Has false means the cohort is empty — the page renders
// — instead of a number.
type Figure struct {
	Value   float64    `json:"value"`
	Prev    float64    `json:"prev"`
	Has     bool       `json:"has"`
	PrevHas bool       `json:"prev_has"`
	Weeks   [7]float64 `json:"weeks"`
}

// RevertedRefiled splits the window's reverted cards into bare reverts
// and refiles (an alert filed the fix at the revert); Prev is
// [reverted, refiled], Weeks counts reverts per bucket.
type RevertedRefiled struct {
	Reverted float64    `json:"reverted"`
	Refiled  float64    `json:"refiled"`
	Prev     [2]float64 `json:"prev"`
	Has      bool       `json:"has"`
	Weeks    [7]float64 `json:"weeks"`
}

// MixShare is one model's share of the window's tokens, 0..100.
type MixShare struct {
	Alias string  `json:"alias"`
	Share float64 `json:"share"`
}

// DiffSize is the median diff of shipped cards; Prev is
// [files, insertions, deletions], Weeks the median files per bucket.
type DiffSize struct {
	Files      float64    `json:"files"`
	Insertions float64    `json:"insertions"`
	Deletions  float64    `json:"deletions"`
	Prev       [3]float64 `json:"prev"`
	Has        bool       `json:"has"`
	PrevHas    bool       `json:"prev_has"`
	Weeks      [7]float64 `json:"weeks"`
}

// ChatWork is the project agent's tool work; Weeks counts calls.
type ChatWork struct {
	Calls       float64    `json:"calls"`
	Minutes     float64    `json:"minutes"`
	PrevCalls   float64    `json:"prev_calls"`
	PrevMinutes float64    `json:"prev_minutes"`
	Weeks       [7]float64 `json:"weeks"`
}

// MetricsReport is every figure of one project over one window, the
// shape the endpoint serves and the page renders.
type MetricsReport struct {
	Repository string    `json:"repository"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`

	Deploys          Figure          `json:"deploys"`
	LeadTime         Figure          `json:"lead_time"`
	CardCycle        Figure          `json:"card_cycle"`
	Throughput       Figure          `json:"throughput"`
	ChangeFailure    Figure          `json:"change_failure"`
	RevertedRefiled  RevertedRefiled `json:"reverted_refiled"`
	TimeToRestore    Figure          `json:"time_to_restore"`
	Spend            Figure          `json:"spend"`
	PerCard          Figure          `json:"per_card"`
	TokensPerCard    Figure          `json:"tokens_per_card"`
	ModelMix         []MixShare      `json:"model_mix"`
	Interventions    Figure          `json:"interventions"`
	Escalations      Figure          `json:"escalations"`
	HumanBlocked     Figure          `json:"human_blocked"`
	ReviewAcceptance Figure          `json:"review_acceptance"`
	SelfDeploys      Figure          `json:"self_deploys"`
	DiffSize         DiffSize        `json:"diff_size"`
	ChatWork         ChatWork        `json:"chat_tool_work"`
	CIDuration       Figure          `json:"ci_duration"`
}

// metricWindow is the spans every figure reads: the window, the
// equal-length window before it, and the seven weeks the sparks cover.
// From and To are UTC midnights; From inclusive, To exclusive.
type metricWindow struct {
	From, PrevFrom, PrevTo, To, SparkFrom time.Time
}

func newMetricWindow(from, to time.Time) metricWindow {
	end := to.AddDate(0, 0, 1)
	days := int(end.Sub(from).Hours() / 24)
	return metricWindow{From: from, PrevFrom: from.AddDate(0, 0, -days), PrevTo: from, To: end,
		SparkFrom: end.AddDate(0, 0, -7*7)}
}

// Weeks queries bucket by ($2 − spark)/7 days over [spark, to); scalar
// queries take their own bounds as $2 and $3. 604800 is one week in
// seconds — Postgres divides an interval by no interval.
func metricBucket(col string) string {
	return metricBucketFrom(col, "$2")
}

func metricBucketFrom(col, spark string) string {
	return fmt.Sprintf(`floor(EXTRACT(EPOCH FROM (%s - %s::timestamptz)) / 604800)::int`, col, spark)
}

func metricScalar(ctx context.Context, db *sql.DB, query string, args ...any) (float64, bool, error) {
	var v sql.NullFloat64
	if err := db.QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
		return 0, false, err
	}
	return v.Float64, v.Valid, nil
}

func metricWeeks(ctx context.Context, db *sql.DB, query string, args ...any) ([7]float64, error) {
	var weeks [7]float64
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return weeks, err
	}
	defer rows.Close()
	for rows.Next() {
		var b int
		var v float64
		if err := rows.Scan(&b, &v); err != nil {
			return weeks, err
		}
		if b >= 0 && b < len(weeks) {
			weeks[b] = v
		}
	}
	return weeks, rows.Err()
}

func metricRatio(num, den float64) (float64, bool) {
	if den == 0 {
		return 0, false
	}
	return num / den, true
}

// metricCount is the shape of every plain count: a fact even at zero.
func metricCount(ctx context.Context, db *sql.DB, repo string, w metricWindow, scalar, weeks string) (Figure, error) {
	f := Figure{Has: true, PrevHas: true}
	var err error
	if f.Value, _, err = metricScalar(ctx, db, scalar, repo, w.From, w.To); err != nil {
		return f, err
	}
	if f.Prev, _, err = metricScalar(ctx, db, scalar, repo, w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	f.Weeks, err = metricWeeks(ctx, db, weeks, repo, w.SparkFrom, w.To)
	return f, err
}

// metricMedian is the shape of every median: absent over an empty
// cohort.
func metricMedian(ctx context.Context, db *sql.DB, repo string, w metricWindow, scalar, weeks string) (Figure, error) {
	var f Figure
	var err error
	if f.Value, f.Has, err = metricScalar(ctx, db, scalar, repo, w.From, w.To); err != nil {
		return f, err
	}
	if f.Prev, f.PrevHas, err = metricScalar(ctx, db, scalar, repo, w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	f.Weeks, err = metricWeeks(ctx, db, weeks, repo, w.SparkFrom, w.To)
	return f, err
}

// metricRatioFigure reads num/den pairs for window, previous and each
// bucket; a zero denominator reads absent.
func metricRatioFigure(ctx context.Context, db *sql.DB, repo string, w metricWindow, scalar, weeks string) (Figure, error) {
	var f Figure
	read := func(from, to time.Time) (num, den float64, err error) {
		err = db.QueryRowContext(ctx, scalar, repo, from, to).Scan(&num, &den)
		return
	}
	num, den, err := read(w.From, w.To)
	if err != nil {
		return f, err
	}
	f.Value, f.Has = metricRatio(num, den)
	if num, den, err = read(w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	f.Prev, f.PrevHas = metricRatio(num, den)
	rows, err := db.QueryContext(ctx, weeks, repo, w.SparkFrom, w.To)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	for rows.Next() {
		var b int
		var bn, bd float64
		if err := rows.Scan(&b, &bn, &bd); err != nil {
			return f, err
		}
		if b >= 0 && b < 7 {
			f.Weeks[b], _ = metricRatio(bn, bd)
		}
	}
	return f, rows.Err()
}

// metricMeanOverCards divides a project total by the window's done
// cards: tokens per card, interventions per card.
func metricMeanOverCards(ctx context.Context, db *sql.DB, repo string, w metricWindow, total, totalWeeks string) (Figure, error) {
	const cards = `SELECT count(*) FROM tasks WHERE repository = $1 AND kind <> 'chat' AND status = 'done' AND finished_at >= $2 AND finished_at < $3`
	var f Figure
	num, _, err := metricScalar(ctx, db, total, repo, w.From, w.To)
	if err != nil {
		return f, err
	}
	den, _, err := metricScalar(ctx, db, cards, repo, w.From, w.To)
	if err != nil {
		return f, err
	}
	f.Value, f.Has = metricRatio(num, den)
	if num, _, err = metricScalar(ctx, db, total, repo, w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	if den, _, err = metricScalar(ctx, db, cards, repo, w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	f.Prev, f.PrevHas = metricRatio(num, den)
	tRows, err := metricWeeks(ctx, db, totalWeeks, repo, w.SparkFrom, w.To)
	if err != nil {
		return f, err
	}
	dRows, err := metricWeeks(ctx, db, doneCardWeeks(`count(*)`), repo, w.SparkFrom, w.To)
	if err != nil {
		return f, err
	}
	for i := range f.Weeks {
		f.Weeks[i], _ = metricRatio(tRows[i], dRows[i])
	}
	return f, nil
}

// The card filter every card figure shares: done cards of the project,
// finished in the span. $2 and $3 are the span bounds.
const doneCardWhere = `repository = $1 AND kind <> 'chat' AND status = 'done' AND finished_at >= $2 AND finished_at < $3`

func doneCardWeeks(order string) string {
	return fmt.Sprintf(`SELECT %s, %s FROM tasks WHERE repository = $1 AND kind <> 'chat' AND status = 'done' AND finished_at >= $2 AND finished_at < $3 GROUP BY 1`,
		metricBucket("finished_at"), order)
}

// metricDeploys: project deployments the rollout wait confirmed.
func metricDeploys(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	return metricCount(ctx, db, repo, w,
		`SELECT count(*) FROM project_rolls WHERE repository = $1 AND ready_at >= $2 AND ready_at < $3`,
		`SELECT `+metricBucket("ready_at")+", count(*) FROM project_rolls WHERE repository = $1 AND ready_at >= $2 AND ready_at < $3 GROUP BY 1")
}

// metricLeadTime: push to live, the median over the window's rolls.
func metricLeadTime(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM project_rolls r JOIN ci_runs c ON c.repository = r.repository AND c.commit = r.commit
		WHERE r.repository = $1 AND r.commit <> '' AND r.ready_at >= %s AND r.ready_at < $3`
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (r.ready_at - c.pushed_at)) * 1000)`
	return metricMedian(ctx, db, repo, w,
		fmt.Sprintf(`SELECT `+agg+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("r.ready_at")+`, `+agg+pair+` GROUP BY 1`, "$2"))
}

// metricCardCycle: creation to done, the median of the window's cards.
func metricCardCycle(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (finished_at - created_at)) * 1000)`
	return metricMedian(ctx, db, repo, w,
		`SELECT `+agg+` FROM tasks WHERE `+doneCardWhere,
		doneCardWeeks(agg))
}

// metricThroughput: cards done.
func metricThroughput(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	return metricCount(ctx, db, repo, w,
		`SELECT count(*) FROM tasks WHERE `+doneCardWhere,
		doneCardWeeks(`count(*)`))
}

// metricChangeFailure: the window's done cards that were reverted.
func metricChangeFailure(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM tasks WHERE ` + doneCardWhere
	const weeks = `SELECT %s, count(*) FILTER (WHERE reverted_at IS NOT NULL), count(*) FROM tasks WHERE repository = $1 AND kind <> 'chat' AND status = 'done' AND finished_at >= $2 AND finished_at < $3 GROUP BY 1`
	return metricRatioFigure(ctx, db, repo, w,
		`SELECT count(*) FILTER (WHERE reverted_at IS NOT NULL), count(*)`+pair,
		fmt.Sprintf(weeks, metricBucket("finished_at")))
}

// metricRevertedRefiled: of the window's reverted cards, those an alert
// refiled at the revert and those left as bare reverts.
func metricRevertedRefiled(ctx context.Context, db *sql.DB, repo string, w metricWindow) (RevertedRefiled, error) {
	const q = `SELECT count(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM alerts a WHERE a.task_id = tasks.id AND a.kind = 'alert' AND a.raised_at = tasks.reverted_at)),
		count(*) FILTER (WHERE EXISTS (SELECT 1 FROM alerts a WHERE a.task_id = tasks.id AND a.kind = 'alert' AND a.raised_at = tasks.reverted_at))
		FROM tasks WHERE ` + doneCardWhere + ` AND reverted_at IS NOT NULL`
	var r RevertedRefiled
	if err := db.QueryRowContext(ctx, q, repo, w.From, w.To).Scan(&r.Reverted, &r.Refiled); err != nil {
		return r, err
	}
	r.Has = r.Reverted+r.Refiled > 0
	if err := db.QueryRowContext(ctx, q, repo, w.PrevFrom, w.PrevTo).Scan(&r.Prev[0], &r.Prev[1]); err != nil {
		return r, err
	}
	var err error
	r.Weeks, err = metricWeeks(ctx, db, `SELECT `+metricBucket("reverted_at")+", count(*) FROM tasks WHERE repository = $1 AND kind <> 'chat' AND reverted_at IS NOT NULL AND reverted_at >= $2 AND reverted_at < $3 GROUP BY 1", repo, w.SparkFrom, w.To)
	return r, err
}

// metricTimeToRestore: alert to the fix card's finish, by fingerprint.
func metricTimeToRestore(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM alerts a JOIN tasks ot ON ot.id = a.task_id
		CROSS JOIN LATERAL (SELECT min(t.finished_at) AS fix FROM tasks t WHERE t.alert_fingerprint = a.fingerprint AND t.finished_at > a.raised_at) f
		WHERE a.kind = 'alert' AND a.raised_at >= %s AND a.raised_at < $3 AND ot.repository = $1 AND f.fix IS NOT NULL`
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (f.fix - a.raised_at)) * 1000)`
	return metricMedian(ctx, db, repo, w,
		fmt.Sprintf(`SELECT `+agg+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("a.raised_at")+", "+agg+pair+` GROUP BY 1`, "$2"))
}

// metricSpend: the ledger's euros, chat tasks included — what the
// project cost. Days, so the bucket math is date arithmetic.
func metricSpend(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM ledger l JOIN tasks t ON t.id = l.task_id
		WHERE t.repository = $1 AND l.day >= %s::date AND l.day < $3::date`
	const agg = `COALESCE(sum(l.eur), 0)::float8`
	return metricCount(ctx, db, repo, w,
		fmt.Sprintf(`SELECT `+agg+pair, "$2"),
		fmt.Sprintf(`SELECT ((l.day - $2::date) / 7), `+agg+pair+` GROUP BY 1`, "$2"))
}

// metricPerCard: what one card costs, the median of the window's cards'
// whole-life ledger totals.
func metricPerCard(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY (SELECT COALESCE(sum(l.eur), 0) FROM ledger l WHERE l.task_id = tasks.id))`
	return metricMedian(ctx, db, repo, w,
		`SELECT `+agg+` FROM tasks WHERE `+doneCardWhere,
		doneCardWeeks(agg))
}

// metricTokensPerCard: the tokens the window's cards' calls moved.
func metricTokensPerCard(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM audit a JOIN tasks t ON t.id = a.task_id
		WHERE t.repository = $1 AND t.kind <> 'chat' AND a.ts >= %s AND a.ts < $3`
	const agg = `COALESCE(sum(a.prompt_tokens + a.output_tokens), 0)::float8`
	return metricMeanOverCards(ctx, db, repo, w,
		fmt.Sprintf(`SELECT `+agg+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("a.ts")+", "+agg+pair+` GROUP BY 1`, "$2"))
}

// metricModelMix: the window's tokens by model, top three by tokens
// then the rest folded into other.
func metricModelMix(ctx context.Context, db *sql.DB, repo string, w metricWindow) ([]MixShare, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.model_alias, sum(a.prompt_tokens + a.output_tokens)::float8
		FROM audit a JOIN tasks t ON t.id = a.task_id WHERE t.repository = $1 AND a.ts >= $2 AND a.ts < $3 GROUP BY 1`, repo, w.From, w.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pair struct {
		alias  string
		tokens float64
	}
	var ps []pair
	var total float64
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.alias, &p.tokens); err != nil {
			return nil, err
		}
		ps = append(ps, p)
		total += p.tokens
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, nil
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].tokens != ps[j].tokens {
			return ps[i].tokens > ps[j].tokens
		}
		return ps[i].alias < ps[j].alias
	})
	var mix []MixShare
	for i, p := range ps {
		alias := p.alias
		if i >= 3 {
			if alias == "" {
				continue
			}
			alias = "other"
		}
		if len(mix) > 0 && mix[len(mix)-1].Alias == alias {
			mix[len(mix)-1].Share += 100 * p.tokens / total
			continue
		}
		mix = append(mix, MixShare{Alias: alias, Share: 100 * p.tokens / total})
	}
	return mix, nil
}

// metricInterventions: human acts per card, over the window's cards.
func metricInterventions(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM events WHERE repository = $1 AND via IN ('button', 'chat', 'webhook', 'console') AND at >= %s AND at < $3`
	return metricMeanOverCards(ctx, db, repo, w,
		fmt.Sprintf(`SELECT count(*)`+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("at")+", count(*)"+pair+` GROUP BY 1`, "$2"))
}

// metricEscalations: cards the gate supervisor escalated to a human.
func metricEscalations(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const pair = ` FROM alerts a JOIN tasks t ON t.id = a.task_id
		WHERE a.kind = 'escalation' AND a.raised_at >= %s AND a.raised_at < $3 AND t.repository = $1`
	return metricCount(ctx, db, repo, w,
		fmt.Sprintf(`SELECT count(*)`+pair, "$2"),
		fmt.Sprintf(`SELECT `+metricBucket("a.raised_at")+", count(*)"+pair+` GROUP BY 1`, "$2"))
}

// metricSelfDeploys: the factory's own rolls — every project reads the
// same count; the table has no repository.
func metricSelfDeploys(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const scalar = `SELECT count(*) FROM rolls WHERE live_at >= $1 AND live_at < $2`
	weeks := `SELECT ` + metricBucketFrom("live_at", "$1") + `, count(*) FROM rolls WHERE live_at >= $1 AND live_at < $2 GROUP BY 1`
	f := Figure{Has: true, PrevHas: true}
	var err error
	if f.Value, _, err = metricScalar(ctx, db, scalar, w.From, w.To); err != nil {
		return f, err
	}
	if f.Prev, _, err = metricScalar(ctx, db, scalar, w.PrevFrom, w.PrevTo); err != nil {
		return f, err
	}
	f.Weeks, err = metricWeeks(ctx, db, weeks, w.SparkFrom, w.To)
	return f, err
}

// metricDiffSize: the median diff of the window's shipped cards.
func metricDiffSize(ctx context.Context, db *sql.DB, repo string, w metricWindow) (DiffSize, error) {
	const q = `SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY files), percentile_cont(0.5) WITHIN GROUP (ORDER BY insertions), percentile_cont(0.5) WITHIN GROUP (ORDER BY deletions)
		FROM tasks WHERE ` + doneCardWhere + ` AND files > 0`
	read := func(from, to time.Time) (f, i, d float64, ok bool, err error) {
		var nf, ni, nd sql.NullFloat64
		if err = db.QueryRowContext(ctx, q, repo, from, to).Scan(&nf, &ni, &nd); err != nil {
			return
		}
		return nf.Float64, ni.Float64, nd.Float64, nf.Valid, nil
	}
	var out DiffSize
	var err error
	if out.Files, out.Insertions, out.Deletions, out.Has, err = read(w.From, w.To); err != nil {
		return out, err
	}
	if out.Prev[0], out.Prev[1], out.Prev[2], out.PrevHas, err = read(w.PrevFrom, w.PrevTo); err != nil {
		return out, err
	}
	out.Weeks, err = metricWeeks(ctx, db, `SELECT `+metricBucket("finished_at")+`, percentile_cont(0.5) WITHIN GROUP (ORDER BY files) FROM tasks WHERE `+doneCardWhere+` AND files > 0 GROUP BY 1`, repo, w.SparkFrom, w.To)
	return out, err
}

// metricChatWork: the project agent's tool calls and their minutes.
func metricChatWork(ctx context.Context, db *sql.DB, repo string, w metricWindow) (ChatWork, error) {
	const q = `SELECT (SELECT COALESCE(sum(jsonb_array_length(tool_calls)), 0)::float8 FROM chat_messages WHERE repository = $1 AND role = 'assistant' AND at >= $2 AND at < $3),
		(SELECT COALESCE(sum((c ->> 'ms')::numeric / 60000), 0)::float8 FROM chat_messages m CROSS JOIN LATERAL jsonb_array_elements(COALESCE(m.tool_calls, '[]')) c
			WHERE m.repository = $1 AND m.role = 'assistant' AND m.at >= $2 AND m.at < $3)`
	var c ChatWork
	if err := db.QueryRowContext(ctx, q, repo, w.From, w.To).Scan(&c.Calls, &c.Minutes); err != nil {
		return c, err
	}
	if err := db.QueryRowContext(ctx, q, repo, w.PrevFrom, w.PrevTo).Scan(&c.PrevCalls, &c.PrevMinutes); err != nil {
		return c, err
	}
	var err error
	c.Weeks, err = metricWeeks(ctx, db, `SELECT `+metricBucket("at")+", COALESCE(sum(jsonb_array_length(tool_calls)), 0)::float8 FROM chat_messages WHERE repository = $1 AND role = 'assistant' AND at >= $2 AND at < $3 GROUP BY 1", repo, w.SparkFrom, w.To)
	return c, err
}

// metricCIDuration: the median Actions run, push to finish.
func metricCIDuration(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	const agg = `percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms)`
	return metricMedian(ctx, db, repo, w,
		`SELECT `+agg+` FROM ci_runs WHERE repository = $1 AND pushed_at >= $2 AND pushed_at < $3`,
		`SELECT `+metricBucket("pushed_at")+", "+agg+` FROM ci_runs WHERE repository = $1 AND pushed_at >= $2 AND pushed_at < $3 GROUP BY 1`)
}

// The two figures the event sequence carries, read in Go from one pull
// of the project's done cards and their status events. The pull spans
// [min(SparkFrom, PrevFrom), To) — every window the figures read — and
// carries each card's events unbounded below, so a span that opens
// before the pull still closes; event resolution is the clock's
// granularity.

type metricStatusEvent struct {
	task   string
	at     time.Time
	status string
}

type metricCard struct {
	id              string
	created, doneAt time.Time
}

func metricStatusPull(ctx context.Context, db *sql.DB, repo string, w metricWindow) ([]metricCard, []metricStatusEvent, error) {
	lo := w.SparkFrom
	if w.PrevFrom.Before(lo) {
		lo = w.PrevFrom
	}
	cards, err := func() ([]metricCard, error) {
		rows, err := db.QueryContext(ctx, `SELECT id, created_at, finished_at FROM tasks
			WHERE repository = $1 AND kind <> 'chat' AND status = 'done' AND finished_at >= $2 AND finished_at < $3`, repo, lo, w.To)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []metricCard
		for rows.Next() {
			var c metricCard
			if err := rows.Scan(&c.id, &c.created, &c.doneAt); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, rows.Err()
	}()
	if err != nil {
		return nil, nil, err
	}
	events, err := func() ([]metricStatusEvent, error) {
		rows, err := db.QueryContext(ctx, `SELECT e.task_id, e.at, e.detail ->> 'status' FROM events e JOIN tasks t ON t.id = e.task_id
			WHERE t.repository = $1 AND t.kind <> 'chat' AND t.status = 'done' AND t.finished_at >= $2 AND t.finished_at < $3
			AND e.kind = 'task_status' ORDER BY e.task_id, e.at, e.id`, repo, lo, w.To)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []metricStatusEvent
		for rows.Next() {
			var e metricStatusEvent
			if err := rows.Scan(&e.task, &e.at, &e.status); err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, rows.Err()
	}()
	return cards, events, err
}

func eventsOf(events []metricStatusEvent, task string) []metricStatusEvent {
	var out []metricStatusEvent
	for _, e := range events {
		if e.task == task {
			out = append(out, e)
		}
	}
	return out
}

// blockedSeconds measures the waiting-human/waiting-poster spans of one
// card: from each such status to the next status event, a span still
// open at the finish capped there.
func blockedSeconds(events []metricStatusEvent, finish time.Time) (secs float64) {
	var since time.Time
	waiting := false
	for _, e := range events {
		if waiting {
			secs += e.at.Sub(since).Seconds()
			waiting = false
		}
		if e.status == "waiting-human" || e.status == "waiting-poster" {
			waiting, since = true, e.at
		}
	}
	if waiting {
		secs += finish.Sub(since).Seconds()
	}
	return
}

// reviewVerdict reads a card's first review's outcome: accepted (the
// next status closes or waits for a human) or changes (back to
// running); ok is false when the card was never reviewed or the verdict
// is neither.
func reviewVerdict(events []metricStatusEvent) (accepted bool, ok bool) {
	for i, e := range events {
		if e.status != "in-review" {
			continue
		}
		if i+1 >= len(events) {
			return false, false
		}
		switch events[i+1].status {
		case "closing", "waiting-human":
			return true, true
		case "running":
			return false, true
		}
		return false, false
	}
	return false, false
}

func firstReviewAt(events []metricStatusEvent) time.Time {
	for _, e := range events {
		if e.status == "in-review" {
			return e.at
		}
	}
	return time.Time{}
}

func withinWindow(at, lo, hi time.Time) bool {
	return !at.Before(lo) && at.Before(hi)
}

func weekBucket(at, spark time.Time) int {
	return int(at.Sub(spark) / (7 * 24 * time.Hour))
}

// metricHumanBlocked: the share of the window's cards' lifetime spent
// blocked on a human.
func metricHumanBlocked(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	cards, events, err := metricStatusPull(ctx, db, repo, w)
	if err != nil {
		return Figure{}, err
	}
	var blocked, life, pBlocked, pLife float64
	var wBlocked, wLife [7]float64
	for _, c := range cards {
		secs := blockedSeconds(eventsOf(events, c.id), c.doneAt)
		lifeSecs := c.doneAt.Sub(c.created).Seconds()
		switch {
		case withinWindow(c.doneAt, w.From, w.To):
			blocked, life = blocked+secs, life+lifeSecs
		case withinWindow(c.doneAt, w.PrevFrom, w.PrevTo):
			pBlocked, pLife = pBlocked+secs, pLife+lifeSecs
		}
		if withinWindow(c.doneAt, w.SparkFrom, w.To) {
			if b := weekBucket(c.doneAt, w.SparkFrom); b >= 0 && b < 7 {
				wBlocked[b], wLife[b] = wBlocked[b]+secs, wLife[b]+lifeSecs
			}
		}
	}
	var f Figure
	f.Value, f.Has = metricRatio(blocked, life)
	f.Prev, f.PrevHas = metricRatio(pBlocked, pLife)
	for b := range f.Weeks {
		f.Weeks[b], _ = metricRatio(wBlocked[b], wLife[b])
	}
	return f, nil
}

// metricReviewAcceptance: the share of reviewed cards whose first
// review closed the card, weeks bucketed by the review.
func metricReviewAcceptance(ctx context.Context, db *sql.DB, repo string, w metricWindow) (Figure, error) {
	cards, events, err := metricStatusPull(ctx, db, repo, w)
	if err != nil {
		return Figure{}, err
	}
	var acc, tot, pAcc, pTot float64
	var wAcc, wTot [7]float64
	for _, c := range cards {
		evs := eventsOf(events, c.id)
		accepted, ok := reviewVerdict(evs)
		if !ok {
			continue
		}
		switch {
		case withinWindow(c.doneAt, w.From, w.To):
			tot++
			if accepted {
				acc++
			}
		case withinWindow(c.doneAt, w.PrevFrom, w.PrevTo):
			pTot++
			if accepted {
				pAcc++
			}
		}
		if withinWindow(c.doneAt, w.SparkFrom, w.To) {
			if b := weekBucket(firstReviewAt(evs), w.SparkFrom); b >= 0 && b < 7 {
				wTot[b]++
				if accepted {
					wAcc[b]++
				}
			}
		}
	}
	var f Figure
	f.Value, f.Has = metricRatio(acc, tot)
	f.Prev, f.PrevHas = metricRatio(pAcc, pTot)
	for b := range f.Weeks {
		f.Weeks[b], _ = metricRatio(wAcc[b], wTot[b])
	}
	return f, nil
}

// Metrics assembles the report every figure function fills one field of.
func (s *PGStore) Metrics(ctx context.Context, repository string, from, to time.Time) (MetricsReport, error) {
	db, w := s.db, newMetricWindow(from, to)
	r := MetricsReport{Repository: repository, From: w.From, To: w.To}
	var err error
	if r.Deploys, err = metricDeploys(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.LeadTime, err = metricLeadTime(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.CardCycle, err = metricCardCycle(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.Throughput, err = metricThroughput(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.ChangeFailure, err = metricChangeFailure(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.RevertedRefiled, err = metricRevertedRefiled(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.TimeToRestore, err = metricTimeToRestore(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.Spend, err = metricSpend(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.PerCard, err = metricPerCard(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.TokensPerCard, err = metricTokensPerCard(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.ModelMix, err = metricModelMix(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.Interventions, err = metricInterventions(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.Escalations, err = metricEscalations(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.HumanBlocked, err = metricHumanBlocked(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.ReviewAcceptance, err = metricReviewAcceptance(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.SelfDeploys, err = metricSelfDeploys(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.DiffSize, err = metricDiffSize(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.ChatWork, err = metricChatWork(ctx, db, repository, w); err != nil {
		return r, err
	}
	if r.CIDuration, err = metricCIDuration(ctx, db, repository, w); err != nil {
		return r, err
	}
	return r, nil
}
