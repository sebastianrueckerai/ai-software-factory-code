package factoryd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The plan (stage11-design.md section 1.2): a plan card's planner answers
// a plan, an approver accepts one revision, and factoryd files its cards.
const (
	// SignalPlanAccepted is the console's Accept plan (AcceptPlan): Author
	// the approver, Revision and SHA256 the revision they read.
	SignalPlanAccepted = "plan_accepted"
	// SignalPlanRejected is the console's Reject (RejectPlan): Author the
	// writer, Body why. The plan task ends failed.
	SignalPlanRejected = "plan_rejected"

	ActivityRunPlan        = "RunPlan"
	ActivityRequestPlan    = "RequestPlan"
	ActivityCreateChildren = "CreateChildren"
)

// The roles of a plan's cards.
const (
	RoleFoundation  = "foundation"
	RoleFeature     = "feature"
	RoleIntegration = "integration"
)

// The plan's own caps: the report's transport cap (maxReport) never cuts a
// valid plan.
const (
	maxPlanBytes      = 12 << 10
	maxCardBrief      = 1500
	maxCardTitle      = 120
	maxCardDirs       = 10
	maxCardAcceptance = 10
	maxAcceptanceItem = 300
)

// Plan is the planner's answer: one fenced json block of its last message.
type Plan struct {
	Summary string     `json:"summary"`
	Cards   []PlanCard `json:"cards"`
}

// PlanCard is one card of a plan: one pull request, one session.
type PlanCard struct {
	Key         string   `json:"key"`
	Role        string   `json:"role"`
	Title       string   `json:"title"`
	Brief       string   `json:"brief"`
	Directories []string `json:"directories"`
	DependsOn   []string `json:"depends_on"`
	Acceptance  []string `json:"acceptance"`
	Size        string   `json:"size"`
}

// PlanRevision is one valid plan of a plan task, as stored.
type PlanRevision struct {
	TaskID    string
	Revision  int
	Plan      json.RawMessage
	SHA256    string
	CreatedAt time.Time
}

// PlanSpec is the spec of an approval request of kind plan.
type PlanSpec struct {
	Revision int    `json:"revision"`
	SHA256   string `json:"sha256"`
}

// PlanResult is one planner session: the revision its valid plan became,
// the findings of an invalid one, or the stop that asks a human.
type PlanResult struct {
	Revision int
	SHA256   string
	Findings []string
	Escalate string
}

// maxPlanEcho bounds the last message a planner answered without a plan,
// as its next session's finding.
const maxPlanEcho = 2000

// PlanRef is the plan revision a plan task waits on, and its comment on
// the issue: only later comments ask for changes. Zero: no plan yet.
type PlanRef struct {
	Revision int
	SHA256   string
	Comment  int64
}

// ChildrenResult is CreateChildren's: the cards filed, or why the
// acceptance was refused.
type ChildrenResult struct {
	Issues  []int64
	Refused []string
}

var (
	planBlock = regexp.MustCompile("(?s)```json[ \t]*\r?\n(.*?)\r?\n[ \t]*```")
	cardKey   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	roles     = []string{RoleFoundation, RoleFeature, RoleIntegration}
	// acceptedComment is PlanAcceptedComment of any revision
	acceptedComment = regexp.MustCompile(`^Plan revision [0-9]+ accepted\.$`)
)

// ValidatePlan judges the planner's last message against the process's plan
// shape and the pinned limits: one line per finding, none for a valid plan.
func ValidatePlan(text string, shape policy.PlanShape, limits policy.Limits) []string {
	_, findings := readPlan(text, shape, limits)
	return findings
}

// readPlan is ValidatePlan with the plan it decoded.
func readPlan(text string, shape policy.PlanShape, limits policy.Limits) (Plan, []string) {
	blocks := planBlock.FindAllStringSubmatch(text, -1)
	if len(blocks) == 0 {
		return Plan{}, []string{"plan: no fenced json block in the last message"}
	}
	block := blocks[len(blocks)-1][1]
	if len(block) > maxPlanBytes {
		size := fmt.Sprintf("%.1f KiB", float64(len(block))/1024)
		if size == "12.0 KiB" {
			size = fmt.Sprintf("%d bytes", len(block))
		}
		return Plan{}, []string{"plan: the plan is " + size + "; at most 12 KiB: shorten the briefs"}
	}
	var p Plan
	dec := json.NewDecoder(strings.NewReader(block))
	dec.DisallowUnknownFields()
	err := dec.Decode(&p)
	if err == nil && dec.Decode(new(json.RawMessage)) != io.EOF {
		err = errors.New("data after the plan's object")
	}
	if err != nil {
		return Plan{}, []string{"plan: json: " + strings.TrimPrefix(err.Error(), "json: ")}
	}
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf("plan: "+format, args...)) }
	if n := len(p.Cards); n < 2 || n > shape.MaxCards {
		add("%d cards; this process allows 2 to %d", n, shape.MaxCards)
	}
	keys := map[string]int{}
	for i, c := range p.Cards {
		if !cardKey.MatchString(c.Key) {
			add("card %d: key %q: lowercase letters, digits and dashes, 2 to 32", i+1, c.Key)
		}
		if keys[c.Key]++; keys[c.Key] == 2 {
			add("card key %s is used twice", c.Key)
		}
	}
	for _, c := range p.Cards {
		if !slices.Contains(roles, c.Role) {
			add("card %s: role %q, want one of %v", c.Key, c.Role, roles)
		}
	}
	foundation, integration := cardsOf(p, RoleFoundation), cardsOf(p, RoleIntegration)
	if shape.Foundation && len(foundation) != 1 {
		add("%d cards with role foundation; the process wants exactly one", len(foundation))
	}
	if shape.Integration && len(integration) != 1 {
		add("%d cards with role integration; the process wants exactly one", len(integration))
	}
	for _, c := range p.Cards {
		if c.Role == RoleFoundation && len(c.DependsOn) > 0 {
			add("card %s: a foundation card depends on nothing", c.Key)
		}
		for _, d := range c.DependsOn {
			switch {
			case d == c.Key:
				add("card %s: depends on itself", c.Key)
			case keys[d] == 0:
				add("card %s: depends on %q, which is no card of the plan", c.Key, d)
			case slices.Contains(integration, d):
				add("card %s: depends on the integration card, which comes last", c.Key)
			}
		}
	}
	if cycle := planCycle(p, planEdges(p)); cycle != nil {
		add("the dependencies form a cycle: %s", strings.Join(cycle, " -> "))
	}
	for _, c := range p.Cards {
		out = append(out, cardFindings(c, limits)...)
	}
	return p, out
}

// cardFindings are one card's findings on its own fields.
func cardFindings(c PlanCard, limits policy.Limits) []string {
	var out []string
	add := func(format string, args ...any) {
		out = append(out, fmt.Sprintf("plan: card %s: "+format, append([]any{c.Key}, args...)...))
	}
	switch c.Size {
	case "small", "medium", "large":
	case "":
		add(`size "": want small, medium or large`)
	default:
		add("size %s: split it; a card is one package, one page or one set of endpoints", c.Size)
	}
	switch n := utf8.RuneCountInString(c.Title); {
	case n == 0:
		add("title empty")
	case n > maxCardTitle:
		add("title over %d characters", maxCardTitle)
	}
	switch n := utf8.RuneCountInString(c.Brief); {
	case n == 0:
		add("brief empty")
	case n > maxCardBrief:
		add("brief over %d characters", maxCardBrief)
	}
	if len(c.Directories) > maxCardDirs {
		add("%d directories; want 0 to %d", len(c.Directories), maxCardDirs)
	}
	for _, d := range c.Directories {
		clean := path.Clean(d)
		first, _, _ := strings.Cut(clean, "/")
		switch {
		case d == "" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../"):
			add("directory %q: a relative path inside the repository", d)
		case slices.Contains(WorkflowDirs, first):
			add("directory %q: closed to agents, the template owns it", d)
		}
	}
	if n := len(c.Acceptance); n < 1 || n > maxCardAcceptance {
		add("%d acceptance tests; want 1 to %d", n, maxCardAcceptance)
	}
	for i, a := range c.Acceptance {
		switch n := utf8.RuneCountInString(a); {
		case n == 0:
			add("acceptance test %d is empty", i+1)
		case n > maxAcceptanceItem:
			add("acceptance test %d is over %d characters", i+1, maxAcceptanceItem)
		}
	}
	return out
}

// cardsOf are the keys of p's cards of role.
func cardsOf(p Plan, role string) []string {
	var keys []string
	for _, c := range p.Cards {
		if c.Role == role {
			keys = append(keys, c.Key)
		}
	}
	return keys
}

// planEdges are each card's dependencies as factoryd files them: the ones
// the planner listed among the plan's cards, every card's on the
// foundation card, the integration card's on every other card. Edges
// ValidatePlan refuses on their own (out of the foundation, into the
// integration card, onto itself or no card) are left out, so a cycle is
// only ever one of the feature cards.
func planEdges(p Plan) map[string][]string {
	foundation, integration := cardsOf(p, RoleFoundation), cardsOf(p, RoleIntegration)
	keys := map[string]bool{}
	for _, c := range p.Cards {
		keys[c.Key] = true
	}
	edges := map[string][]string{}
	add := func(from, to string) {
		if from != to && !slices.Contains(edges[from], to) {
			edges[from] = append(edges[from], to)
		}
	}
	for _, c := range p.Cards {
		if c.Role == RoleFoundation {
			continue
		}
		for _, f := range foundation {
			add(c.Key, f)
		}
		for _, d := range c.DependsOn {
			if keys[d] && !slices.Contains(integration, d) {
				add(c.Key, d)
			}
		}
		if c.Role == RoleIntegration {
			for _, o := range p.Cards {
				if o.Role != RoleIntegration {
					add(c.Key, o.Key)
				}
			}
		}
	}
	return edges
}

// planCycle is the first cycle of edges in the plan's order, as the keys
// along it with the first repeated; nil when there is none.
func planCycle(p Plan, edges map[string][]string) []string {
	const (
		unseen = iota
		onPath
		done
	)
	state := map[string]int{}
	var stack []string
	var visit func(k string) []string
	visit = func(k string) []string {
		state[k] = onPath
		stack = append(stack, k)
		for _, d := range edges[k] {
			switch state[d] {
			case onPath:
				i := slices.Index(stack, d)
				return append(slices.Clone(stack[i:]), d)
			case unseen:
				if c := visit(d); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[k] = done
		return nil
	}
	for _, c := range p.Cards {
		if state[c.Key] == unseen {
			if cycle := visit(c.Key); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// planOrder is the plan's cards in topological order of edges: each after
// every card it depends on, ties in the plan's order.
func planOrder(p Plan, edges map[string][]string) []PlanCard {
	var out []PlanCard
	placed := map[string]bool{}
	for len(out) < len(p.Cards) {
		progress := false
		for _, c := range p.Cards {
			if placed[c.Key] || slices.ContainsFunc(edges[c.Key], func(d string) bool { return !placed[d] }) {
				continue
			}
			out, placed[c.Key], progress = append(out, c), true, true
			break
		}
		if !progress {
			panic("planOrder: a cycle ValidatePlan let through")
		}
	}
	return out
}

// dependsOn are the keys card key depends on (edges) in filing order.
func dependsOn(order []PlanCard, edges map[string][]string, key string) []string {
	var out []string
	for _, c := range order {
		if slices.Contains(edges[key], c.Key) {
			out = append(out, c.Key)
		}
	}
	return out
}

// planSHA is the sha256 of the plan's canonical JSON, which is what a
// revision stores.
func planSHA(p Plan) (json.RawMessage, string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

// planMarker marks a plan revision's comment on the plan's issue.
func planMarker(taskID string, revision int) string {
	return fmt.Sprintf("<!-- factoryd plan %s revision %d -->", taskID, revision)
}

// cardMarker marks a plan card's issue.
func cardMarker(taskID, key string) string {
	return fmt.Sprintf("<!-- factoryd plan %s card %s -->", taskID, key)
}

// PlanRow is one card of a plan as the console's plan card shows it.
type PlanRow struct {
	Key, Role, Title, Size string
	DependsOn              []string
}

// PlanRows are p's cards in filing order, each with its dependencies, the
// edges factoryd adds included, as the plan comment lists them.
func PlanRows(p Plan) []PlanRow {
	edges := planEdges(p)
	order := planOrder(p, edges)
	rows := make([]PlanRow, len(order))
	for i, c := range order {
		rows[i] = PlanRow{Key: c.Key, Role: c.Role, Title: c.Title, Size: c.Size, DependsOn: dependsOn(order, edges, c.Key)}
	}
	return rows
}

// cell is s as one Markdown table cell.
func cell(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", `\|`)
}

// planComment is revision's comment on the plan's issue before its
// marker: the summary and one row per card in filing order, with its issue
// once filed (issues by key), and each brief in a details block.
func planComment(taskID string, revision int, p Plan, issues map[string]int64) string {
	edges := planEdges(p)
	var b strings.Builder
	fmt.Fprintf(&b, "factoryd: plan revision %d of task %s, written by the planner. An approver accepts it on the factory console; "+
		"a comment here asks the planner for a new revision; closing the issue ends the plan.\n\n%s\n\n", revision, taskID, strings.TrimSpace(p.Summary))
	b.WriteString("| # | key | role | title | size | depends on | issue |\n|---|---|---|---|---|---|---|\n")
	order := planOrder(p, edges)
	for i, c := range order {
		issue := ""
		if n := issues[c.Key]; n != 0 {
			issue = fmt.Sprintf("#%d", n)
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s | %s |\n", i+1, cell(c.Key), cell(c.Role), cell(c.Title), cell(c.Size),
			cell(strings.Join(dependsOn(order, edges, c.Key), ", ")), issue)
	}
	for _, c := range order {
		fmt.Fprintf(&b, "\n<details><summary>%s: %s</summary>\n\n%s\n\n%s</details>\n", cell(c.Key), cell(c.Title), strings.TrimSpace(c.Brief), cardFacts(c))
	}
	return b.String()
}

// cardFacts are a card's directories, acceptance tests and size as its
// issue and the plan comment list them.
func cardFacts(c PlanCard) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Directories (guidance, not a boundary): %s\n", cmp.Or(strings.Join(c.Directories, ", "), "none"))
	b.WriteString("Acceptance:\n")
	for _, a := range c.Acceptance {
		fmt.Fprintf(&b, "- %s\n", a)
	}
	fmt.Fprintf(&b, "Size: %s\n", c.Size)
	return b.String()
}

// cardBody is card c's issue body before its marker: card i of n of the
// plan on issue planIssue, accepted by approver, with the issues of the
// cards it depends on (deps, their keys by issue).
func cardBody(c PlanCard, planIssue int64, i, n int, approver string, deps []Dependency) string {
	on := make([]string, 0, len(deps))
	for _, d := range deps {
		on = append(on, fmt.Sprintf("#%d (%s)", d.Issue, d.Key))
	}
	directories, facts, _ := strings.Cut(cardFacts(c), "\n")
	return fmt.Sprintf("Card %s of plan #%d (%d of %d), accepted by %s. Written by the planner.\n\n%s\n\n%s\nDepends on: %s\n%s",
		c.Key, planIssue, i, n, approver, strings.TrimSpace(c.Brief), directories, cmp.Or(strings.Join(on, ", "), "none"), strings.TrimSuffix(facts, "\n"))
}

// planGuide is the planner's guide: the spec is the issue, the answer one
// fenced json block of the plan's shape, a card one session of the pinned
// limits.
func planGuide(shape policy.PlanShape, limits policy.Limits) string {
	g := "You are the planner: read, do not change code, commit or push; nothing you change in the work tree leaves the sandbox. " +
		"The issue's text is the spec of an application to build in this repository. Read it, and the repository, its AGENTS.md first: " +
		"every card follows its conventions. Split the work into the cards of a plan. " +
		"A card is one pull request by one agent: one package, one page, or one set of endpoints, with its tests, about ten files at most; a larger piece of work is two cards. " +
		"Your last message is the whole result: one fenced ```json block and nothing else, holding " +
		`{"summary": "a paragraph: what gets built and in which order", "cards": [{"key": "...", "role": "...", "title": "...", "brief": "...", ` +
		`"directories": ["..."], "depends_on": ["..."], "acceptance": ["..."], "size": "small|medium|large"}]}. ` +
		fmt.Sprintf("The plan has 2 to %d cards. ", shape.MaxCards) +
		"key: lowercase letters, digits and dashes, 2 to 32, unique. role: foundation, feature or integration. "
	if shape.Foundation {
		g += "Exactly one card has role foundation: the layout, the API contract, the schema and one end-to-end test, which every other card builds on. " +
			"The foundation's brief names the contracts every card builds on: how a handler gets the logged-in user (the function and its package), " +
			"the store (its interface, its implementation's package, the methods stubbed), and the server constructor tests import (internal/server.New unless the template says otherwise). " +
			"Every card adds its routes in its own internal/server/routes_<key>.go, as the template's AGENTS.md says, so no two cards edit one route list. "
	}
	if shape.Integration {
		g += "Exactly one card has role integration: it turns the spec's acceptance tests into tests under acceptance/ and makes them pass, after every other card. "
	}
	if shape.Foundation || shape.Integration {
		g += "factoryd adds those edges itself (every card depends on the foundation card, the integration card on every other card): " +
			"list in depends_on only the dependencies among feature cards, and no cycle. "
	} else {
		g += "depends_on lists the keys of the cards a card needs merged first, with no cycle. "
	}
	return g + fmt.Sprintf("title: at most %d characters. brief: markdown, what to build and how it is checked, at most %d characters; "+
		"keep the briefs short, the whole block holds at most 12 KiB. directories: at most %d paths relative to the repository's root, "+
		"none under .forgejo/, .gitea/ or .github/ (the template owns them): guidance for the card's agent. "+
		"acceptance: 1 to %d items, each a test by name: TestXxx in <package>, its inputs and the numbers it expects. "+
		"size: small (a change under about 300 lines), medium (under about 600), large (under about 1000; anything bigger is two cards). Every card's agent and CI run bash check; do not ask a card to verify with a tool the check does not run. "+
		"A writer reads your plan on the issue; a comment asking for changes comes back to you as a finding, with your last plan: "+
		"answer the whole plan again.", maxCardTitle, maxCardBrief, maxCardDirs, maxCardAcceptance)
}

// PlanAcceptedComment is the console's comment, as the approver, on the
// plan's issue when they accept revision: the plan's wait does not read it
// as a change request.
func PlanAcceptedComment(revision int) string {
	return fmt.Sprintf("Plan revision %d accepted.", revision)
}

// ErrStalePlan refuses an acceptance of a revision that is no longer the
// newest; its message is the card's.
type ErrStalePlan struct{ Newest int }

func (e ErrStalePlan) Error() string {
	return fmt.Sprintf("the plan changed to revision %d; reload", e.Newest)
}

// AcceptPlan is the console's Accept plan, by an approver: it signals the
// plan task's workflow when revision (at sha) is the newest and awaits
// acceptance; a newer revision refuses it with ErrStalePlan.
func AcceptPlan(ctx context.Context, store Store, wf Workflows, taskID string, revision int, sha, by string) error {
	if _, err := pendingPlan(ctx, store, taskID); err != nil {
		return err
	}
	newest, ok, err := store.PlanRevision(ctx, taskID, 0)
	if err != nil {
		return err
	}
	if !ok || newest.Revision != revision || newest.SHA256 != sha {
		return ErrStalePlan{Newest: newest.Revision}
	}
	return wf.Signal(ctx, taskID, SignalPlanAccepted, TaskSignal{Author: by, Revision: revision, SHA256: sha})
}

// RejectPlan is the console's Reject, by a writer: the plan task ends
// failed with why.
func RejectPlan(ctx context.Context, store Store, wf Workflows, taskID, by, why string) error {
	if _, err := pendingPlan(ctx, store, taskID); err != nil {
		return err
	}
	return wf.Signal(ctx, taskID, SignalPlanRejected, TaskSignal{Author: by, Body: why})
}

// pendingPlan is the task's pending approval request of kind plan.
func pendingPlan(ctx context.Context, store Store, taskID string) (ApprovalRequest, error) {
	pending, err := store.ApprovalRequests(ctx, "pending")
	if err != nil {
		return ApprovalRequest{}, err
	}
	for _, a := range pending {
		if a.Kind == ApprovalPlan && a.TaskID == taskID {
			return a, nil
		}
	}
	return ApprovalRequest{}, fmt.Errorf("task %s has no plan waiting for acceptance", taskID)
}

func (s *PGStore) AddPlanRevision(ctx context.Context, id string, plan json.RawMessage, sha string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `WITH newest AS (SELECT revision, sha256 FROM plan_revisions WHERE task_id = $1 ORDER BY revision DESC LIMIT 1),
		added AS (INSERT INTO plan_revisions (task_id, revision, plan, sha256)
			SELECT $1, COALESCE((SELECT revision FROM newest), 0) + 1, $2::jsonb, $3
			WHERE NOT EXISTS (SELECT 1 FROM newest WHERE sha256 = $3) RETURNING revision)
		SELECT revision FROM added UNION ALL SELECT revision FROM newest WHERE sha256 = $3`, id, []byte(plan), sha).Scan(&n)
	return n, err
}

func (s *PGStore) PlanRevision(ctx context.Context, id string, n int) (PlanRevision, bool, error) {
	var p PlanRevision
	var plan []byte
	err := s.db.QueryRowContext(ctx, `SELECT task_id, revision, plan, sha256, created_at FROM plan_revisions
		WHERE task_id = $1 AND ($2 = 0 OR revision = $2) ORDER BY revision DESC LIMIT 1`, id, n).Scan(&p.TaskID, &p.Revision, &plan, &p.SHA256, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanRevision{}, false, nil
	}
	p.Plan = plan
	return p, err == nil, err
}

func (s *PGStore) CreatePlanCard(ctx context.Context, t Task) (Task, error) {
	if err := s.insertTask(ctx, t, `ON CONFLICT (parent_task, plan_key) WHERE source = 'plan' DO NOTHING`); err != nil {
		return Task{}, err
	}
	got, ok, err := s.oneTask(ctx, `parent_task = $1 AND source = $2 AND plan_key = $3`, t.Parent, SourcePlan, t.PlanKey)
	if err == nil && !ok {
		err = fmt.Errorf("card %s of plan %s not found after its insert", t.PlanKey, t.Parent)
	}
	return got, err
}

func (s *PGStore) SetTaskIssue(ctx context.Context, id string, issue int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET issue_number = $2 WHERE id = $1`, id, issue)
	return oneRow(res, err, "task "+id)
}

// planLoop is the program of a task of session mode plan: planner sessions
// until one answers a valid plan, then the approvers' wait. A writer's
// comment sends the planner back with the last plan; Accept plan files the
// cards (a refusal sends the planner back); Reject ends the task failed, a
// closed issue closed. Every session counts against the session cap, where
// the card waits for a human and a writer's comment buys one more.
func (t *taskRun) planLoop() (string, error) {
	if err := workflow.ExecuteActivity(t.short, ActivityEnsureIssue, t.taskID).Get(t.ctx, nil); err != nil {
		return t.finish("failed", "ensure issue: "+errMessage(err))
	}
	var st RunState
	if err := workflow.ExecuteActivity(t.short, ActivityReconcile, t.taskID).Get(t.ctx, &st); err != nil {
		return t.finish("failed", "reconcile: "+errMessage(err))
	}
	switch {
	case st.Skip:
		return st.Status, nil
	case st.IssueClosed:
		return t.finish("closed", ReasonIssueClosed+" before the plan was accepted")
	}
	u := st.Usage
	var ref PlanRef
	healed := st.Status == "waiting-human"
	if healed {
		if err := workflow.ExecuteActivity(t.short, ActivityRequestPlan, t.taskID, 0).Get(t.ctx, &ref); err != nil {
			return t.finish("failed", "request plan: "+errMessage(err))
		}
		t.escalation = ref.Comment
	}
	wait := ref.Revision != 0
	var findings []string
	var ask string // a question for a human: the cap card, or a stop's
	reason := "no session answered a plan"
	// the comment the running or next session answers: writers' comments
	// newer than it ask for changes
	floor := t.escalation
	for {
		if !wait && ask == "" && u.Attempts >= t.cfg.MaxSessions+u.ExtraAttempts {
			ask = t.sessionCapCard(reason, findings)
			if healed {
				ask = healedReason
			}
		}
		if ask != "" {
			if err := workflow.ExecuteActivity(t.short, ActivityEscalate, t.taskID, ask, []string(nil)).Get(t.ctx, &t.escalation); err != nil {
				return t.finish("failed", escalateFailed+errMessage(err))
			}
			ask, wait = "", true
		}
		healed = false
		if wait {
			final, why, next := t.awaitPlan(&ref, floor)
			if final != "" {
				return t.finish(final, why)
			}
			findings, wait = next, false
			if u.Attempts >= t.cfg.MaxSessions+u.ExtraAttempts {
				t.grant(&u)
			}
			continue
		}
		remaining := t.cfg.WallClockCap + u.ExtraTime - u.AgentTime
		if remaining <= 0 {
			return t.finish("failed", "wall-clock cap reached")
		}
		if err := workflow.ExecuteActivity(t.short, ActivitySetState, t.taskID, "running").Get(t.ctx, nil); err != nil {
			return t.finish("failed", "set running: "+errMessage(err))
		}
		floor = t.escalation
		began := workflow.Now(t.ctx)
		var res PlanResult
		v, decided, err := t.race(t.sessionOptions(remaining), &res, ActivityRunPlan, t.taskID, t.cfg.Models.Stronger, t.cfg.PolicyVersion, findings)
		u.AgentTime += workflow.Now(t.ctx).Sub(began)
		u.Attempts++
		t.recordUsage(u)
		if final, why, ok := t.planRejected(); ok {
			return t.finish(final, why)
		}
		switch {
		case decided && v.final != "":
			final, why, _ := closedBeforeStart(v.final, v.reason)
			return t.finish(final, why)
		case decided:
			reason, findings, ask = "the gate supervisor stopped the planner session", nil, v.escalate
			continue
		case isTerminal(err):
			return t.finish("failed", "planner session: "+errMessage(err))
		case err != nil:
			reason = "planner session failed: " + errMessage(err)
			findings = append([]string{"the last planner session failed: " + errMessage(err)}, t.planComments(floor)...)
			continue
		case res.Escalate != "":
			reason, findings, ask = "the planner asked a human", nil, res.Escalate
			continue
		case len(res.Findings) > 0:
			reason, findings = "the plan was invalid", append(res.Findings, t.planComments(floor)...)
			continue
		}
		if err := workflow.ExecuteActivity(t.short, ActivityRequestPlan, t.taskID, res.Revision).Get(t.ctx, &ref); err != nil {
			return t.finish("failed", "request plan: "+errMessage(err))
		}
		t.escalation, findings, wait = ref.Comment, nil, true
		t.approvalRequested = workflow.Now(t.ctx)
		reason = fmt.Sprintf("plan revision %d waits for an approver", ref.Revision)
	}
}

// planRejected takes a Reject sent while a session ran: the task ends
// before the next session spends.
func (t *taskRun) planRejected() (final, why string, ok bool) {
	var sig TaskSignal
	if workflow.GetSignalChannel(t.ctx, SignalPlanRejected).ReceiveAsync(&sig) {
		t.recordPlanSignal(SignalPlanRejected, sig, "")
		return "failed", fmt.Sprintf("plan rejected by %s: %s", sig.Author, sig.Body), true
	}
	return "", "", false
}

// planComments takes the writers' comments signalled so far: each newer
// than floor, the comment the session answered, asks for changes; the
// console's acceptance comment asks for nothing.
func (t *taskRun) planComments(floor int64) []string {
	var out []string
	var sig TaskSignal
	for workflow.GetSignalChannel(t.ctx, SignalHumanComment).ReceiveAsync(&sig) {
		detail := map[string]any{"signal": SignalHumanComment, "author": sig.Author, "comment": sig.Comment}
		switch {
		case sig.Comment <= floor:
			detail["ignored"] = "older than the plan or question the planner answered"
		case acceptedComment.MatchString(strings.TrimSpace(sig.Body)):
			detail["ignored"] = "the console's acceptance comment"
		default:
			out = append(out, "writer "+sig.Author+" asked for changes to the plan: "+sig.Body)
		}
		if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal", detail).Get(t.ctx, nil); err != nil {
			workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
		}
	}
	return out
}

// awaitPlan waits, at no cost, for the approvers on revision ref (none
// yet: only a writer's comment or an end): the task's end, or the
// findings the next planner session starts with. An accepted revision
// files its cards and ends the task done; a refused acceptance, or a
// writer's comment, goes back to the planner.
func (t *taskRun) awaitPlan(ref *PlanRef, floor int64) (final, why string, findings []string) {
	for {
		sel := workflow.NewSelector(t.ctx)
		for _, n := range []string{SignalIssueClosed, SignalHumanComment, SignalPlanAccepted, SignalPlanRejected} {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
		}
		sel.Select(t.ctx)
		var sig TaskSignal
		if workflow.GetSignalChannel(t.ctx, SignalIssueClosed).ReceiveAsync(&sig) {
			t.counted(SignalIssueClosed, sig)
			return "closed", ReasonIssueClosed + " before the plan was accepted", nil
		}
		if final, why, ok := t.planRejected(); ok {
			return final, why, nil
		}
		for workflow.GetSignalChannel(t.ctx, SignalPlanAccepted).ReceiveAsync(&sig) {
			if ref.Revision == 0 {
				t.recordPlanSignal(SignalPlanAccepted, sig, "no plan waits for acceptance")
				continue
			}
			if sig.Revision != ref.Revision || sig.SHA256 != ref.SHA256 {
				t.recordPlanSignal(SignalPlanAccepted, sig, fmt.Sprintf("the plan changed to revision %d", ref.Revision))
				continue
			}
			t.recordPlanSignal(SignalPlanAccepted, sig, "")
			var res ChildrenResult
			if err := workflow.ExecuteActivity(t.short, ActivityCreateChildren, t.taskID, ref.Revision, sig.Author, t.cfg.PolicyVersion).Get(t.ctx, &res); err != nil {
				return "failed", "create the plan's cards: " + errMessage(err), nil
			}
			if len(res.Refused) > 0 {
				*ref = PlanRef{}
				return "", "", res.Refused
			}
			return "done", fmt.Sprintf("plan revision %d accepted: %d cards filed", sig.Revision, len(res.Issues)), nil
		}
		findings = t.planComments(floor)
		if len(findings) > 0 {
			return "", "", findings
		}
	}
}

// recordPlanSignal records an approver's or writer's plan signal on the
// timeline, with why it does not count when it does not.
func (t *taskRun) recordPlanSignal(name string, sig TaskSignal, ignored string) {
	detail := map[string]any{"signal": name, "author": sig.Author, "revision": sig.Revision}
	switch {
	case ignored != "":
		detail["ignored"] = ignored
	case !t.approvalRequested.IsZero():
		// the approvers' wait ends: its step carries the wait's start
		detail["started"] = rfc3339(t.approvalRequested)
		t.approvalRequested = time.Time{}
	}
	if err := workflow.ExecuteActivity(t.short, ActivityRecordStep, t.taskID, "signal", detail).Get(t.ctx, nil); err != nil {
		workflow.GetLogger(t.ctx).Warn("record signal", "error", err)
	}
}

// planShape is the plan lane's shape of the process task t follows in
// version v, and the process's name.
func planShape(v policy.Version, t Task) (policy.PlanShape, string, error) {
	name := t.Process
	if name == "" {
		_, project, _ := v.ProjectOf(t.Repository)
		name = project.Process
	}
	lane, ok := v.Processes[name].Lane(policy.LanePlan)
	if !ok || lane.Plan == nil {
		return policy.PlanShape{}, "", terminalError("task %s: process %q has no lane plan in configuration version %d", t.ID, name, v.ID)
	}
	return *lane.Plan, name, nil
}

// RunPlan is one planner session under the pinned version, with findings
// and the last plan: a valid plan becomes the task's next revision, an
// invalid one comes back as its findings.
func (r *Runner) RunPlan(ctx context.Context, taskID, model string, pin int64, findings []string) (PlanResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PlanResult{}, err
	}
	v, err := r.pinnedVersion(ctx, pin)
	if err != nil {
		return PlanResult{}, err
	}
	shape, _, err := planShape(v, t)
	if err != nil {
		return PlanResult{}, err
	}
	last, ok, err := r.store.PlanRevision(ctx, taskID, 0)
	if err != nil {
		return PlanResult{}, err
	}
	if ok {
		findings = append(slices.Clone(findings), fmt.Sprintf("your last plan, revision %d: answer the whole plan again, changed as the findings ask:\n```json\n%s\n```", last.Revision, last.Plan))
	}
	text, why, escalated, err := r.reportSession(ctx, t, repo, model, t.Standing(), pin, planGuide(shape, v.Document.Limits), findings)
	if err != nil {
		return PlanResult{}, err
	}
	if escalated {
		r.record(ctx, taskID, "plan_escalated", map[string]any{"reason": text})
		return PlanResult{Escalate: text}, nil
	}
	p, found := readPlan(text, shape, v.Document.Limits)
	switch {
	case text == "":
		found = append(found, "plan: the session ended without a last message: "+why)
	case len(p.Cards) == 0 && len(found) > 0:
		found = append(found, "the planner's last message: "+textutil.Truncate(strings.TrimSpace(text), maxPlanEcho))
	}
	if len(found) > 0 {
		r.record(ctx, taskID, "plan_invalid", map[string]any{"findings": found})
		return PlanResult{Findings: found}, nil
	}
	raw, sha, err := planSHA(p)
	if err != nil {
		return PlanResult{}, err
	}
	n, err := r.store.AddPlanRevision(ctx, taskID, raw, sha)
	if err != nil {
		return PlanResult{}, err
	}
	r.record(ctx, taskID, "plan_revision", map[string]any{"revision": n, "cards": len(p.Cards), "sha256": sha})
	return PlanResult{Revision: n, SHA256: sha}, nil
}

// RequestPlan puts revision (0: the newest, none: a zero PlanRef) before
// the approvers: its comment on the plan's issue, once; an approval
// request of kind plan, which supersedes the one of an earlier revision;
// the task waiting on a human.
func (r *Runner) RequestPlan(ctx context.Context, taskID string, revision int) (PlanRef, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PlanRef{}, err
	}
	rev, ok, err := r.store.PlanRevision(ctx, taskID, revision)
	switch {
	case err != nil:
		return PlanRef{}, err
	case !ok && revision == 0:
		return PlanRef{}, nil
	case !ok:
		return PlanRef{}, terminalError("task %s has no plan revision %d", taskID, revision)
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return PlanRef{}, terminalError("task %s: plan revision %d: %v", taskID, rev.Revision, err)
	}
	comment, err := r.postPlan(ctx, t, repo, rev.Revision, p, nil)
	if err != nil {
		return PlanRef{}, err
	}
	pending, err := r.store.ApprovalRequests(ctx, "pending")
	if err != nil {
		return PlanRef{}, err
	}
	want := PlanSpec{Revision: rev.Revision, SHA256: rev.SHA256}
	spec, err := json.Marshal(want)
	if err != nil {
		return PlanRef{}, err
	}
	if !slices.ContainsFunc(pending, func(a ApprovalRequest) bool {
		var have PlanSpec
		return a.Kind == ApprovalPlan && a.TaskID == taskID && json.Unmarshal(a.Spec, &have) == nil && have == want
	}) {
		id, err := r.store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: taskID, Project: t.Repository, Spec: spec,
			Reason: fmt.Sprintf("plan revision %d of issue #%d, %d cards, waits for an approver", rev.Revision, t.IssueNumber, len(p.Cards))})
		if err != nil {
			return PlanRef{}, err
		}
		r.record(ctx, taskID, "approval_requested", map[string]any{"approval": id, "revision": rev.Revision, "sha256": rev.SHA256})
	}
	if t.Status != "waiting-human" {
		if err := r.store.TransitionTask(ctx, taskID, []string{"queued", "running"}, "waiting-human"); err != nil {
			return PlanRef{}, terminalError("task %s is %s, cannot wait for its approvers: %v", taskID, t.Status, err)
		}
		mirrorTaskState(ctx, r.forgejo, t, "waiting-human")
	}
	return PlanRef{Revision: rev.Revision, SHA256: rev.SHA256, Comment: comment}, nil
}

// postPlan puts revision's comment on the plan's issue, sanitised, with
// the cards' issues once filed: it edits the comment its marker finds, or
// posts it.
func (r *Runner) postPlan(ctx context.Context, t Task, repo Repo, revision int, p Plan, issues map[string]int64) (int64, error) {
	marker := planMarker(t.ID, revision)
	body := r.untrusted(t, planComment(t.ID, revision, p, issues)) + "\n" + marker
	have, err := r.forgejo.ListIssueComments(ctx, repo.Owner, repo.Name, t.IssueNumber)
	if err != nil {
		return 0, err
	}
	for _, c := range have {
		if c.Author == r.forgejo.BotUser && strings.Contains(c.Body, marker) {
			if c.Body == body {
				return c.ID, nil
			}
			return c.ID, r.forgejo.EditIssueComment(ctx, repo.Owner, repo.Name, c.ID, body)
		}
	}
	return r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, body)
}

// CreateChildren files the cards of the plan task's accepted revision,
// once however often it runs: it judges the revision again (a finding
// refuses the acceptance: the approval ends, the planner gets it); files
// each card in topological order as a backlog task row and a bot issue
// found by its marker or filed; records the edges in Forgejo and
// task_deps; puts the issues into the plan's comment.
func (r *Runner) CreateChildren(ctx context.Context, taskID string, revision int, approver string, pin int64) (ChildrenResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return ChildrenResult{}, err
	}
	v, err := r.pinnedVersion(ctx, pin)
	if err != nil {
		return ChildrenResult{}, err
	}
	shape, process, err := planShape(v, t)
	if err != nil {
		return ChildrenResult{}, err
	}
	rev, ok, err := r.store.PlanRevision(ctx, taskID, revision)
	if err != nil {
		return ChildrenResult{}, err
	}
	if !ok {
		return ChildrenResult{}, terminalError("task %s has no plan revision %d", taskID, revision)
	}
	p, findings := readPlan("```json\n"+string(rev.Plan)+"\n```", shape, v.Document.Limits)
	if len(findings) > 0 {
		note := fmt.Sprintf("accepting revision %d was refused: %s", revision, strings.Join(findings, "; "))
		if err := r.store.ResolveApprovals(ctx, taskID, ApprovalPlan, "denied", FactoryDecider, note); err != nil {
			return ChildrenResult{}, err
		}
		r.commentOnce(ctx, t, repo, t.IssueNumber, "factoryd: "+r.untrusted(t, note))
		r.record(ctx, taskID, "plan_refused", map[string]any{"revision": revision, "by": approver, "findings": findings})
		return ChildrenResult{Refused: findings}, nil
	}
	edges := planEdges(p)
	order := planOrder(p, edges)
	_, project, _ := v.ProjectOf(t.Repository)
	cards := map[string]Task{}
	var open []webhookIssue
	var issues []int64
	for i, c := range order {
		var deps []Dependency
		for _, d := range dependsOn(order, edges, c.Key) {
			deps = append(deps, Dependency{TaskID: cards[d].ID, Issue: cards[d].IssueNumber, Key: d})
		}
		title := r.untrusted(t, c.Title)
		body := r.untrusted(t, cardBody(c, t.IssueNumber, i+1, len(order), approver, deps))
		card, err := r.store.CreatePlanCard(ctx, Task{ID: NewTaskID(), Priority: t.Priority, RiskClass: "standard",
			BudgetEUR: policy.CardBudget(v.Document.Budget, project.Budget, c.Size, 0), Status: "backlog", Lane: policy.LaneBacklog, Process: process,
			Objective: title + "\n\n" + body, Repository: t.Repository, Kind: "agent", Source: SourcePlan, Parent: taskID, PlanKey: c.Key,
			Author: approver, AuthorWriter: true})
		if err != nil {
			return ChildrenResult{}, err
		}
		if card.IssueNumber == 0 {
			marker := cardMarker(taskID, c.Key)
			if open == nil {
				if open, err = r.forgejo.OpenIssues(ctx, repo.Owner, repo.Name); err != nil {
					return ChildrenResult{}, err
				}
			}
			i := slices.IndexFunc(open, func(is webhookIssue) bool {
				return is.Poster.UserName == r.forgejo.BotUser && strings.Contains(is.Body, marker)
			})
			if i >= 0 {
				card.IssueNumber = open[i].Index
			} else {
				ref, err := r.forgejo.CreateIssue(ctx, repo.Owner, repo.Name, title, body+"\n\n"+marker)
				if err != nil {
					return ChildrenResult{}, err
				}
				card.IssueNumber = ref.Number
			}
			if err := r.forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, card.IssueNumber, []string{LabelBacklog, "state/backlog"}); err != nil {
				return ChildrenResult{}, err
			}
			if err := r.store.SetTaskIssue(ctx, card.ID, card.IssueNumber); err != nil {
				return ChildrenResult{}, err
			}
			r.timeline.recordEvent(ctx, card.ID, EventCardCreated, map[string]any{"by": approver, "issue": card.IssueNumber, "plan": taskID})
		}
		cards[c.Key] = card
		issues = append(issues, card.IssueNumber)
	}
	for _, c := range order {
		if len(edges[c.Key]) == 0 {
			continue
		}
		child := cards[c.Key]
		have, err := r.forgejo.IssueDependencies(ctx, repo.Owner, repo.Name, child.IssueNumber)
		if err != nil {
			return ChildrenResult{}, err
		}
		var ids []string
		for _, d := range dependsOn(order, edges, c.Key) {
			dep := cards[d]
			ids = append(ids, dep.ID)
			if !slices.Contains(have, dep.IssueNumber) {
				if err := r.forgejo.AddIssueDependency(ctx, repo, child.IssueNumber, dep.IssueNumber); err != nil {
					return ChildrenResult{}, err
				}
			}
		}
		if err := r.store.AddTaskDeps(ctx, child.ID, ids); err != nil {
			return ChildrenResult{}, err
		}
	}
	byKey := map[string]int64{}
	for k, c := range cards {
		byKey[k] = c.IssueNumber
	}
	if _, err := r.postPlan(ctx, t, repo, revision, p, byKey); err != nil {
		return ChildrenResult{}, err
	}
	if err := r.store.ResolveApprovals(ctx, taskID, ApprovalPlan, "approved", approver, fmt.Sprintf("revision %d accepted: %d cards filed", revision, len(issues))); err != nil {
		return ChildrenResult{}, err
	}
	r.record(ctx, taskID, "children_created", map[string]any{"revision": revision, "by": approver, "issues": issues})
	return ChildrenResult{Issues: issues}, nil
}

// EditIssueComment replaces comment id's body.
func (f *Forgejo) EditIssueComment(ctx context.Context, owner, repo string, id int64, body string) error {
	payload, err := json.Marshal(map[string]any{"body": body})
	if err != nil {
		return err
	}
	_, err = f.call(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/repos/%s/%s/issues/comments/%d", owner, repo, id), payload, http.StatusOK)
	return err
}

// IssueDependencies are the numbers of the issues issue number depends on
// (Forgejo's blocking dependencies), all pages.
func (f *Forgejo) IssueDependencies(ctx context.Context, owner, repo string, number int64) ([]int64, error) {
	deps, err := GetPages[struct {
		Number int64 `json:"number"`
	}](ctx, f.call, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/dependencies?", owner, repo, number))
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(deps))
	for _, d := range deps {
		out = append(out, d.Number)
	}
	return out, nil
}

// AddIssueDependency makes issue number of repo depend on issue dependency
// of the same repository (swagger issueCreateIssueDependencies).
func (f *Forgejo) AddIssueDependency(ctx context.Context, repo Repo, number, dependency int64) error {
	payload, err := json.Marshal(map[string]any{"owner": repo.Owner, "repo": repo.Name, "index": dependency})
	if err != nil {
		return err
	}
	_, err = f.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/dependencies", repo.Owner, repo.Name, number), payload, http.StatusCreated)
	return err
}

// planOf is plan card t's section "The spec and the plan" (its plan's
// issue text and the accepted plan's summary) and its role; "" for a task
// no plan filed.
func (r *Runner) planOf(ctx context.Context, t Task) (spec, role string, err error) {
	if t.Source != SourcePlan || t.Parent == "" {
		return "", "", nil
	}
	plan, _, err := r.task(ctx, t.Parent)
	if err != nil {
		return "", "", err
	}
	rev, ok, err := r.store.PlanRevision(ctx, t.Parent, 0)
	if err != nil || !ok {
		return "", "", cmp.Or(err, error(terminalError("plan %s of card %s has no revision", t.Parent, t.ID)))
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return "", "", terminalError("plan %s revision %d: %v", t.Parent, rev.Revision, err)
	}
	role = RoleFeature
	if i := slices.IndexFunc(p.Cards, func(c PlanCard) bool { return c.Key == t.PlanKey }); i >= 0 {
		role = p.Cards[i].Role
	}
	return plan.Objective + "\n\nThe plan's summary: " + strings.TrimSpace(p.Summary), role, nil
}
