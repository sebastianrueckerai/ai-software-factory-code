package factoryd

import (
	"cmp"
	"fmt"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// StallQuestion is the stall card's question: what of repository's head
// at sha has been pending since when, the cards the stall holds, and the
// three answers.
func StallQuestion(repository, sha string, since time.Time, pending []string, cards []int64) HumanQuestion {
	held := "No merged card waits on the chain."
	if len(cards) > 0 {
		held = "The stall holds " + issueRefs(cards) + ": merged, they wait for a green chain to cover them."
	}
	b := SourceBriefs[SourceChainStall](BriefFacts{Name: repository})
	var said Moments
	pendingSince := fmt.Sprintf("The chain of %s has been pending at commit %s since %s, past the stall bound. It waits on %s.",
		repository, ShortSHA(sha), said.Say(since, LayoutStamp), textutil.AndList(pending))
	happened := fmt.Sprintf("The chain at commit %s has been pending since %s, waiting on %s.", ShortSHA(sha), said.Say(since, LayoutWords), plural(len(pending), "check"))
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: pendingSince +
			"\n\n" + held + "\n\n“" + LabelOf(AnswerRerunAgain) + "” runs the pending run and keeps this card. “" + LabelOf(AnswerCheckAgain) +
			"” closes it and lets the watch run on. A head still pending a stall bound later asks again. “" + LabelOf(ActClose+"/"+variantDismiss) +
			"” resolves the stall alert and asks nothing more until the head changes. A head that concludes, green or red, closes this card by itself.",
		Choices:  choices(AnswerRerunAgain, AnswerCheckAgain, ActClose+"/"+variantDismiss),
		Happened: happened,
		Moments:  said,
	}
}

// RepairQuestion is the human repair card of repository's red head at
// sha, on a project without agents: findings name each red run and red
// required context, runURL is the failing run's page ("" without one),
// cards the issues of the cards whose change the head carried.
func RepairQuestion(repository, sha string, findings []string, runURL string, cards []int64) HumanQuestion {
	short := ShortSHA(sha)
	carried := "No card's change rides this head. The chain alone is the work."
	if len(cards) > 0 {
		carried = "The head carries " + issueRefs(cards) + ": merged, they wait for a green chain to cover them. This card is their repair."
	}
	if runURL != "" {
		carried = "The run: " + runURL + "\n\n" + carried
	}
	b := SourceBriefs[SourceChainRepair](BriefFacts{Name: repository})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("The chain of %s is red at commit %s: %s. The project runs no agents, so a human repairs it.", repository, short, textutil.AndList(findings)) +
			"\n\n" + carried + "\n\nRead the run's log or the red status, find the cause, fix it from main's head, and push: the fix rides the chain again, and a green chain ends every wait. " +
			"A green head closes this card by itself, and so does a head that moves on. Closing it yourself dismisses the red at commit " + short + ": no new card asks until the head changes.",
		Choices:  humanDoneChoices,
		Happened: fmt.Sprintf("The chain of %s is red at commit %s, with %s.", repository, short, plural(len(findings), "failed check")),
		Waits:    waitOnChain(cards),
		Check:    fmt.Sprintf("Closes the card and lets the red at commit %s stand. A red chain at a newer head files a new card.", short),
	}
}

// FactoryPRQuestion is the human card of pull request number at url, of
// brief b, which factoryd opened on a project without agents: no card's
// gates carry it, so a human merges it or closes it, on the card or in
// Forgejo, and either ends the card. refusal is why factoryd did not merge
// it itself, "" for a pull request it never merges.
func FactoryPRQuestion(b BriefText, number int64, url, refusal string) HumanQuestion {
	lead := "The factory opened the pull request, and the project runs no agents: no card's gates check it, so a human merges it. "
	if refusal != "" {
		lead = "The factory opened the pull request and did not merge it itself: " + refusal + ". A human merges it. "
	}
	return HumanQuestion{
		Ask:      b.Title,
		Why:      b.Why,
		Info:     lead + "The pull request: " + url + "\n\nRead its change, then merge it or close it, on the card or in Forgejo: either ends this card.",
		Choices:  choices(ActApproveMerge, ActDenyMerge+"/reject"),
		Happened: fmt.Sprintf("The factory opened pull request #%d, and with no agents on the project, no gate checks it.", number),
	}
}

// AlertQuestion is the human card of alert a on project in namespace ns,
// which informs: what fires where, and why a human looks into it. why says
// why no agent takes it, body is the alert's write-up. An event's card
// (eventLabel) says that no resolve clears it and, by its rule, names the
// object its labels carry and the fact that closes it (CloseGoneEvents).
func AlertQuestion(a SigNozAlert, project, ns, why, body string) HumanQuestion {
	name, of := oneLine(a.Labels["alertname"]), cmp.Or(alertService(a.Labels), ns)
	_, _, source := alertParts(a)
	if a.Labels[eventLabel] == "true" {
		const event = "The alert is an event, not a state: SigNoz's resolve only means the rule's window moved on, so this card "
		info := event + "stays open until a human closes it."
		if object := a.Labels["k8s.object.name"]; dnsLabel.MatchString(object) {
			switch name {
			case jobFailedAlert:
				if cron := cronJobOf(object); cron != "" {
					info = event + fmt.Sprintf("closes when the CronJob `%s` completes a run, or when a human closes it.", cron)
				}
				info = fmt.Sprintf("The Job `%s` in namespace `%s` failed: read its pods' logs (`kubectl -n %s logs job/%s --all-containers`). ", object, ns, ns, object) + info
			case claimAlert:
				info = fmt.Sprintf("A provisioner refused the volume claim `%s` in namespace `%s`: read its events (`kubectl -n %s describe pvc %s`). ", object, ns, ns, object) +
					event + "closes when the claim is bound or gone, unless a human acted on it first."
			case helmReleaseAlert:
				info = fmt.Sprintf("The HelmRelease `%s` in namespace `%s` failed or went back: read its status (`kubectl -n %s describe helmrelease %s`). ", object, ns, ns, object) +
					event + "closes when Flux reads the HelmRelease Ready again or it is gone, unless a human acted on it first."
			}
		}
		body = info + "\n\n" + body
	}
	whyLine := "It may point at a fault on " + of
	if why != "" {
		whyLine += ", and " + why
	}
	q := HumanQuestion{
		Ask:     source + " fires the alert \u201c" + name + "\u201d on " + of,
		Why:     whyLine + ".",
		Info:    body,
		Choices: choices(ActClose+"/"+variantNoted, KeyPark),
		// the body's firing line names the start (AlertIssue)
		Moments: Moments{AlertStart(a)}.In(body),
	}
	q.Happened = firedLine(a, &q.Moments)
	return q
}

// firedLine is the story line of alert a that fires, its start kept in
// said; "" without its start.
func firedLine(a SigNozAlert, said *Moments) string {
	if a.StartsAt.IsZero() {
		return ""
	}
	line := "It has fired since " + said.Say(a.StartsAt, LayoutWords)
	if v := oneLine(a.Annotations["value"]); v != "" {
		line += ", at " + v
	}
	return line + "."
}

// AlertClearedQuestion is the face of the human card of alert name on of,
// raised by source (SigNoz or Flux), once it cleared: what happened, why
// it still matters, and when it fired, from to until. Its answers stay
// the firing card's.
func AlertClearedQuestion(source, name, of string, from, until time.Time) HumanQuestion {
	q := HumanQuestion{Ask: source + " cleared the alert \u201c" + oneLine(name) + "\u201d on " + of,
		Why: "The fault behind it may still be there, though the alert no longer fires."}
	if !from.IsZero() {
		q.Happened = "It fired from " + q.Moments.Say(from, LayoutWords) + " until " + q.Moments.Say(until, LayoutWords) + "."
	}
	return q
}

// RollRefusedQuestion is the card of a green main CI run whose images the
// roller will not roll: lines name each unsigned image and why.
func RollRefusedQuestion(short string, names, lines []string, runURL string) HumanQuestion {
	b := SourceBriefs[SourceRollRefused](BriefFacts{Name: strings.Join(names, ", ")})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: "Main CI's run is green at commit " + short + ", and the factory rolls none of its images: an image it rolls must carry a cosign signature by the factory's key (k8s/cosign.pub), and these do not." +
			"\n\nThe run: " + runURL + "\n\n- " + strings.Join(lines, "\n- ") + "\n\n" +
			"Main CI's image jobs sign each image they push. No signature means a job did not sign it or the tag was pushed outside main CI. " +
			"A signature by another key means the code repository's Actions secret `COSIGN_PRIVATE_KEY` is not the key k8s/cosign.pub holds. " +
			"Find which, fix it, and close this issue. The next green push run of main is judged again, and an empty commit on main starts one.",
		Choices:  humanDoneChoices,
		Happened: fmt.Sprintf("Main's run is green at commit %s, and %s of it %s no signature by the factory's key.", short, plural(len(names), "image"), carry(len(names))),
		Waits:    "The new images stay off the cluster until they are signed.",
		Check:    "Closes the card. The factory judges the images of main's next green run again.",
	}
}

// RerunQuestion is the card of a red main whose push run index of commit
// sha failed only in jobs that lost their runner: failed names the jobs,
// lost holds their records, which name the instants said, why says why
// factoryd's own re-run did not end it.
func RerunQuestion(index int64, sha, failed string, lost []string, said Moments, why string) HumanQuestion {
	b := SourceBriefs[SourceRerun](BriefFacts{Name: failed})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("CI run %d on main, at commit %s, failed, but none of its steps did: each failed job lost its runner while it ran "+
			"(the runner killed or shut down, or its docker daemon restarted). No change to the code fixes that, so no agent works on this card. %s", index, ShortSHA(sha), why) +
			"\n\n" + strings.Join(lost, "\n\n") + "\n\n" +
			"Re-run each job from its page. The project's merge queue is paused until a push run of main is green: the re-run's green " +
			"result ends the pause, and the factory closes this issue.",
		Choices:  humanDoneChoices,
		Happened: fmt.Sprintf("CI run %d on main failed in %s, and each failed job lost its runner while it ran.", index, failed),
		Waits:    queuePaused,
		Check:    mainRunChecked,
		Moments:  said,
	}
}

// StillRedQuestion is the human card of a red main whose remedy merged
// while main's push run index at sha, at link, is red still.
func StillRedQuestion(index int64, sha, remedy, link string) HumanQuestion {
	b := SourceBriefs[SourceStillRed](BriefFacts{})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("CI run %d on main, at commit %s, failed after %s merged. The factory files one automatic remedy for a red main, "+
			"the fix-forward card or the revert after it, and main stayed red after it, so a human decides.", index, ShortSHA(sha), remedy) +
			"\n\nThe run: " + link + "\n\nRead its log, then fix main or revert by hand. The project's merge queue is paused until a push run of " +
			"main is green: that run ends the pause, and the factory closes this issue.",
		Choices:  humanDoneChoices,
		Happened: fmt.Sprintf("CI run %d on main failed at commit %s after %s merged.", index, ShortSHA(sha), remedy),
		Waits:    queuePaused,
		Check:    mainRunChecked,
	}
}

// ConfigRejectedQuestion is the card of a configuration tree dir factoryd
// refused at commit short: stays says which version holds, link is the
// tree, reasons the loader's refusal.
func ConfigRejectedQuestion(dir, short, link, stays, reasons string) HumanQuestion {
	b := SourceBriefs[SourceConfig](BriefFacts{Name: dir + "/"})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("The commit %s of %s/ was rejected. ", short, dir) + stays + "\n\nReasons (`factoryd config check " + dir + "` shows the same):\n\n" + fenced(reasons) + "\n\n" +
			"Fix the files and push. The next valid commit on main becomes current within 30 s and closes this issue. The tree: " + link,
		Choices:  humanDoneChoices,
		Happened: fmt.Sprintf("The factory refused commit %s of %s/. %s", short, dir, stays),
		Waits:    "The configuration before it stays in force.",
		Check:    fmt.Sprintf("Closes the card. The factory checks each new commit of %s/, and a valid one takes effect within 30 seconds.", dir),
	}
}

// MonthCapCardQuestion is the board card of the month cap limit reached in
// month m at now: its whole spend, and the count since a reset when one
// started it; m.Refusal is the gateway's newest refusal, if any. When that
// refusal is LiteLLM's outer guard and the count is below limit, the guard
// fired under the cap and link is the file that sets it.
func MonthCapCardQuestion(limit float64, m MonthSpend, link string, now time.Time) HumanQuestion {
	whole, count, capEUR := textutil.EUR(m.whole()), textutil.EUR(m.EUR), textutil.EUR(limit)
	var said Moments
	since := ""
	if m.Reset != nil {
		since = fmt.Sprintf("counted since %s, when %s reset the month's count", said.Say(m.Reset.At, resetLayout(m.Reset.At, now)), m.Reset.Login)
	}
	if outerGuardRefused(m.Refusal, m.EUR, limit) {
		of := whole + " EUR"
		spend := fmt.Sprintf("The factory's variable spend this month (%s, UTC) is %s EUR, below the cap of %s EUR (`budget.month_eur`), "+
			"yet LiteLLM's proxy-wide `max_budget`, the outer guard behind the cap, refused a call. Either `max_budget` sits under 1.25 times the cap, "+
			"or LiteLLM's USD counter ran ahead of the ledger in EUR. scripts/budget-backstop-check tells them apart: it fails when `max_budget` is under 1.25 times `budget.month_eur`. "+
			"If it passes, the counter ran ahead and `max_budget` needs the raise all the same.", m.Month, whole, capEUR)
		happened := fmt.Sprintf("LiteLLM refused a model call at %s EUR of spend this month, below the cap of %s EUR.", whole, capEUR)
		if m.Reset != nil {
			of = count + " EUR since a reset"
			spend = fmt.Sprintf("The factory's variable spend this month (%s, UTC) is %s EUR; %s: %s EUR, below the cap of %s EUR (`budget.month_eur`). "+
				"Yet LiteLLM's proxy-wide `max_budget`, the outer guard behind the cap, refused a call: it counts the whole month, and a reset of the count does not move it.",
				m.Month, whole, since, count, capEUR)
			happened = fmt.Sprintf("LiteLLM refused a model call at %s EUR of spend this month, %s EUR of it since %s reset the month's count.", whole, count, m.Reset.Login)
		}
		b := SourceBriefs[SourceMonthCap](BriefFacts{Name: capEUR + " EUR", Of: of})
		return HumanQuestion{
			Ask: b.Title,
			Why: b.Why,
			Info: spend + "\n\nRaise `max_budget` in k8s/platform/litellm/helmrelease.yaml, the LiteLLM helmrelease, which stays at 1.25 times the cap or more: " + link +
				"\n\nThe policy cap needs no change. Close this issue once decided.\n\nThe newest refusal:\n\n" + fenced(m.Refusal),
			Choices:  choices(ActClose+"/"+variantNoted, KeyPark),
			Happened: happened,
			Waits:    "Model calls fail until the guard's limit is raised.",
			Moments:  said,
		}
	}
	spend := fmt.Sprintf("The factory's variable spend in %s (UTC, over the cost ledger: model calls, paid tools, commissioned servers) is %s EUR of the cap of %s EUR (`budget.month_eur`). ", m.Month, whole, capEUR)
	happened := fmt.Sprintf("The factory spent %s EUR of its %s EUR limit in %s.", whole, capEUR, monthWords(m.Month))
	if m.Reset != nil {
		spend = fmt.Sprintf("The factory's variable spend in %s (UTC, over the cost ledger: model calls, paid tools, commissioned servers) is %s EUR; %s: %s EUR of the cap of %s EUR (`budget.month_eur`). ",
			m.Month, whole, since, count, capEUR)
		happened = fmt.Sprintf("The factory spent %s EUR of its %s EUR limit since %s reset the month's count, %s EUR in %s.", count, capEUR, m.Reset.Login, whole, monthWords(m.Month))
	}
	info := "The gateway refuses every further call until the month ends, a human resets the month's count in the console's status panel, or the cap is raised by a commit. " +
		"A task it refused waits on its card and resumes on its own once the count is reset, the cap is raised or the month turns. Raising the cap is a commit to the `month_eur` line, " +
		"like any setting: " + link + "\n\nClose this issue once decided."
	if m.Refusal != "" {
		info += "\n\nThe newest refusal:\n\n" + fenced(m.Refusal)
	}
	b := SourceBriefs[SourceMonthCap](BriefFacts{Name: capEUR + " EUR"})
	return HumanQuestion{
		Ask:      b.Title,
		Why:      b.Why,
		Info:     spend + info,
		Choices:  choices(ActClose+"/"+variantNoted, KeyPark),
		Happened: happened,
		Waits:    "Every model call waits until a human resets the month's count in the console's status panel, the limit is raised by a commit, or the month ends.",
		Moments:  said,
	}
}

// RoutineQuestion is the card a routine that files to a person puts: its
// title the ask, its brief and provenance the info.
func RoutineQuestion(rt policy.Routine, brief, provenance string, agents bool) HumanQuestion {
	b := RoutineBrief(rt)
	return HumanQuestion{
		Ask:     b.Title,
		Why:     b.Why,
		Info:    brief + "\n\n" + provenance,
		Choices: humanWorkChoices(agents),
	}
}

// MovedQuestion is the card of an issue filed for an agent on a project
// that runs no agents now: the issue's title is the ask.
func MovedQuestion(title, body, poster string) HumanQuestion {
	q := IssueQuestion(title, body, poster, false)
	q.Why = "filed by " + poster + " for an agent, and the project runs no agents now"
	return q
}

// IssueQuestion is the card of an issue a person filed into the human
// lane: the issue's title is the ask.
func IssueQuestion(title, body, poster string, agents bool) HumanQuestion {
	return HumanQuestion{
		Ask:     oneLine(title),
		Why:     "filed by " + poster + " for a human, so no agent works on it",
		Info:    body,
		Choices: humanWorkChoices(agents),
	}
}

// monthWords is month, 2006-01, as its name; "this month" when it is no
// month.
func monthWords(month string) string {
	if m, err := time.Parse("2006-01", month); err == nil {
		return m.Month().String()
	}
	return "this month"
}

// carry is "carries" for one, else "carry".
func carry(n int) string {
	if n == 1 {
		return "carries"
	}
	return "carry"
}

// waitOnChain is what waits on a chain that is not green: the merged cards
// it holds, "" for none.
func waitOnChain(cards []int64) string {
	switch len(cards) {
	case 0:
		return ""
	case 1:
		return issueRefs(cards) + " waits for a green chain."
	}
	return issueRefs(cards) + " wait for a green chain."
}

// The lines of a red main's human card: the merge queue waits on it, and a
// green push run ends the wait.
const (
	queuePaused    = "The merge queue stays paused until a push run of main is green."
	mainRunChecked = "Closes the card. The factory reads main's next push run, and a green one ends the queue's pause."
)

// humanDoneChoices are the answers of a card whose work a human does where
// no agent may: "I did it" and "Not now".
var humanDoneChoices = choices(ActClose+"/"+variantDone, KeyPark)

// humanWorkChoices are the answers of a card a human does: "I did it", the
// factory's hand-over only where the project runs agents, and "Not now".
func humanWorkChoices(agents bool) []QuestionChoice {
	keys := []string{ActClose + "/" + variantDone}
	if agents {
		keys = append(keys, AnswerGo)
	}
	return choices(append(keys, KeyPark)...)
}

// choices are the stored choices of the answers keys name.
func choices(keys ...string) []QuestionChoice {
	out := make([]QuestionChoice, len(keys))
	for i, k := range keys {
		c, ok := ChoiceOf(k)
		if !ok {
			panic("factoryd: the vocabulary has no answer " + k)
		}
		out[i] = c
	}
	return out
}
