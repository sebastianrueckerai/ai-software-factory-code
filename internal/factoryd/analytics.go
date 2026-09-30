package factoryd

import (
	"cmp"
	"context"
	"log"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// The kinds of factory.human.intervention (ruling 185).
const (
	InterventionApprove          = "approve"
	InterventionRequestChanges   = "request_changes"
	InterventionReject           = "reject"
	InterventionEscalationAnswer = "escalation_answer"
	InterventionPosterAnswer     = "poster_answer"
	InterventionExtraTime        = "extra_time"
	InterventionAlertVerdict     = "alert_verdict_click"
)

// projectLookup finds the project a repository belongs to.
type projectLookup interface {
	ProjectByRepository(ctx context.Context, repository string) (Project, bool, error)
}

// projectLabels are the project name and merge rule a task's metrics
// carry: its repository's project row, else the repository and the
// default merge rule human.
func projectLabels(ctx context.Context, store projectLookup, repository string) (project, mergeRule string) {
	p, ok, err := store.ProjectByRepository(ctx, repository)
	if err != nil {
		log.Printf("metrics: project of %s: %v", repository, err)
	}
	if !ok {
		return repository, policy.MergeHuman
	}
	return p.Name, cmp.Or(p.MergeRule, policy.MergeHuman)
}

// taskReader reads a task row.
type taskReader interface {
	Task(ctx context.Context, id string) (Task, bool, error)
}

// processLane are the process and lane a task's metrics carry, none where
// its row holds none: a report task carries its parent's (the presort's
// card waits in backlog, the screening's is in ready).
func processLane(ctx context.Context, store taskReader, t Task) (process, lane string) {
	if t.Report() {
		parent, ok, err := store.Task(ctx, t.Parent)
		if err != nil {
			log.Printf("metrics: parent %s of task %s: %v", t.Parent, t.ID, err)
		}
		if ok {
			t = parent
		}
	}
	return cmp.Or(t.Process, "none"), cmp.Or(t.Lane, "none")
}

// countOutcome records a finished task's outcome and cost; every path that
// ends a task calls it once, after the task turned final, so
// factory.task.outcome equals the final rows of tasks. merged is the cost
// outcome of a done task with a pull request, any other the final state; a
// task that ran no session (a roll, a human-lane task) has alias none.
func countOutcome(ctx context.Context, m *telemetry.Metrics, store interface {
	projectLookup
	taskReader
}, t Task, final string) {
	if m == nil {
		return
	}
	project, mergeRule := projectLabels(ctx, store, t.Repository)
	process, lane := processLane(ctx, store, t)
	alias := cmp.Or(t.ModelAlias, "none")
	m.TaskOutcome.Add(ctx, 1, metric.WithAttributes(attribute.String("project", project),
		attribute.String("state", final), attribute.String("model_alias", alias),
		attribute.Int("attempts", t.Usage.Attempts), attribute.String("merge_rule", mergeRule),
		attribute.String("process", process), attribute.String("lane", lane)))
	outcome := final
	if final == "done" && t.PRNumber != 0 {
		outcome = "merged"
	}
	m.TaskCost.Record(ctx, t.SpentEUR, metric.WithAttributes(attribute.String("model_alias", alias), attribute.String("outcome", outcome),
		attribute.String("process", process), attribute.String("lane", lane)))
}

// countReview records one review answer of a task in process and lane:
// none when the call gave no verdict, strict_improvement unset when the
// answer left it out.
func (r *Runner) countReview(ctx context.Context, rv Review, round int, process, lane string) {
	strict := "unset"
	if rv.StrictImprovement != nil {
		strict = strconv.FormatBool(*rv.StrictImprovement)
	}
	r.cfg.Metrics.ReviewVerdict.Add(ctx, 1, metric.WithAttributes(attribute.String("verdict", cmp.Or(rv.Verdict, "none")),
		attribute.String("strict_improvement", strict), attribute.Int("round", round),
		attribute.String("process", process), attribute.String("lane", lane)))
}
