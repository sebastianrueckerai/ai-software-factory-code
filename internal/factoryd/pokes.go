package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Pokes (roadmap 239): factoryd's own turns in the inbox thread of each
// member a project tells, its notify list else its approvers, on 145's
// event classes: a card that needs a human (and one that needs them no
// more, its question withdrawn), a card stopped by its budget, a terminal
// failure, a card or plan done, the agent's own starts and filings at
// assisted and autonomous, a change of the level, and a routine's run
// that tells the inbox (roadmap 267). A poke
// is a row written through the message path: no model turn, one per state
// change, behind a turn in flight in the inbox as every writer's rows
// (roadmap 263). A repeat of the newest poke about the same card counts on
// that row (74's rule).

const (
	// MessagePoke is a poke's kind; MessageEscalation a poke about a card
	// that waits for a person, which the chat marks "needs you";
	// MessageAsk the chat's own question to the thread's member;
	// MessageBudget the row a turn ends on when the gateway refuses its
	// call for money, the chat task's budget (birth kind
	// EventChatBudgetSpent, which a fund resumes) or a factory cap;
	// MessageStop the row a member's stop ends a turn on, which the next
	// turn's context leaves out as the budget row's.
	MessagePoke       = "poke"
	MessageEscalation = "escalation"
	MessageAsk        = "ask"
	MessageBudget     = "budget"
	MessageStop       = "stop"
)

// pokeKinds are the events a poke may answer.
var pokeKinds = []string{EventQuestionAsked, EventQuestionWithdrawn, EventTaskEnded, EventAllChildrenDone, EventAutonomyChanged,
	EventCardFiled, EventCardRetried, EventSessionGranted, EventRoutineRan, EventChatBudgetSpent,
	EventPullRequestMerged, EventCardHoldStranded}

// agentVerbs are the agent's own actions a poke tells, by event kind.
var agentVerbs = map[string]string{EventCardFiled: "filed", EventCardRetried: "retried", EventSessionGranted: "granted"}

// PokeDelivery is a poke's messenger half (roadmap 145): the member level
// its row asks (empty for a row that never leaves the factory), the
// members whose derived default is needs-you (the resolved notify list),
// and whether the project's file allows delivery.
type PokeDelivery struct {
	RowLevel string
	Derived  []string
	On       bool
}

// PokeStore is what the poke writer reads and writes.
type PokeStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	// PokeEvents are the poke events after the poke cursor, oldest first,
	// at most limit.
	PokeEvents(ctx context.Context, limit int) ([]Event, error)
	EnsureInbox(ctx context.Context, repository, login string) (int64, error)
	// PokeRecipients are a poke's recipients on repository: base plus every
	// member whose explicit level row (needs-you or all) joins them.
	PokeRecipients(ctx context.Context, repository string, base []string) ([]string, error)
	// ChainInform is the merged inform's words: the status of the task's
	// own carry row, the issue of the repository's repair card on the red
	// (0 for none; a merged one's fix is in main), and whether a green
	// chain already covers the task.
	ChainInform(ctx context.Context, repository string, pr int64, taskID string) (state string, repair int64, covered bool, err error)
	// Poke writes m into the inbox thread of each of logins on repository,
	// queued where queued says so, one pending delivery row per connection
	// whose member's level passes plan's row level, and moves the cursor
	// past event id, in one transaction; it answers each login's new row,
	// 0 for a repeat.
	Poke(ctx context.Context, id int64, repository string, logins []string, queued []bool, m ChatMessage, plan PokeDelivery) ([]int64, error)
}

// Line is the chat's one line of writers on each thread (roadmap 263), the
// console's: Behind takes a place behind thread n's turn in flight and
// those waiting before it. queued says one is ahead, so the writer stores
// its rows queued; land, called once with their ids, starts them in their
// order when the place comes and leaves it.
type Line interface {
	Behind(repository, login string, n int64) (queued bool, land func(ids ...int64))
}

// Poker writes the pokes.
type Poker struct {
	Store   PokeStore
	Current func() policy.Version
	Line    Line
}

// pokeBatch bounds the events one read takes.
const pokeBatch = 100

// Loop pokes every interval until ctx ends.
func (p *Poker) Loop(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := p.Pass(ctx); err != nil {
				log.Printf("pokes: %v", err)
			}
		}
	}
}

// Pass pokes for every poke event after the cursor, oldest first; an
// event that pokes nobody only moves the cursor.
func (p *Poker) Pass(ctx context.Context) error {
	for {
		events, err := p.Store.PokeEvents(ctx, pokeBatch)
		if err != nil {
			return err
		}
		tree := p.Current().Tree
		for _, e := range events {
			var t Task
			if e.TaskID != "" {
				if t, _, err = p.Store.Task(ctx, e.TaskID); err != nil {
					return err
				}
			}
			_, project, inTree := tree.ProjectOf(e.Repository)
			if e.Kind == EventPullRequestMerged && !Final(t.Status) && endsMerged(t, tree) {
				// Finish lands the card in merged after its cleanup: a pass
				// reading the merge first finds it on its way there
				t.Status = "merged"
			}
			if e.Kind == EventPullRequestMerged && t.Status == "merged" {
				// the inform's words carry the chain's facts at poke time,
				// not at merge: the poller may have concluded the chain
				// between the two
				state, repair, covered, err := p.Store.ChainInform(ctx, e.Repository, t.PRNumber, t.ID)
				if err != nil {
					return err
				}
				if e.Detail == nil {
					e.Detail = map[string]any{}
				}
				e.Detail["chain"], e.Detail["repair"], e.Detail["covered"] = state, float64(repair), covered
			}
			var parent Task
			if HelperStep(t.Source) != "" && t.Parent != "" {
				if parent, _, err = p.Store.Task(ctx, t.Parent); err != nil {
					return err
				}
			}
			m, ok := PokeOf(e, t, parent)
			var to []string
			if ok {
				to = pokeTo(tree, e.Repository)
			}
			if e.Kind == EventAutonomyChanged {
				// the member who set the level knows it: the row tells the others
				to = slices.DeleteFunc(slices.Clone(to), func(login string) bool { return login == e.Actor })
			}
			plan := PokeDelivery{RowLevel: deliveryLevel(m), Derived: to, On: true}
			if inTree {
				plan.On = project.MessengersOn()
			}
			if err := p.poke(ctx, e, to, m, plan); err != nil {
				return err
			}
		}
		if len(events) < pokeBatch {
			return nil
		}
	}
}

// poke writes m into the inbox of each of logins behind a turn in flight
// there and moves the cursor past event e.
func (p *Poker) poke(ctx context.Context, e Event, logins []string, m ChatMessage, plan PokeDelivery) error {
	if plan.On && len(logins) > 0 {
		joined, err := p.Store.PokeRecipients(ctx, e.Repository, logins)
		if err != nil {
			return err
		}
		logins = joined
	}
	queued, ids := make([]bool, len(logins)), make([]int64, len(logins))
	var lands []func(...int64)
	defer func() {
		for i, land := range lands {
			land(ids[i])
		}
	}()
	for i, login := range logins {
		n, err := p.Store.EnsureInbox(ctx, e.Repository, login)
		if err != nil {
			return err
		}
		var land func(...int64)
		queued[i], land = p.Line.Behind(e.Repository, login, n)
		lands = append(lands, land)
	}
	got, err := p.Store.Poke(ctx, e.ID, e.Repository, logins, queued, m, plan)
	if err != nil {
		return err
	}
	copy(ids, got)
	return nil
}

// deliveryLevel is the member level a poke's row asks for its messenger
// copy: every ask that needs a person, a chat budget spent among them,
// and the informs of a finished or failed card or plan; the rest stays
// in the inbox.
func deliveryLevel(m ChatMessage) string {
	if m.Kind == MessageEscalation {
		return LevelNeedsYou
	}
	switch m.BirthKind {
	case EventTaskEnded, EventAllChildrenDone:
		return LevelAll
	case EventChatBudgetSpent, EventCardHoldStranded:
		return LevelNeedsYou
	}
	return ""
}

// pokeTo are the members a poke on repository tells: its project's
// notify list, else its approvers.
func pokeTo(tree policy.Tree, repository string) []string {
	if _, c, ok := tree.ProjectOf(repository); ok && len(c.Notify) > 0 {
		return c.Notify
	}
	return tree.ApproversOf(repository)
}

// PokeOf is event e's poke, t the card it names, false for an event that
// pokes nobody. A message about a card's ask is its face's words
// (askMessage), with the ask of a merge's, a plan's or a criteria request,
// whose case's face asks for it; a notice is one line of at most noticeMax
// runes. A helper
// task's message names its step on parent, the card the step stands on:
// its ask is the parent's, and says which step asks.
func PokeOf(e Event, t, parent Task) (ChatMessage, bool) {
	name, bare, step := inboxName(e, t), inboxNumber(e, t), ""
	if s := HelperStep(t.Source); s != "" && parent.ID != "" {
		name, bare = s+" of "+inboxName(e, parent), s+" of "+inboxNumber(e, parent)
		if e.Kind == EventQuestionAsked {
			name, step = inboxName(e, parent), capitalize(s)+" asks."
		}
	}
	str := func(k string) string { s, _ := e.Detail[k].(string); return s }
	num := func(k string) int64 { n, _ := e.Detail[k].(float64); return int64(n) }
	bl := func(k string) bool { v, _ := e.Detail[k].(bool); return v }
	m := ChatMessage{Role: "assistant", Kind: MessagePoke, TaskID: e.TaskID, BirthKind: e.Kind}
	switch e.Kind {
	case EventQuestionAsked:
		if str("settled") != "" {
			// the factory answered the ask as it asked it: no one is needed
			return m, false
		}
		if revision, _ := toInt(e.Detail["revision"]); revision > 0 && str("case_kind") == "" || bl("held") {
			// the planner's row or factoryd's own human card's: its case
			// climbs, and its face's row asks
			return m, false
		}
		m.Kind = MessageEscalation
		why := str("why")
		if q := cmp.Or(str("question"), str("ask")); why == "" && !strings.Contains(q, "\n") && utf8.RuneCountInString(name+q) <= askLead {
			// an ask whose row stores no why says its question where it fits
			why = q
		}
		suggested := suggestion(e.Detail, offersOf(e.Detail["offers"]))
		switch e.Detail["case_kind"] {
		case string(CasePlanCriteria), string(CasePlanQuestion), string(CasePlanConfirmation):
			// the face of a request's case says the request's ask
			step = str("ask")
		case string(CaseMergeApproval):
			step, why, suggested, m.Choices = mergePoke(e, t)
		}
		m.Content = askMessage(name, step, why, suggested)
		m.Decider = str("decider_line")
		if cs, ok := e.Detail["choices"].([]any); ok {
			for _, c := range cs {
				if s, ok := c.(string); ok {
					m.Choices = append(m.Choices, s)
				}
			}
		}
	case EventQuestionWithdrawn:
		// a row after the card's escalation: the inbox offers its answer
		// row no more. A monitor's case sits beside the card's own ask, so
		// its row names the flag; the case id stays in the event's detail
		if bl("held") {
			return m, false
		}
		// the reason is the human's record: past a notice's length the card
		// keeps its number alone, and only past that the reason goes
		noMore := func(card string) string {
			if e.Detail["case_kind"] == string(CaseMonitorFlag) {
				return "The flag on " + card + " needs you no more"
			}
			return capitalize(card) + " needs you no more"
		}
		reason := ": " + strings.TrimSuffix(str("reason"), ".") + "."
		m.Content = notice(noMore(name)+reason, notice(noMore(bare)+reason, noMore(name)+"."))
	case EventPullRequestMerged:
		// a card still merged informs once at its merge, its words the
		// chain's facts as this poke reads them (chain, repair and covered
		// in the detail); one already done — or already covered, the
		// poller concluding between merge and poke — informs nobody
		if t.Status != "merged" || bl("covered") {
			return m, false
		}
		m.Content = NothingForYou + " " + name + " is merged. The checks on main run now."
		if repair := num("repair"); str("chain") == "failure" || repair > 0 {
			m.Content = NothingForYou + " " + name + " is merged, and the checks on main failed."
			if repair > 0 {
				m.Content = notice(m.Content+fmt.Sprintf(" #%d repairs them.", repair), m.Content)
			}
		}
	case EventTaskEnded:
		switch {
		case t.Kind != "agent":
			return m, false
		case str("state") == "failed":
			reason := FirstLine(str("reason"))
			if code, rest, ok := strings.Cut(reason, ": "); ok && reasonCode.MatchString(code) {
				// the gate's code word leads a machine's reason, not a human's line
				reason = rest
			}
			m.Content = notice(capitalize(name)+" failed: "+strings.TrimSuffix(reason, ".")+".", capitalize(name)+" failed.")
			if !t.Report() {
				// a helper's report is no work card: no retry files it again
				m.Choices = []string{LabelOf(ActRetry), LabelOf(ActNote)}
			}
		case str("state") == "done" && t.Parent == "":
			m.Content = fmt.Sprintf("%s %s is done and live. It cost %s EUR.", NothingForYou, name, textutil.EUR(t.SpentEUR))
		default:
			return m, false
		}
	case EventAllChildrenDone:
		m.Content = fmt.Sprintf("%s The plan %s is done: all %d cards.", NothingForYou, name, num("cards"))
	case EventCardHoldStranded:
		// a hold the human released, or one a retry's card took over,
		// before the pass strands nothing
		if !t.Held() || t.Hold.Release != str("release_task") {
			return m, false
		}
		ended, fit := "was closed", "Release the hold or hold it again."
		if str("status") == "failed" {
			// a retry of the release card takes the hold over (takeOverEdges)
			ended, fit = "failed", fmt.Sprintf("Retry #%d or release the hold.", num("release_issue"))
		}
		held := capitalize(name) + " stays on hold: its release card "
		m.Content = notice(held+str("release")+" "+ended+". "+fit, held+ended+". "+fit)
	case EventChatBudgetSpent:
		// the chat's own task is no card
		m.TaskID = ""
		budget, _ := e.Detail["budget_eur"].(float64)
		spent, _ := e.Detail["spent_eur"].(float64)
		m.Content = fmt.Sprintf("The chat's budget for this month is used up: %s of %s EUR spent. Its turns wait until it is funded or the month turns.",
			textutil.EUR(spent), textutil.EUR(budget))
	case EventRoutineRan:
		if str("poke") == "" {
			return m, false
		}
		m.Content = str("poke")
		m.Decider = RoutineDecider(str("routine"), int(num("run")), str("schedule"))
	case EventAutonomyChanged:
		m.TaskID = ""
		from, to := str("from"), str("to")
		switch {
		case from != to:
			m.Content = fmt.Sprintf("%s %s set the autonomy from %s to %s.", NothingForYou, e.Actor, from, to)
		case num("queue_cap") > 0:
			m.Content = fmt.Sprintf("%s %s set the queue cap to %d.", NothingForYou, e.Actor, num("queue_cap"))
		default:
			return m, false
		}
	default:
		verb, level := agentVerbs[e.Kind], str("autonomy")
		if verb == "" || str("decider") != "agent" || level == "" || level == policy.AutonomyManual {
			return m, false
		}
		if level == policy.AutonomyAutonomous && str("merge_rule") == policy.MergeAuto {
			level = policy.AutonomyFullAuto
		}
		switch e.Kind {
		case EventCardFiled:
			m.Content = NothingForYou + " The agent filed " + name + "."
		case EventCardRetried:
			m.Content = NothingForYou + " The agent tried " + name + " again."
		case EventSessionGranted:
			m.Content = NothingForYou + " The agent gave " + name + " one more session."
		}
		m.Decider = level + " level · " + verb + " by the agent"
	}
	m.Moments = MomentsOf(e.Detail).In(m.Content)
	return m, true
}

// NothingForYou opens every inbox message that needs nothing from the
// human.
const NothingForYou = "Nothing for you."

// reasonCode is the code word a gate's reason opens with ("checks").
var reasonCode = regexp.MustCompile(`^[a-z][a-z_]*$`)

// noticeMax bounds a notice, messageMax an ask's message, in runes;
// askLead a question's card and the question where its row stores no why,
// so its suggestion fits beside.
const (
	noticeMax  = 160
	messageMax = 280
	askLead    = 200
)

// notice is line, or short where line runs past noticeMax: a notice drops
// its detail rather than cut it.
func notice(line, short string) string {
	if utf8.RuneCountInString(line) > noticeMax {
		return short
	}
	return line
}

// askMessage is the inbox's message about a card's ask, the one source
// its face is: the card by its name, which links it, a request's ask (a
// question's stands on its card), why it asks, and the suggested answer
// with who suggests it. A why that would take the message past messageMax
// stays on the card, which holds the rest.
func askMessage(name, ask, why, suggested string) string {
	head, tail := name+" needs you."+then(ask), then(suggested)
	if full := head + then(capitalize(why)) + tail; utf8.RuneCountInString(full) <= messageMax {
		return full
	}
	return head + tail
}

// then is s as the next sentence of a message, "" for none.
func then(s string) string {
	if s = sentence(s); s != "" {
		return " " + s
	}
	return ""
}

// suggestion is the line of an ask row's suggested answer, offers its
// answers: who suggests which label, "" for none.
func suggestion(detail map[string]any, offers []AskOffer) string {
	r := RecommendationOf(detail)
	if r.Action == "" {
		return ""
	}
	label := ""
	for _, o := range offers {
		if o.Value == r.Action || choicePrefix+o.Value == r.Action {
			label = o.Label
		}
	}
	if label == "" {
		a, ok := AnswerOf(r.Action)
		if !ok {
			return ""
		}
		label = a.Label
	}
	return capitalize(r.By) + " suggests “" + label + "”."
}

// offersOf are the answers an ask row stores under offers.
func offersOf(raw any) []AskOffer {
	var out []AskOffer
	if b, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// LabelOf is action's label in the vocabulary.
func LabelOf(action string) string {
	a, _ := AnswerOf(action)
	return a.Label
}

var (
	// keySpan is an answer's key in a code span, with the variant an agent
	// passes beside it: "`deny_merge` with `reject`"
	keySpan = regexp.MustCompile("`([a-z_]+)`(?: with `([a-z_]+)`)?")
	// paramSpan is the names or values of a field an agent passes, in
	// brackets: " (`depends_on`)", " (`met`, `not_met` or `waived`)"
	paramSpan = regexp.MustCompile(" \\((?:`[^`]+`(?:, | or | and )?)+\\)")
)

// ForHuman is refusal s as a human reads it: s names each answer by the
// key an agent types, in a code span, and a human reads its label; s
// names in brackets the field an agent passes, which a human's form
// fills, and a human reads no name of it.
func ForHuman(s string) string {
	s = keySpan.ReplaceAllStringFunc(s, func(m string) string {
		g := keySpan.FindStringSubmatch(m)
		label := cmp.Or(LabelOf(g[1]+"/"+g[2]), LabelOf(g[1]))
		if label == "" {
			return m
		}
		return "“" + label + "”"
	})
	return paramSpan.ReplaceAllString(s, "")
}

// RoutineDecider is the decider line of a poke run n of routine makes,
// schedule its schedule in words.
func RoutineDecider(routine string, n int, schedule string) string {
	return fmt.Sprintf("routine %s · run %d · %s", routine, n, schedule)
}

// mergePoke is what the inbox says of the face row e of a merge case on
// card t: the face's ask and why, the answer a rung or the face suggests,
// and the answer the inbox can press. A merge whose promises a human
// judges first opens on the first one's page, as its card does, and
// offers no press.
func mergePoke(e Event, t Task) (ask, why, suggested string, choices []string) {
	c := Case{TaskID: e.TaskID, Repository: e.Repository, Status: CaseAtHuman, Kind: CaseMergeApproval, Facts: e.Detail}
	if open := factStrings(c, "open"); len(open) > 0 {
		c = OnPromise(c, PromiseFact{ID: open[0], Kind: CriterionHuman})
		face := FaceOf(c, Facts{CaseFacts: FactsOf(c), Card: t})
		return face.Ask, face.Why, "", nil
	}
	offers := offersOf(e.Detail["offers"])
	for _, o := range offers {
		if o.Value == ActApproveMerge || o.Value == ActRerunChecks {
			choices = []string{o.Label}
		}
	}
	ask, _ = e.Detail["ask"].(string)
	why, _ = e.Detail["why"].(string)
	return ask, why, suggestion(e.Detail, offers), choices
}

// inboxName is how the inbox names e's card (CardName), "A card" for a
// card with neither issue nor title.
func inboxName(e Event, t Task) string {
	return cmp.Or(CardName(t, e.IssueNumber), "A card")
}

// inboxNumber is inboxName without the card's title: its number alone,
// the whole name for a card with none.
func inboxNumber(e Event, t Task) string {
	return cmp.Or(CardName(Task{IssueNumber: t.IssueNumber}, e.IssueNumber), inboxName(e, t))
}

// nameTitleMax bounds the title a line names a card by, in runes.
const nameTitleMax = 70

// CardName is how a line names card t: its issue, which links it, and its
// title cut at a word to nameTitleMax runes; the title alone, quoted, for
// a card with no issue; "" for neither. issue stands in for a card with
// no issue of its own.
func CardName(t Task, issue int64) string {
	if t.IssueNumber != 0 {
		issue = t.IssueNumber
	}
	title := wordCut(CardTitle(t), nameTitleMax)
	switch {
	case issue != 0 && title != "":
		return fmt.Sprintf("#%d (%s)", issue, title)
	case issue != 0:
		return fmt.Sprintf("#%d", issue)
	case title != "":
		return "“" + title + "”"
	}
	return ""
}

// cardRef is how the agent's wake names e's card: its issue, else its
// task id.
func cardRef(e Event, t Task) string {
	switch {
	case t.IssueNumber != 0:
		return fmt.Sprintf("#%d", t.IssueNumber)
	case e.IssueNumber != 0:
		return fmt.Sprintf("#%d", e.IssueNumber)
	}
	return e.TaskID
}

func (s *PGStore) ChainInform(ctx context.Context, repository string, pr int64, taskID string) (string, int64, bool, error) {
	var state string
	var repair int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT status FROM chain_runs WHERE repository = $1 AND pr_number = $2), ''),
		COALESCE((SELECT min(issue_number) FROM tasks WHERE repository = $1 AND source = $3 AND status <> ALL($4) AND status <> 'merged' AND issue_number > 0), 0)`,
		repository, pr, SourceChainRepair, FinalStatuses).Scan(&state, &repair); err != nil {
		return "", 0, false, err
	}
	covered, err := s.ChainCovered(ctx, repository)
	if err != nil {
		return "", 0, false, err
	}
	return state, repair, covered[taskID] > 0, nil
}

func (s *PGStore) PokeEvents(ctx context.Context, limit int) ([]Event, error) {
	// the first read starts at the newest event: no history pokes
	if _, err := s.db.ExecContext(ctx, `INSERT INTO chat_poke_cursor (event_id) SELECT coalesce(max(id), 0) FROM events
		ON CONFLICT DO NOTHING`); err != nil {
		return nil, err
	}
	return s.events(ctx, `SELECT `+eventColumns+` FROM events WHERE id > (SELECT event_id FROM chat_poke_cursor)
		AND kind = ANY($1) ORDER BY id LIMIT $2`, pokeKinds, limit)
}

// bumpRepeat counts m as a repeat on the newest poke row about its card
// in login's thread n, when that row says the same; false: no repeat.
func bumpRepeat(ctx context.Context, db sqlRunner, repository, login string, n int64, m ChatMessage) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE chat_messages SET repeats = repeats + 1, at = now()
		WHERE id = (SELECT id FROM chat_messages WHERE repository = $1 AND login = $2 AND conversation = $3
			AND task_id = $4 AND kind IN ($5, $6) ORDER BY id DESC LIMIT 1)
		AND kind = $7 AND content = $8`, repository, login, n, m.TaskID, MessagePoke, MessageEscalation, m.Kind, m.Content)
	if err != nil {
		return false, err
	}
	repeated, err := res.RowsAffected()
	return repeated > 0, err
}

func (s *PGStore) Poke(ctx context.Context, id int64, repository string, logins []string, queued []bool, m ChatMessage, plan PokeDelivery) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	ids := make([]int64, len(logins))
	for i, login := range logins {
		n, err := ensureInbox(ctx, tx, repository, login)
		if err != nil {
			return nil, err
		}
		m.Conversation, m.Queued = n, queued[i]
		if m.TaskID != "" {
			// a repeat: the newest poke about the card says the same
			repeated, err := bumpRepeat(ctx, tx, repository, login, n, m)
			if err != nil {
				return nil, err
			}
			if repeated {
				continue
			}
		}
		if ids[i], err = insertChatMessage(ctx, tx, repository, login, m); err != nil {
			return nil, err
		}
	}
	// the card's own thread takes every ask too (the card door): the row
	// lands on the binding member's thread beside the inboxes'
	if m.Kind == MessageEscalation && m.TaskID != "" {
		if who, n, ok, err := cardThreadOf(ctx, tx, repository, m.TaskID); err != nil {
			return nil, err
		} else if ok {
			repeated, err := bumpRepeat(ctx, tx, repository, who, n, m)
			if err != nil {
				return nil, err
			}
			if !repeated {
				cm := m
				cm.Conversation, cm.Queued = n, false
				if _, err := insertChatMessage(ctx, tx, repository, who, cm); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := queueDeliveries(ctx, tx, repository, logins, ids, plan); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_poke_cursor SET event_id = greatest(event_id, $1)`, id); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}
