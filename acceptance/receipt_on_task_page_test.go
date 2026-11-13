package acceptance

// Package acceptance holds the plan's end-to-end tests. This file is
// receipt_on_task_page (roadmap 66) over the console rig: the task
// page's card drawer shows the task's stored receipt — every fact, and
// the labelled assessments apart from them; a task with no receipt
// shows the drawer's empty receipt state. The drawer's receipt tab is
// GET /cards/{id}/drawer?tab=receipt on the receipt view, read through
// the console's Receipts port over the store.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// seededTaskReceipt is a merged task's stored receipt, one of every fact
// and one assessment of every source, as the merge path records it.
func seededTaskReceipt(task string) factoryd.MergeReceipt {
	exit, late := 0, 1
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	return factoryd.MergeReceipt{Version: factoryd.MergeReceiptVersion, Facts: factoryd.ReceiptFacts{
		TaskID: task, Repository: "sf/code", Issue: 91, PR: 17,
		Base: "b45e12ab34cd56ef78ab34cd56ef78ab34cd56ef", Head: "h90de12ab34cd56ef78ab34cd56ef78ab34cd56ef",
		MergeCommit:   "m33aa12ab34cd56ef78ab34cd56ef78ab34cd56ef",
		DiffSHA256:    "d1ff0e12ab34cd56ef78ab34cd56ef78ab34cd56ef78ab34cd56ef78ab34cd56ef78ab",
		PolicyVersion: 19, CostEUR: 1.25,
		Gates: []factoryd.ReceiptGate{
			{Gate: "check", Command: "bash check changed", Exit: &exit, Verdict: "pass"},
			{Gate: "policy", Verdict: "allow"},
			{Gate: "review", Command: "the model review", Exit: &late, Verdict: "concerns"},
		},
		Decisions: []factoryd.ReceiptDecision{
			{Kind: "merge", Decision: "approve", Draw: "2 sessions and 40 minutes of agent time", By: "sebastian",
				Note: "adds the column safely", At: &at},
		},
	}, Assessments: []factoryd.ReceiptAssessment{
		{Source: "review", Verdict: "approve"},
		{Source: "audit", Verdict: "findings"},
		{Source: "judgement", Criterion: "the cache keeps entries", Verdict: "pass",
			Evidence: "the acceptance run kept the entries across the restart"},
	}}
}

// The merged task's drawer carries its stored receipt on the receipt
// tab: every fact — the task, the base and head SHAs, the diff's hash,
// the policy version, every gate's command, exit code and verdict, the
// cost, each human decision — and the model's assessments after the
// facts, each under its source's label. A task with no receipt shows
// the empty state and none of the fact values.
func TestAcceptanceReceiptOnTaskPage(t *testing.T) {
	r, _, _ := NewAcceptanceRig(t, false, nil)
	r.mu.Lock()
	r.tasks = append(r.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-merged", Kind: "agent", Status: "merged", Track: "direct",
			Process: "maintenance", Repository: "sf/code", IssueNumber: 91, PRNumber: 17, Objective: "Add the column"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-bare", Kind: "agent", Status: "done", Track: "direct",
			Process: "maintenance", Repository: "sf/code", IssueNumber: 92, Objective: "No receipt on this one"}})
	r.receipts = map[string]factoryd.MergeReceipt{"t-merged": seededTaskReceipt("t-merged")}
	r.mu.Unlock()
	ck, _ := r.Login(t)

	rec := r.request(http.MethodGet, "/cards/t-merged/drawer?tab=receipt", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the merged task's receipt tab = %d:\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<dt>Task</dt><dd><span class="mono">t-merged</span> · sf/code · #91 · PR #17</dd>`,
		`<dt>Base</dt><dd class="mono">b45e12ab34cd56ef78ab34cd56ef78ab34cd56ef</dd>`,
		`<dt>Head</dt><dd class="mono">h90de12ab34cd56ef78ab34cd56ef78ab34cd56ef</dd>`,
		`<dt>Merge commit</dt><dd class="mono">m33aa12ab34cd56ef78ab34cd56ef78ab34cd56ef</dd>`,
		`<dt>diff_sha256</dt><dd class="mono">d1ff0e12ab34cd56ef78ab34cd56ef78ab34cd56ef78ab34cd56ef78ab34cd56ef78ab</dd>`,
		`<dt>Policy version</dt><dd class="mono">19</dd>`,
		`<dt>Cost</dt><dd class="mono">€1.25</dd>`,
		`<tr><td>check</td><td><code>bash check changed</code></td><td class="mono">0</td><td>pass</td></tr>`,
		`<tr><td>policy</td><td><code>—</code></td><td class="mono">—</td><td>allow</td></tr>`,
		`<tr><td>review</td><td><code>the model review</code></td><td class="mono">1</td><td>concerns</td></tr>`,
		`<li><span class="why">merge</span> approve by sebastian: adds the column safely · drew 2 sessions and 40 minutes of agent time <time>11-13 09:30Z</time></li>`,
		// the assessments follow the facts, each under its source label
		`<h4>The model's assessments</h4>`,
		`<li><span class="why">review</span> approve</li>`,
		`<li><span class="why">audit</span> findings</li>`,
		`<li><span class="why">judgement · the cache keeps entries</span> pass · evidence: the acceptance run kept the entries across the restart</li>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the merged task's drawer lacks %q:\n%s", want, body)
		}
	}
	if want := `hx-get="/cards/t-merged/drawer?tab=receipt"`; !strings.Contains(body, want) {
		t.Errorf("the drawer's tab row lacks the receipt tab %q:\n%s", want, body)
	}

	// the task with no receipt: the empty state, and none of the
	// receipt task's fact values on it
	bare := r.request(http.MethodGet, "/cards/t-bare/drawer?tab=receipt", nil, ck).Body.String()
	if want := `<p class="meta">No receipt yet: this task has none stored.</p>`; !strings.Contains(bare, want) {
		t.Errorf("the receiptless task's drawer lacks the empty state %q:\n%s", want, bare)
	}
	seed := seededTaskReceipt("t-merged")
	for _, no := range []string{
		seed.Facts.Base, seed.Facts.Head, seed.Facts.MergeCommit, seed.Facts.DiffSHA256,
		`<dt>Policy version</dt>`, `<dt>Cost</dt>`, `<code>bash check changed</code>`,
		`<span class="why">merge</span>`, `<span class="why">review</span>`, `<h4>The model's assessments</h4>`,
	} {
		if strings.Contains(bare, no) {
			t.Errorf("the receiptless task's drawer carries %q:\n%s", no, bare)
		}
	}
}
