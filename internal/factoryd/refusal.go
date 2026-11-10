package factoryd

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// StateRefused is the state label of an issue the intake made no card
// for: the heal pass leaves it be until the issue is edited or reopened.
const StateRefused = "state/refused"

// Refusal is the insert's refusal of a card whose issue or roadmap item a
// card already holds: the intake tells the issue why, once.
type Refusal struct {
	Kind       string // "issue" or "roadmap"
	Holder     string
	Status     string // the holder's, for an issue
	Item       int
	IssueNo    int64
	Repository string
}

const (
	RefusalIssue   = "issue"
	RefusalRoadmap = "roadmap"
)

func (r *Refusal) Error() string {
	if r.Kind == RefusalRoadmap {
		return fmt.Sprintf("card %s already builds roadmap item %d of %s", r.Holder, r.Item, r.Repository)
	}
	return fmt.Sprintf("card %s already holds issue %d of %s and is still %s", r.Holder, r.IssueNo, r.Repository, r.Status)
}

// Key tells one reason from another on the same issue.
func (r *Refusal) Key() string { return r.Kind + " " + r.Holder }

// IntakeRefusal is a recorded refusal: its key and the text the issue got.
type IntakeRefusal struct {
	Key, Reason string
}

var refileOf = regexp.MustCompile(`(?im)^\W*refile of #(\d+)\b`)

// AskedRefile is the number in the body's line "Refile of #<n>", the
// closed or failed card whose roadmap item this issue takes over; 0
// without one.
func AskedRefile(body string) int64 {
	m := refileOf.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// RecordRefusal keeps reason as issue's refusal under key; a second
// record of the same key changes nothing.
func (s *PGStore) RecordRefusal(ctx context.Context, repository string, issue int64, key, reason string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO intake_refusals (repository, issue_number, key, reason)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, repository, issue, key, reason)
	return err
}

// IntakeRefusals are issue's recorded refusals, oldest first.
func (s *PGStore) IntakeRefusals(ctx context.Context, repository string, issue int64) ([]IntakeRefusal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, reason FROM intake_refusals
		WHERE repository = $1 AND issue_number = $2 ORDER BY at, key`, repository, issue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntakeRefusal
	for rows.Next() {
		var r IntakeRefusal
		if err := rows.Scan(&r.Key, &r.Reason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
