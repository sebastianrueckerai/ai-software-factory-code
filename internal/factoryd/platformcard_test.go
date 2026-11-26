package factoryd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// platformStore is the fakeStore with the platform card's reads: the
// cluster store's deaths, and the platform cards with their faces.
type platformStore struct {
	*fakeStore
	*fakeClusterStore
	// faces counts the face writes
	faces int
	// failCards is how many card rows still fail, after their issue
	failCards int
	// taskReads counts the card reads
	taskReads int
}

func (s *platformStore) PlatformDeaths(ctx context.Context, since time.Time) ([]PodDeath, error) {
	return s.fakeClusterStore.PlatformDeaths(ctx, since)
}

func (s *platformStore) Task(ctx context.Context, id string) (Task, bool, error) {
	s.taskReads++
	return s.fakeStore.Task(ctx, id)
}

func (s *platformStore) PlatformCards(ctx context.Context) ([]Task, error) {
	var out []Task
	for _, t := range s.tasks {
		if t.Source == SourcePlatformDeaths {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b Task) int { return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(b.ID, a.ID)) })
	return out, nil
}

func (s *platformStore) FacePlatformCard(ctx context.Context, id, objective, info string, said Moments) error {
	c := s.tasks[id]
	if c.Source != SourcePlatformDeaths {
		return fmt.Errorf("platform card %s: %w", id, ErrNotFound)
	}
	s.faces++
	c.Objective, c.Moments = objective, said.In(objective)
	s.tasks[id] = c
	return s.SetAskInfo(ctx, id, SourcePlatformDeaths, info)
}

func (s *platformStore) CreateHumanCard(ctx context.Context, t Task, ask HumanAsk) (Task, bool, error) {
	if s.failCards > 0 {
		s.failCards--
		return Task{}, false, errors.New("database gone")
	}
	return s.fakeStore.CreateHumanCard(ctx, t, ask)
}

// platformRig is a platform card over a fresh store at *now, its Forgejo
// and two work cards: #12 on sf/acme and #7 on sf/other.
func platformRig(t *testing.T, now *time.Time) (*PlatformCard, *platformStore, *alertForgejo) {
	t.Helper()
	store := &platformStore{fakeStore: newFakeStore(), fakeClusterStore: newFakeClusterStore()}
	store.chainNow = func() time.Time { return *now }
	for _, c := range []Task{{ID: "t-acme", Repository: "sf/acme", IssueNumber: 12}, {ID: "t-other", Repository: "sf/other", IssueNumber: 7}} {
		c.Kind, c.Status, c.Track = "agent", "running", policy.TrackDirect
		if err := store.CreateTask(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	forgejo, fj := newAlertForgejo(t)
	kube := newFakeKube()
	kube.nodeStates = map[string]NodeState{}
	for _, n := range []string{"sf-1", "sf-2", "sf-3"} {
		kube.nodeStates[n] = NodeState{Name: n, BootID: n + "-b", Ready: true}
	}
	return &PlatformCard{Store: store, Forgejo: forgejo, Repo: Repo{Owner: "sf", Name: "ai-software-factory-infra"}, Kube: kube,
		Now: func() time.Time { return *now }}, store, fj
}

// nov is day d of November 2026 at hh:mm UTC.
func nov(d, hh, mm int) time.Time { return time.Date(2026, 11, d, hh, mm, 0, 0, time.UTC) }

func sessionDeath(task, session string, when time.Time, rule DeathRule, node string) PodDeath {
	return PodDeath{Kind: "session", TaskID: task, SessionID: session, Record: DeathRecord{Class: DeathPlatform, Rule: rule, At: when, Node: node}}
}

func ciDeath(runner string, when time.Time, rule DeathRule, node string) PodDeath {
	return PodDeath{Kind: "ci", Runner: runner, Record: DeathRecord{Class: DeathPlatform, Rule: rule, At: when, Node: node}}
}

func gateDeath(task, uid string, when time.Time, rule DeathRule, node string) PodDeath {
	return PodDeath{Kind: "gate", TaskID: task, Record: DeathRecord{Class: DeathPlatform, Rule: rule, At: when, Node: node, PodUID: uid}}
}

func putDeaths(t *testing.T, store *platformStore, deaths ...PodDeath) {
	t.Helper()
	for _, d := range deaths {
		if err := store.PutPodDeath(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
}

// platformCards are the store's platform cards, newest first.
func platformCards(t *testing.T, store *platformStore) []Task {
	t.Helper()
	cards, err := store.PlatformCards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cards
}

// platformAsks are card id's asks, oldest first.
func platformAsks(store *platformStore, id string) []map[string]any {
	var out []map[string]any
	for _, e := range store.events {
		if e.TaskID == id && e.Kind == EventQuestionAsked && e.Detail["source"] == SourcePlatformDeaths {
			out = append(out, e.Detail)
		}
	}
	return out
}

// platformHead opens the face of n deaths; platformTail closes every face.
func platformHead(n int) string {
	return fmt.Sprintf("The platform ended the factory's work %d times, newest first:\n\n", n)
}

const (
	platformTitle = "The platform keeps ending the factory's work. Check the nodes."
	platformTail  = "\n\nWhile this card is open, its list in the console grows with each session, CI job or gate pod the platform ends, " +
		"and no second card is filed. No agent works it."
)

// The three deaths of the rig's first card, newest first: a CI job's at
// 14:02, a gate pod of #12 at 13:30 and a session of sf/other's #7 at
// 13:10, on 23 November.
var (
	firstCI      = ciDeath("ci-job-42", nov(23, 14, 2), RuleGuardKill, "sf-2")
	firstGate    = gateDeath("t-acme", "uid-g1", nov(23, 13, 30), RuleNodeRebooted, "sf-1")
	firstSession = sessionDeath("t-other", "s1", nov(23, 13, 10), RuleNodeOOM, "sf-1")
	firstLines   = "- 23 Nov 14:02 UTC, a CI job: the node's memory guard killed the pod on sf-2\n" +
		"- 23 Nov 13:30 UTC, a gate of card #12: the node restarted on sf-1\n" +
		"- 23 Nov 13:10 UTC, card sf/other#7: the node ran out of memory on sf-1"
	firstHappened = "The platform ended the factory's work 3 times between 23 Nov 13:10 UTC and 23 Nov 14:02 UTC."
)

// Hand-derived: at 15:00 a session death of 25 hours ago lies outside the
// window, so two deaths in it file nothing. The third, a gate pod of #12,
// files one card at once, with its issue, on sf/acme: the newest death, a
// CI job's, hit no card, and the next newest hit #12 of sf/acme. Its face
// lists the three newest first, each with its day, and #7 of another
// repository by its repository; its story names the three's span. A
// second pass files nothing more. Three CI deaths alone file on the
// configuration repository.
func TestPlatformCardFilesAtThree(t *testing.T) {
	ctx := context.Background()
	now := nov(23, 15, 0)
	w, store, fj := platformRig(t, &now)
	putDeaths(t, store, sessionDeath("t-acme", "s0", now.Add(-25*time.Hour), RuleGuardKill, "sf-1"), firstSession, firstCI)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 0 || len(platformCards(t, store)) != 0 {
		t.Fatalf("two deaths filed %d issues and %d cards, want none", len(fj.issues), len(platformCards(t, store)))
	}
	putDeaths(t, store, firstGate)
	for range 2 {
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	cards := platformCards(t, store)
	if len(fj.issues) != 1 || len(cards) != 1 {
		t.Fatalf("three deaths filed %d issues and %d cards, want one of each", len(fj.issues), len(cards))
	}
	card := cards[0]
	info := platformHead(3) + firstLines + platformTail
	if card.Kind != "human" || card.Status != "waiting-human" || card.Repository != "sf/acme" || card.IssueNumber != 1 || fj.repos[0] != "sf/acme" {
		t.Fatalf("the card = %+v on %s, want a human card waiting on a human with issue #1 of sf/acme", card, fj.repos[0])
	}
	if card.Objective != platformTitle+"\n\n"+info {
		t.Errorf("the card's face =\n%s\nwant\n%s", card.Objective, platformTitle+"\n\n"+info)
	}
	if !strings.HasPrefix(fj.issues[0], platformTitle+"\n\n") || !strings.Contains(fj.issues[0], info) {
		t.Errorf("the issue =\n%s\nwant the title and the face", fj.issues[0])
	}
	if got := fj.labels[1]; !slices.Equal(got, []string{"human", "state/waiting-human"}) {
		t.Errorf("labels = %v", got)
	}
	if asks := platformAsks(store, card.ID); len(asks) != 1 || asks[0]["info"] != info || asks[0]["happened"] != firstHappened {
		t.Errorf("the card's asks = %v, want one that knows the face and that %q", asks, firstHappened)
	}

	// three CI deaths alone file on the configuration repository
	w, store, fj = platformRig(t, &now)
	putDeaths(t, store, ciDeath("ci-job-1", nov(23, 14, 0), RuleNodeOOM, "sf-3"), ciDeath("ci-job-2", nov(23, 14, 1), RuleNodeOOM, "sf-3"),
		ciDeath("ci-job-3", nov(23, 14, 2), RuleNodeOOM, ""))
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	cards = platformCards(t, store)
	want := platformHead(3) + "- 23 Nov 14:02 UTC, a CI job: the node ran out of memory\n" +
		"- 23 Nov 14:01 UTC, a CI job: the node ran out of memory on sf-3\n" +
		"- 23 Nov 14:00 UTC, a CI job: the node ran out of memory on sf-3" + platformTail
	if len(cards) != 1 || cards[0].Repository != "sf/ai-software-factory-infra" || len(fj.repos) != 1 || fj.repos[0] != "sf/ai-software-factory-infra" ||
		cards[0].Objective != platformTitle+"\n\n"+want {
		t.Fatalf("three CI deaths filed %+v on %v, want one card on sf/ai-software-factory-infra with the face\n%s", cards, fj.repos, want)
	}
}

// A start of the whole cluster, every node the cluster has booting within
// 10 minutes of the others, explains the sessions it ended: their nodes
// restarted, or their pods were gone after factoryd's restart. They file no
// card however many they are. Restarts of one node whose peers stayed up,
// of every node over more than 10 minutes, or other deaths beside a cold
// start still file it.
func TestPlatformCardLeavesOutAColdStart(t *testing.T) {
	ctx := context.Background()
	boots := func(at ...time.Time) []PlatformIncident {
		var out []PlatformIncident
		for i, n := range []string{"sf-1", "sf-2", "sf-3"} {
			out = append(out, PlatformIncident{Key: "boot/" + n + "/old", At: nov(20, 9, 0), Kind: IncidentNodeBoot, Subject: n, Node: n})
			if i < len(at) {
				out = append(out, PlatformIncident{Key: "boot/" + n + "/new", At: at[i], Kind: IncidentNodeBoot, Subject: n, Node: n})
			}
		}
		return out
	}
	died := func(session string, at time.Time, rule DeathRule) PodDeath {
		d := sessionDeath("t-acme", session, at, rule, "sf-1")
		d.Record.Started = nov(23, 12, 0)
		return d
	}
	coldStart := []PodDeath{died("s1", nov(23, 13, 6), RuleNodeRebooted), died("s2", nov(23, 13, 7), RuleGoneAfterRestart),
		died("s3", nov(23, 13, 12), RuleNodeRebooted), died("s4", nov(23, 13, 40), RuleGoneAfterRestart)}
	guard := []PodDeath{died("g1", nov(23, 14, 0), RuleGuardKill), died("g2", nov(23, 14, 1), RuleGuardKill), died("g3", nov(23, 14, 2), RuleGuardKill)}
	for name, tc := range map[string]struct {
		boots  []PlatformIncident
		deaths []PodDeath
		filed  int
	}{
		"every node within 10 minutes":        {boots(nov(23, 13, 1), nov(23, 13, 5), nov(23, 13, 10)), coldStart, 0},
		"one node whose peers stayed up":      {boots(nov(23, 13, 5)), coldStart[:3], 1},
		"every node over half an hour":        {boots(nov(23, 13, 0), nov(23, 13, 15), nov(23, 13, 30)), coldStart[:3], 1},
		"three guard kills beside cold start": {boots(nov(23, 13, 1), nov(23, 13, 5), nov(23, 13, 10)), slices.Concat(coldStart, guard), 1},
	} {
		t.Run(name, func(t *testing.T) {
			now := nov(23, 15, 0)
			w, store, _ := platformRig(t, &now)
			if _, err := store.AddPlatformIncidents(ctx, tc.boots); err != nil {
				t.Fatal(err)
			}
			putDeaths(t, store, tc.deaths...)
			if err := w.Check(ctx); err != nil {
				t.Fatal(err)
			}
			if cards := platformCards(t, store); len(cards) != tc.filed {
				t.Fatalf("cards filed %d, want %d", len(cards), tc.filed)
			}
		})
	}
}

// Hand-derived: the open card on sf/acme takes a fourth death, a session
// of sf/other's #7 at 15:05, on its face, newest first, and its ask knows
// the new face, while its story still names the three it was filed for:
// no second card, and the card stays on sf/acme though the newest death
// hit sf/other. A pass with nothing new writes nothing. Twelve deaths list
// the ten newest, then the two more.
func TestPlatformCardUpdatesInPlace(t *testing.T) {
	ctx := context.Background()
	now := nov(23, 15, 0)
	w, store, fj := platformRig(t, &now)
	putDeaths(t, store, firstSession, firstCI, firstGate)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = nov(23, 15, 10)
	putDeaths(t, store, sessionDeath("t-other", "s2", nov(23, 15, 5), RuleDisruption, "sf-3"))
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	cards := platformCards(t, store)
	info := platformHead(4) + "- 23 Nov 15:05 UTC, card sf/other#7: Kubernetes stopped the pod on sf-3\n" + firstLines + platformTail
	if len(fj.issues) != 1 || len(cards) != 1 || cards[0].Repository != "sf/acme" || cards[0].Objective != platformTitle+"\n\n"+info {
		t.Fatalf("after a fourth death: %d issues, cards %+v; want the one card of sf/acme under the face\n%s", len(fj.issues), cards, platformTitle+"\n\n"+info)
	}
	if asks := platformAsks(store, cards[0].ID); len(asks) != 1 || asks[0]["info"] != info || asks[0]["happened"] != firstHappened {
		t.Errorf("the card's asks = %v, want its one ask to know the new face and still that %q", asks, firstHappened)
	}
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if store.faces != 1 {
		t.Errorf("faces written = %d, want 1: a pass with nothing new writes none", store.faces)
	}

	now = nov(23, 15, 30)
	var more []PodDeath
	for i := range 8 {
		more = append(more, ciDeath(fmt.Sprintf("ci-job-%d", 50+i), nov(23, 15, 11+i), RuleGuardKill, "sf-2"))
	}
	putDeaths(t, store, more...)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := 7; i >= 0; i-- {
		lines = append(lines, fmt.Sprintf("- 23 Nov 15:%02d UTC, a CI job: the node's memory guard killed the pod on sf-2", 11+i))
	}
	lines = append(lines, "- 23 Nov 15:05 UTC, card sf/other#7: Kubernetes stopped the pod on sf-3",
		"- 23 Nov 14:02 UTC, a CI job: the node's memory guard killed the pod on sf-2", "- and 2 more")
	info = platformHead(12) + strings.Join(lines, "\n") + platformTail
	if card, _, _ := store.Task(ctx, cards[0].ID); len(fj.issues) != 1 || card.Objective != platformTitle+"\n\n"+info {
		t.Fatalf("twelve deaths: %d issues, the face\n%s\nwant\n%s", len(fj.issues), card.Objective, platformTitle+"\n\n"+info)
	}
}

// Hand-derived: a card filed on Friday 20 November at 15:00 for three
// deaths of that day stays open over the weekend. On Monday at 09:00 the
// three lie more than 24 hours back, yet the open card's face still lists
// them, below a CI job's death of Sunday at 10:00.
func TestPlatformCardKeepsItsDeathsWhileOpen(t *testing.T) {
	ctx := context.Background()
	now := nov(20, 15, 0)
	w, store, fj := platformRig(t, &now)
	putDeaths(t, store, sessionDeath("t-other", "s1", nov(20, 13, 10), RuleNodeOOM, "sf-1"), ciDeath("ci-job-42", nov(20, 14, 2), RuleGuardKill, "sf-2"),
		gateDeath("t-acme", "uid-g1", nov(20, 13, 30), RuleNodeRebooted, "sf-1"))
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	putDeaths(t, store, ciDeath("ci-job-70", nov(22, 10, 0), RuleNodeOOM, "sf-3"))
	now = nov(23, 9, 0)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	cards := platformCards(t, store)
	want := platformTitle + "\n\n" + platformHead(4) + "- 22 Nov 10:00 UTC, a CI job: the node ran out of memory on sf-3\n" +
		"- 20 Nov 14:02 UTC, a CI job: the node's memory guard killed the pod on sf-2\n" +
		"- 20 Nov 13:30 UTC, a gate of card #12: the node restarted on sf-1\n" +
		"- 20 Nov 13:10 UTC, card sf/other#7: the node ran out of memory on sf-1" + platformTail
	if len(fj.issues) != 1 || len(cards) != 1 || cards[0].Objective != want {
		t.Fatalf("on Monday: %d issues, cards %+v; want the one card under\n%s", len(fj.issues), cards, want)
	}
}

// Hand-derived: the card filed at 15:00 for three deaths takes a fourth at
// 15:05 and closes at 15:10. The same four file nothing again, and neither
// does a death at 15:10, the close itself, beside two later ones. The
// third death after the close, at 15:50, files a new card, on sf/acme,
// whose face lists the three later deaths alone. A day later, after one
// more death, the new card's window reaches back past the close, and its
// face lists its own four.
func TestPlatformCardAfterACloseCountsNewDeathsOnly(t *testing.T) {
	ctx := context.Background()
	now := nov(23, 15, 0)
	w, store, fj := platformRig(t, &now)
	putDeaths(t, store, firstSession, firstCI, firstGate)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	first := platformCards(t, store)[0]
	putDeaths(t, store, sessionDeath("t-other", "s2", nov(23, 15, 5), RuleDisruption, "sf-3"))
	now = nov(23, 15, 6)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	now = nov(23, 15, 10)
	if err := store.TransitionTask(ctx, first.ID, humanCardWaits, "done"); err != nil {
		t.Fatal(err)
	}
	now = nov(23, 15, 20)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 {
		t.Fatalf("the closed card's deaths filed %d issues, want the first alone", len(fj.issues))
	}
	putDeaths(t, store, ciDeath("ci-job-60", nov(23, 15, 10), RuleGuardKill, "sf-2"), sessionDeath("t-acme", "s3", nov(23, 15, 30), RuleNodeOOM, "sf-1"),
		ciDeath("ci-job-61", nov(23, 15, 40), RuleGuardKill, "sf-2"))
	now = nov(23, 15, 45)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 1 {
		t.Fatalf("two deaths after the close filed %d issues, want the first alone", len(fj.issues))
	}
	putDeaths(t, store, ciDeath("ci-job-62", nov(23, 15, 50), RuleNodeRebooted, "sf-2"))
	now = nov(23, 15, 55)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	cards := platformCards(t, store)
	want := platformTitle + "\n\n" + platformHead(3) +
		"- 23 Nov 15:50 UTC, a CI job: the node restarted on sf-2\n" +
		"- 23 Nov 15:40 UTC, a CI job: the node's memory guard killed the pod on sf-2\n" +
		"- 23 Nov 15:30 UTC, card #12: the node ran out of memory on sf-1" + platformTail
	if len(fj.issues) != 2 || len(cards) != 2 || cards[0].ID == first.ID || cards[0].Status != "waiting-human" || cards[0].Repository != "sf/acme" ||
		cards[0].Objective != want {
		t.Fatalf("three deaths after the close: %d issues, cards %+v; want a second card on sf/acme under\n%s", len(fj.issues), cards, want)
	}

	putDeaths(t, store, ciDeath("ci-job-63", nov(24, 16, 0), RuleGuardKill, "sf-1"))
	now = nov(24, 16, 30)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	want = platformTitle + "\n\n" + platformHead(4) +
		"- 24 Nov 16:00 UTC, a CI job: the node's memory guard killed the pod on sf-1\n" +
		"- 23 Nov 15:50 UTC, a CI job: the node restarted on sf-2\n" +
		"- 23 Nov 15:40 UTC, a CI job: the node's memory guard killed the pod on sf-2\n" +
		"- 23 Nov 15:30 UTC, card #12: the node ran out of memory on sf-1" + platformTail
	if second, _, _ := store.Task(ctx, cards[0].ID); len(fj.issues) != 2 || second.Objective != want {
		t.Fatalf("a day later: %d issues, the second card under\n%s\nwant\n%s", len(fj.issues), second.Objective, want)
	}
}

// Hand-derived: the store fails the card's row after its issue #1 stands
// on sf/acme. A fourth death lands before the next pass, which takes on
// issue #1 and files no second issue: when the death is a CI job's, and
// when it hit sf/other's #7, which makes sf/other the newest death's
// repository.
func TestPlatformCardTakesOnItsStrandedIssue(t *testing.T) {
	for name, fourth := range map[string]PodDeath{
		"a CI job":           ciDeath("ci-job-43", nov(23, 14, 30), RuleGuardKill, "sf-2"),
		"another repository": sessionDeath("t-other", "s2", nov(23, 14, 30), RuleGuardKill, "sf-2"),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := nov(23, 15, 0)
			w, store, fj := platformRig(t, &now)
			fj.listOpen, store.failCards = true, 1
			putDeaths(t, store, firstSession, firstCI, firstGate)
			if err := w.Check(ctx); err == nil {
				t.Fatal("the failed row raised nothing")
			}
			if len(fj.issues) != 1 || len(platformCards(t, store)) != 0 {
				t.Fatalf("after the failed row: %d issues, %d cards; want the issue alone", len(fj.issues), len(platformCards(t, store)))
			}
			putDeaths(t, store, fourth)
			now = nov(23, 15, 1)
			if err := w.Check(ctx); err != nil {
				t.Fatal(err)
			}
			cards := platformCards(t, store)
			if len(fj.issues) != 1 || len(cards) != 1 || cards[0].IssueNumber != 1 || cards[0].Repository != "sf/acme" {
				t.Fatalf("the next pass: %d issues on %v, cards %+v; want the card on issue #1 of sf/acme and no second issue", len(fj.issues), fj.repos, cards)
			}
		})
	}
}

// Hand-derived: the three deaths hit sf/acme's #12, newest, and sf/other's
// #7, so the filing searches sf/acme, sf/other and the configuration
// repository for a stranded issue. A repository that answers 500 is
// passed over: with sf/other down the card files on sf/acme, with sf/acme
// down on sf/other. With all three down the pass fails and files nothing.
func TestPlatformCardFilesPastAnUnreadableRepository(t *testing.T) {
	for _, c := range []struct {
		down []string
		want string
	}{
		{[]string{"sf/other"}, "sf/acme"},
		{[]string{"sf/acme"}, "sf/other"},
		{[]string{"sf/acme", "sf/other", "sf/ai-software-factory-infra"}, ""},
	} {
		t.Run(strings.Join(c.down, ","), func(t *testing.T) {
			ctx := context.Background()
			now := nov(23, 15, 0)
			w, store, fj := platformRig(t, &now)
			fj.downRepos = map[string]bool{}
			for _, r := range c.down {
				fj.downRepos[r] = true
			}
			putDeaths(t, store, firstSession, firstCI, firstGate)
			err := w.Check(ctx)
			cards := platformCards(t, store)
			if c.want == "" {
				if err == nil || len(fj.issues) != 0 || len(cards) != 0 {
					t.Fatalf("every repository down: err %v, %d issues, %d cards; want an error and nothing filed", err, len(fj.issues), len(cards))
				}
				return
			}
			if err != nil || len(fj.issues) != 1 || len(cards) != 1 || cards[0].Repository != c.want || fj.repos[0] != c.want {
				t.Fatalf("%v down: err %v, issues on %v, cards %+v; want one card on %s", c.down, err, fj.repos, cards, c.want)
			}
		})
	}
}

// Hand-derived: an open card's pass reads the cards of the deaths its face
// shows, the newest ten, and counts every death: 25 deaths on 25 cards
// make at most 10 card reads and a face of 25.
func TestPlatformCardReadsTheCardsItShows(t *testing.T) {
	ctx := context.Background()
	now := nov(23, 15, 0)
	w, store, _ := platformRig(t, &now)
	var deaths []PodDeath
	for i := range 25 {
		id := fmt.Sprintf("t-%02d", i)
		if err := store.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "running", Track: policy.TrackDirect, Repository: "sf/acme", IssueNumber: int64(100 + i)}); err != nil {
			t.Fatal(err)
		}
		deaths = append(deaths, sessionDeath(id, "s", nov(23, 14, i), RuleGuardKill, "sf-1"))
	}
	putDeaths(t, store, deaths[:3]...)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	putDeaths(t, store, deaths[3:]...)
	now, store.taskReads = nov(23, 15, 1), 0
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	card := platformCards(t, store)[0]
	if store.taskReads > 10 || !strings.HasPrefix(card.Objective, platformTitle+"\n\n"+platformHead(25)+"- 23 Nov 14:24 UTC, card #124:") ||
		!strings.Contains(card.Objective, "- 23 Nov 14:15 UTC, card #115:") || !strings.Contains(card.Objective, "- and 15 more\n") {
		t.Fatalf("the open card's pass read %d cards, its face\n%s\nwant at most 10 reads and a face of 25 that lists #124 to #115", store.taskReads, card.Objective)
	}
}

// The platform card keeps the instants its story and its list name beside
// their UTC words: the newest death's minute and the oldest's, each once,
// whose words the story repeats.
func TestPlatformCardKeepsItsInstants(t *testing.T) {
	deaths := []PodDeath{ciDeath("ci-job-2", nov(23, 14, 2), RuleGuardKill, "sf-2"), ciDeath("ci-job-1", nov(22, 23, 30), RuleGuardKill, "sf-2")}
	q := PlatformDeathsQuestion(deaths, nil, "sf/acme")
	if want := "The platform ended the factory's work 2 times between 22 Nov 23:30 UTC and 23 Nov 14:02 UTC."; q.Happened != want {
		t.Errorf("the story %q, want %q", q.Happened, want)
	}
	if !strings.Contains(q.Info, "- 23 Nov 14:02 UTC, a CI job:") || !strings.Contains(q.Info, "- 22 Nov 23:30 UTC, a CI job:") {
		t.Errorf("the list %q, want both deaths in UTC", q.Info)
	}
	if want := (Moments{{At: nov(23, 14, 2), Layout: LayoutDay}, {At: nov(22, 23, 30), Layout: LayoutDay}}); !reflect.DeepEqual(q.Moments, want) {
		t.Errorf("moments %+v, want %+v", q.Moments, want)
	}
}

// Hand-derived: three deaths in the minute 14:02 make the story name that
// minute alone.
func TestPlatformCardStoryOfOneMinute(t *testing.T) {
	var deaths []PodDeath
	for _, s := range []int{40, 10, 0} {
		deaths = append(deaths, ciDeath(fmt.Sprintf("ci-job-%d", s), nov(23, 14, 2).Add(time.Duration(s)*time.Second), RuleGuardKill, "sf-2"))
	}
	if q := PlatformDeathsQuestion(deaths, nil, "sf/acme"); q.Happened != "The platform ended the factory's work 3 times at 23 Nov 14:02 UTC." {
		t.Errorf("the story %q, want the minute alone", q.Happened)
	}
}

// quietRig is the rig with an open card filed at 14:10 on 23 November for
// three CI deaths at 13:50, 13:58 and 14:02, and a Timeline that records
// the card's withdrawal. Sessions started at 13:59 and at 14:02 itself fall
// outside "after 14:02"; those at the given times fall inside.
func quietRig(t *testing.T, started ...time.Time) (*PlatformCard, *platformStore, *alertForgejo, *time.Time) {
	t.Helper()
	now := nov(23, 14, 10)
	w, store, fj := platformRig(t, &now)
	w.Timeline = &Timeline{Objects: &fakeObjectStore{}, Events: store.fakeStore, Store: store.fakeStore}
	putDeaths(t, store, ciDeath("ci-job-1", nov(23, 13, 50), RuleGuardKill, "sf-2"), ciDeath("ci-job-2", nov(23, 13, 58), RuleGuardKill, "sf-2"),
		ciDeath("ci-job-3", nov(23, 14, 2), RuleGuardKill, "sf-2"))
	if err := w.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, at := range append([]time.Time{nov(23, 13, 59), nov(23, 14, 2)}, started...) {
		if _, err := store.Record(context.Background(), Event{TaskID: "t-acme", Kind: EventSessionStarted, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	return w, store, fj, &now
}

// sevenSessions start after 14:02 on 23 November.
func sevenSessions() []time.Time {
	var out []time.Time
	for i := range 7 {
		out = append(out, nov(23, 15, i))
	}
	return out
}

// Hand-derived: the card filed at 14:10 on 23 November for deaths at 13:50,
// 13:58 and 14:02 closes once 24 hours have passed since the newest death,
// 14:02, which is 14:02 on 24 November, and 3 sessions have started after
// it. Of nine session starts, those at 13:59 and at 14:02 itself do not
// count, so 7 do. At 14:01 on the 24th the day is not over; with 2 starts
// after 14:02 the factory has not shown the platform answers.
func TestPlatformCardClosesAfterAQuietDay(t *testing.T) {
	ctx := context.Background()
	const sentence = "No platform death since 23 November at 14:02 UTC, while 7 sessions started, so the card closes."
	t.Run("a quiet day with 7 sessions", func(t *testing.T) {
		w, store, fj, now := quietRig(t, sevenSessions()...)
		card := platformCards(t, store)[0]
		*now = nov(24, 15, 0)
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
		if got := platformCards(t, store)[0]; got.Status != "done" {
			t.Fatalf("the card is %s, want done", got.Status)
		}
		if want := fmt.Sprintf("#%d %s", card.IssueNumber, sentence); !slices.Contains(fj.comments, want) {
			t.Errorf("the issue's comments = %q, want %q", fj.comments, want)
		}
		requireWithdrawn(t, store.fakeStore, card.ID, "no platform death since 23 November at 14:02 UTC, while 7 sessions started, so the card closes",
			Moments{{At: nov(23, 14, 2), Layout: LayoutWords}})
	})
	t.Run("14:01 on the next day", func(t *testing.T) {
		w, store, _, now := quietRig(t, sevenSessions()...)
		*now = nov(24, 14, 1)
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
		if got := platformCards(t, store)[0]; got.Status != "waiting-human" || len(withdrawnRows(store.fakeStore, got.ID)) != 0 {
			t.Fatalf("the card is %s with %d withdrawals, want it waiting on the human with none", got.Status, len(withdrawnRows(store.fakeStore, got.ID)))
		}
	})
	t.Run("2 sessions", func(t *testing.T) {
		w, store, _, now := quietRig(t, sevenSessions()[:2]...)
		*now = nov(24, 15, 0)
		if err := w.Check(ctx); err != nil {
			t.Fatal(err)
		}
		if got := platformCards(t, store)[0]; got.Status != "waiting-human" || len(withdrawnRows(store.fakeStore, got.ID)) != 0 {
			t.Fatalf("the card is %s with %d withdrawals, want it waiting on the human with none", got.Status, len(withdrawnRows(store.fakeStore, got.ID)))
		}
	})
}

// Hand-derived: the card before the open one closed itself after a quiet
// day, so a burst that came back after it stays with the human: the open
// card is still waiting after a quiet day with 7 sessions, and no row
// withdraws its ask.
func TestAPlatformCardAfterAQuietCloseStaysWithTheHuman(t *testing.T) {
	ctx := context.Background()
	w, store, _, now := quietRig(t, sevenSessions()...)
	prev := Task{ID: "pd-prev", Kind: "human", Status: "done", Source: SourcePlatformDeaths, Repository: "sf/acme", IssueNumber: 5, CreatedAt: nov(22, 9, 0)}
	if err := store.CreateTask(ctx, prev); err != nil {
		t.Fatal(err)
	}
	body := "No platform death since 22 November at 10:00 UTC, while 5 sessions started, so the card closes."
	if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: prev.ID, Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 5, Body: body}); err != nil {
		t.Fatal(err)
	}
	*now = nov(24, 15, 0)
	if err := w.Check(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range platformCards(t, store) {
		if c.ID != prev.ID && (c.Status != "waiting-human" || len(withdrawnRows(store.fakeStore, c.ID)) != 0) {
			t.Fatalf("the open card is %s with %d withdrawals, want it waiting on the human with none", c.Status, len(withdrawnRows(store.fakeStore, c.ID)))
		}
	}
}
