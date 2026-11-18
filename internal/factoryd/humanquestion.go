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
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("The chain of %s has been pending at %s since %s UTC, past the stall bound. It waits on %s.",
			repository, ShortSHA(sha), since.UTC().Format("2006-01-02 15:04"), textutil.AndList(pending)) +
			"\n\n" + held + "\n\n“" + LabelOf(AnswerRerunAgain) + "” runs the pending run and keeps this card. “" + LabelOf(AnswerCheckAgain) +
			"” closes it and lets the watch run on. A head still pending a stall bound later asks again. “" + LabelOf(ActClose+"/"+variantDismiss) +
			"” resolves the stall alert and asks nothing more until the head changes. A head that concludes, green or red, closes this card by itself.",
		Choices:  choices(AnswerRerunAgain, AnswerCheckAgain, ActClose+"/"+variantDismiss),
		Happened: fmt.Sprintf("The chain at %s has been pending since %s, waiting on %s.", ShortSHA(sha), timeWords(since), plural(len(pending), "check")),
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
		Info: fmt.Sprintf("The chain of %s is red at %s: %s. The project runs no agents, so a human repairs it.", repository, short, textutil.AndList(findings)) +
			"\n\n" + carried + "\n\nRead the run's log or the red status, find the cause, fix it from main's head, and push: the fix rides the chain again, and a green chain ends every wait. " +
			"A green head closes this card by itself, and so does a head that moves on. Closing it yourself dismisses the red at " + short + ": no new card asks until the head changes.",
		Choices:  humanDoneChoices,
		Happened: fmt.Sprintf("The chain of %s is red at %s, with %s.", repository, short, plural(len(findings), "failed check")),
		Waits:    waitOnChain(cards),
		Check:    fmt.Sprintf("Closes the card and lets the red at %s stand. A red chain at a newer head files a new card.", short),
	}
}

// FactoryPRQuestion is the human card of pull request number at url, of
// brief b, which factoryd opened on a project without agents: no card's
// gates carry it, so a human merges it or closes it, on the card or in
// Forgejo, and either ends the card.
func FactoryPRQuestion(b BriefText, number int64, url string) HumanQuestion {
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: "The factory opened the pull request, and the project runs no agents: no card's gates check it, so a human merges it. " +
			"The pull request: " + url + "\n\nRead its change, then merge it or close it, on the card or in Forgejo: either ends this card.",
		Choices:  choices(ActApproveMerge, ActDenyMerge+"/reject"),
		Happened: fmt.Sprintf("The factory opened pull request #%d, and with no agents on the project, no gate checks it.", number),
	}
}

// AlertQuestion is the human card of alert a on project in namespace ns,
// which informs: what fires where, and why a human looks into it. why says
// why no agent takes it, body is the alert's write-up. An event's card
// (eventLabel) says that no resolve clears it and names the failed Job its
// labels carry.
func AlertQuestion(a SigNozAlert, project, ns, why, body string) HumanQuestion {
	name, of := oneLine(a.Labels["alertname"]), cmp.Or(alertService(a.Labels), ns)
	_, _, source := alertParts(a)
	if a.Labels[eventLabel] == "true" {
		info := "The alert is an event, not a state: SigNoz's resolve only means the rule's window moved on, so this card stays open until a human closes it."
		if job := a.Labels["k8s.object.name"]; dnsLabel.MatchString(job) {
			if cron := cronJobOf(job); cron != "" {
				info = fmt.Sprintf("The alert is an event, not a state: SigNoz's resolve only means the rule's window moved on, so this card closes when the CronJob `%s` completes a run, or when a human closes it.", cron)
			}
			info = fmt.Sprintf("The Job `%s` in namespace `%s` failed: read its pods' logs (`kubectl -n %s logs job/%s --all-containers`). ", job, ns, ns, job) + info
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
		Choices: choices(ActClose+"/"+variantNoted, ActNote+"/"+variantPark),
	}
	q.Happened = firedLine(a)
	return q
}

// firedLine is the story line of alert a that fires, "" without its start.
func firedLine(a SigNozAlert) string {
	if a.StartsAt.IsZero() {
		return ""
	}
	line := "It has fired since " + timeWords(a.StartsAt)
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
		q.Happened = "It fired from " + timeWords(from) + " until " + timeWords(until) + "."
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
		Info: "Main CI's run is green at " + short + ", and the factory rolls none of its images: an image it rolls must carry a cosign signature by the factory's key (k8s/cosign.pub), and these do not." +
			"\n\nThe run: " + runURL + "\n\n- " + strings.Join(lines, "\n- ") + "\n\n" +
			"Main CI's image jobs sign each image they push. No signature means a job did not sign it or the tag was pushed outside main CI. " +
			"A signature by another key means the code repository's Actions secret `COSIGN_PRIVATE_KEY` is not the key k8s/cosign.pub holds. " +
			"Find which, fix it, and close this issue. The next green push run of main is judged again, and an empty commit on main starts one.",
		Choices:  humanDoneChoices,
		Happened: fmt.Sprintf("Main's run is green at %s, and %s of it %s no signature by the factory's key.", short, plural(len(names), "image"), carry(len(names))),
		Waits:    "The new images stay off the cluster until they are signed.",
		Check:    "Closes the card. The factory judges the images of main's next green run again.",
	}
}

// RerunQuestion is the card of a red main whose push run index of commit
// sha failed only in jobs that lost their runner: failed names the jobs,
// lost holds their records, why says why factoryd's own re-run did not
// end it.
func RerunQuestion(index int64, sha, failed string, lost []string, why string) HumanQuestion {
	b := SourceBriefs[SourceRerun](BriefFacts{Name: failed})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("Main CI's push run %d of %s failed, but none of its steps did: each failed job lost its runner while it ran "+
			"(the runner killed or shut down, or its docker daemon restarted). No change to the code fixes that, so no agent works on this card. %s", index, ShortSHA(sha), why) +
			"\n\n" + strings.Join(lost, "\n\n") + "\n\n" +
			"Re-run each job from its page. The project's merge queue is paused until a push run of main is green: the re-run's green " +
			"result ends the pause, and the factory closes this issue.",
		Choices:  humanDoneChoices,
		Happened: fmt.Sprintf("Main's push run %d failed in %s, and each failed job lost its runner while it ran.", index, failed),
		Waits:    queuePaused,
		Check:    mainRunChecked,
	}
}

// StillRedQuestion is the human card of a red main whose remedy merged
// while main's push run index at sha, at link, is red still.
func StillRedQuestion(index int64, sha, remedy, link string) HumanQuestion {
	b := SourceBriefs[SourceStillRed](BriefFacts{})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("Main CI's push run %d of %s failed after %s merged. The factory files one automatic remedy for a red main, "+
			"the fix-forward card or the revert after it, and main stayed red after it, so a human decides.", index, ShortSHA(sha), remedy) +
			"\n\nThe run: " + link + "\n\nRead its log, then fix main or revert by hand. The project's merge queue is paused until a push run of " +
			"main is green: that run ends the pause, and the factory closes this issue.",
		Choices:  humanDoneChoices,
		Happened: fmt.Sprintf("Main's push run %d failed at %s after %s merged.", index, ShortSHA(sha), remedy),
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

// MonthCapCardQuestion is the board card of the month cap reached at eur
// of limit in month; refusal is the gateway's newest refusal, if any. When
// that refusal is LiteLLM's outer guard and eur is below limit, the guard
// fired under the cap and link is the file that sets it.
func MonthCapCardQuestion(limit float64, month string, eur float64, link, refusal string) HumanQuestion {
	if outerGuardRefused(refusal, eur, limit) {
		b := SourceBriefs[SourceMonthCap](BriefFacts{Name: fmt.Sprintf("%s EUR", textutil.EUR(limit)), Of: fmt.Sprintf("%s EUR", textutil.EUR(eur))})
		return HumanQuestion{
			Ask: b.Title,
			Why: b.Why,
			Info: fmt.Sprintf("The factory's variable spend this month (%s, UTC) is %s EUR, below the cap of %s EUR (`budget.month_eur`), "+
				"yet LiteLLM's proxy-wide `max_budget`, the outer guard behind the cap, refused a call. Either `max_budget` sits under 1.25 times the cap, "+
				"or LiteLLM's USD counter ran ahead of the ledger in EUR. scripts/budget-backstop-check tells them apart: it fails when `max_budget` is under 1.25 times `budget.month_eur`. "+
				"If it passes, the counter ran ahead and `max_budget` needs the raise all the same.", month, textutil.EUR(eur), textutil.EUR(limit)) +
				"\n\nRaise `max_budget` in k8s/platform/litellm/helmrelease.yaml, the LiteLLM helmrelease, which stays at 1.25 times the cap or more: " + link +
				"\n\nThe policy cap needs no change. Close this issue once decided.\n\nThe newest refusal:\n\n" + fenced(refusal),
			Choices:  choices(ActClose+"/"+variantNoted, ActNote+"/"+variantPark),
			Happened: fmt.Sprintf("LiteLLM refused a model call at %s EUR of spend this month, below the cap of %s EUR.", textutil.EUR(eur), textutil.EUR(limit)),
			Waits:    "Model calls fail until the guard's limit is raised.",
		}
	}
	info := "The gateway refuses every further call until the month ends or the cap is raised. " +
		"A task it refused waits on its card and resumes on its own once the cap is raised or the month turns. Raising the cap is a commit to the `month_eur` line, " +
		"like any setting: " + link + "\n\nClose this issue once decided."
	if refusal != "" {
		info += "\n\nThe newest refusal:\n\n" + fenced(refusal)
	}
	b := SourceBriefs[SourceMonthCap](BriefFacts{Name: fmt.Sprintf("%s EUR", textutil.EUR(limit))})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("The factory's variable spend in %s (UTC, over the cost ledger: model calls, paid tools, commissioned servers) is %s EUR of the cap of %s EUR (`budget.month_eur`). ", month, textutil.EUR(eur), textutil.EUR(limit)) +
			info,
		Choices:  choices(ActClose+"/"+variantNoted, ActNote+"/"+variantPark),
		Happened: fmt.Sprintf("The factory spent %s EUR of its %s EUR limit in %s.", textutil.EUR(eur), textutil.EUR(limit), monthWords(month)),
		Waits:    "Every model call waits until the limit is raised or the month ends.",
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

// timeWords is t as a face says it: "16 November at 05:12 UTC".
func timeWords(t time.Time) string { return t.UTC().Format("2 January at 15:04 UTC") }

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
var humanDoneChoices = choices(ActClose+"/"+variantDone, ActNote+"/"+variantPark)

// humanWorkChoices are the answers of a card a human does: "I did it", the
// factory's hand-over only where the project runs agents, and "Not now".
func humanWorkChoices(agents bool) []QuestionChoice {
	keys := []string{ActClose + "/" + variantDone}
	if agents {
		keys = append(keys, AnswerGo)
	}
	return choices(append(keys, ActNote+"/"+variantPark)...)
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
