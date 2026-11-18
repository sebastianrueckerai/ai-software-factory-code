package factoryd

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// BriefFacts are the plain names a Go-filed card's brief is written from:
// Name what the card is about and Of where it is, each "" where its source
// names none. A name is a word a human reads, never an id.
type BriefFacts struct{ Name, Of string }

// SourceBriefs are the briefs of the cards Go code files, by source: the
// title, the why line and the summary, written at filing and stored; the
// filer adds the card's text. A human card's title is its ask, its why
// line the question's why, and it needs no summary.
var SourceBriefs = map[string]func(BriefFacts) BriefText{
	policy.SourceToolRequest: func(f BriefFacts) BriefText {
		return BriefText{Title: "Add " + f.Name + " to the image " + f.Of,
			Why: "A card needs " + f.Name + " in its sandbox, and the image " + f.Of + " lacks it.",
			Summary: "It adds " + f.Name + " to the image " + f.Of + " in the project's images file. The factory then builds the image, signs it and pins it, " +
				"and the card that asked runs on it. The image's build checks the change."}
	},
	SourceRoll: func(f BriefFacts) BriefText {
		title := "Deploy the new images " + f.Name
		if utf8.RuneCountInString(title) > titleMax {
			title = fmt.Sprintf("Deploy the %d new images", strings.Count(f.Name, ", ")+1)
		}
		return BriefText{Title: title,
			Why: "Main published new images, and the cluster runs the old ones until they are deployed.",
			Summary: "It points the image digests under k8s/ at the images main published and merges the change. " +
				"Flux deploys it, and the card ends once every image runs live."}
	},
	SourceRollRefused: func(f BriefFacts) BriefText {
		return BriefText{Title: "Fix the signing of the images " + f.Name + ", then close this card",
			Why: "The factory deploys only images signed with its key, so these images stay off the cluster."}
	},
	SourceRollFailed: func(f BriefFacts) BriefText {
		return BriefText{Title: f.Name + " " + rollAskMarker + ". Try the roll again?",
			Why: "Main's newest images are not live, and every card merged since waits on a green chain."}
	},
	SourceMainFix: func(f BriefFacts) BriefText {
		return BriefText{Title: "Fix the failing " + checksOf(f.Name) + " on main",
			Why: "Main fails the " + checksOf(f.Name) + ", so the merge queue stays paused until main is green.",
			Summary: "A fix from main's head that makes the " + checksOf(f.Name) + " pass. The card merges ahead of the queue, " +
				"and main's next green run ends the pause."}
	},
	SourceRerun: func(f BriefFacts) BriefText {
		return BriefText{Title: "Re-run the " + checksOf(f.Name) + " on main after a lost runner",
			Why: "A lost runner, not the code, turned main red."}
	},
	SourceStillRed: func(BriefFacts) BriefText {
		return BriefText{Title: "Fix main by hand or revert the change, as main stays red after its remedy merged",
			Why: "The factory files one remedy for a red main, and main stayed red after it."}
	},
	SourceRevert: func(BriefFacts) BriefText {
		return BriefText{Title: "Revert the change that turned main red",
			Why: "Main stayed red past its bound with no fix landed, and every merge waits on it.",
			Summary: "A pull request that reverts the red commit from main. The project's checks run on it, " +
				"and its merge ends the queue's pause."}
	},
	SourceGroupRevert: func(f BriefFacts) BriefText {
		return BriefText{Title: "Revert plan " + f.Name + " from main",
			Why: "The plan's merges left main broken, so its cards come off main together.",
			Summary: "It reverts the plan's merge commits from main, newest first, and changes nothing else. " +
				"The project's checks run on the revert before it merges."}
	},
	SourceChainRepair: func(f BriefFacts) BriefText {
		return BriefText{Title: "Repair the deploy chain of " + f.Name,
			Why: "The deploy chain of " + f.Name + " is red, and merged cards wait for a green chain to cover them.",
			Summary: "A fix from main's head that turns the chain green. The chain runs again on the fix, " +
				"and a green chain ends every wait."}
	},
	SourceChainStall: func(f BriefFacts) BriefText {
		return BriefText{Title: "The deploy chain of " + f.Name + " stalled. Run it again?",
			Why: "The chain has been pending past its stall bound, and merged cards wait on it."}
	},
	SourceAlert: func(f BriefFacts) BriefText {
		return BriefText{Title: "Look into the alert " + f.Name + " on " + f.Of,
			Why: "The alert " + f.Name + " fires on " + f.Of + " and may point at a fault there.",
			Summary: "The agent reads the alert, finds the cause and fixes it, or shows that the alert is noise. " +
				"The alert resolves once the fault is gone."}
	},
	SourceGitHubSync: func(f BriefFacts) BriefText {
		return BriefText{Title: "Bring in the changes from GitHub for " + f.Name,
			Why: "Someone committed to the project on GitHub, and the factory's copy lacks the change.",
			Summary: "A pull request that brings the GitHub branch's commits into the project. " +
				"The project's checks run on it, and its merge brings the change in."}
	},
	SourceSchemaCopy: func(f BriefFacts) BriefText {
		return BriefText{Title: "Copy the changed schemas to " + f.Name,
			Why: "A new build changed the configuration schemas, and their copies in " + f.Name + " no longer match.",
			Summary: "A pull request that writes the changed schemas into their copies. " +
				"The policy check runs on it, and its merge makes the copies match the build."}
	},
	SourceTemplateUpdate: func(f BriefFacts) BriefText {
		return BriefText{Title: "Update the project from its template " + f.Name,
			Why: "The template " + f.Name + " changed since the project took it, and the project lacks the changes.",
			Summary: "A pull request that applies the template's later changes to the project's files. " +
				"The project's checks run on it before it merges."}
	},
	SourceImageRemoval: func(f BriefFacts) BriefText {
		return BriefText{Title: "Remove the unused image " + f.Name,
			Why: "No card ran on the image " + f.Name + " for 30 days, and its builds and pin cost space.",
			Summary: "It deletes the image's entry from the project's images file and changes nothing else. " +
				"The factory's check runs on the change, and after the merge the factory unpins the image."}
	},
	SourceScreening: func(BriefFacts) BriefText {
		return BriefText{Title: "Screen the issue before an agent works on it",
			Why: "An issue from outside the project could ask an agent for something unsafe.",
			Summary: "A model reads the issue and answers whether an agent may work on it. " +
				"Its report goes on the issue, and the card starts or waits on a human by its answer."}
	},
	SourceSizing: func(BriefFacts) BriefText {
		return BriefText{Title: "Size the card before its first session",
			Why: "A card that is not small is planned first, so its size is judged before it starts.",
			Summary: "A model reads the card and answers its size. " +
				"The size sets the card's bounds, and a card too big for one session goes to the plan track."}
	},
	SourceProver: func(BriefFacts) BriefText {
		return BriefText{Title: "Write the tests that prove the plan's criteria",
			Why: "The plan's criteria need proofs written apart from the cards that build the work.",
			Summary: "A model writes a test for each of the plan's criteria and files them on main. " +
				"factoryd runs them after the cards merge, and each criterion stands on its test."}
	},
	// Name is the UTC month, 2006-01
	SourceChatBudget: func(f BriefFacts) BriefText {
		return BriefText{Title: "The chat's budget for " + monthWords(f.Name) + " is spent. Add money to it?",
			Why: "Until it is raised, the project agent judges no stopped card, so every stop comes to you."}
	},
	// Of is the spend at which LiteLLM's outer guard refused under the cap
	// of Name, "" when the cap itself is reached
	SourceMonthCap: func(f BriefFacts) BriefText {
		if f.Of != "" {
			return BriefText{Title: "LiteLLM's outer guard refused calls at " + f.Of + ", below the month cap of " + f.Name + ". Raise its limit?",
				Why: "The guard behind the cap refuses model calls the cap still allows."}
		}
		return BriefText{Title: "The factory spent its monthly limit of " + f.Name + ". Raise it in policy.yaml, or wait for the month to end?",
			Why: "The gateway refuses every further model call until the limit is raised or the month ends."}
	},
	SourceConfig: func(f BriefFacts) BriefText {
		return BriefText{Title: "Fix the rejected configuration in " + f.Name + " and push",
			Why: "The factory refused the newest configuration, so the version before it stays in force."}
	},
	SourceProductionHeld: func(f BriefFacts) BriefText {
		return BriefText{Title: "Deploys of " + f.Name + " to production wait for an open plan. Deploy now, or send the plan back?",
			Why: "While a plan is open on main, no merge goes to production, so a half-built feature never goes live."}
	},
	SourceRoutine: func(f BriefFacts) BriefText {
		return BriefText{Title: f.Name,
			Why: "The routine " + f.Of + " files this card on its schedule.",
			Summary: "The agent works the routine's brief and opens one pull request. " +
				"The card's checks and its review judge the work before it merges."}
	},
}

// checksOf names the checks names lists, "check X" or "checks X, Y".
func checksOf(names string) string {
	if strings.Contains(names, ", ") {
		return "checks " + names
	}
	return "check " + names
}
