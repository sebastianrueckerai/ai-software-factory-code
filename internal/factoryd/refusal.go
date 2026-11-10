package factoryd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// StateRefused is the state label of an issue the intake made no card
// for: the heal pass leaves it be until the issue is edited or reopened.
const StateRefused = "state/refused"

// Refusal is the insert's refusal of a card whose issue or roadmap item a
// card already holds: the intake tells the issue why, once.
type Refusal struct {
	Kind        string // "issue" or "roadmap"
	Holder      string
	Status      string // the holder's
	HolderIssue int64  // the roadmap holder's issue
	Item        int
	IssueNo     int64
	Repository  string
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

// Key tells one reason from another on the same issue: the holder and
// how it holds the item, since the way forward differs by that and by
// nothing else.
func (r *Refusal) Key() string { return r.Kind + " " + r.Holder + " " + RoadmapHold(r.Status) }

// RoadmapHold is how a card holds a roadmap item: "open" while it works,
// "ended" once closed or failed, "built" once its change is in main
// (merged or done).
func RoadmapHold(status string) string {
	switch status {
	case "closed", "failed":
		return "ended"
	case "done", "merged":
		return "built"
	}
	return "open"
}

// IntakeRefusal is a recorded refusal: its key, the text the issue got,
// when it was last refused and the hash of the issue's text then.
type IntakeRefusal struct {
	Key, Reason, BodySHA string
	At                   time.Time
}

// IssueSHA is the hash of an issue's title and body, to tell an edit.
func IssueSHA(title, body string) string {
	sum := sha256.Sum256([]byte(IssueObjective(title, body)))
	return hex.EncodeToString(sum[:])
}

var refileOf = regexp.MustCompile(`(?im)^[ \t]*(?:[-*][ \t]+)?refile of #(\d+)\b`)

// AskedRefile is the number in the body's last line "Refile of #<n>", the
// closed or failed card whose roadmap item this issue takes over; 0
// without one. The last one wins: a line copied from an older issue does
// not hide the one the filer adds below it.
func AskedRefile(body string) int64 {
	all := refileOf.FindAllStringSubmatch(body, -1)
	if all == nil {
		return 0
	}
	n, _ := strconv.ParseInt(all[len(all)-1][1], 10, 64)
	return n
}

// RecordRefusal keeps reason as issue's refusal under key, with the hash
// of the issue's text; a second record of the key holds the current
// reason, time and hash.
func (s *PGStore) RecordRefusal(ctx context.Context, repository string, issue int64, key, reason, bodySHA string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO intake_refusals (repository, issue_number, key, reason, body_sha)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (repository, issue_number, key) DO UPDATE SET at = now(), body_sha = EXCLUDED.body_sha, reason = EXCLUDED.reason`,
		repository, issue, key, reason, bodySHA)
	return err
}

// IntakeRefusals are issue's recorded refusals, the least recently refused first.
func (s *PGStore) IntakeRefusals(ctx context.Context, repository string, issue int64) ([]IntakeRefusal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, reason, body_sha, at FROM intake_refusals
		WHERE repository = $1 AND issue_number = $2 ORDER BY at, key`, repository, issue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntakeRefusal
	for rows.Next() {
		var r IntakeRefusal
		if err := rows.Scan(&r.Key, &r.Reason, &r.BodySHA, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
