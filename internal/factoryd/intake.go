package factoryd

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// Intake is the Forgejo webhook endpoint: every issue becomes a card of
// the lane its labels ask for (a starting lane's task, which starts its
// workflow, a backlog card, or a human-lane task), every PR/issue comment
// or review becomes an intake event, and the events a task workflow waits
// on (merge, changes requested, issue closed, an answer) are signalled to
// it. It implements http.Handler directly; main wires it at POST
// /webhooks/forgejo.
type Intake struct {
	Store     Store
	Forgejo   *Forgejo
	Workflows Workflows
	BotUser   string // factory-agent: its own issues/comments never spawn tasks or events
	Secret    string // HMAC-SHA256 webhook secret, shared with Forgejo's webhook config
	// Policy is the configuration in force: a new task's budget, each
	// project's approvers, who may move a card into a lane moved by the
	// approvers.
	Policy func() policy.Tree
	// Projects are where the heal pass looks for open issues no webhook
	// turned into a task: every active project's repository.
	Projects interface {
		Projects(ctx context.Context) ([]Project, error)
	}
	// MainPushed is told of every push to a repository's main.
	MainPushed func(ctx context.Context, repository string) error
	// Metrics counts the answers among the comments; nil counts none.
	Metrics *telemetry.Metrics
	// Timeline records the events of the cards intake creates and moves;
	// nil records none.
	Timeline *Timeline

	trustMu sync.Mutex
	trust   map[string]time.Time // repo + user → trusted until
}

// trustTTL is how long a positive verdict is reused; a denial is checked
// again every time, so a newly granted user is heard at once.
const trustTTL = time.Minute

// trusted reports whether user may steer a task through comments and
// reviews: code write on the repository of a project (Forgejo.Writers). A
// comment by anyone else is recorded, never signalled, unless it is the
// issue author's answer to an information request.
func (in *Intake) trusted(ctx context.Context, repository, user string) (bool, error) {
	key := repository + "\x00" + user
	in.trustMu.Lock()
	until, ok := in.trust[key]
	in.trustMu.Unlock()
	if ok && time.Now().Before(until) {
		return true, nil
	}
	p, ok, err := in.Store.ProjectByRepository(ctx, repository)
	if err != nil {
		return false, err
	}
	if !ok {
		log.Printf("intake: %s is no project's repository: %s not trusted to steer a task", repository, user)
		return false, nil
	}
	writers, err := in.Forgejo.Writers(ctx, p)
	if err != nil {
		return false, fmt.Errorf("writers of %s: %w", repository, err)
	}
	if !slices.Contains(writers, user) {
		log.Printf("intake: %s cannot write %s: not trusted to steer a task", user, repository)
		return false, nil
	}
	in.trustMu.Lock()
	if in.trust == nil {
		in.trust = map[string]time.Time{}
	}
	in.trust[key] = time.Now().Add(trustTTL)
	in.trustMu.Unlock()
	return true, nil
}

func (in *Intake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	eventType := r.Header.Get("X-Forgejo-Event-Type")
	if !validWebhookSignature(in.Secret, body, r.Header.Get("X-Forgejo-Signature")) {
		log.Printf("webhook intake: rejected %s: bad signature", strconv.Quote(eventType))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	// A delivery is marked processed only after it was: a crash in between
	// makes a redelivery process it again, which every step tolerates (task
	// creation is keyed by issue, intake events by delivery id, workflow
	// starts by workflow id, and the workflow coalesces signals).
	delivery := r.Header.Get("X-Forgejo-Delivery")
	if delivery != "" {
		seen, err := in.Store.DeliverySeen(ctx, delivery)
		if err != nil {
			log.Printf("webhook intake: delivery lookup: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if seen {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	var procErr error
	switch eventType {
	case "issues", "issue_assign", "issue_label", "issue_milestone":
		procErr = in.handleIssue(ctx, body, eventType)
	case "issue_comment", "pull_request_comment":
		procErr = in.handleIssueComment(ctx, body, eventType, delivery)
	case "pull_request_review_approved", "pull_request_review_rejected", "pull_request_review_comment":
		procErr = in.handleReview(ctx, body, eventType, delivery)
	case "pull_request":
		procErr = in.handlePullRequest(ctx, body)
	case "push":
		procErr = in.handlePush(ctx, body)
	default:
		// an event type this factory does not act on yet: accepted so
		// Forgejo's delivery list reads all 2xx, nothing to process
	}
	if procErr != nil {
		log.Printf("webhook intake: %s: %v", strconv.Quote(eventType), procErr)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if delivery != "" {
		if err := in.Store.RecordDelivery(ctx, delivery); err != nil {
			log.Printf("webhook intake: record delivery: %v", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// validWebhookSignature verifies X-Forgejo-Signature: hex(HMAC-SHA256(body,
// secret)), no "sha256=" prefix. Checked before any parsing of body.
func validWebhookSignature(secret string, body []byte, signature string) bool {
	if secret == "" || signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(signature), []byte(want)) == 1
}

type webhookUser struct {
	UserName string `json:"login"`
}

type webhookLabel struct {
	Name string `json:"name"`
}

type webhookRepository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

type webhookIssue struct {
	Index     int64          `json:"number"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Poster    webhookUser    `json:"user"`
	Labels    []webhookLabel `json:"labels"`
	CreatedAt time.Time      `json:"created_at"`
}

type issuesPayload struct {
	Action     string            `json:"action"`
	Issue      webhookIssue      `json:"issue"`
	Repository webhookRepository `json:"repository"`
	Sender     webhookUser       `json:"sender"`
}

type webhookComment struct {
	ID     int64       `json:"id"`
	Poster webhookUser `json:"user"`
	Body   string      `json:"body"`
}

type issueCommentPayload struct {
	Action     string            `json:"action"`
	Issue      webhookIssue      `json:"issue"`
	Comment    webhookComment    `json:"comment"`
	Repository webhookRepository `json:"repository"`
	IsPull     bool              `json:"is_pull"`
}

type prBranchInfo struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type webhookPullRequest struct {
	Index  int64        `json:"number"`
	Head   prBranchInfo `json:"head"`
	Merged bool         `json:"merged"`
}

type reviewPayload struct {
	Content string `json:"content"`
}

type pullRequestPayload struct {
	Action      string             `json:"action"`
	PullRequest webhookPullRequest `json:"pull_request"`
	Repository  webhookRepository  `json:"repository"`
	Sender      webhookUser        `json:"sender"`
	Review      *reviewPayload     `json:"review"`
}

func labelNames(labels []webhookLabel) []string {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return names
}

// priorityLabelWeight maps a label to its priority-int contribution. A
// task's priority is the highest weight among the labels actually present
// (the brief: "priority from priority/p0..p3 and severity/critical|high|
// medium|low labels, higher of the two mappings"); absent either kind of
// label, the default is 0.
var priorityLabelWeight = map[string]int{
	"priority/p0": 3, "priority/p1": 2, "priority/p2": 1, "priority/p3": 0,
	"severity/critical": 3, "severity/high": 2, "severity/medium": 1, "severity/low": 0,
}

// LabelNoise on a human-lane alert issue closes it as not a problem;
// LabelReal on an agent-lane one closed without a merge (a duplicate of a
// real alert) judges its alert real.
const (
	LabelNoise = "noise"
	LabelReal  = "real"
)

// BootLabels are the labels factoryd creates on every project's repository
// at each reconcile (boot included), before anything files
// an issue or mirrors state onto one (Forgejo drops an unknown label name
// silently, an issue template's too): the lanes, the task states, the
// label of a routine's card, and the priority and severity labels intake
// reads (colours are #RRGGBB).
var BootLabels = map[string]string{
	"agent":                "#ededed",
	"human":                "#c2e0c6",
	"bug":                  "#cf222e",
	"improvement":          "#0e8a16",
	"routine":              "#006b75",
	LabelNoise:             "#bfbfbf",
	LabelReal:              "#d93f0b",
	"state/backlog":        "#c5def5",
	"state/queued":         "#ededed",
	"state/waiting-deps":   "#bfd4f2",
	"state/running":        "#1f6feb",
	"state/waiting-human":  "#d4a72c",
	"state/waiting-poster": "#fbca04",
	"state/in-review":      "#8250df",
	"state/done":           "#2da44e",
	"state/failed":         "#cf222e",
	"state/closed":         "#6e7781",
	"priority/p0":          "#b60205",
	"priority/p1":          "#d93f0b",
	"priority/p2":          "#fbca04",
	"priority/p3":          "#c5def5",
	"severity/critical":    "#b60205",
	"severity/high":        "#d93f0b",
	"severity/medium":      "#fbca04",
	"severity/low":         "#c5def5",
}

func priorityFromLabels(names []string) int {
	best := 0
	for _, n := range names {
		if w, ok := priorityLabelWeight[n]; ok && w > best {
			best = w
		}
	}
	return best
}

func objectiveFromIssue(issue webhookIssue) string { return IssueObjective(issue.Title, issue.Body) }

// IssueObjective is the objective of a card whose issue has title and
// body: the title, then the body after a blank line.
func IssueObjective(title, body string) string {
	if body == "" {
		return title
	}
	return title + "\n\n" + body
}

// ReproCommand is the issue body's first fenced code block labelled repro
// (a line "```repro", the command lines, a line "```"): the task's
// reproduction, which GateReproduction runs against the pull request head.
// "" when there is none, or it is empty or never closed.
func ReproCommand(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "```repro" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "```" {
				return strings.TrimSpace(strings.Join(lines[i+1:j], "\n"))
			}
		}
		return ""
	}
	return ""
}

// sandboxCheckoutRule is the sandbox rule every reproduction refusal
// names: the sandbox has one checkout, the command runs in place.
const sandboxCheckoutRule = "the sandbox has one checkout, the command runs in place"

// ValidateReproCommand is the static check on a reproduction command at
// filing: it returns the refusal reason, or "" when the command may run.
// GateReproduction runs the command in place in the checked-out worktree:
// one that chains cd across a mount point, or creates a git worktree under
// /tmp, exits 128 ("not a git repository") in the agent sandbox, so a
// session would only discover what this check names here. Each cd is
// counted, each cd - undoes one, and a command that does not end where it
// started (pending 0) is refused: a cd the command undoes itself
// (cd x && cmd && cd -) passes.
// The reason is the sentence between "command <reason>;"; intakeIssue
// wraps it into the filing refusal.
func ValidateReproCommand(cmd string) string {
	if cmd == "" {
		return ""
	}
	pending := 0
	for _, seg := range splitReproSegments(cmd) {
		var words []string
		for _, f := range strings.Fields(seg) {
			// a leading VAR=value assignment is env, not the command
			if _, val, ok := strings.Cut(f, "="); ok && val != "" && !strings.ContainsAny(f[:strings.Index(f, "=")], "=/") {
				continue
			}
			words = append(words, f)
		}
		if len(words) == 0 {
			continue
		}
		if strings.EqualFold(words[0], "cd") {
			if len(words) > 1 && words[1] == "-" {
				// cd - undoes one cd even when none is open: a
				// leading or doubled one moves the shell, too
				pending--
				continue
			}
			pending++
			continue
		}
		if strings.EqualFold(words[0], "git") {
			for _, w := range words[1:] {
				if strings.EqualFold(w, "worktree") {
					return "creates a git worktree: " + sandboxCheckoutRule
				}
			}
		}
	}
	if pending != 0 {
		return "chains cd out of the checked-out worktree: " + sandboxCheckoutRule
	}
	return ""
}

// reproChainOperators are the shell operators a reproduction command may
// chain segments with: &&, ;, || and newlines.
var reproChainOperators = []string{"&&", "||", ";"}

// reproRefusal is the filing refusal of a reproduction command with
// reason: the reason names the sandbox rule, the card lands in the
// backlog.
func reproRefusal(reason string) string {
	return fmt.Sprintf("factoryd: the reproduction command %s. The card stays in backlog.", reason)
}

// splitReproSegments cuts a reproduction command at its chain operators
// and newlines, leaving the segments between them.
func splitReproSegments(cmd string) []string {
	var segments []string
	for _, line := range strings.Split(cmd, "\n") {
		rest := line
		for {
			cut, op := -1, ""
			for _, o := range reproChainOperators {
				if i := strings.Index(rest, o); i >= 0 && (cut < 0 || i < cut) {
					cut, op = i, o
				}
			}
			if cut < 0 {
				segments = append(segments, rest)
				break
			}
			segments = append(segments, rest[:cut])
			rest = rest[cut+len(op):]
		}
	}
	return segments
}

// line "Size: <size>" naming a card size and its last line "Budget: <n>" or
// "Budget: <n> EUR" with n above 0, keys in any case; "" and 0 for none.
// The last wins: the console appends the form's size line to the member's
// text.
func askedBudget(body string) (size string, eur float64) {
	for l := range strings.Lines(body) {
		key, v, _ := strings.Cut(strings.ToLower(l), ":")
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(key) {
		case "size":
			if slices.Contains(policy.CardSizes, v) {
				size = v
			}
		case "budget":
			if n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, "eur")), 64); err == nil && n > 0 {
				eur = n
			}
		}
	}
	return size, eur
}

// The model rungs a card starts on (roadmap 222, item 5): the ladder's
// start, or its stronger alias.
const (
	RungCheap    = "cheap"
	RungFrontier = "frontier"
)

// askedRung is the issue body's last line "Rung: <rung>, <reason>", key
// in any case; "" without one.
func askedRung(body string) (rung, reason string) {
	for l := range strings.Lines(body) {
		key, v, _ := strings.Cut(l, ":")
		if strings.ToLower(strings.TrimSpace(key)) != "rung" {
			continue
		}
		r, why, _ := strings.Cut(strings.TrimSpace(v), ",")
		if r = strings.ToLower(strings.TrimSpace(r)); r == RungCheap || r == RungFrontier {
			rung, reason = r, strings.TrimSpace(why)
		}
	}
	return rung, reason
}

// rungOf is the rung a card filed with body and size starts on: the
// body's Rung line, else frontier for a large card or one that names
// factoryd's own package, else cheap. A plan card's session runs on the
// stronger rung regardless (plan.go).
func rungOf(body, size string) string {
	if rung, _ := askedRung(body); rung != "" {
		return rung
	}
	if size == "large" || strings.Contains(body, "internal/factoryd") || slices.ContainsFunc(askedDirectories(body), func(d string) bool {
		return strings.HasPrefix(d, "internal/factoryd")
	}) {
		return RungFrontier
	}
	return RungCheap
}

// askedDirectories are the directories a writer's issue body names for its
// card's map: its last line "Directories: <dir>, <dir>", key in any case;
// none without one.
func askedDirectories(body string) []string {
	var dirs []string
	for l := range strings.Lines(body) {
		key, v, _ := strings.Cut(l, ":")
		if !strings.EqualFold(strings.TrimSpace(key), "directories") {
			continue
		}
		dirs = nil
		for d := range strings.SplitSeq(v, ",") {
			if d = strings.TrimSpace(d); d != "" {
				dirs = append(dirs, d)
			}
		}
	}
	return dirs
}

// briefObjective is objective as a model's brief carries it: its body
// without the lines the intake reads into the card's fields (Size, Budget,
// Rung), and when mapped, without the card's directories line, which the
// brief's repository map lists (roadmap 201). Every other line stands as
// its author wrote it.
func briefObjective(objective string, mapped bool) string {
	title, body, ok := strings.Cut(objective, "\n\n")
	if !ok {
		return objective
	}
	var kept strings.Builder
	for l := range strings.Lines(body) {
		size, eur := askedBudget(l)
		rung, _ := askedRung(l)
		key, _, _ := strings.Cut(l, ":")
		dirs := strings.EqualFold(strings.TrimSpace(key), "directories") || strings.TrimSpace(key) == cardDirectories
		if size == "" && eur == 0 && rung == "" && (!mapped || !dirs) {
			kept.WriteString(l)
		}
	}
	return strings.TrimRight(title+"\n\n"+strings.TrimLeft(kept.String(), "\n"), "\n")
}

// pushPayload is a push webhook's body: the ref, the repository and the
// push's commits, each with the paths it added, removed and changed.
type pushPayload struct {
	Ref        string `json:"ref"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Commits []pushCommit `json:"commits"`
}

// pushCommit is one commit of a push payload.
type pushCommit struct {
	ID       string     `json:"id"`
	Message  string     `json:"message"`
	Author   pushAuthor `json:"author"`
	Added    []string   `json:"added"`
	Removed  []string   `json:"removed"`
	Modified []string   `json:"modified"`
}

// pushAuthor is a push commit's author: the Forgejo account and the name
// git recorded.
type pushAuthor struct {
	Name     string `json:"name"`
	UserName string `json:"username"`
}

// who is the author's name: the account's login, else git's own.
func (a pushAuthor) who() string { return cmp.Or(a.UserName, a.Name) }

// handlePush hands a push to a repository's main to MainPushed, and one
// to a document repository's master to the doc_written rows of its
// outside changes.
func (in *Intake) handlePush(ctx context.Context, body []byte) error {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode push: %w", err)
	}
	switch {
	case p.Ref == "refs/heads/main" && in.MainPushed != nil:
		return in.MainPushed(ctx, p.Repository.FullName)
	case p.Ref == "refs/heads/master":
		return in.docsWritten(ctx, p)
	}
	return nil
}

// DocRootOK is the document tools' path allow-list, the door and the
// webhook share it: docs/roadmap.md, a file under docs/plans/ or
// docs/board/, a new docs/<name>.md; no escapes.
func DocRootOK(path string) bool {
	if !strings.HasPrefix(path, "docs/") || !strings.HasSuffix(path, ".md") {
		return false
	}
	if strings.Contains(path, "..") || strings.Contains(path, "//") {
		return false
	}
	rest := strings.TrimPrefix(path, "docs/")
	return rest == "roadmap.md" || strings.HasPrefix(rest, "plans/") || strings.HasPrefix(rest, "board/") ||
		rest != "" && !strings.Contains(rest, "/")
}

// docsWritten raises the outside document changes of a push to master of
// a document repository (the 2026-10-15 plan, item 6): one doc_written
// row per changed path under the document roots, via webhook, for every
// project naming the repository as its chat's document repository, the
// commit that changed the path its actor. A commit carrying the door's
// attribution trailers is the factory's own and raised its event at the
// door, so none of its paths count.
func (in *Intake) docsWritten(ctx context.Context, p pushPayload) error {
	if in.Policy == nil || in.Timeline == nil {
		return nil
	}
	changed, order := map[string]pushCommit{}, []string(nil)
	for _, c := range p.Commits {
		if strings.Contains(c.Message, "Factory-Role:") {
			continue
		}
		for _, path := range slices.Concat(c.Added, c.Removed, c.Modified) {
			if _, seen := changed[path]; !seen {
				order = append(order, path)
			}
			changed[path] = c
		}
	}
	if len(changed) == 0 {
		return nil
	}
	tree := in.Policy()
	var projects []policy.ProjectConfig
	for _, pc := range tree.Projects {
		if tree.Document.ChatOf(pc).DocRepo == p.Repository.FullName {
			projects = append(projects, pc)
		}
	}
	for _, pc := range projects {
		for _, path := range order {
			if !DocRootOK(path) {
				continue
			}
			c := changed[path]
			in.Timeline.RecordProjectRow(ctx, Event{Repository: pc.Repository, Kind: EventDocWritten,
				Actor: c.Author.who(), Via: ViaWebhook,
				Detail: map[string]any{"path": path, "commit": c.ID}})
		}
	}
	return nil
}

// handleIssue hands an issue event to intakeIssue; a close goes to the
// task the issue is linked to.
func (in *Intake) handleIssue(ctx context.Context, body []byte, eventType string) error {
	var p issuesPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode %s: %w", eventType, err)
	}
	if p.Action == "closed" {
		return in.handleIssueClosed(ctx, p)
	}
	// only a label event's sender moves a backlog card
	sender := ""
	if eventType == "issue_label" {
		sender = p.Sender.UserName
	}
	return in.intakeIssue(ctx, "webhook intake", p.Repository.FullName, p.Repository.DefaultBranch, p.Issue, sender)
}

// ResyncIssues takes in the open issues of every active project that no
// task is linked to yet: Forgejo never retries a failed delivery, so an issue
// filed or labelled while factoryd was down is otherwise never seen. Each
// goes through the webhook's own intake: a linked task takes the labels,
// and an unlinked issue is judged by its poster and labels (the heal pass
// knows no sender).
func (in *Intake) ResyncIssues(ctx context.Context) error {
	if in.Projects == nil {
		return nil
	}
	ps, err := in.Projects.Projects(ctx)
	if err != nil {
		return fmt.Errorf("projects: %w", err)
	}
	var errs []error
	for _, p := range ps {
		if !p.Active() {
			continue
		}
		repo := p.Repo()
		issues, err := in.Forgejo.OpenIssues(ctx, repo.Owner, repo.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("issues of %s: %w", repo, err))
			continue
		}
		for _, issue := range issues {
			if err := in.intakeIssue(ctx, "heal pass", repo.String(), "", issue, ""); err != nil {
				errs = append(errs, fmt.Errorf("issue #%d of %s: %w", issue.Index, repo, err))
			}
		}
	}
	return errors.Join(errs...)
}

// outsiderLabels are the labels intake reads off an outside account's new
// issue: Forgejo 15 applies whatever labels its new-issue form posts, from
// anyone, so the writers' lane and priority labels (agent, plan, human,
// priority/*, severity/*) count only from a writer; bug and improvement
// are the templates' own.
func outsiderLabels(names []string) []string {
	var out []string
	for _, n := range names {
		if _, weighted := priorityLabelWeight[n]; n != LabelAgent && n != LabelPlan && n != LaneHuman && !weighted {
			out = append(out, n)
		}
	}
	return out
}

// intakeIssue turns an issue of repo into a card of the lane its labels
// ask for (every issue of a project on agents none is the human lane's),
// idempotent per (repository, issue number): a redelivery, a later label
// event or a heal pass finds the task already linked, and only a label
// event's sender, or for a pass without one the user the issue's timeline
// names as the lane label's adder, moves a backlog card into a starting
// lane. An issue the bot filed with no task linked is ignored.
// defaultBranch is the repository's when the caller knows it (a webhook
// payload), else read. via names the path in the log.
func (in *Intake) intakeIssue(ctx context.Context, via, repo, defaultBranch string, issue webhookIssue, sender string) error {
	p, ok, err := in.Store.ProjectByRepository(ctx, repo)
	if err != nil {
		return fmt.Errorf("project of %s: %w", repo, err)
	}
	if !ok || !p.Active() {
		if issue.Poster.UserName != in.BotUser {
			log.Printf("%s: issue #%d on %s: no active project has this repository: ignored", via, issue.Index, repo)
		}
		return nil
	}
	names := labelNames(issue.Labels)
	if t, ok, err := in.Store.TaskByIssue(ctx, repo, issue.Index); err != nil {
		return fmt.Errorf("task by issue: %w", err)
	} else if ok {
		return in.labelled(ctx, via, p, t, names, defaultBranch, sender)
	}
	if issue.Poster.UserName == in.BotUser {
		return nil // the bot's own issues never spawn a second task
	}

	poster := issue.Poster.UserName
	writer, err := in.trusted(ctx, repo, poster)
	if err != nil {
		return err
	}
	// a writer's label event vouches for the labels the issue carries
	vouched := false
	if sender != "" && sender != poster && !writer {
		if vouched, err = in.trusted(ctx, repo, sender); err != nil {
			return err
		}
	}
	if kept := outsiderLabels(names); !writer && !vouched && len(kept) != len(names) && p.Agents == policy.AgentsCoder {
		// off the issue too, so no later writer's event reads them
		names = kept
		if parts, err := SplitRepo(repo); err != nil {
			return err
		} else if err := in.Forgejo.ReplaceIssueLabels(ctx, parts.Owner, parts.Name, issue.Index, kept); err != nil {
			log.Printf("%s: issue #%d on %s: drop an outside account's writer labels: %v", via, issue.Index, repo, err)
		}
	}
	size, eur := askedBudget(issue.Body)
	task := Task{
		ID:           NewTaskID(),
		Priority:     priorityFromLabels(names),
		RiskClass:    "standard",
		BudgetEUR:    policy.CardBudget(in.Policy().Document.Budget, p.Budget, size, eur),
		Size:         size,
		Rung:         rungOf(issue.Body, size),
		Objective:    objectiveFromIssue(issue),
		Repository:   repo,
		Kind:         "agent",
		IssueNumber:  issue.Index,
		Source:       "webhook",
		CreatedAt:    issue.CreatedAt,
		Author:       poster,
		AuthorWriter: writer,
		ReproCommand: ReproCommand(issue.Body),
	}
	if p.Agents == policy.AgentsNone || IssueLane(names, p.Lanes) == LaneHuman {
		// no runner ever claims the human lane: it waits on a human from
		// the start (architecture 5.6's board column) and closing its
		// issue finishes it
		task.Kind, task.Status, task.ReproCommand = "human", "waiting-human", ""
	}
	var refused string
	reproRefused := false
	if task.Kind == "agent" {
		if task, refused, err = in.enterOnCreation(ctx, p, task, names, defaultBranch, sender); err != nil {
			return err
		}
		if reason := ValidateReproCommand(task.ReproCommand); reason != "" {
			// the sandbox rule is known at filing: the card stays in the
			// backlog and no session discovers it under GateReproduction
			task.ReproCommand = ""
			refused = strings.TrimSpace(refused + "\n" + reproRefusal(reason))
			reproRefused = true
			if task.Lane != policy.LaneBacklog {
				task.Lane, task.Status, task.RequiredCapabilities, task.SHA, task.Gate, task.GateSince = policy.LaneBacklog, "backlog", nil, "", "", nil
			}
		}
	}
	if err := in.Store.CreateTask(ctx, task); err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	if refused != "" {
		in.refuseEntry(ctx, via, repo, issue.Index, names, refused)
	} else if task.Lane != "" && task.Lane != policy.LaneBacklog {
		in.keepLaneLabel(ctx, via, repo, issue.Index, names, task.Lane)
	}
	log.Printf("%s: issue #%d on %s by %s: %s task %s", via, issue.Index, repo, poster, cmp.Or(task.Lane, LaneHuman), task.ID)
	in.Timeline.recordEvent(ctx, task.ID, EventCardCreated, map[string]any{"by": poster, "issue": issue.Index})
	mirrorTaskState(ctx, in.Forgejo, task, task.Status)
	// a card refused only its reproduction command keeps its write-up: the
	// entry was allowed, refuseEntry's labels alone come off again
	if err := in.enterLane(ctx, p, task, poster, defaultBranch); err != nil {
		return err
	}
	if reproRefused {
		if parts, err := SplitRepo(repo); err != nil {
			log.Printf("%s: issue #%d on %s: restore the lane label: %v", via, issue.Index, repo, err)
		} else if err := in.Forgejo.ReplaceIssueLabels(ctx, parts.Owner, parts.Name, issue.Index, names); err != nil {
			log.Printf("%s: issue #%d on %s: restore the lane label: %v", via, issue.Index, repo, err)
		}
	}
	return nil
}

// enterLane is card t's entry into its lane of project p by user by: the
// event, then the lane's task or the backlog's write-up.
func (in *Intake) enterLane(ctx context.Context, p Project, t Task, by, defaultBranch string) error {
	return enterLane(ctx, in.Timeline, in.Workflows, t, by, func() error { return in.writeUp(ctx, p, t, defaultBranch) })
}

// labelled acts on a label event, or a heal pass's read, of the issue of
// card t: a backlog card whose labels now ask for a starting lane enters
// it through the label added or removed, a queued card's workflow start
// that failed is retried, and a backlog write-up a crash kept from opening
// opens. Every other card's labels change nothing.
func (in *Intake) labelled(ctx context.Context, via string, p Project, t Task, names []string, defaultBranch, sender string) error {
	switch {
	case t.Kind == "agent" && t.Status == "queued":
		// a redelivery retries a workflow start that failed the first time
		if moved, err := in.humanLaneWithoutAgents(ctx, t); err != nil || moved {
			return err
		}
		return in.Workflows.Start(ctx, t.ID)
	case t.Status != "backlog" || p.Agents != policy.AgentsCoder:
		return nil
	}
	switch name := IssueLane(names, p.Lanes); {
	case name == policy.LaneBacklog:
		return in.writeUp(ctx, p, t, defaultBranch)
	case name == LaneHuman:
		return nil
	case laneLabelled(names, name):
		if sender == "" {
			var err error
			if sender, err = in.laneLabelAdder(ctx, t, name, names); err != nil {
				return err
			}
		}
		return in.enterOnLabelAdded(ctx, via, p, t, name, names, defaultBranch, sender)
	default:
		return in.enterOnLabelRemoved(ctx, via, p, t, names, defaultBranch, sender)
	}
}

// laneLabelAdder is who last added a label of names that asks for lane
// to card t's issue, the bot aside: the sender a lost label webhook would
// have carried, read off the timeline by a pass that knows none. A removal
// is never inferred: an entry names who added a label, not who took one.
func (in *Intake) laneLabelAdder(ctx context.Context, t Task, lane string, names []string) (string, error) {
	asking := []string{LabelPlan}
	if lane == policy.LaneReady {
		asking = []string{LabelAgent, "bug"}
	}
	asking = slices.DeleteFunc(asking, func(n string) bool { return !slices.Contains(names, n) })
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return "", err
	}
	by, err := in.Forgejo.LabelAdder(ctx, repo.Owner, repo.Name, t.IssueNumber, asking, in.BotUser)
	if err != nil {
		return "", fmt.Errorf("who added %v to issue #%d: %w", asking, t.IssueNumber, err)
	}
	return by, nil
}

// writeUp opens backlog card t's write-up at the branch head (ruling 185)
// when project p's backlog lane names a write_up class: its report lands
// on the issue while the card stays in the backlog.
func (in *Intake) writeUp(ctx context.Context, p Project, t Task, defaultBranch string) error {
	backlog, _ := laneOf(p.Lanes, policy.LaneBacklog)
	if backlog.WriteUp == "" {
		return nil
	}
	if _, ok, err := in.Store.ReportOf(ctx, t.ID, SourcePresort); err != nil || ok {
		return err
	}
	sha, err := in.baseSHA(ctx, t.Repository, defaultBranch)
	if err != nil {
		return err
	}
	_, err = openReport(ctx, in.Store, in.Workflows, in.Policy().Document.Budget, t, SourcePresort, backlog.WriteUp, sha)
	return err
}

// humanLaneWithoutAgents moves queued agent task t to the human lane when
// its project no longer lets agents work (agents none): nothing restarts
// it. moved reports whether it did.
func (in *Intake) humanLaneWithoutAgents(ctx context.Context, t Task) (moved bool, err error) {
	p, ok, err := in.Store.ProjectByRepository(ctx, t.Repository)
	if err != nil || !ok || p.Agents != policy.AgentsNone {
		return false, err
	}
	if moved, err = in.Store.ToHumanLane(ctx, t.ID); err != nil || !moved {
		return moved, err
	}
	log.Printf("intake: task %s: project %s is on agents none: queued task moved to the human lane", t.ID, p.Name)
	t.Kind = "human"
	mirrorTaskState(ctx, in.Forgejo, t, "waiting-human")
	return true, nil
}

func (in *Intake) baseSHA(ctx context.Context, repo, defaultBranch string) (string, error) {
	parts, err := SplitRepo(repo)
	if err != nil {
		return "", err
	}
	sha, err := in.Forgejo.DefaultBranchHeadSHA(ctx, parts.Owner, parts.Name, defaultBranch)
	if err != nil {
		return "", fmt.Errorf("base sha: %w", err)
	}
	return sha, nil
}

// The sources of a pull request factoryd opened outside any card's
// workflow (roadmap 187): each becomes a card of its own through
// FileFactoryPRTask, so the chat, the board and the merge rule cover it.
const (
	// SourceGitHubSync is a Sync from GitHub pull request (Registrar.GitHubSync).
	SourceGitHubSync = "github-sync"
	// SourceSchemaCopy is a schema copy pull request (SchemaCopies).
	SourceSchemaCopy = "schema-copy"
	// SourceTemplateUpdate is a template update pull request (TemplateUpdates).
	SourceTemplateUpdate = "template-update"
)

// factoryPRSources are the sources of a pull request factoryd opened
// itself: like a roll they have no lane and run ready's process and class.
func factoryPRSources(source string) bool {
	return source == SourceGitHubSync || source == SourceSchemaCopy || source == SourceTemplateUpdate || source == SourceRevert
}

// FileTask files task t the way the API, the alert intake, the roller
// and the configuration loader do (task 42): the issue on repo first, as
// the bot, labelled labels and its state, then the task row linked to it.
// A task of kind human waits on a human from the start and has no
// workflow; an agent task's goes through FileAgentTask. No row exists
// without its issue.
func FileTask(ctx context.Context, store interface {
	CreateTask(ctx context.Context, t Task) error
}, forgejo *Forgejo, repo Repo, title, body string, labels []string, t Task) (Task, IssueRef, error) {
	if t.Kind == "human" {
		t.Status = "waiting-human"
	}
	ref, err := forgejo.CreateIssue(ctx, repo.Owner, repo.Name, title, body)
	if err != nil {
		return Task{}, IssueRef{}, fmt.Errorf("create issue: %w", err)
	}
	if err := forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, ref.Number, append(slices.Clone(labels), "state/"+t.Status)); err != nil {
		log.Printf("task %s: label issue %d: %v", t.ID, ref.Number, err)
	}
	t.Repository, t.IssueNumber = repo.String(), ref.Number
	if err := store.CreateTask(ctx, t); err != nil {
		return Task{}, IssueRef{}, err
	}
	return t, ref, nil
}

// FileAgentTask files agent task t on project p's repository through
// FileTask, entering its process by fileEntry, and starts it where it
// entered a starting lane (a failed start is logged and healed by the next
// HealPass). tl records the card's events.
func FileAgentTask(ctx context.Context, store interface {
	CreateTask(ctx context.Context, t Task) error
}, forgejo *Forgejo, workflows Workflows, tl *Timeline, p Project, title, body string, labels []string, t Task) (Task, IssueRef, error) {
	t, labels, err := fileEntry(p, t, labels)
	if err != nil {
		return Task{}, IssueRef{}, err
	}
	t, ref, err := FileTask(ctx, store, forgejo, p.Repo(), title, body, labels, t)
	if err != nil {
		return Task{}, IssueRef{}, err
	}
	by := cmp.Or(t.Author, forgejo.BotUser)
	if t.Lane != "" {
		tl.recordEvent(ctx, t.ID, EventCardCreated, map[string]any{"by": by, "issue": ref.Number})
	}
	if err := enterLane(ctx, tl, workflows, t, by, nil); err != nil {
		log.Printf("task %s: start workflow: %v", t.ID, err)
	}
	return t, ref, nil
}

// FileFactoryPRTask files a card for pull request prNumber, which factoryd
// opened outside any card's workflow (a GitHub sync, a schema copy, a
// template update, a revert) through FileAgentTask, its row inserted with
// the pull request linked: no card of these sources exists without it.
// Its run finds the pull request recorded and verifies it instead of
// opening one. t.Source names the kind (factoryPRSources); fileEntry gives
// such a card ready's class and no lane, like a roll, so the workflow
// starts at once and moves running before its verify reaches in-review.
// It runs no session, and its budget is its model gates': budget's task
// default, at most p's card maximum and the task maximum (CardBudget).
func FileFactoryPRTask(ctx context.Context, store interface {
	CreateTask(ctx context.Context, t Task) error
}, forgejo *Forgejo, workflows Workflows, tl *Timeline, budget policy.Budget, p Project, title, body string, labels []string, t Task, prNumber int64) (Task, IssueRef, error) {
	if prNumber == 0 {
		return Task{}, IssueRef{}, fmt.Errorf("a %s card needs its pull request", t.Source)
	}
	t.PRNumber, t.BudgetEUR = prNumber, policy.CardBudget(budget, p.Budget, "", 0)
	return FileAgentTask(ctx, store, forgejo, workflows, tl, p, title, body, labels, t)
}

func (in *Intake) handleIssueClosed(ctx context.Context, p issuesPayload) error {
	t, ok, err := in.Store.TaskByIssue(ctx, p.Repository.FullName, p.Issue.Index)
	if err != nil {
		return fmt.Errorf("task by issue: %w", err)
	}
	if !ok {
		return nil
	}
	return in.issueClosed(ctx, t, p.Sender.UserName)
}

// issueClosed acts on task t's issue closed by closer, from the webhook or
// the heal pass alike: the task's workflow is signalled (it asks Forgejo
// whether a merge closed it), a human-lane task is simply done, a failed
// task's branch is deleted. An issue's author can close it without any
// repository access, so only a user trusted to steer counts, and the bot
// account only where factoryd recorded the close as its own
// (IntakeFactoryClosed): a leaked bot login ends no task.
func (in *Intake) issueClosed(ctx context.Context, t Task, closer string) error {
	if closer == in.BotUser {
		events, err := in.Store.IntakeEvents(ctx, t.ID)
		if err != nil {
			return err
		}
		if !botCloseRecorded(events, t) {
			log.Printf("intake: issue #%d on %s closed by %s, which factoryd did not record as its own: task %s left as it is", t.IssueNumber, t.Repository, closer, t.ID)
			return nil
		}
	} else {
		if ok, err := in.trusted(ctx, t.Repository, closer); err != nil || !ok {
			return err
		}
	}
	switch {
	case t.Kind == "human":
		return in.finishHumanTask(ctx, t, closer)
	case t.Status == "backlog":
		return in.closeBacklogCard(ctx, t)
	case t.Status == "failed":
		return in.deleteFailedBranch(ctx, t)
	}
	_, err := in.decide(ctx, t.ID, "issue_closed", closer, t.IssueNumber)
	return err
}

// deleteFailedBranch: Finish keeps a failed task's branch on the fork for a
// retry; its issue's close gives it up. The workflow is over, so nothing is
// signalled.
func (in *Intake) deleteFailedBranch(ctx context.Context, t Task) error {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	fork := in.Forgejo.Fork(repo)
	return in.Forgejo.DeleteBranch(ctx, fork.Owner, fork.Name, taskBranch(t))
}

// CloseCard is the console's Close of card id, which has no issue, for
// by: a backlog or failed card ends closed in the store; any other through
// its workflow, signalled as by its issue's close.
func (in *Intake) CloseCard(ctx context.Context, id, by string) error {
	t, _, err := in.Store.Task(ctx, id)
	switch {
	case err != nil:
		return err
	case t.Status == "backlog":
		return in.closeBacklogCard(ctx, t)
	case t.Status == "failed":
		return in.Store.TransitionTask(ctx, id, []string{"failed"}, "closed")
	}
	return in.Workflows.Signal(ctx, id, SignalIssueClosed, TaskSignal{Author: by})
}

// closeBacklogCard: a backlog card whose issue closed ends closed, no
// workflow ever started.
func (in *Intake) closeBacklogCard(ctx context.Context, t Task) error {
	if err := in.Store.TransitionTask(ctx, t.ID, []string{"backlog"}, "closed"); err != nil {
		return err
	}
	countOutcome(ctx, in.Metrics, in.Store, t, "closed")
	mirrorTaskState(ctx, in.Forgejo, t, "closed")
	taskEnded(ctx, in.Timeline, in.Store, in.Forgejo, in.Workflows, t, "closed", "")
	return nil
}

// decide records a decision — issue number closed, pull request number
// merged or closed — once and signals it once: between the webhook and
// the heal passes that read the same state off Forgejo, and not again on
// every pass while the workflow winds the task down.
func (in *Intake) decide(ctx context.Context, taskID, kind, author string, number int64) (fresh bool, err error) {
	e, fresh, err := in.record(ctx, IntakeEvent{TaskID: taskID, Kind: kind, Author: author, CommentID: number, Trusted: true})
	if err != nil {
		return false, err
	}
	return fresh, in.signalEvent(ctx, taskID, e, number)
}

// finishHumanTask: a human-lane task has no workflow; its closed issue is
// its end. A SigNoz alert's gets its verdict from the close (ruling 180):
// noise when the issue carries LabelNoise, else real.
func (in *Intake) finishHumanTask(ctx context.Context, t Task, closer string) error {
	if t.Status != "waiting-human" {
		return nil
	}
	if t.Source == SourceAlert {
		repo, err := SplitRepo(t.Repository)
		if err != nil {
			return err
		}
		labels, err := in.Forgejo.IssueLabels(ctx, repo.Owner, repo.Name, t.IssueNumber)
		if err != nil {
			return fmt.Errorf("labels of the alert's issue: %w", err)
		}
		verdict := VerdictReal
		if slices.Contains(labels, LabelNoise) {
			verdict = VerdictNoise
		}
		if err := in.Store.JudgeTaskAlerts(ctx, t.ID, AlertSigNoz, verdict, closer); err != nil {
			return err
		}
	}
	if err := in.Store.TransitionTask(ctx, t.ID, []string{"waiting-human"}, "done"); err != nil {
		return err
	}
	countOutcome(ctx, in.Metrics, in.Store, t, "done")
	mirrorTaskState(ctx, in.Forgejo, t, "done")
	taskEnded(ctx, in.Timeline, in.Store, in.Forgejo, in.Workflows, t, "done", "")
	return nil
}

// handlePullRequest signals a closed factory pull request to its task's
// workflow: merged, or closed without a merge (the reviewer rejected it).
func (in *Intake) handlePullRequest(ctx context.Context, body []byte) error {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode pull_request: %w", err)
	}
	// the bot's own close (Finish, a .forgejo refusal) lands on a workflow
	// that already knows; Reconcile reads the state for a resumed one
	if p.Action != "closed" || p.Sender.UserName == in.BotUser {
		return nil
	}
	taskID, ok, err := in.taskOfHead(ctx, p.Repository.FullName, p.PullRequest.Head.Repo.FullName, p.PullRequest.Head.Ref, p.PullRequest.Index)
	if err != nil || !ok {
		return err
	}
	t, ok, err := in.agentTask(ctx, taskID)
	if err != nil || !ok {
		return err
	}
	kind := "pull_request_closed"
	if p.PullRequest.Merged {
		kind = "pull_request_merged"
	}
	fresh, err := in.decide(ctx, taskID, kind, p.Sender.UserName, p.PullRequest.Index)
	switch {
	case fresh && p.PullRequest.Merged:
		in.humanMerged(ctx, taskID, p.Sender.UserName, p.PullRequest.Index)
	case fresh:
		in.intervened(ctx, t, InterventionReject)
	}
	return err
}

// humanMerged records the event pull_request_merged of task taskID's pull
// request pr merged in Forgejo by by; factory-merge's merges are Merge's.
func (in *Intake) humanMerged(ctx context.Context, taskID, by string, pr int64) {
	if by != MergeUser {
		in.Timeline.recordEvent(ctx, taskID, EventPullRequestMerged, map[string]any{"pr": pr, "by": by})
	}
}

// agentTask is task id when it exists and runs a workflow to signal: a
// human-lane task has none.
func (in *Intake) agentTask(ctx context.Context, id string) (Task, bool, error) {
	t, ok, err := in.Store.Task(ctx, id)
	// a backlog card runs nothing until it enters a starting lane
	return t, ok && t.Kind == "agent" && t.Status != "backlog", err
}

// taskOfHead is the task whose pull request has head ref in headRepo: a
// factory/<id> branch on the bot's fork of repo, and an agent card is
// resolved by that head alone (ruling 97: the same branch name on repo
// itself or on anyone else's fork is not the task's). A pull request
// factoryd opened outside any card's workflow has another head (a
// template update, a GitHub sync) or a factory/schema-* one that is no
// task id, so when the head names no task, pr resolves the factory pull
// request card that records that number, if it is still open.
func (in *Intake) taskOfHead(ctx context.Context, repo, headRepo, ref string, pr int64) (string, bool, error) {
	if base, err := SplitRepo(repo); err == nil && headRepo == in.Forgejo.Fork(base).String() {
		if id, ok := strings.CutPrefix(ref, "factory/"); ok {
			if _, have, err := in.Store.Task(ctx, id); err != nil {
				return "", false, fmt.Errorf("task of head %s: %w", ref, err)
			} else if have {
				return id, true, nil
			}
		}
	}
	cards, err := in.Store.TasksByPR(ctx, repo, pr)
	if err != nil {
		return "", false, fmt.Errorf("the cards of pull request %d: %w", pr, err)
	}
	for _, t := range cards {
		if factoryPRSources(t.Source) && !Final(t.Status) {
			return t.ID, true, nil
		}
	}
	return "", false, nil
}

// handleIssueComment records a new comment as an intake event against the
// task it belongs to: directly by issue number for an issue comment, or by
// resolving the pull request for a pull request comment (the factory/<task
// id> head of an agent card's, or the factory pull request card its number
// names). A comment on anything else the factory did not file (no matching
// task) is silently dropped, and so are edits and deletions: the workflow
// saw the comment when it was created.
func (in *Intake) handleIssueComment(ctx context.Context, body []byte, eventType, delivery string) error {
	var p issueCommentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode %s: %w", eventType, err)
	}
	if p.Action != "created" || p.Comment.Poster.UserName == in.BotUser {
		return nil
	}
	repo := p.Repository.FullName

	var taskID string
	if p.IsPull {
		repoParts, err := SplitRepo(repo)
		if err != nil {
			return err
		}
		pr, err := in.Forgejo.PullRequestState(ctx, repoParts.Owner, repoParts.Name, p.Issue.Index)
		if err != nil {
			return fmt.Errorf("pull request head: %w", err)
		}
		id, ok, err := in.taskOfHead(ctx, repo, pr.HeadRepo, pr.Head, p.Issue.Index)
		if err != nil || !ok {
			return err
		}
		taskID = id
	} else {
		t, ok, err := in.Store.TaskByIssue(ctx, repo, p.Issue.Index)
		if err != nil {
			return fmt.Errorf("task by issue: %w", err)
		}
		if !ok {
			return nil
		}
		taskID = t.ID
	}
	t, ok, err := in.agentTask(ctx, taskID)
	if err != nil || !ok {
		return err
	}
	return in.recordComment(ctx, t, p.IsPull, p.Issue.Index, IssueComment{ID: p.Comment.ID, Author: p.Comment.Poster.UserName, Body: p.Comment.Body}, delivery)
}

// recordComment records comment c on task t's issue (onPR false) or pull
// request number and signals it to the workflow when its author is
// trusted: an issue comment may answer an escalation, a pull request
// comment asks for changes.
func (in *Intake) recordComment(ctx context.Context, t Task, onPR bool, number int64, c IssueComment, delivery string) error {
	kind := "issue_comment"
	if onPR {
		kind = "pull_request_comment"
	}
	trusted, err := in.trusted(ctx, t.Repository, c.Author)
	if err != nil {
		return err
	}
	if !onPR && !trusted && t.Author != "" && c.Author == t.Author {
		kind = IntakePosterComment // information for a request, whatever the author's access; never authority
	}
	answer := false
	if !onPR {
		if answer, err = in.planAnswer(ctx, t, c); err != nil {
			return err
		}
	}
	e, fresh, err := in.record(ctx, IntakeEvent{
		TaskID: t.ID, Kind: kind, Author: c.Author, Body: c.Body, DeliveryID: delivery, CommentID: c.ID, Trusted: trusted, Signalled: answer,
	})
	if err != nil {
		return err
	}
	switch {
	case fresh && answer:
		in.intervened(ctx, t, InterventionEscalationAnswer)
	case fresh && !onPR:
		in.countAnswer(ctx, t, c.Author, trusted, c.ID)
	}
	return in.signalEvent(ctx, t.ID, e, number)
}

// planAnswer is whether c is the console's comment of its author's answer
// to a question of any revision of plan task t, newest first
// (PlanAnswerComment): a healed comment may come after the next revision.
// The console stored the answer before it commented and signalled the
// workflow itself: the comment is recorded as signalled, no change
// request.
func (in *Intake) planAnswer(ctx context.Context, t Task, c IssueComment) (bool, error) {
	var idx int
	if _, err := fmt.Sscanf(c.Body, "Answer %d:", &idx); err != nil || t.Lane != policy.LanePlan || idx < 1 {
		return false, nil
	}
	rev, ok, err := in.Store.PlanRevision(ctx, t.ID, 0)
	if err != nil || !ok {
		return false, err
	}
	for n := rev.Revision; n >= 1; n-- {
		qs, err := in.Store.PlanQuestions(ctx, t.ID, n)
		if err != nil {
			return false, err
		}
		if idx <= len(qs) && qs[idx-1].Answer != "" && qs[idx-1].AnsweredBy == c.Author && c.Body == PlanAnswerComment(idx, qs[idx-1].Answer) {
			return true, nil
		}
	}
	return false, nil
}

// countAnswer counts a new issue comment that answers a task waiting on a
// human: the poster's on a task waiting for the poster, a writer's on a
// task waiting for an answer or for the poster. Each of those records
// question_answered via webhook, so one question leaves one event whatever
// the input (ruling 252). The console's Answer button posts its answer as
// an issue comment itself and files its own row naming that comment; the
// partial unique index on (task_id, detail->>'comment') keeps one row per
// answer whatever the timing.
func (in *Intake) countAnswer(ctx context.Context, t Task, author string, trusted bool, commentID int64) {
	switch {
	case t.Status == "waiting-poster" && author == t.Author:
		in.intervened(ctx, t, InterventionPosterAnswer)
	case trusted && (t.Status == "waiting-human" || t.Status == "waiting-poster"):
		in.intervened(ctx, t, InterventionEscalationAnswer)
	default:
		return
	}
	in.recordAnswer(ctx, t, author, commentID)
}

// recordAnswer writes the question_answered row of the comment (0 = none of
// its own) that answered t's question, by its author, through the webhook.
func (in *Intake) recordAnswer(ctx context.Context, t Task, author string, commentID int64) {
	detail := map[string]any{}
	if commentID != 0 {
		detail["comment"] = commentID
	}
	in.Timeline.RecordRow(ctx, t.ID, EventQuestionAnswered, author, ViaWebhook, detail)
}

// intervened counts a human intervention of kind in task t.
func (in *Intake) intervened(ctx context.Context, t Task, kind string) {
	process, lane := processLane(ctx, in.Store, t)
	in.Metrics.Intervened(ctx, kind, process, lane)
}

// record stores e once and returns the stored row, and whether it is new: the one just inserted,
// or the one an earlier delivery of the same webhook or a heal-pass
// re-read of the same comment left, so that a signal that failed then is
// sent now.
func (in *Intake) record(ctx context.Context, e IntakeEvent) (IntakeEvent, bool, error) {
	id, err := in.Store.InsertIntakeEvent(ctx, e)
	if err != nil {
		return IntakeEvent{}, false, err
	}
	if id != 0 {
		e.ID = id
		return e, true, nil
	}
	events, err := in.Store.IntakeEvents(ctx, e.TaskID)
	if err != nil {
		return IntakeEvent{}, false, err
	}
	i := slices.IndexFunc(events, func(have IntakeEvent) bool {
		return (e.DeliveryID != "" && have.DeliveryID == e.DeliveryID) ||
			(e.CommentID != 0 && have.Kind == e.Kind && have.CommentID == e.CommentID)
	})
	if i < 0 {
		return IntakeEvent{}, false, fmt.Errorf("intake event for task %s (%s, delivery %q, comment %d) recorded before but not found", e.TaskID, e.Kind, e.DeliveryID, e.CommentID)
	}
	return events[i], false, nil
}

// signalEvent delivers a trusted event not yet signalled to the task's
// workflow, once: a pull request comment or rejecting review on pull
// request pr asks for changes, an issue comment may answer an escalation,
// a decision ends the task, an approval is only recorded. The event is
// claimed before the signal, so the webhook path and a heal pass holding
// the same one send it once between them; a failed signal releases the
// claim.
func (in *Intake) signalEvent(ctx context.Context, taskID string, e IntakeEvent, pr int64) error {
	if !e.Trusted && e.Kind != IntakePosterComment || e.Signalled {
		return nil
	}
	name, sig, _ := signalOf(e, pr)
	if name == "" {
		return nil
	}
	if claimed, err := in.Store.SetSignalled(ctx, e.ID, true); err != nil || !claimed {
		return err
	}
	if err := in.Workflows.Signal(ctx, taskID, name, sig); err != nil {
		if _, release := in.Store.SetSignalled(ctx, e.ID, false); release != nil {
			log.Printf("intake: event %d of task %s: signal failed (%v) and its claim stays: %v; the heal pass re-sends a decision after 15 min, a comment never", e.ID, taskID, err, release)
		}
		return err
	}
	return nil
}

// signalOf is the signal event e sends, "" for one that sends none, and
// whether it is a decision that ends the task. A pull request decision
// carries its own pull request (its CommentID), a change request pr.
func signalOf(e IntakeEvent, pr int64) (name string, sig TaskSignal, decision bool) {
	sig = TaskSignal{Author: e.Author, Body: e.Body}
	switch e.Kind {
	case "issue_comment":
		name, sig.Comment = SignalHumanComment, e.CommentID
	case IntakePosterComment:
		name, sig.Comment = SignalPosterComment, e.CommentID
	case "pull_request_comment", "pull_request_review_rejected":
		name, sig.PR = SignalChangesRequested, pr
	case "issue_closed":
		name, decision = SignalIssueClosed, true
	case "pull_request_merged":
		name, sig.PR, decision = SignalPRMerged, e.CommentID, true
	case "pull_request_closed":
		name, sig.PR, decision = SignalPRClosed, e.CommentID, true
	}
	return name, sig, decision
}

// decisionResignalAfter is how old a signalled decision must be before
// the heal pass sends it again to a task still in review or waiting on a
// human: longer than a workflow takes to wind a decided task down.
const decisionResignalAfter = 15 * time.Minute

// resignalDecision sends decision e again: claimed but never delivered
// (factoryd died between the claim and the signal), it would otherwise
// strand the task. The workflow ignores a decision it already acted on.
func (in *Intake) resignalDecision(ctx context.Context, t Task, e IntakeEvent) error {
	name, sig, decision := signalOf(e, t.PRNumber)
	// a decision on another pull request of the task branch was ignored
	// by the workflow and stays ignored
	if !decision || !e.Trusted || !e.Signalled || time.Since(e.CreatedAt) < decisionResignalAfter ||
		!slices.Contains([]string{"waiting-deps", "in-review", "waiting-human", "waiting-poster"}, t.Status) || (name != SignalIssueClosed && sig.PR != t.PRNumber) {
		return nil
	}
	log.Printf("intake: task %s still %s after decision %d (%s): signalling it again", t.ID, t.Status, e.ID, e.Kind)
	return in.Workflows.Signal(ctx, t.ID, name, sig)
}

// handleReview records a pull request review as an intake event, resolved to
// its task directly from the payload's own head branch (no extra API call:
// unlike a comment, a review payload already carries the pull request).
// reviewKey stands in for the id a review's webhook payload lacks: the
// reviewer, the head and the text, so a redelivery under a new delivery id
// is recorded once (the event kind carries the verdict).
func reviewKey(author, head, body string) int64 {
	h := fnv.New64a()
	for _, part := range []string{author, head, body} {
		h.Write([]byte(part)) // #nosec G104 -- hash.Hash never fails
		h.Write([]byte{0})    // #nosec G104 -- as above
	}
	return int64(h.Sum64()>>1) | 1
}

func (in *Intake) handleReview(ctx context.Context, body []byte, eventType, delivery string) error {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode %s: %w", eventType, err)
	}
	if p.Sender.UserName == in.BotUser {
		return nil
	}
	taskID, ok, err := in.taskOfHead(ctx, p.Repository.FullName, p.PullRequest.Head.Repo.FullName, p.PullRequest.Head.Ref, p.PullRequest.Index)
	if err != nil || !ok {
		return err
	}
	t, ok, err := in.agentTask(ctx, taskID)
	if err != nil || !ok {
		return err
	}
	reviewBody := ""
	if p.Review != nil {
		reviewBody = p.Review.Content
	}
	trusted, err := in.trusted(ctx, p.Repository.FullName, p.Sender.UserName)
	if err != nil {
		return err
	}
	e, fresh, err := in.record(ctx, IntakeEvent{
		TaskID: taskID, Kind: eventType, Author: p.Sender.UserName, Body: reviewBody, DeliveryID: delivery, Trusted: trusted,
		CommentID: reviewKey(p.Sender.UserName, p.PullRequest.Head.SHA, reviewBody),
	})
	if err != nil {
		return err
	}
	if fresh && trusted {
		switch eventType {
		case "pull_request_review_approved":
			in.intervened(ctx, t, InterventionApprove)
		case "pull_request_review_rejected":
			in.intervened(ctx, t, InterventionRequestChanges)
		}
	}
	return in.signalEvent(ctx, taskID, e, p.PullRequest.Index)
}

// healAuthor names the heal pass as the author of a state change it read
// from Forgejo rather than received as a webhook.
const healAuthor = "factoryd heal pass"

// Resync re-reads what Forgejo holds for task t and signals the workflow
// what it may have missed: a webhook is delivered once, so a merge, close
// or comment made while factoryd was down or erroring is otherwise lost.
// Terminal states (pull request merged or closed, issue closed) are
// recorded and signalled once, like comments; a decision that has not
// moved the task out of review or the human wait within
// decisionResignalAfter is signalled again on every pass (the claim may
// have outlived a factoryd that died before the signal). A closed issue
// is credited to its last closer per the timeline and gated like a
// webhook's sender. A trusted event recorded but not signalled (factoryd
// died between the two) is signalled now. Reviews carry no id
// in their webhook payload and are not re-read.
func (in *Intake) Resync(ctx context.Context, t Task) error {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	if t.Kind == "human" || t.Status == "backlog" {
		if t.IssueNumber == 0 {
			return nil
		}
		closer, err := in.Forgejo.IssueClosedBy(ctx, repo.Owner, repo.Name, t.IssueNumber)
		if err != nil || closer == "" {
			return err
		}
		return in.issueClosed(ctx, t, closer)
	}
	events, err := in.Store.IntakeEvents(ctx, t.ID)
	if err != nil {
		return err
	}
	if t.PRNumber != 0 {
		pr, err := in.Forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber)
		if err != nil {
			return fmt.Errorf("pull request state: %w", err)
		}
		switch {
		case pr.Merged:
			var fresh bool
			if fresh, err = in.decide(ctx, t.ID, "pull_request_merged", healAuthor, t.PRNumber); fresh {
				in.humanMerged(ctx, t.ID, pr.MergedBy, t.PRNumber)
			}
		case pr.State == "closed":
			_, err = in.decide(ctx, t.ID, "pull_request_closed", healAuthor, t.PRNumber)
		}
		if err != nil {
			return err
		}
		if err := in.resyncComments(ctx, t, true, t.PRNumber, events); err != nil {
			return err
		}
	}
	// an issue whose closer cannot be read still has its comments re-read
	var closeErr error
	if t.IssueNumber != 0 {
		closer, err := in.Forgejo.IssueClosedBy(ctx, repo.Owner, repo.Name, t.IssueNumber)
		switch {
		case err != nil:
			closeErr = fmt.Errorf("issue state: %w", err)
		case closer != "":
			if err := in.issueClosed(ctx, t, closer); err != nil {
				return err
			}
		}
		if err := in.resyncComments(ctx, t, false, t.IssueNumber, events); err != nil {
			return err
		}
	}
	for _, e := range events {
		if err := in.signalEvent(ctx, t.ID, e, t.PRNumber); err != nil {
			return err
		}
		if err := in.resignalDecision(ctx, t, e); err != nil {
			return err
		}
	}
	return closeErr
}

// resyncComments records and signals the comments on issue or pull request
// number that none of the task's known events cover. A comment event
// recorded before comment ids were (migration 0010) has id 0 and matches
// no comment, so any comment no newer than the latest such event counts
// as covered rather than recorded and signalled twice.
func (in *Intake) resyncComments(ctx context.Context, t Task, onPR bool, number int64, known []IntakeEvent) error {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	comments, err := in.Forgejo.ListIssueComments(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return fmt.Errorf("comments: %w", err)
	}
	kind := "issue_comment"
	if onPR {
		kind = "pull_request_comment"
	}
	var idless time.Time
	for _, e := range known {
		if e.CommentID == 0 && (e.Kind == "issue_comment" || e.Kind == "pull_request_comment") && e.CreatedAt.After(idless) {
			idless = e.CreatedAt
		}
	}
	for _, c := range comments {
		if c.Author == in.BotUser || !c.CreatedAt.After(idless) || slices.ContainsFunc(known, func(e IntakeEvent) bool {
			return (e.Kind == kind || !onPR && e.Kind == IntakePosterComment) && e.CommentID == c.ID
		}) {
			continue
		}
		if err := in.recordComment(ctx, t, onPR, number, c, ""); err != nil {
			return err
		}
	}
	return nil
}
