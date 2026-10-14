package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// SourceDelegate is the source of a card a running agent filed with the
// delegate tool (roadmap 222, item 6): a small direct card on the cheap
// rung, a child of the caller's card, which it depends on.
const SourceDelegate = "delegate"

// DelegationStore is what the delegate hand-over reads and writes.
type DelegationStore interface {
	Task(ctx context.Context, id string) (Task, bool, error)
	CreateTask(ctx context.Context, t Task) error
	AddTaskDeps(ctx context.Context, id string, dependsOn []string) error
	ProjectByRepository(ctx context.Context, repository string) (Project, bool, error)
	CountAdmitted(ctx context.Context, repository string) (int, error)
	CountBacklog(ctx context.Context, repository string) (int, error)
}

// DelegationDeps is what DelegationHandler files a card with.
type DelegationDeps struct {
	Store     DelegationStore
	Forgejo   *Forgejo
	Workflows Workflows
	Timeline  *Timeline
	Budget    func() policy.Budget
	// Policy reads the tree in force, for the ready check the delegated
	// card passes (roadmap 239).
	Policy func() policy.Tree
}

// DelegationHandler serves the gateway's hand-over of an agent's delegate
// call: the child card in lane ready with its dependency on the caller,
// filed before its workflow starts so the wait sees the dependency.
func DelegationHandler(d DelegationDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TaskID string `json:"task_id"`
			Title  string `json:"title"`
			Brief  string `json:"brief"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		in.Title, in.Brief = strings.TrimSpace(in.Title), strings.TrimSpace(in.Brief)
		if in.TaskID == "" || in.Title == "" || utf8.RuneCountInString(in.Title) > 255 || in.Brief == "" {
			http.Error(w, "task_id, a title of 1 to 255 characters and a brief are required", http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		parent, ok, err := d.Store.Task(ctx, in.TaskID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no task "+in.TaskID, http.StatusNotFound)
			return
		}
		if parent.Status != "running" {
			http.Error(w, "task "+in.TaskID+" is "+parent.Status, http.StatusConflict)
			return
		}
		p, ok, err := d.Store.ProjectByRepository(ctx, parent.Repository)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no project has the repository "+parent.Repository, http.StatusConflict)
			return
		}
		if d.Policy != nil {
			if line := FileRefusal(ctx, d.Store, d.Policy(), parent.Repository, in.Title, in.Title, in.Brief, "small"); line != "" {
				http.Error(w, "the ready check refuses the filing: "+line, http.StatusConflict)
				return
			}
		}
		child, ref, err := fileDelegation(ctx, d, p, parent, in.Title, in.Brief)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("task %s: delegated %q as task %s, issue #%d", parent.ID, in.Title, child.ID, ref.Number)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"issue": ref.Number, "task_id": child.ID}) // #nosec G104 -- the answer's encode error changes nothing
	}
}

// fileDelegation files the child: the issue and the row (FileTask), the
// dependency on the parent, then its workflow (enterLane), which waits in
// waiting-deps until the parent is done.
func fileDelegation(ctx context.Context, d DelegationDeps, p Project, parent Task, title, brief string) (Task, IssueRef, error) {
	body := fmt.Sprintf("Delegated by card #%d (%s): it starts when that card is done and builds on its merge.\n\n%s\n\nSize: small\nRung: cheap, delegated routine work",
		parent.IssueNumber, parent.ID, brief)
	t, labels, err := fileEntry(p, Task{
		ID: NewTaskID(), Priority: parent.Priority, RiskClass: "standard", BudgetEUR: policy.CardBudget(d.Budget(), p.Budget, "small", 0),
		Objective: title + "\n\n" + body, Repository: parent.Repository, Source: SourceDelegate, Parent: parent.ID,
		Rung: RungCheap, Size: "small", Author: parent.Author, AuthorWriter: true,
	}, nil)
	if err != nil {
		return Task{}, IssueRef{}, err
	}
	t, ref, err := FileTask(ctx, d.Store, d.Forgejo, p.Repo(), title, body, labels, t)
	if err != nil {
		return Task{}, IssueRef{}, err
	}
	if err := d.Store.AddTaskDeps(ctx, t.ID, []string{parent.ID}); err != nil {
		return Task{}, IssueRef{}, err
	}
	if parent.IssueNumber > 0 {
		if err := d.Forgejo.AddIssueDependency(ctx, p.Repo(), ref.Number, parent.IssueNumber); err != nil {
			log.Printf("task %s: issue dependency on #%d: %v", t.ID, parent.IssueNumber, err)
		}
	}
	by := d.Forgejo.BotUser
	d.Timeline.recordEvent(ctx, t.ID, EventCardCreated, map[string]any{"by": by, "issue": ref.Number, "parent": parent.ID})
	if err := enterLane(ctx, d.Timeline, d.Workflows, t, by, nil); err != nil {
		log.Printf("task %s: start workflow: %v", t.ID, err)
	}
	return t, ref, nil
}
