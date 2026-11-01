package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// poolRig is a rig whose project ai-software-factory-code holds budget as
// its loaded policy's budget block and funded EUR of agent-funded usage
// this month — or a usage read that fails with fail. The fund rig's own
// project, so the read-outs ride the board's existing fixtures.
func poolRig(t *testing.T, budget *policy.ProjectBudget, funded float64, fail error) *rig {
	t.Helper()
	r := newRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", MergeRule: "human", Objects: "factory",
		Agents: "coder", Process: "maintenance", Budget: budget}}
	r.chat.funded = map[string]float64{"sf/code": funded}
	r.chat.fundedErr = fail
	return r
}

// poolFace is the project's face as every reader reads it: the status
// line's pool, the board's text and HTML, and the chat's status line —
// one projection through them all.
func poolFace(t *testing.T, r *rig) (pool poolLine, board, html, snap string) {
	t.Helper()
	ck, s := r.loggedIn(t)
	req := httptest.NewRequest(http.MethodGet, "/board?project=ai-software-factory-code", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	d, err := r.c.boardOf(req, boardFilter{Project: "ai-software-factory-code", View: viewWide}, actor{})
	if err != nil {
		t.Fatalf("the board of ai-software-factory-code: %v", err)
	}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/board?project=ai-software-factory-code", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /board = %d: %s", rec.Code, rec.Body)
	}
	snap, err = (&turn{c: r.c, r: req, a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent"},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}}).snapshot()
	if err != nil {
		t.Fatalf("the project's status line: %v", err)
	}
	// the template's escaping alone differs; the figures are the same words
	return d.Status.Pool, boardWords(d), strings.ReplaceAll(rec.Body.String(), "&#39;", "'"), snap
}

// The project face's pool status is one projection the board's HTML, the
// board's text and the chat's status line read: the configured, used and
// remaining EUR of the UTC month agree as literal figures, usage is never
// erased by lowering the pool, and an unreadable aggregate is never zero.
func TestProjectPoolStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		budget     *policy.ProjectBudget
		funded     float64
		fail       error
		present    bool
		configured float64
		used       float64
		remaining  float64
		want       string
		notWant    string
	}{
		{name: "unset", budget: nil, funded: 3,
			want: "no pool: discretionary_eur is absent or 0, every agent raise waits for a member"},
		{name: "standing", budget: &policy.ProjectBudget{DiscretionaryEUR: 10, RaiseMaxEUR: 2}, funded: 3,
			present: true, configured: 10, used: 3, remaining: 7,
			want: "€3.00 of €10.00 funded this UTC month, €7.00 left, each raise at most €2.00; fund is confirmed at this autonomy: a raise is the member's money and meets no pool bound"},
		{name: "lowered below its use", budget: &policy.ProjectBudget{DiscretionaryEUR: 2, RaiseMaxEUR: 1}, funded: 3,
			present: true, configured: 2, used: 3, remaining: 0,
			want:    "€3.00 funded this UTC month of a pool now at €2.00: usage stands and nothing is left until the pool is raised or the month turns",
			notWant: "€2.00 left"},
		{name: "zero ceiling", budget: &policy.ProjectBudget{DiscretionaryEUR: 0, RaiseMaxEUR: 1}, funded: 3,
			want: "no pool: discretionary_eur is absent or 0"},
		{name: "unreadable usage", budget: &policy.ProjectBudget{DiscretionaryEUR: 10, RaiseMaxEUR: 2}, funded: 3,
			fail: errors.New("the store is down"), present: true, configured: 10,
			want:    "the pool stands at €10.00 but its usage could not be read: how much is left is unknown, and a standing pool is not unattended authority",
			notWant: "left, "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, board, html, snap := poolFace(t, poolRig(t, tc.budget, tc.funded, tc.fail))
			if pool.Present != tc.present {
				t.Errorf("Present = %v, want %v (%s)", pool.Present, tc.present, pool.Words)
			}
			if pool.ConfiguredEUR != tc.configured || pool.UsedEUR != tc.used || pool.RemainingEUR != tc.remaining {
				t.Errorf("the pool = configured %v, used %v, remaining %v; want %v, %v, %v",
					pool.ConfiguredEUR, pool.UsedEUR, pool.RemainingEUR, tc.configured, tc.used, tc.remaining)
			}
			if !strings.Contains(pool.Words, tc.want) {
				t.Errorf("the pool's words lack %q:\n%s", tc.want, pool.Words)
			}
			if tc.notWant != "" && strings.Contains(pool.Words, tc.notWant) {
				t.Errorf("the pool's words say %q:\n%s", tc.notWant, pool.Words)
			}
			if len(pool.Month) != 7 || !strings.Contains(pool.Policy, "policy version") {
				t.Errorf("the pool's month %q or policy identity %q is blank", pool.Month, pool.Policy)
			}
			for name, got := range map[string]string{"the board's text": board, "the board's HTML": html, "the chat's status line": snap} {
				if tc.present && !strings.Contains(got, tc.want) {
					t.Errorf("%s lacks the pool's figures %q:\n%s", name, tc.want, got)
				}
				if !tc.present && strings.Contains(got, "discretionary") {
					t.Errorf("%s paints a pool where none stands:\n%s", name, got)
				}
			}
		})
	}
}

// Funding readiness holds the same pool — €10 configured, €3 funded —
// and reads the loaded policy's actual rules: the autonomy as loaded, the
// confirm list resolved over it and the per-raise guard. Only fully
// permitted authority is called ready for unattended funding; a confirmed
// fund or a missing guard is not, whatever the pool holds, and reading it
// changes no permission.
func TestProjectPoolReadiness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project policy.ProjectConfig
		budget  *policy.ProjectBudget
		ready   bool
		want    string
	}{
		{name: "manual", project: policy.ProjectConfig{MergeRule: "human"},
			budget: &policy.ProjectBudget{DiscretionaryEUR: 10, RaiseMaxEUR: 2},
			want:   "fund is confirmed at this autonomy: a raise is the member's money and meets no pool bound"},
		{name: "assisted", project: policy.ProjectConfig{Autonomy: policy.AutonomyAssisted, MergeRule: "human"},
			budget: &policy.ProjectBudget{DiscretionaryEUR: 10, RaiseMaxEUR: 2},
			want:   "fund is confirmed at this autonomy"},
		{name: "autonomous", project: policy.ProjectConfig{Autonomy: policy.AutonomyAutonomous, MergeRule: "human"},
			budget: &policy.ProjectBudget{DiscretionaryEUR: 10, RaiseMaxEUR: 2}, ready: true,
			want: "the loaded policy permits an unattended raise from it"},
		{name: "full auto", project: policy.ProjectConfig{Autonomy: policy.AutonomyAutonomous, MergeRule: policy.MergeAuto},
			budget: &policy.ProjectBudget{DiscretionaryEUR: 10, RaiseMaxEUR: 2}, ready: true,
			want: "the loaded policy permits an unattended raise from it"},
		{name: "autonomous without the guard", project: policy.ProjectConfig{Autonomy: policy.AutonomyAutonomous, MergeRule: "human"},
			budget: &policy.ProjectBudget{DiscretionaryEUR: 10},
			want:   "raise_max_eur is absent or 0: every agent raise refuses, so the pool funds nothing until it is set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := poolRig(t, tc.budget, 3, nil)
			tc.project.Repository, tc.project.Objects, tc.project.Agents, tc.project.Process, tc.project.Budget = "sf/code", "factory", "coder", "maintenance", tc.budget
			r.ledger.files["ai-software-factory-code"] = tc.project
			pool, board, html, snap := poolFace(t, r)
			if pool.Ready != tc.ready {
				t.Errorf("Ready = %v, want %v (%s)", pool.Ready, tc.ready, pool.Words)
			}
			if !strings.Contains(pool.Words, tc.want) {
				t.Errorf("the pool's words lack %q:\n%s", tc.want, pool.Words)
			}
			if pool.Ready != strings.Contains(pool.Words, "the loaded policy permits an unattended raise") {
				t.Errorf("the pool's words name unattended raises wrongly:\n%s", pool.Words)
			}
			for name, got := range map[string]string{"the board's text": board, "the board's HTML": html, "the chat's status line": snap} {
				if !strings.Contains(got, tc.want) {
					t.Errorf("%s lacks the readiness words %q:\n%s", name, tc.want, got)
				}
			}
			if pool.ConfiguredEUR != 10 || pool.UsedEUR != 3 || pool.RemainingEUR != 7 {
				t.Errorf("the same pool = configured %v, used %v, remaining %v; want 10, 3, 7",
					pool.ConfiguredEUR, pool.UsedEUR, pool.RemainingEUR)
			}
		})
	}
}
