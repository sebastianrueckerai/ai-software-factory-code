package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// BoardTask is a task on the console's board with the reason of its live
// ask case ("" when it asks none) and the Choices its agent offered with that
// question, a card in waiting-deps the issues it waits on
// (WaitingOn: "#3, #5"), in any status the issues of the
// cards it depends on that are not done (Dependencies: "#3, #5", for the
// face's "blocked by n" chip), and the entries ahead of it in its merge
// queue (-1 when it holds no entry). Question is the ask's five-part
// question when a template shaped it (nil on an unshaped ask); AnswersPending counts
// the trusted answers the card's workflow has not taken: a card that
// holds some cannot process another, so its answer box yields to the
// liveness warning. ChainState is the verdict of the newest run that
// carries the card's pull request ("" for none) — its commit's AND of
// runs and required contexts: a merged card reads it as its chain word
// (roadmap 297), the chain's own fact, no roll in it.
// ChainLive is deployLive read as a column: whether the card's change runs
// on the cluster — a green run covers it, it never opened a pull request,
// or it finished before the chain began recording — the fact a finished
// card's own page waits on.
// ChainRun is the forge's number of the run that carries the card's pull
// request, 0 while none does; ChainGreen is the repository's newest green
// run's number, 0 for none, and ChainGreenAt when it went green — the run
// and the moment a done card's cover clause names (C38). The numbers are
// what the forge's own pages and the board's links speak, not the API ids
// the store keeps.
type BoardTask struct {
	Task
	// Escalation, EscalatedAt, Choices, Question and QuestionComment are the
	// card's live ask case as the board shows it: the stop's words, when its
	// face was asked, the agent's choices, the ask's five parts and the
	// comment of its face. AskCase is that case's id, 0 for none.
	Escalation   string
	EscalatedAt  time.Time
	Choices      []string
	AskCase      int64
	WaitingOn    string
	Dependencies string
	QueueAhead   int
	// SlotWait is the face's "waits for a slot, N ahead (L live)" of a card
	// queued for a work slot, "" for any other: the resumed cards come
	// first, then the never-started ones by filing.
	SlotWait string
	Question *HumanQuestion
	// askRow is the detail of the live ask case's newest question_asked row.
	askRow map[string]any
	// QuestionComment is the issue comment of the question, 0 for none
	QuestionComment int64
	AnswersPending  int
	ChainState      string
	ChainLive       bool
	ChainRun        int64
	ChainGreen      int64
	ChainGreenAt    time.Time
	// RollID and RollIssue name the roll task that carries a merged card's
	// pull request in an opened or merged request; "" and 0 for none.
	RollID    string
	RollIssue int64
}

// deployLive is the SQL test that a finished card's change runs on the
// cluster (roadmap 297): a card that never opened a pull request, or that
// a green run covers — the run that carried its change is recorded and
// the newest green run on main, the head row, the green row or any card's
// carry row, sits at or after it, so a green run later than its own red
// carry deploys it — or that no chain names at all and that finished before the
// chain began recording runs (none recorded yet: every card, while none
// is; a done card only — a merged one is live through coverage alone).
// A carry run with no green run at or after it keeps the card
// waiting; every row's status is its commit's verdict — the runs AND
// the required contexts — so coverage demands them too.
const deployLive = `(tasks.pr_number = 0
	OR ` + chainCovers + `
	OR tasks.status = 'done'
	   AND NOT EXISTS (SELECT 1 FROM chain_runs cr WHERE cr.repository = tasks.repository AND cr.pr_number = tasks.pr_number)
	   AND COALESCE((SELECT min(updated_at) FROM chain_runs WHERE repository = tasks.repository), 'infinity'::timestamptz) > COALESCE(tasks.finished_at, tasks.created_at))`

// chainGreen is the forge's number of the newest green run of the card's
// repository, 0 for none — the number the board's links and words speak,
// not the API id the row keeps.
const chainGreen = `COALESCE((SELECT max(green.run_index) FROM chain_runs green WHERE green.repository = tasks.repository AND green.status = 'success'), 0)`

// chainCovers is the SQL test that a green run covers the card's change:
// the run that carried it is recorded and the newest green run sits at or
// after it. It compares run ids, not numbers: both order a repository's
// runs the same way, and the ids are the rows' own keys. deployLive and
// the reconciler's ChainCovered both read it.
const chainCovers = `(EXISTS (SELECT 1 FROM chain_runs carry WHERE carry.repository = tasks.repository AND carry.pr_number = tasks.pr_number)
	AND COALESCE((SELECT max(green.run_id) FROM chain_runs green WHERE green.repository = tasks.repository AND green.status = 'success'), 0)
	   >= (SELECT min(mine.run_id) FROM chain_runs mine WHERE mine.repository = tasks.repository AND mine.pr_number = tasks.pr_number))`

// rollCarries is the SQL test that request rr lists the card's pull request.
const rollCarries = `tasks.pr_number > 0 AND rr.repository = tasks.repository AND rr.prs @> ARRAY[tasks.pr_number]::bigint[]`

// boardTaskQuery reads tasks as BoardTasks; the caller adds the WHERE.
var boardTaskQuery = `SELECT ` + boardTaskColumns + boardTaskFrom

// boardTaskColumns and boardTaskFrom are boardTaskQuery's two halves, so
// FinishedTasks reads one column more.
var boardTaskColumns = taskColumns + `, esc.at, esc.ask, COALESCE(esc.case_id, 0),
		        CASE WHEN status = 'waiting-deps' THEN COALESCE((SELECT string_agg('#' || d.issue_number, ', ' ORDER BY d.issue_number)
		        FROM task_deps td JOIN tasks d ON d.id = td.depends_on WHERE td.task_id = tasks.id AND d.status <> 'done'), '') ELSE '' END,
		        COALESCE((SELECT string_agg('#' || d.issue_number, ', ' ORDER BY d.issue_number)
		        FROM task_deps td JOIN tasks d ON d.id = td.depends_on WHERE td.task_id = tasks.id AND d.issue_number > 0 AND d.status <> 'done'), ''),
		        COALESCE((SELECT count(o.task_id) FROM merge_queue me LEFT JOIN merge_queue o ON o.project = me.project
		        AND (o.entered_at, o.task_id) < (me.entered_at, me.task_id) WHERE me.task_id = tasks.id GROUP BY me.task_id), -1),
		        COALESCE(esc.comment_id, 0),
		        (SELECT count(*) FROM intake_events ans WHERE ans.task_id = tasks.id AND ans.kind = 'issue_comment'
		        AND ans.trusted AND NOT ans.signalled AND ans.created_at > COALESCE(esc.at, 'infinity')),
		        COALESCE((SELECT cr.status FROM chain_runs cr WHERE tasks.pr_number > 0 AND cr.repository = tasks.repository AND cr.pr_number = tasks.pr_number
		        ORDER BY cr.run_id DESC LIMIT 1), ''),
		        (` + deployLive + `),
		        COALESCE((SELECT cr.run_index FROM chain_runs cr WHERE tasks.pr_number > 0 AND cr.repository = tasks.repository AND cr.pr_number = tasks.pr_number), 0),
		        ` + chainGreen + `,
		        (SELECT g.updated_at FROM chain_runs g WHERE g.repository = tasks.repository AND g.status = 'success' ORDER BY g.run_id DESC, g.pr_number LIMIT 1),
		        COALESCE(rl.roll_id, ''), COALESCE(rl.roll_issue, 0)`

var boardTaskFrom = `
		 FROM tasks LEFT JOIN LATERAL (SELECT roll.id AS roll_id, roll.issue_number AS roll_issue FROM roll_requests rr JOIN tasks roll ON roll.id = rr.task_id
		        WHERE tasks.status = 'merged' AND ` + rollCarries + ` AND rr.state IN ('opened', 'merged')
		        ORDER BY rr.created_at DESC LIMIT 1) rl ON true
		 LEFT JOIN LATERAL (SELECT c.id AS case_id, a.at, a.detail AS ask, case_face_comment(c.id) AS comment_id
		        FROM cases c LEFT JOIN LATERAL (SELECT ev.at, ev.detail FROM events ev WHERE ev.task_id = c.task_id AND ev.kind = '` + EventQuestionAsked + `'
		            AND ev.detail->>'case_id' = c.id::text ORDER BY ev.id DESC LIMIT 1) a ON true
		        WHERE c.task_id = tasks.id AND c.status IN ` + caseLiveStatuses + ` AND c.kind NOT IN ` + ownAnswerKindsSQL + `
		        ORDER BY c.id DESC LIMIT 1) esc ON true`

func scanBoardTask(scan func(dest ...any) error) (BoardTask, error) {
	var b BoardTask
	var at, greenAt sql.NullTime
	var ask []byte
	t, err := scanTask(func(dest ...any) error {
		return scan(append(dest, &at, &ask, &b.AskCase, &b.WaitingOn, &b.Dependencies, &b.QueueAhead, &b.QuestionComment, &b.AnswersPending,
			&b.ChainState, &b.ChainLive, &b.ChainRun, &b.ChainGreen, &greenAt, &b.RollID, &b.RollIssue)...)
	})
	if err != nil {
		return b, err
	}
	b.ChainGreenAt = greenAt.Time
	b.Task, b.EscalatedAt = t, at.Time
	if len(ask) > 0 {
		if err := json.Unmarshal(ask, &b.askRow); err != nil {
			return b, err
		}
	}
	return b, nil
}

// fillAsks sets the ask of every task in tasks that has a live ask case:
// the stop's words, the agent's choices and the five-part question, read
// from the case, read by its key, and its newest question_asked row, which
// the board's query already joined by the card's own task id, by the one
// derivation the readers of the cases take (askBody, askChoices,
// questionOfAsk).
func (s *PGStore) fillAsks(ctx context.Context, tasks []BoardTask) error {
	var ids []int64
	for _, b := range tasks {
		if b.AskCase != 0 {
			ids = append(ids, b.AskCase)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	cases, err := s.cases(ctx, `SELECT `+caseColumns+` FROM cases WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return err
	}
	for i := range tasks {
		j := slices.IndexFunc(cases, func(c Case) bool { return c.ID == tasks[i].AskCase })
		if j < 0 {
			continue
		}
		tasks[i].Escalation = askBody(cases[j])
		tasks[i].Choices = askChoices(cases[j], tasks[i].askRow)
		tasks[i].Question = questionOfAsk(tasks[i].askRow)
	}
	return nil
}

// BoardTasks lists repository's open tasks for the console's board, most
// urgent first; a chat task is no card. A merged task is open: its card
// waits until the chain runs its change live. The escalation reason comes
// from the escalation's intake event, so a poll reads no timeline object.
func (s *PGStore) BoardTasks(ctx context.Context, repository string) ([]BoardTask, error) {
	rows, err := s.db.QueryContext(ctx, boardTaskQuery+`
		 WHERE repository = $2 AND kind <> $1 AND status NOT IN ('done', 'failed', 'closed')
		 ORDER BY priority DESC, finished_at DESC NULLS LAST, tasks.id`, KindChat, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []BoardTask
	for rows.Next() {
		b, err := scanBoardTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	setSlotWaits(tasks)
	return tasks, s.fillAsks(ctx, tasks)
}

// setSlotWaits words the wait of every card in tasks, one project's live
// cards, that queues for a work slot.
func setSlotWaits(tasks []BoardTask) {
	live := 0
	var resumed, fresh []int
	for i, b := range tasks {
		switch {
		case holdsSlot(b.Task):
			live++
		case b.Status == "queued" && b.ResumedAt != nil:
			resumed = append(resumed, i)
		case waitsForSlot(b.Task) && strings.HasPrefix(b.QueueReason, WorkBoundReason):
			fresh = append(fresh, i)
		}
	}
	slices.SortFunc(resumed, func(a, b int) int {
		return cmp.Or(tasks[a].ResumedAt.Compare(*tasks[b].ResumedAt), strings.Compare(tasks[a].ID, tasks[b].ID))
	})
	slices.SortFunc(fresh, func(a, b int) int {
		x, y := tasks[a], tasks[b]
		return cmp.Or(x.CreatedAt.Compare(y.CreatedAt), cmp.Compare(x.IssueNumber, y.IssueNumber), strings.Compare(x.ID, y.ID))
	})
	for n, i := range slices.Concat(resumed, fresh) {
		tasks[i].SlotWait = fmt.Sprintf("waits for a slot, %d ahead (%d live)", n, live)
	}
}

// RollCarries are the cards the requests of roll task id carry: those
// whose pull request one of them lists, by issue.
func (s *PGStore) RollCarries(ctx context.Context, id string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE EXISTS (SELECT 1 FROM roll_requests rr
		WHERE (rr.task_id = $1 OR $1 = ANY(rr.failed_tasks)) AND `+rollCarries+`) ORDER BY tasks.issue_number, tasks.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FinishedQuery asks for a page of one project's finished cards on the
// board: those in Statuses finished after Since, or failed holding a plan
// key while a card waits on them (their Retry), by arrival, newest first
// (oldest first with Oldest), ties by id; the page after the card After
// names, at most Limit of them, all for 0.
type FinishedQuery struct {
	Repository string
	Statuses   []string
	Since      time.Time
	Oldest     bool
	After      Cursor
	Limit      int
}

// Cursor names the card a page of finished cards ends with: its arrival
// and its id; the zero Cursor is before the first.
type Cursor struct {
	At time.Time
	ID string
}

// CursorOf is the cursor of t, the last card of a page.
func CursorOf(t Task) Cursor { return Cursor{At: arrival(t), ID: t.ID} }

// arrival is when finished card t arrived in its column, as the console
// orders it: its finish stamp, else its filing.
func arrival(t Task) time.Time {
	if t.FinishedAt != nil {
		return *t.FinishedAt
	}
	return t.CreatedAt
}

// FinishedTasks lists the page q asks for, and how many cards the read
// leaves from the page on, the page's own included; a chat task is no
// card, nor a finished human task. A done card whose change is not live
// yet is no finished card: it waits in the board's progress (roadmap 296).
func (s *PGStore) FinishedTasks(ctx context.Context, q FinishedQuery) ([]BoardTask, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+boardTaskColumns+`, count(*) OVER ()`+boardTaskFrom+`
		 WHERE repository = $1 AND status = ANY($2) AND kind NOT IN ($3, 'human')
		   AND (finished_at > $4 OR status = 'failed' AND plan_key IS NOT NULL AND EXISTS (SELECT 1 FROM task_deps td JOIN tasks w ON w.id = td.task_id
		       WHERE td.depends_on = tasks.id AND w.status = 'waiting-deps'))
		   AND (status <> 'done' OR `+deployLive+`)
		   AND ($6 = '' OR COALESCE(finished_at, created_at) = $5 AND tasks.id > $6
		       OR CASE WHEN $7 THEN COALESCE(finished_at, created_at) > $5 ELSE COALESCE(finished_at, created_at) < $5 END)
		 ORDER BY CASE WHEN $7 THEN COALESCE(finished_at, created_at) END, COALESCE(finished_at, created_at) DESC, tasks.id
		 LIMIT NULLIF($8, 0)`,
		q.Repository, q.Statuses, KindChat, q.Since, q.After.At, q.After.ID, q.Oldest, q.Limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var tasks []BoardTask
	rest := 0
	for rows.Next() {
		b, err := scanBoardTask(func(dest ...any) error { return rows.Scan(append(dest, &rest)...) })
		if err != nil {
			return nil, 0, err
		}
		tasks = append(tasks, b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return tasks, rest, s.fillAsks(ctx, tasks)
}

// BoardTask is task id as BoardTasks reads it, whether or not the board
// lists it; false when there is no such task, or it is a chat task.
func (s *PGStore) BoardTask(ctx context.Context, id string) (BoardTask, bool, error) {
	b, err := scanBoardTask(s.db.QueryRowContext(ctx, boardTaskQuery+` WHERE tasks.id = $1 AND kind <> $2`,
		id, KindChat).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return BoardTask{}, false, nil
	}
	if err != nil {
		return b, false, err
	}
	one := []BoardTask{b}
	err = s.fillAsks(ctx, one)
	return one[0], err == nil, err
}

// ChainFacts are the chain behind one card's change (C38): the run that
// carries its pull request with its verdict and its moment, main's head
// row with the stall ladder on it, the newest green run with its moment,
// and the chain's own cards that act — the repair card a red head files,
// the stall card a stalled one asks with, nil while none stands. A zero
// CarryRun names a change the chain has not carried yet.
type ChainFacts struct {
	CarryRun      int64
	CarryStatus   string
	CarryAt       time.Time
	HeadRun       int64
	HeadStatus    string
	HeadAt        time.Time
	RerunAt       time.Time
	Dismissed     bool
	GreenRun      int64
	GreenAt       time.Time
	Stall, Repair *Task
}

// ChainFacts reads the chain behind repository's pull request pr: its
// carry row with the head row of main and the stall ladder on it, the
// newest green run, and the chain's acting cards. Zero facts without a
// carry row: the chain has not picked the change up, so there is nothing
// to read.
func (s *PGStore) ChainFacts(ctx context.Context, repository string, pr int64) (ChainFacts, error) {
	var f ChainFacts
	var headRun sql.NullInt64
	var headStatus sql.NullString
	var headAt, rerunAt, greenAt sql.NullTime
	var dismissed sql.NullBool
	err := s.db.QueryRowContext(ctx, `SELECT c.run_index, c.status, c.updated_at,
			h.run_index, h.status, h.updated_at, h.rerun_at, h.dismissed_at,
			COALESCE((SELECT max(g.run_index) FROM chain_runs g WHERE g.repository = $1 AND g.status = 'success'), 0),
			(SELECT g.updated_at FROM chain_runs g WHERE g.repository = $1 AND g.status = 'success' ORDER BY g.run_id DESC, g.pr_number LIMIT 1)
		FROM chain_runs c LEFT JOIN chain_runs h ON h.repository = c.repository AND h.pr_number = 0
		WHERE c.repository = $1 AND c.pr_number = $2`, repository, pr).
		Scan(&f.CarryRun, &f.CarryStatus, &f.CarryAt, &headRun, &headStatus, &headAt, &rerunAt, &dismissed, &f.GreenRun, &greenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ChainFacts{}, nil
	}
	if err != nil {
		return ChainFacts{}, err
	}
	f.HeadRun, f.HeadStatus, f.HeadAt, f.RerunAt, f.GreenAt = headRun.Int64, headStatus.String, headAt.Time, rerunAt.Time, greenAt.Time
	for _, actor := range []struct {
		source string
		into   **Task
	}{{SourceChainStall, &f.Stall}, {SourceChainRepair, &f.Repair}} {
		card, ok, err := s.ChainCard(ctx, repository, actor.source)
		if err != nil {
			return ChainFacts{}, err
		}
		if ok {
			*actor.into = &card
		}
	}
	return f, nil
}

// SortBoardTasks orders open tasks in memory as BoardTasks' ORDER BY
// does: most urgent first, then by finish time, newest first, a finish
// stamp missing last. The in-memory fakes sort with it, so the test path
// and the store cannot drift apart; ties keep their order, where the SQL
// tie-breaks by id.
func SortBoardTasks(tasks []BoardTask) {
	slices.SortStableFunc(tasks, func(a, b BoardTask) int {
		return cmp.Or(cmp.Compare(b.Priority, a.Priority), byFinish(a.Task, b.Task))
	})
}

// FinishedPage is FinishedTasks over tasks in memory, for the fakes,
// which keep no dependencies: a card without a finish stamp is in the
// window. A done card whose change is not live yet is no finished card, as
// FinishedTasks reads it; the fakes model a chain row as the task's
// ChainState at its arrival.
func FinishedPage(tasks []BoardTask, q FinishedQuery) ([]BoardTask, int) {
	order := func(a, b Task) int {
		by := arrival(b).Compare(arrival(a))
		if q.Oldest {
			by = -by
		}
		return cmp.Or(by, cmp.Compare(a.ID, b.ID))
	}
	var out []BoardTask
	for _, t := range tasks {
		if t.Repository == q.Repository && slices.Contains(q.Statuses, t.Status) && t.Kind != KindChat && t.Kind != "human" &&
			(t.FinishedAt == nil || t.FinishedAt.After(q.Since)) && (q.After.ID == "" || order(Task{ID: q.After.ID, FinishedAt: &q.After.At}, t.Task) < 0) &&
			(t.Status != "done" || DeployLive(tasks, t)) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b BoardTask) int { return order(a.Task, b.Task) })
	rest := len(out)
	if q.Limit > 0 {
		out = out[:min(q.Limit, len(out))]
	}
	return out, rest
}

// byFinish compares two cards by their finish stamps, newest first, a
// stamp missing last.
func byFinish(a, b Task) int {
	switch {
	case a.FinishedAt == nil && b.FinishedAt == nil:
		return 0
	case a.FinishedAt == nil:
		return 1
	case b.FinishedAt == nil:
		return -1
	}
	return b.FinishedAt.Compare(*a.FinishedAt)
}

// DeployLive is deployLive for the fakes: a card without a pull request,
// or whose chain verdict went green, or that no chain names and that
// arrived before the first chain the set records. A chain that names it
// as not green keeps it waiting.
func DeployLive(tasks []BoardTask, t BoardTask) bool {
	if t.PRNumber == 0 || t.ChainState == "success" {
		return true
	}
	if t.ChainState != "" {
		return false
	}
	for _, o := range tasks {
		if o.ChainState != "" && !arrival(o.Task).After(arrival(t.Task)) {
			return false
		}
	}
	return true
}
