package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SourcePlatformDeaths is the source of the human-lane card factoryd files
// when the platform ends sessions again and again.
const SourcePlatformDeaths = "platform_deaths"

// platformDeathsFile platform deaths within platformDeathsWindow, across
// every card, file the card; its face lists the platformDeathsShown newest.
const (
	platformDeathsFile   = 3
	platformDeathsWindow = 24 * time.Hour
	platformDeathsShown  = 10
)

// PlatformCardStore is the store the platform card reads and writes.
type PlatformCardStore interface {
	humanCardStore
	PlatformDeaths(ctx context.Context, since time.Time) ([]PodDeath, error)
	// PlatformCards lists the platform cards, every repository's in any
	// status, newest first.
	PlatformCards(ctx context.Context) ([]Task, error)
	// FacePlatformCard writes objective on platform card id and info on
	// its newest ask, in one transaction.
	FacePlatformCard(ctx context.Context, id, objective, info string) error
	Task(ctx context.Context, id string) (Task, bool, error)
}

// PlatformCard files one card in the human's lane when the platform ends
// platformDeathsFile sessions, CI jobs or gate pods within
// platformDeathsWindow, and keeps the open card's face on the deaths of
// the window: while it is open no second card is filed, and a death at or
// before the end of the newest closed card counts no more. No agent works
// the card, since a fix of the nodes needs their admin key.
type PlatformCard struct {
	Store   PlatformCardStore
	Forgejo *Forgejo
	// Repo is the configuration repository, where a card whose deaths hit
	// no card files.
	Repo Repo
	// Timeline records the card's card_created; nil records none.
	Timeline *Timeline
	Now      func() time.Time
}

func (w *PlatformCard) Check(ctx context.Context) error {
	cards, err := w.Store.PlatformCards(ctx)
	if err != nil {
		return err
	}
	var open *Task
	var ended time.Time
	for i, c := range cards {
		switch {
		case slices.Contains(humanCardWaits, c.Status):
			open = cmp.Or(open, &cards[i])
		case c.FinishedAt != nil && c.FinishedAt.After(ended):
			ended = *c.FinishedAt
		}
	}
	deaths, err := w.Store.PlatformDeaths(ctx, w.Now().Add(-platformDeathsWindow))
	if err != nil {
		return err
	}
	deaths = slices.DeleteFunc(deaths, func(d PodDeath) bool { return !d.Record.At.After(ended) })
	if open == nil && len(deaths) < platformDeathsFile {
		return nil
	}
	hit := map[string]Task{}
	for _, d := range deaths {
		if _, read := hit[d.TaskID]; d.TaskID == "" || read {
			continue
		}
		if hit[d.TaskID], _, err = w.Store.Task(ctx, d.TaskID); err != nil {
			return err
		}
	}
	if open != nil {
		title, _, _ := strings.Cut(open.Objective, "\n\n")
		info := platformDeathsInfo(deaths, hit, open.Repository)
		if face := IssueObjective(title, info); face != open.Objective {
			return w.Store.FacePlatformCard(ctx, open.ID, face, info)
		}
		return nil
	}
	repo := w.Repo
	if i := slices.IndexFunc(deaths, func(d PodDeath) bool { return hit[d.TaskID].Repository != "" }); i >= 0 {
		if repo, err = SplitRepo(hit[deaths[i].TaskID].Repository); err != nil {
			return err
		}
	}
	q := PlatformDeathsQuestion(deaths, hit, repo.String())
	t := Task{ID: NewTaskID(), RiskClass: "standard", Source: SourcePlatformDeaths, Author: w.Forgejo.BotUser}
	got, _, err := FileHumanCard(ctx, w.Store, w.Forgejo, repo, BriefText{Title: q.Ask, Why: q.Why, Body: q.Info}, []string{"human"}, t,
		HumanAsk{Q: q, By: "factoryd", Asker: SourcePlatformDeaths, Via: ViaFactoryd})
	if err != nil {
		return err
	}
	w.Timeline.recordEvent(ctx, got.ID, EventCardCreated, map[string]any{"by": cmp.Or(got.Author, w.Forgejo.BotUser), "issue": got.IssueNumber})
	log.Printf("platform card: %s filed #%d for %d platform deaths", repo, got.IssueNumber, len(deaths))
	return nil
}

// PlatformDeathsQuestion is the platform card's ask over deaths, newest
// first, on repository repo; hit holds the cards they hit by id.
func PlatformDeathsQuestion(deaths []PodDeath, hit map[string]Task, repo string) HumanQuestion {
	b := SourceBriefs[SourcePlatformDeaths](BriefFacts{Name: strconv.Itoa(len(deaths))})
	q := HumanQuestion{Ask: b.Title, Why: b.Why, Info: platformDeathsInfo(deaths, hit, repo), Choices: choices(ActClose+"/"+variantNoted, KeyPark)}
	if len(deaths) > 0 {
		q.Happened = "The newest, at " + clock(deaths[0].Record.At) + " UTC: " + platformCause(deaths[0].Record) + "."
	}
	return q
}

// platformDeathsInfo is the platform card's face on repository repo: the
// newest platformDeathsShown deaths, one a line.
func platformDeathsInfo(deaths []PodDeath, hit map[string]Task, repo string) string {
	var b strings.Builder
	if len(deaths) == 0 {
		b.WriteString("The platform ended nothing in the last 24 hours.\n")
	} else {
		b.WriteString("The platform ended these in the last 24 hours, newest first:\n\n")
	}
	for _, d := range deaths[:min(len(deaths), platformDeathsShown)] {
		b.WriteString("- " + platformDeathLine(d, hit[d.TaskID], repo) + "\n")
	}
	if more := len(deaths) - platformDeathsShown; more > 0 {
		fmt.Fprintf(&b, "- and %d more\n", more)
	}
	b.WriteString("\nThis card updates when the platform ends another session, and no second card is filed while it is open. " +
		"No agent works it: a fix of the nodes needs their admin key, which no agent holds.")
	return b.String()
}

// platformDeathLine is death d as the card on repository repo lists it;
// card is the card it hit, a card of another repository named with its
// repository.
func platformDeathLine(d PodDeath, card Task, repo string) string {
	who := "CI job " + d.Runner
	if d.Kind != "ci" {
		switch {
		case card.IssueNumber == 0:
			who = "a card"
		case card.Repository != repo:
			who = fmt.Sprintf("card %s#%d", card.Repository, card.IssueNumber)
		default:
			who = fmt.Sprintf("card #%d", card.IssueNumber)
		}
		if d.Kind == "gate" {
			who = "a gate of " + who
		}
	}
	return clock(d.Record.At) + " UTC, " + who + ": " + platformCause(d.Record)
}

// platformCause is what ended the pod, on its node when the record names
// one.
func platformCause(r DeathRecord) string {
	if r.Node == "" {
		return r.Short()
	}
	return r.Short() + " on " + r.Node
}

// PlatformCards lists the platform cards, every repository's in any
// status, newest first.
func (s *PGStore) PlatformCards(ctx context.Context) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks WHERE source = $1 ORDER BY created_at DESC, id DESC`, SourcePlatformDeaths)
}

// FacePlatformCard writes objective on platform card id and info on the
// card's newest ask of its own, in one transaction. A card that is no
// platform card is ErrNotFound.
func (s *PGStore) FacePlatformCard(ctx context.Context, id, objective, info string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, sourceFaceSQL, id, objective, SourcePlatformDeaths)
	if err := oneRow(res, err, "platform card "+id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, askInfoSQL, id, info, EventQuestionAsked, SourcePlatformDeaths); err != nil {
		return err
	}
	return tx.Commit()
}
