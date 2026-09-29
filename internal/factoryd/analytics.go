package factoryd

import (
	"cmp"
	"context"
	"log"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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

// countFinish records a finished task's outcome and cost: merged is the
// cost outcome of a done task with a pull request, any other the final
// state; a task that ran no session (a roll) has alias none.
func (r *Runner) countFinish(ctx context.Context, t Task, final string) {
	project, mergeRule := projectLabels(ctx, r.store, t.Repository)
	alias := cmp.Or(t.ModelAlias, "none")
	r.cfg.Metrics.TaskOutcome.Add(ctx, 1, metric.WithAttributes(attribute.String("project", project),
		attribute.String("state", final), attribute.String("model_alias", alias),
		attribute.Int("attempts", t.Usage.Attempts), attribute.String("merge_rule", mergeRule)))
	outcome := final
	if final == "done" && t.PRNumber != 0 {
		outcome = "merged"
	}
	r.cfg.Metrics.TaskCost.Record(ctx, t.SpentEUR, metric.WithAttributes(attribute.String("model_alias", alias), attribute.String("outcome", outcome)))
}

// countReview records one review answer: none when the call gave no
// verdict, strict_improvement unset when the answer left it out.
func (r *Runner) countReview(ctx context.Context, rv Review, round int) {
	strict := "unset"
	if rv.StrictImprovement != nil {
		strict = strconv.FormatBool(*rv.StrictImprovement)
	}
	r.cfg.Metrics.ReviewVerdict.Add(ctx, 1, metric.WithAttributes(attribute.String("verdict", cmp.Or(rv.Verdict, "none")),
		attribute.String("strict_improvement", strict), attribute.Int("round", round)))
}
