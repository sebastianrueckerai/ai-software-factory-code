package policy

import (
	"context"
	"database/sql"
	"time"
)

// AliasSwitch is one row of alias_switches: the gateway moved Alias to Twin
// (Kind SwitchOn) or back (SwitchReturn) at At, because the rate of the
// primary deployment Deployment was Rate seconds per 1000 output tokens.
type AliasSwitch struct {
	Alias, Twin, Kind string
	At                time.Time
	Rate              float64
	Deployment        string
}

// The kinds of an alias switch.
const (
	SwitchOn     = "switch"
	SwitchReturn = "return"
)

// LatencySample is one successful model call: what answered it and how long
// it took for how many output tokens.
type LatencySample struct {
	Alias, Deployment string
	OutputTokens      int
	DurationMS        int
}

// LatencySamples reads the successful model calls of aliases since since,
// those a deployment answered.
func LatencySamples(ctx context.Context, db *sql.DB, aliases []string, since time.Time) ([]LatencySample, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT model_alias, deployment, output_tokens, duration_ms FROM audit
		 WHERE model_alias = ANY($1) AND ts >= $2 AND tool = '' AND decision = 'allowed' AND deployment <> '' AND status LIKE '2%'`,
		aliases, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LatencySample
	for rows.Next() {
		var s LatencySample
		if err := rows.Scan(&s.Alias, &s.Deployment, &s.OutputTokens, &s.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RecordAliasSwitch stores sw unless the alias's newest row already has its
// kind (a second gateway replica recording the same decision).
func RecordAliasSwitch(ctx context.Context, db *sql.DB, sw AliasSwitch) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO alias_switches (alias, twin, kind, rate_s_per_1000, deployment)
		 SELECT $1, $2, $3, $4, $5
		 WHERE COALESCE((SELECT kind FROM alias_switches WHERE alias = $1 ORDER BY id DESC LIMIT 1), $6) <> $3`,
		sw.Alias, sw.Twin, sw.Kind, sw.Rate, sw.Deployment, SwitchReturn)
	return err
}

// OpenAliasSwitches are the aliases whose newest switch row is a switch, by
// alias.
func OpenAliasSwitches(ctx context.Context, db *sql.DB) ([]AliasSwitch, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT alias, twin, kind, at, rate_s_per_1000, deployment FROM (
		   SELECT DISTINCT ON (alias) alias, twin, kind, at, rate_s_per_1000, deployment FROM alias_switches ORDER BY alias, id DESC
		 ) newest WHERE kind = $1 ORDER BY alias`, SwitchOn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AliasSwitch
	for rows.Next() {
		var s AliasSwitch
		if err := rows.Scan(&s.Alias, &s.Twin, &s.Kind, &s.At, &s.Rate, &s.Deployment); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
