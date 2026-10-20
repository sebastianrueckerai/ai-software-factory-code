package factoryd

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The question's shape (the question-format concept): every question a
// card puts to a person is five parts -- the ask, one sentence with the
// stop's numbers; the why, what stopped; the info, what a person needs
// for the call, what each answer does; the recommendation, at most one
// action, with the source that backs it; and the premade answers, each a
// value and the label the person reads. The deterministic families -- the
// stops the code makes, most asks today -- fill from the stop's data by
// code, so a card's question is shaped the same way every time it stops.

// Question choice values. A click routes exactly as the label's text
// typed as an answer does today, except park and keep waiting, which
// record the answer and leave the card waiting on its person: no comment
// is posted for them, because a posted comment would start the attempt
// the person declined to start.
const (
	AnswerGrant       = "grant"
	AnswerPark        = "park"
	AnswerFund        = "fund"
	AnswerClose       = "close"
	AnswerKeepWaiting = "keep_waiting"
	AnswerGo          = "go"
)

// HumanQuestion is the five-part question a card asks. Source names what
// backs the recommendation: the check-in's judgment, the agent's, or a
// factoryd rule; empty without one.
type HumanQuestion struct {
	Ask            string           `json:"ask"`
	Why            string           `json:"why"`
	Info           string           `json:"info"`
	Recommendation string           `json:"recommendation,omitempty"`
	Source         string           `json:"source,omitempty"`
	Choices        []QuestionChoice `json:"choices,omitempty"`
}

// QuestionChoice is one premade answer: the value the record carries and
// the label the person reads.
type QuestionChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ChoiceValue returns the value of the choice labelled label, "" when the
// question has no such choice.
func (q *HumanQuestion) ChoiceValue(label string) string {
	for _, c := range q.Choices {
		if c.Label == label {
			return c.Value
		}
	}
	return ""
}

// ChoiceLabel returns the label of the choice valued value, "" when the
// question has no such choice.
func (q *HumanQuestion) ChoiceLabel(value string) string {
	for _, c := range q.Choices {
		if c.Value == value {
			return c.Label
		}
	}
	return ""
}

// Offers reports whether the question has a choice valued value.
func (q *HumanQuestion) Offers(value string) bool {
	return q.ChoiceLabel(value) != ""
}

// Labels returns the choice labels, the words a click posts as the
// answer.
func (q *HumanQuestion) Labels() []string {
	out := make([]string, 0, len(q.Choices))
	for _, c := range q.Choices {
		out = append(out, c.Label)
	}
	return out
}

// QuestionFacts are the stop's numbers a template fills from, as far as
// the caller has them at hand; zero fields leave the ask with the reason's
// own numbers or without that number.
type QuestionFacts struct {
	CheckinCalls int     // check-in calls the task has made
	CheckinCap   int     // the policy's check-in call cap
	CheckinSpent float64 // EUR of the check-in share spent
	CheckinShare float64 // EUR, the policy's check-in share of the budget
}

// ShapedQuestion shapes the question the escalation reason asks: by
// template for the deterministic stops, and from the reason itself for
// every other ask (plainQuestion), so no question reaches a card without
// its five parts.
func ShapedQuestion(reason string, f QuestionFacts, choices []string) *HumanQuestion {
	switch {
	case strings.HasPrefix(reason, checkinSpendAsk+"check-in call cap reached"),
		reason == "check-in call cap reached":
		return callCapQuestion(f)
	case strings.HasPrefix(reason, checkinSpendAsk+"the check-in's share of the budget is spent"),
		reason == "the check-in's share of the budget is spent":
		return checkinShareQuestion(f)
	case strings.HasPrefix(reason, capReached):
		return sessionCapQuestion()
	case strings.HasPrefix(reason, budgetRefused):
		return budgetQuestion(reason)
	case strings.HasPrefix(reason, monthCapReached), strings.HasPrefix(reason, projectCapReached):
		return monthCapQuestion(reason)
	case strings.HasPrefix(reason, gateStopped) && strings.Contains(reason, "WaitChecks ("):
		return waitChecksQuestion(reason)
	case strings.HasPrefix(reason, "The card was quiet for "):
		return quietQuestion(reason)
	}
	return plainQuestion(reason, choices)
}

// plainQuestion is the question an ask no template covers gets, so no
// card puts words alone to a person: an agent's ask keeps its question
// as the headline, every other stop gets one that names the decision.
// The reason's Why: line is the why (else its first line) and its
// Recommendation: line the recommendation, sourced by the reason's
// prefix. An agent's own choices lead the answers, as "say:" values so
// none can pass for a factory control; park and close follow.
func plainQuestion(reason string, choices []string) *HumanQuestion {
	first := escalationQuestion(reason)
	q := &HumanQuestion{
		Ask:     "This card stopped and needs your decision: how should it go on?",
		Why:     first,
		Info:    plainInfo,
		Choices: continueChoices(),
	}
	agent := strings.HasPrefix(reason, AgentAsks)
	if agent {
		q.Ask = first
	}
	for line := range strings.Lines(reason) {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Why: "); ok {
			q.Why = v
		} else if v, ok := strings.CutPrefix(line, "Recommendation: "); ok {
			q.Recommendation = v
		}
	}
	if q.Recommendation != "" {
		switch {
		case agent:
			q.Source = "the agent's"
		case strings.HasPrefix(reason, gateStopped):
			q.Source = "the gate supervisor's"
		case strings.HasPrefix(reason, "check-in: "):
			q.Source = "the check-in's"
		default:
			q.Source = "the factory's"
		}
	}
	if agent && len(choices) > 0 {
		q.Choices = nil
		seen := map[string]bool{"Leave it waiting": true, "End the card": true}
		for _, c := range choices {
			c = strings.TrimSpace(c)
			if c != "" && !seen[c] {
				seen[c] = true
				q.Choices = append(q.Choices, QuestionChoice{"say:" + c, c})
			}
		}
		q.Choices = append(q.Choices, QuestionChoice{AnswerPark, "Leave it waiting"}, QuestionChoice{AnswerClose, "End the card"})
	}
	return q
}

// plainInfo is what an answer does at a stop no template covers.
const plainInfo = "An answer starts the attempt again: with a pull request it re-verifies it through the gates, without one your comment rides along as guidance. 'Leave it waiting' holds the card here; 'End the card' closes the task and its pull request."

// continueChoices are the answers a spend stop takes: yes goes on, park
// and close end the wait.
func continueChoices() []QuestionChoice {
	return []QuestionChoice{
		{AnswerGrant, "Go on"},
		{AnswerPark, "Leave it waiting"},
		{AnswerClose, "End the card"},
	}
}

// continueInfo is what an answer does at a spend stop: the attempt
// starts again with the member's comment as guidance, re-verifying the
// pull request when there is one; the spent budget stays spent.
const continueInfo = "An answer starts the attempt again: with a pull request it re-verifies it through the gates, without one your comment rides along as guidance. The spent budget stays spent."

func callCapQuestion(f QuestionFacts) *HumanQuestion {
	ask := "The card's check-in call cap is reached. May it go on?"
	if f.CheckinCap > 0 {
		calls := f.CheckinCalls
		if calls == 0 {
			calls = f.CheckinCap
		}
		ask = fmt.Sprintf("The card has spent %d of its %d check-in calls. May it go on?", calls, f.CheckinCap)
	}
	return &HumanQuestion{
		Ask:     ask,
		Why:     "Its check-in call cap is reached: the check-in stopped the session, and its checks from here go to a person.",
		Info:    continueInfo,
		Choices: continueChoices(),
	}
}

func checkinShareQuestion(f QuestionFacts) *HumanQuestion {
	ask := "The card has spent its check-in share of the budget. May it go on?"
	if f.CheckinSpent > 0 || f.CheckinShare > 0 {
		ask = fmt.Sprintf("The card has spent €%.2f of its €%.2f check-in share (%d calls). May it go on?",
			f.CheckinSpent, f.CheckinShare, f.CheckinCalls)
	}
	return &HumanQuestion{
		Ask:     ask,
		Why:     "The check-in's share of the budget is spent: the check-in stopped the session, and its checks from here go to a person.",
		Info:    continueInfo,
		Choices: continueChoices(),
	}
}

func sessionCapQuestion() *HumanQuestion {
	return &HumanQuestion{
		Ask:  "The card's session cap is reached. Grant it one more session?",
		Why:  capReached + ".",
		Info: "A grant starts one more session; your comment goes with it as guidance. No answer leaves the card waiting here.",
		Choices: []QuestionChoice{
			{AnswerGrant, "Grant one session"},
			{AnswerPark, "Leave it waiting"},
			{AnswerClose, "End the card"},
		},
	}
}

// budgetShape parses the budget refusal's numbers: "%.3f EUR of the
// %.3f budget remains, and a session on %s needs %.6f EUR".
var budgetShape = regexp.MustCompile(`([\d.]+) EUR of the ([\d.]+) EUR budget remains, and a session on (\S+) needs ([\d.]+) EUR`)

// BudgetNeed is the EUR a session needs, as the budget refusal's reason
// states it; 0 when the reason carries no such number. The fund answer
// of a budget refusal raises the card by it.
func BudgetNeed(reason string) float64 {
	m := budgetShape.FindStringSubmatch(reason)
	if m == nil {
		return 0
	}
	need, err := strconv.ParseFloat(m[4], 64)
	if err != nil || !(need > 0) {
		return 0
	}
	return need
}

func budgetQuestion(reason string) *HumanQuestion {
	q := &HumanQuestion{
		Ask:  "The card's remaining budget cannot fund a call. Fund one more session?",
		Why:  "The remaining budget cannot fund a call: no answer starts an attempt until the budget can fund one.",
		Info: "Funding raises the card's budget on its policy card; an answer starts an attempt only once the budget can fund a call. Close ends the card.",
		Choices: []QuestionChoice{
			{AnswerFund, "Fund one more session"},
			{AnswerClose, "End the card"},
		},
	}
	if m := budgetShape.FindStringSubmatch(reason); m != nil {
		q.Ask = fmt.Sprintf("The card has €%s of its €%s budget left, and a session on %s needs €%s. Fund one more session?",
			m[1], m[2], m[3], m[4])
	}
	return q
}

func monthCapQuestion(reason string) *HumanQuestion {
	project := strings.HasPrefix(reason, projectCapReached)
	name := "month"
	if project {
		name = "project's month"
	}
	// the gateway's detail, with its numbers, rides as the why whole
	detail := strings.TrimSpace(capRefusedSuffix(reason))
	why := "The gateway refuses every further call (refused_month_cap) until the cap is raised or the month ends."
	if detail != "" {
		why = "The gateway refused this session: " + detail + " The gateway refuses every further call until the cap is raised or the month ends."
	}
	return &HumanQuestion{
		Ask:  fmt.Sprintf("The %s cap is reached and the card's session was refused. Raise the cap to let it finish?", name),
		Why:  why,
		Info: "Raising the cap is a commit to the month_eur line in the policy file; the cap card on the board links it. This card resumes once the cap moves.",
		Choices: []QuestionChoice{
			{AnswerFund, "Raised it — retry now"},
			{AnswerPark, "Leave it waiting"},
			{AnswerClose, "End the card"},
		},
	}
}

// capRefusedSuffix is the gateway's detail after the refusal's opening.
func capRefusedSuffix(reason string) string {
	if strings.HasPrefix(reason, projectCapReached) {
		return strings.TrimPrefix(reason, projectCapReached)
	}
	return strings.TrimPrefix(reason, monthCapReached)
}

// waitShape parses the gate stop of the checks: "WaitChecks (attempt %d)
// past its bound of %s: %s", then an optional "\nRecommendation: ...".
var waitShape = regexp.MustCompile(`WaitChecks \(attempt (\d+)\) past its bound of ([^:]+): ([^\n]*)`)

func waitChecksQuestion(reason string) *HumanQuestion {
	q := &HumanQuestion{
		Ask:  "The card's checks have waited past their cap. Keep waiting?",
		Why:  "The gate supervisor stopped the wait: it outlasted its bound.",
		Info: "Keep waiting re-runs the gate's checks now; close ends the card.",
		Choices: []QuestionChoice{
			{AnswerKeepWaiting, "Keep waiting"},
			{AnswerClose, "End the card"},
		},
	}
	if m := waitShape.FindStringSubmatch(reason); m != nil {
		q.Ask = fmt.Sprintf("The card's checks have waited past their cap (attempt %s, bound %s). Keep waiting?", m[1], m[2])
		q.Why = strings.TrimSpace(m[3])
	}
	if i := strings.Index(reason, "\nRecommendation: "); i >= 0 {
		q.Recommendation = strings.TrimSuffix(strings.TrimSpace(reason[i+len("\nRecommendation: "):]), ".")
		q.Source = "the gate supervisor's"
	}
	return q
}

// quietShape parses the sweep's escalation: "The card was quiet for %s
// while %s: is its run gone?".
var quietShape = regexp.MustCompile(`The card was quiet for (.+?) while (.+?): is its run gone\?`)

func quietQuestion(reason string) *HumanQuestion {
	q := &HumanQuestion{
		Ask:  "The card has sat quiet past the sweep's window. Keep waiting?",
		Why:  "Nothing reached its timeline or events for that long, and a nudge signal went unanswered.",
		Info: "Keep waiting re-arms the sweep's quiet window; close ends the card.",
		Choices: []QuestionChoice{
			{AnswerKeepWaiting, "Keep waiting"},
			{AnswerClose, "End the card"},
		},
	}
	if m := quietShape.FindStringSubmatch(reason); m != nil {
		q.Ask = fmt.Sprintf("The card has sat quiet %s while %s. Keep waiting?", m[1], m[2])
	}
	if i := strings.Index(reason, "\nRecommendation: "); i >= 0 {
		q.Recommendation = strings.TrimSuffix(strings.TrimSpace(reason[i+len("\nRecommendation: "):]), ".")
		q.Source = "the stale sweep's"
	}
	return q
}

// The stall card's answers: re-run again re-runs the head's stuck runs
// and keeps the card, check again closes it and lets the watch run on,
// dismiss resolves the stall alert and closes it until the head changes.
const (
	AnswerRerunAgain = "rerun_again"
	AnswerCheckAgain = "check_again"
	AnswerDismiss    = "dismiss"
)

// AnswerRetryRoll is the failed-roll card's retry: its label, posted as
// the member's comment, is what the roller takes for it (consumeRetry).
const AnswerRetryRoll = "retry"

// stallQuestion is the stall card's question: what of repository's head
// at sha has been pending since when, the cards the stall holds, and the
// three answers.
func stallQuestion(repository, sha string, since time.Time, pending []string, cards []int64) HumanQuestion {
	held := "No merged card waits on the chain."
	if len(cards) > 0 {
		held = "The stall holds " + issueRefs(cards) + ": merged, they wait for a green chain to cover them."
	}
	return HumanQuestion{
		Ask: fmt.Sprintf("The chain of %s has been pending at %s since %s UTC. Re-run it, check again or dismiss?",
			repository, sha[:min(7, len(sha))], since.UTC().Format("2006-01-02 15:04")),
		Why: "Pending past the stall bound: " + strings.Join(pending, "; ") + ".",
		Info: held + "\n\nRe-run again re-runs the pending run and keeps this card. Check again closes it and lets the watch run on: " +
			"a head still pending a stall bound later asks again. Dismiss resolves the stall alert and asks nothing more until the head changes. " +
			"A head that concludes, green or red, closes this card by itself.",
		Choices: []QuestionChoice{
			{AnswerRerunAgain, "Re-run again"},
			{AnswerCheckAgain, "Check again"},
			{AnswerDismiss, "Dismiss"},
		},
	}
}

// askOf is an ask of head on one line and the fixed question tail: the
// head is cut so the whole fits MaxAsk, the tail kept whole.
func askOf(head, tail string) string {
	head = strings.Join(strings.Fields(head), " ")
	if len(head)+len(tail) <= MaxAsk {
		return head + tail
	}
	return textutil.Truncate(head, max(MaxAsk-len(tail)-len("…"), 0)) + tail
}

// alertQuestion is the human card of alert a on project in namespace ns:
// why says why no agent takes it, body is the alert's write-up.
func alertQuestion(a SigNozAlert, project, ns, why, body string) HumanQuestion {
	name, on, source := alertParts(a)
	return HumanQuestion{
		Ask: askOf(fmt.Sprintf("%s fires the alert %s%s in %s of %s since %s",
			source, name, on, ns, project, stamp(a.StartsAt)), ": handle it, or wait for it to clear?"),
		Why:  why,
		Info: body,
		Choices: []QuestionChoice{
			{AnswerPark, "Wait for it to clear"},
			{AnswerClose, "Handled, close"},
		},
	}
}

// rollRefusedQuestion is the card of a green main CI run whose images the
// roller will not roll: lines name each unsigned image and why.
func rollRefusedQuestion(short string, names, lines []string, runURL string) HumanQuestion {
	return HumanQuestion{
		Ask: askOf(fmt.Sprintf("roll %s refused: %s", short, strings.Join(names, ", ")), " not signed with the factory's key. Fix the signing, then close."),
		Why: "Main CI's run is green, and factoryd rolls none of its images: an image it rolls must carry a cosign signature by the factory's key (k8s/cosign.pub), and these do not.",
		Info: "The run: " + runURL + "\n\n- " + strings.Join(lines, "\n- ") + "\n\n" +
			"Main CI's image jobs sign each image they push. No signature means a job did not sign it or the tag was pushed outside main CI; " +
			"a signature by another key means the code repository's Actions secret COSIGN_PRIVATE_KEY is not the key k8s/cosign.pub holds. " +
			"Find which, fix it, and close this issue. The next green push run of main is judged again; an empty commit on main starts one.",
		Choices: []QuestionChoice{{AnswerClose, "Fixed, close"}},
	}
}

// rerunQuestion is the card of a red main whose failed jobs lost their
// runner: lost holds the jobs' records.
func rerunQuestion(short, failed string, lost []string) HumanQuestion {
	return HumanQuestion{
		Ask: askOf(fmt.Sprintf("main is red at %s: re-run %s", short, failed), ", its runner was lost."),
		Why: "Main CI's push run failed, but none of its steps did: each failed job lost its runner while it ran " +
			"(the runner killed or shut down, or its docker daemon restarted). No change to the code fixes that, so no agent works on this card.",
		Info: strings.Join(lost, "\n\n") + "\n\n" +
			"Re-run each job from its page. The project's merge queue is paused until a push run of main is green: the re-run's green " +
			"result ends the pause, and factoryd closes this issue.",
		Choices: []QuestionChoice{{AnswerClose, "Re-ran it, close"}},
	}
}

// configRejectedQuestion is the card of a configuration tree dir factoryd
// refused at commit short: stays says which version holds, link is the
// tree, reasons the loader's refusal.
func configRejectedQuestion(dir, short, link, stays, reasons string) HumanQuestion {
	return HumanQuestion{
		Ask: askOf(fmt.Sprintf("config: %s/ at %s", dir, short), " rejected: fix the files and push."),
		Why: stays,
		Info: "Reasons (`factoryd config check " + dir + "` shows the same):\n\n" + fenced(reasons) + "\n\n" +
			"Fix the files and push; the next valid commit on main becomes current within 30 s and closes this issue. The tree: " + link,
		Choices: []QuestionChoice{{AnswerClose, "Fixed, close"}},
	}
}

// monthCapCardQuestion is the board card of the month cap reached at eur
// of limit in month; refusal is the gateway's first refusal, if any.
func monthCapCardQuestion(limit float64, month string, eur float64, link, refusal string) HumanQuestion {
	info := "The gateway refuses every further call with `refused_month_cap` until the month ends or the cap is raised; " +
		"a task it refused waits on a human with the refusal on its card. Raising the cap is a commit to the `month_eur` line, " +
		"like any setting: " + link + "\n\nClose this issue once decided."
	if refusal != "" {
		info += "\n\nThe first refusal:\n\n" + fenced(refusal)
	}
	return HumanQuestion{
		Ask:  fmt.Sprintf("The month cap of %.2f EUR is reached (%s): raise it, or let the factory stand until the month ends?", limit, month),
		Why:  fmt.Sprintf("The factory's variable spend this month (UTC, over the cost ledger: model calls, paid tools, commissioned servers) is %.2f EUR of the cap of %.2f EUR (`budget.month_eur`).", eur, limit),
		Info: info,
		Choices: []QuestionChoice{
			{AnswerPark, "Let it stand"},
			{AnswerClose, "Decided, close"},
		},
	}
}

// routineQuestion is the card a routine that files to a person puts: its
// title the ask, its brief and provenance the info.
func routineQuestion(rt policy.Routine, brief, provenance string, agents bool) HumanQuestion {
	return HumanQuestion{
		Ask:     askOf(rt.Title, ""),
		Why:     "the routine " + rt.Name + " fired and files to a person",
		Info:    brief + "\n\n" + provenance,
		Choices: doneChoices(agents),
	}
}

// doneChoices are the answers of a card a person does: the factory's
// hand-over only where the project runs agents, then Done.
func doneChoices(agents bool) []QuestionChoice {
	if agents {
		return []QuestionChoice{{AnswerGo, "Have the factory do it"}, {AnswerClose, "Done"}}
	}
	return []QuestionChoice{{AnswerClose, "Done"}}
}

// issueQuestion is the card of an issue a person filed into the human
// lane: the issue's title is the ask.
func issueQuestion(title, body, poster string, agents bool) HumanQuestion {
	return HumanQuestion{
		Ask:     askOf(title, ""),
		Why:     "filed by " + poster + " into the human lane",
		Info:    body,
		Choices: doneChoices(agents),
	}
}

// questionFacts reads the numbers the spend templates fill from: the
// check-in calls and spend the task has made and the policy's cap and
// share. It reads nothing for a reason the spend templates do not cover.
func (r *Runner) questionFacts(ctx context.Context, t Task, reason string) QuestionFacts {
	if !strings.HasPrefix(reason, checkinSpendAsk) {
		return QuestionFacts{}
	}
	f := QuestionFacts{}
	if n, err := r.store.CheckinCalls(ctx, t.ID); err != nil {
		log.Printf("task %s: read the check-in calls: %v", t.ID, err)
	} else {
		f.CheckinCalls = n
	}
	if e, err := r.store.CheckinSpent(ctx, t.ID); err != nil {
		log.Printf("task %s: read the check-in spend: %v", t.ID, err)
	} else {
		f.CheckinSpent = e
	}
	if r.cfg.Policy != nil {
		limits := r.cfg.Policy().Document.Limits
		f.CheckinCap = checkinCap(t, limits)
		f.CheckinShare = limits.CheckinShareOf(t.BudgetEUR) * float64(1+t.Usage.ExtraCheckinShare)
	}
	return f
}
