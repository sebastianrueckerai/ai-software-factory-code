package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// seededReceipt is a merged task's stored receipt holding one of every
// fact and one assessment of every source, as the merge path records it.
func seededReceipt() factoryd.MergeReceipt {
	exit, late := 0, 1
	at := time.Date(2026, 11, 12, 14, 4, 0, 0, time.UTC)
	return factoryd.MergeReceipt{Version: factoryd.MergeReceiptVersion, Facts: factoryd.ReceiptFacts{
		TaskID: "t-rcpt", Repository: "sf/code", Issue: 77, PR: 9,
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
		{Source: "judgement", Criterion: "the receipt renders every fact", Verdict: "pass",
			Evidence: "the drawer HTML carries each fact of the stored receipt"},
	}}
}

// The drawer's receipt tab shows the task's stored receipt: every fact —
// the task, the base and head, the diff's hash, the policy version,
// every gate's command, exit code and verdict, the cost, each human
// decision — and the model's assessments under their source labels,
// after the facts. A task with no stored receipt gets the tab's empty
// state, no fact values.
func TestCardDrawerReceiptTab(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-rcpt", Kind: "agent", Status: "done", Track: "direct", Repository: "sf/code",
			IssueNumber: 77, PRNumber: 9, Objective: "Add the column"}},
		{Task: factoryd.Task{ID: "t-bare", Kind: "agent", Status: "done", Track: "direct", Repository: "sf/code",
			IssueNumber: 78, PRNumber: 10, Objective: "No receipt on this one"}},
	}
	r.receipts.rows = map[string]factoryd.MergeReceipt{"t-rcpt": seededReceipt()}
	ck, _ := r.loggedIn(t)
	rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-rcpt/drawer?tab=receipt", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the receipt tab = %d:\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<dt>Task</dt><dd><span class="mono">t-rcpt</span> · sf/code · #77 · PR #9</dd>`,
		`<dt>Base</dt><dd class="mono">b45e12ab34cd56ef78ab34cd56ef78ab34cd56ef</dd>`,
		`<dt>Head</dt><dd class="mono">h90de12ab34cd56ef78ab34cd56ef78ab34cd56ef</dd>`,
		`<dt>Merge commit</dt><dd class="mono">m33aa12ab34cd56ef78ab34cd56ef78ab34cd56ef</dd>`,
		`<dt>diff_sha256</dt><dd class="mono">d1ff0e12ab34cd56ef78ab34cd56ef78ab34cd56ef78ab34cd56ef78ab34cd56ef78ab</dd>`,
		`<dt>Policy version</dt><dd class="mono">19</dd>`,
		`<dt>Cost</dt><dd class="mono">€1.25</dd>`,
		`<tr><td>check</td><td><code>bash check changed</code></td><td class="mono">0</td><td>pass</td></tr>`,
		`<tr><td>policy</td><td><code>—</code></td><td class="mono">—</td><td>allow</td></tr>`,
		`<tr><td>review</td><td><code>the model review</code></td><td class="mono">1</td><td>concerns</td></tr>`,
		`<li><span class="why">merge</span> approve by sebastian: adds the column safely · drew 2 sessions and 40 minutes of agent time <time>11-12 14:04Z</time></li>`,
		// the assessments follow the facts, each under its source label
		`<h4>The model's assessments</h4>`,
		`<li><span class="why">review</span> approve</li>`,
		`<li><span class="why">audit</span> findings</li>`,
		`<li><span class="why">judgement · the receipt renders every fact</span> pass · evidence: the drawer HTML carries each fact of the stored receipt</li>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the receipt tab lacks %q:\n%s", want, body)
		}
	}
	// the tab is on the drawer's tab row
	if want := `hx-get="/cards/t-rcpt/drawer?tab=receipt"`; !strings.Contains(body, want) {
		t.Errorf("the drawer's tab row lacks the receipt tab %q:\n%s", want, body)
	}

	// a task with no stored receipt: the empty state, no fact values
	bare := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-bare/drawer?tab=receipt", nil), ck).Body.String()
	if want := `<p class="meta">No receipt yet: this task has none stored.</p>`; !strings.Contains(bare, want) {
		t.Errorf("the receiptless task's drawer lacks the empty state %q:\n%s", want, bare)
	}
	for _, no := range []string{
		seededReceipt().Facts.Base, seededReceipt().Facts.DiffSHA256,
		`<dt>Policy version</dt>`, `<dt>Cost</dt>`, `<span class="why">review</span>`,
	} {
		if strings.Contains(bare, no) {
			t.Errorf("the receiptless task's drawer carries the fact %q:\n%s", no, bare)
		}
	}
}
