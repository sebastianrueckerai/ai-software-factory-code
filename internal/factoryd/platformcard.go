package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// SourcePlatformDeaths is the source of the human-lane card factoryd files
// when the platform ends sessions again and again.
const SourcePlatformDeaths = "platform_deaths"

// platformDeathsFile platform deaths within platformDeathsWindow, across
// every card, file the card; its face lists the platformDeathsShown newest
// since platformDeathsWindow before its filing.
const (
	platformDeathsFile   = 3
	platformDeathsWindow = 24 * time.Hour
	platformDeathsShown  = 10
)

// PlatformCardStore is the store the platform card reads and writes.
type PlatformCardStore interface {
	humanCardStore
	PlatformDeaths(ctx context.Context, since time.Time) ([]PodDeath, error)
	PlatformIncidents(ctx context.Context, from, to time.Time, limit int) ([]PlatformIncident, error)
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
// platformDeathsWindow, those a start of the whole cluster explains aside
// (withoutColdStarts), and keeps the open card's face on every death since
// platformDeathsWindow before its filing: while it is open no second card
// is filed, and a death at or before the end of the newest closed card
// counts no more. No agent works the card, since a fix of the nodes needs
// their admin key.
type PlatformCard struct {
	Store   PlatformCardStore
	Forgejo *Forgejo
	// Repo is the configuration repository, where a card whose deaths hit
	// no card files.
	Repo Repo
	// Timeline records the card's card_created; nil records none.
	Timeline *Timeline
	// Kube lists the nodes the cluster has.
	Kube Kube
	Now  func() time.Time
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
	since := w.Now().Add(-platformDeathsWindow)
	if open != nil && open.CreatedAt.Add(-platformDeathsWindow).Before(since) {
		since = open.CreatedAt.Add(-platformDeathsWindow)
	}
	deaths, err := w.Store.PlatformDeaths(ctx, since)
	if err != nil {
		return err
	}
	deaths = slices.DeleteFunc(deaths, func(d PodDeath) bool { return !d.Record.At.After(ended) })
	if deaths, err = w.withoutColdStarts(ctx, deaths); err != nil {
		return err
	}
	if open == nil && len(deaths) < platformDeathsFile {
		return nil
	}
	// an open card names the cards of the deaths it shows; a filing
	// searches every repository its deaths hit for its stranded issue
	shown := deaths
	if open != nil {
		shown = deaths[:min(len(deaths), platformDeathsShown)]
	}
	hit := map[string]Task{}
	for _, d := range shown {
		if _, read := hit[d.TaskID]; d.TaskID == "" || read {
			continue
		}
		if hit[d.TaskID], _, err = w.Store.Task(ctx, d.TaskID); err != nil {
			return err
		}
	}
	if open != nil {
		info := platformDeathsInfo(deaths, hit, open.Repository)
		if face := IssueObjective(platformCardTitle, info); face != open.Objective {
			return w.Store.FacePlatformCard(ctx, open.ID, face, info)
		}
		return nil
	}
	repo, err := w.fileRepo(ctx, deaths, hit)
	if err != nil {
		return err
	}
	q := PlatformDeathsQuestion(deaths, hit, repo.String())
	t := Task{ID: NewTaskID(), RiskClass: "standard", Source: SourcePlatformDeaths, Author: w.Forgejo.BotUser, CreatedAt: w.Now()}
	got, _, err := FileHumanCard(ctx, w.Store, w.Forgejo, repo, BriefText{Title: q.Ask, Why: q.Why, Body: q.Info}, []string{"human"}, t,
		HumanAsk{Q: q, By: "factoryd", Asker: SourcePlatformDeaths, Via: ViaFactoryd})
	if err != nil {
		return err
	}
	w.Timeline.recordEvent(ctx, got.ID, EventCardCreated, map[string]any{"by": cmp.Or(got.Author, w.Forgejo.BotUser), "issue": got.IssueNumber})
	log.Printf("platform card: %s filed #%d for %d platform deaths", repo, got.IssueNumber, len(deaths))
	return nil
}

// coldStartSpread is how close together the boots of every node lie when
// the whole cluster starts, as after the human's factory-off and
// factory-on.
const coldStartSpread = 10 * time.Minute

// withoutColdStarts leaves out the deaths a start of the whole cluster
// explains: a node that restarted, or a pod gone after factoryd's restart,
// in a life that spans one. Each node the cluster has boots within
// coldStartSpread of the others there, by the incident log's first sight
// of each boot, which a death read on factoryd's own start may precede by
// as much. The records stay platform deaths; they file no card.
func (w *PlatformCard) withoutColdStarts(ctx context.Context, deaths []PodDeath) ([]PodDeath, error) {
	explained := func(d PodDeath) bool {
		return d.Record.Rule == RuleNodeRebooted || d.Record.Rule == RuleGoneAfterRestart
	}
	began := func(r DeathRecord) time.Time {
		if r.Started.IsZero() {
			return r.At
		}
		return r.Started
	}
	if !slices.ContainsFunc(deaths, explained) {
		return deaths, nil
	}
	from := w.Now()
	for _, d := range deaths {
		if explained(d) {
			from = earliest(from, began(d.Record))
		}
	}
	nodes, err := w.Kube.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	incidents, err := w.Store.PlatformIncidents(ctx, from.Add(-coldStartSpread), w.Now(), 0)
	if err != nil {
		return nil, err
	}
	boots := map[string][]time.Time{}
	for _, p := range incidents {
		if p.Kind == IncidentNodeBoot {
			boots[p.Subject] = append(boots[p.Subject], p.At)
		}
	}
	// a start of the whole cluster runs from a boot to the last boot of
	// every node within coldStartSpread after it
	type span struct{ first, last time.Time }
	var starts []span
	for _, ats := range boots {
		for _, at := range ats {
			c, all := span{at, at}, len(nodes) > 0
			for _, n := range nodes {
				i := slices.IndexFunc(boots[n.Name], func(b time.Time) bool { return !b.Before(at) && !b.After(at.Add(coldStartSpread)) })
				if i < 0 {
					all = false
					break
				}
				if b := boots[n.Name][i]; b.After(c.last) {
					c.last = b
				}
			}
			if all {
				starts = append(starts, c)
			}
		}
	}
	return slices.DeleteFunc(deaths, func(d PodDeath) bool {
		return explained(d) && slices.ContainsFunc(starts, func(c span) bool {
			return !began(d.Record).After(c.last) && !c.first.After(d.Record.At.Add(coldStartSpread))
		})
	}), nil
}

// platformCardTitle is the platform card's title and ask, the same at
// every filing, so a filing whose row failed finds its issue by it.
var platformCardTitle = SourceBriefs[SourcePlatformDeaths](BriefFacts{}).Title

// fileRepo is the repository a new card files on: the first of the
// repositories the deaths' cards stand on, newest first, then the
// configuration repository, that holds the bot's open issue of the card's
// title with no card, which a filing whose row failed left; else the
// first of them whose search answered. A repository whose search fails is
// passed over, and only when every search fails is the filing refused.
func (w *PlatformCard) fileRepo(ctx context.Context, deaths []PodDeath, hit map[string]Task) (Repo, error) {
	var repos []Repo
	for _, d := range deaths {
		name := hit[d.TaskID].Repository
		if name == "" || slices.ContainsFunc(repos, func(r Repo) bool { return r.String() == name }) {
			continue
		}
		r, err := SplitRepo(name)
		if err != nil {
			return Repo{}, err
		}
		repos = append(repos, r)
	}
	if !slices.Contains(repos, w.Repo) {
		repos = append(repos, w.Repo)
	}
	var readable []Repo
	var errs []error
	for _, r := range repos {
		n, err := strandedIssue(ctx, w.Store, w.Forgejo, r, platformCardTitle)
		switch {
		case err != nil:
			log.Printf("platform card: %s is passed over for the filing: %s", r, maskedErr(err))
			errs = append(errs, err)
		case n != 0:
			return r, nil
		default:
			readable = append(readable, r)
		}
	}
	if len(readable) == 0 {
		return Repo{}, errors.Join(errs...)
	}
	return readable[0], nil
}

// PlatformDeathsQuestion is the platform card's ask at its filing over
// deaths, newest first, on repository repo; hit holds the cards they hit by
// id. Its story names the deaths' span, which later deaths leave true.
func PlatformDeathsQuestion(deaths []PodDeath, hit map[string]Task, repo string) HumanQuestion {
	b := SourceBriefs[SourcePlatformDeaths](BriefFacts{})
	q := HumanQuestion{Ask: b.Title, Why: b.Why, Info: platformDeathsInfo(deaths, hit, repo), Choices: choices(ActClose+"/"+variantNoted, KeyPark)}
	if len(deaths) > 0 {
		span := "at " + dayStamp(deaths[0].Record.At)
		if oldest := dayStamp(deaths[len(deaths)-1].Record.At); oldest != dayStamp(deaths[0].Record.At) {
			span = "between " + oldest + " and " + dayStamp(deaths[0].Record.At)
		}
		q.Happened = "The platform ended the factory's work " + textutil.Plural(len(deaths), "time", "times") + " " + span + "."
	}
	return q
}

// dayStamp is at in UTC with its day, "23 Nov 14:02 UTC".
func dayStamp(at time.Time) string { return at.UTC().Format("2 Jan 15:04") + " UTC" }

// platformDeathsInfo is the platform card's face on repository repo: the
// count of deaths and the newest platformDeathsShown, one a line.
func platformDeathsInfo(deaths []PodDeath, hit map[string]Task, repo string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The platform ended the factory's work %s, newest first:\n\n", textutil.Plural(len(deaths), "time", "times"))
	for _, d := range deaths[:min(len(deaths), platformDeathsShown)] {
		b.WriteString("- " + platformDeathLine(d, hit[d.TaskID], repo) + "\n")
	}
	if more := len(deaths) - platformDeathsShown; more > 0 {
		fmt.Fprintf(&b, "- and %d more\n", more)
	}
	b.WriteString("\nWhile this card is open, its list in the console grows with each session, CI job or gate pod the platform ends, " +
		"and no second card is filed. No agent works it.")
	return b.String()
}

// platformDeathLine is death d as the card on repository repo lists it;
// card is the card it hit, a card of another repository named with its
// repository.
func platformDeathLine(d PodDeath, card Task, repo string) string {
	who := "a CI job"
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
	return dayStamp(d.Record.At) + ", " + who + ": " + platformCause(d.Record)
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
