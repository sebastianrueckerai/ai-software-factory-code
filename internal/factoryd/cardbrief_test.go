package factoryd

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// briefRefs is a project with cards #2100 and #2101 and one document
// heading, docs/plan.md#phase-b.
type briefRefs struct{}

func (briefRefs) Issue(n int) (string, string, bool) {
	switch n {
	case 2100:
		return "Narrow the close refusal to plans that hold criteria", "queued", true
	case 2101:
		return "Change two lines in the runner's build files", "waiting-human", true
	}
	return "", "", false
}

func (briefRefs) Heading(path, id string) bool {
	return path == "docs/plan.md" && (id == "" || id == "phase-b")
}

// The three live titles of the board on 2026-11-16.
const (
	title2101 = "The two reproducible manifests name the proofs trees: the red goes green and the merge records the six verdicts"
	title2004 = "The three faults end to end: priced, asked, funded"
	title2100 = "The doClose refusal narrowed to plans that hold criteria: the review finding answered on branch card/2084"
)

var acceptedBrief = BriefText{Title: "Narrow the close refusal to plans that hold criteria", Why: "So a plan with no criteria closes without a waiver.",
	Summary: "The console refuses to close a plan while criteria are open. This card limits the refusal to plans that hold criteria. A test closes a plan with none and one with two."}

func TestCheckBriefRefuses(t *testing.T) {
	with := func(f func(*BriefText)) BriefText {
		b := acceptedBrief
		f(&b)
		return b
	}
	titled := func(title string) BriefText { return with(func(b *BriefText) { b.Title = title }) }
	const why = "The why line gives the reason in one sentence: who needs this, or which problem it removes."
	const summary = "The summary says what gets built, what it changes and how it is checked, under 600 characters."
	const verb = "A work title starts with its verb, for example 'Narrow the close refusal to plans that hold criteria'."
	plain := func(x string) string {
		return `The title names "` + x + `". A title uses plain words. Move "` + x + `" to the text.`
	}
	label := func(x string) string {
		return "'" + x + "' names a part of a document. Link it as [words](path#heading), or say what it is."
	}
	for _, tc := range []struct {
		name string
		b    BriefText
		job  BriefJob
		want string
	}{
		{"a title over 70 runes", titled(title2101), BriefWork, "The title has 111 characters. Keep it under 70."},
		{"a second title over 70 runes", titled(title2100), BriefWork, "The title has 105 characters. Keep it under 70."},
		{"an ask over 110 runes", titled(title2101), BriefAsk, "The ask has 111 characters. Keep it under 110."},
		{"a human's title over 70 runes", titled(title2101), BriefHumanIssue, "The title has 111 characters. Keep it under 70."},
		{"a title on two lines", titled("Narrow the close refusal\nto plans that hold criteria"), BriefWork, "The title has 2 lines. Write it on one line."},
		{"an empty title", titled(" "), BriefWork, "The title is empty. Write it in 1 to 70 characters."},
		{"a #N in the title", titled("Change two lines after #2100 merges"), BriefWork, plain("#2100")},
		{"a branch in the title", titled(title2100), BriefWork, plain("card/2084")},
		{"a branch in an ask", titled(title2100), BriefAsk, plain("card/2084")},
		{"a hex id in the title", titled("Roll 50f5db9 to production"), BriefWork, plain("50f5db9")},
		{"a backtick in the title", titled("Change two lines in `Dockerfile.runner`"), BriefWork, plain("`Dockerfile.runner`")},
		{"a camelCase word in the title", titled(title2100), BriefWork, plain("doClose")},
		{"a snake_case word in the title", titled("Drop the human_summary column"), BriefWork, plain("human_summary")},
		{"a colon in the title", titled(title2004), BriefWork, plain(": priced, asked, funded")},
		{"a colon in an ask", titled(title2004), BriefAsk, plain(": priced, asked, funded")},
		{"a work title with an article", titled(title2101), BriefWork, verb},
		{"a second work title with an article", titled(title2004), BriefWork, verb},
		{"a third work title with an article", titled(title2100), BriefWork, verb},
		{"an ask that is a statement", titled(title2004), BriefAsk, "An ask is one question or one instruction."},
		{"an ask of two questions", titled("Fund it? Park it?"), BriefAsk, "An ask is one question or one instruction."},
		{"an ask after two statements", titled("The plan finished. No test ran. Settle each one."), BriefAsk, "An ask is one question or one instruction."},
		{"no why line", with(func(b *BriefText) { b.Why = "" }), BriefWork, why},
		{"no why line on an ask", BriefText{Title: "Fund 2 EUR more, or end the card?"}, BriefAsk, why},
		{"a why line of two sentences", with(func(b *BriefText) {
			b.Why = "It spent 4.00 of 4.00 EUR in 3 sessions. The last session fixed 2 of 10 findings."
		}), BriefWork, why},
		{"a why line over 160 runes", with(func(b *BriefText) { b.Why = "So " + strings.Repeat("a", 157) + "." }), BriefWork, why},
		{"a why line that starts with the title", with(func(b *BriefText) {
			b.Why = "Narrow the close refusal to plans that hold criteria, so a plan with none closes."
		}), BriefWork, why},
		{"no summary", with(func(b *BriefText) { b.Summary = "" }), BriefWork, summary},
		{"a summary over 600 runes", with(func(b *BriefText) { b.Summary = strings.Repeat("a", 600) + "." }), BriefWork, summary},
		{"a #N that is no card", with(func(b *BriefText) { b.Summary = "The review finding on #2084 asks for it." }), BriefWork,
			"#2084 is no card or pull request of this project."},
		{"a #N of a human's issue that is no card", BriefText{Title: "Fix the cache", Body: "Like #2084 did."}, BriefHumanIssue,
			"#2084 is no card or pull request of this project."},
		{"a link to no heading", with(func(b *BriefText) { b.Summary = "It follows [the goals](docs/board-cards.md#5-goals)." }), BriefWork,
			"The document docs/board-cards.md has no heading 5-goals."},
		{"a link to no document", with(func(b *BriefText) { b.Body = "Read [the plan](docs/gone.md) first." }), BriefWork,
			"There is no document docs/gone.md."},
		{"Phase B", with(func(b *BriefText) { b.Summary = "Phase B of the plan narrows the refusal." }), BriefWork, label("Phase B")},
		{"fault N", with(func(b *BriefText) { b.Summary = "This answers fault 3." }), BriefWork, label("fault 3")},
		{"requirement N", with(func(b *BriefText) { b.Why = "So requirement 1b holds." }), BriefAsk, label("requirement 1b")},
		{"step N", BriefText{Title: "Fix the cache", Summary: "Do step 2 first."}, BriefHumanIssue, label("step 2")},
		{"task N", with(func(b *BriefText) { b.Summary = "It builds on Task 3." }), BriefWork, label("Task 3")},
		{"a branch named in the text", with(func(b *BriefText) { b.Body = "The work sits on branch card/2084." }), BriefWork,
			"The text names the branch card/2084. The factory adds the start line itself. Remove the branch."},
		{"an agent's branch in the summary", with(func(b *BriefText) { b.Summary = "Start from factory/t-d9321927378c and test it." }), BriefWork,
			"The text names the branch factory/t-d9321927378c. The factory adds the start line itself. Remove the branch."},
		{"a branch in a human's title", titled("Merge branch card/2084"), BriefHumanIssue,
			"The text names the branch card/2084. The factory adds the start line itself. Remove the branch."},
		{"Lint in the why line", with(func(b *BriefText) { b.Why = "So a plan closes; the waiver goes." }), BriefWork,
			"The why line holds a semicolon. Write two sentences."},
		{"Lint in the title", titled("Leverage the close refusal"), BriefWork, `The title holds "Leverage". Write "use" instead.`},
		{"Lint in an ask", BriefText{Title: "Fund the rung?", Why: "So the card goes on."}, BriefAsk,
			`The ask holds "rung", a word of the factory's own machinery. Say what it means for the reader.`},
		{"Lint in the summary", with(func(b *BriefText) { b.Summary = "The card waits — a test closes it." }), BriefWork,
			"The summary holds an em dash. Write two sentences, or use a comma."},
	} {
		if got := CheckBrief(tc.b, tc.job, briefRefs{}); !slices.Contains(got, tc.want) {
			t.Errorf("%s: CheckBrief = %q, want it to hold %q", tc.name, got, tc.want)
		}
	}
}

func TestCheckBriefAccepts(t *testing.T) {
	with := func(f func(*BriefText)) BriefText {
		b := acceptedBrief
		f(&b)
		return b
	}
	for _, tc := range []struct {
		name string
		b    BriefText
		job  BriefJob
	}{
		{"the work brief", acceptedBrief, BriefWork},
		{"the pronoun I after task and step", with(func(b *BriefText) {
			b.Summary = "This is the task I filed last week. The step I missed was the login."
		}), BriefWork},
		{"a body that numbers its own steps", with(func(b *BriefText) { b.Body = "Step 1: build X. Step 2: test it." }), BriefWork},
		{"a time in the title", with(func(b *BriefText) { b.Title = "Run the backup at 02:00 each night" }), BriefWork},
		{"an abbreviation in the why line", with(func(b *BriefText) { b.Why = "So the cards show their reason, e.g. in the board." }), BriefWork},
		{"a brand in the title", with(func(b *BriefText) { b.Title = "Ship the iOS build" }), BriefWork},
		{"a slash that is no branch", with(func(b *BriefText) { b.Summary = "Keep the branch and/or the tag." }), BriefWork},
		{"a link inside the text", with(func(b *BriefText) { b.Summary = "It serves [the goals](#2-goals) of the plan." }), BriefWork},
		{"links to code and the web", with(func(b *BriefText) {
			b.Summary = "It changes [the board](internal/console/board.go) as [the round](https://example.com/round.md) shows."
		}), BriefWork},
		{"offer and case as plain words", with(func(b *BriefText) {
			b.Summary = "The console offers a retry. In case 2 runs fail, the card waits."
		}), BriefWork},
		{"an ask with a statement before it", BriefText{Title: "Plan X finished, but no test checked its 5 promises. Settle each one.",
			Why: "The last card closed without a merge, so the plan's tests never ran."}, BriefAsk},
		{"an ask that is a question", BriefText{Title: "Add 2.00 EUR to fix the 8 open review findings, or end the card?",
			Why: "It spent 4.00 of 4.00 EUR in 3 sessions."}, BriefAsk},
		{"a human's own title", BriefText{Title: title2004}, BriefHumanIssue},
		{"references that resolve", BriefText{Title: "Change two lines in the runner's build files", Why: "Agents may not push to workflow folders.",
			Summary: "This part of [Phase B](docs/plan.md#phase-b) needs a human. Wait for #2100 to merge first, then #2101 closes. See [the plan](docs/plan.md).",
			Body:    "The pull request github.com/o/r/pull/5 shows the change."}, BriefWork},
	} {
		if got := CheckBrief(tc.b, tc.job, briefRefs{}); len(got) != 0 {
			t.Errorf("%s: CheckBrief = %q, want none", tc.name, got)
		}
	}
}

// countingRefs counts the issue lookups of briefRefs.
type countingRefs struct {
	briefRefs
	asked map[int]int
}

func (r countingRefs) Issue(n int) (string, string, bool) {
	r.asked[n]++
	return r.briefRefs.Issue(n)
}

// A fault named in two fields is one refusal, and each number is looked
// up once.
func TestCheckBriefRefusesAFaultOnce(t *testing.T) {
	b := acceptedBrief
	b.Summary, b.Body = "It answers #2084 after #2100.", "See #2084 and #2100."
	refs := countingRefs{asked: map[int]int{}}
	if got := CheckBrief(b, BriefWork, refs); !slices.Equal(got, []string{"#2084 is no card or pull request of this project."}) {
		t.Fatalf("CheckBrief = %q", got)
	}
	if want := map[int]int{2084: 1, 2100: 1}; !maps.Equal(refs.asked, want) {
		t.Fatalf("lookups %v, want %v", refs.asked, want)
	}
}

// A writer that cannot meet the rules files on its third try, and the
// caller marks the brief unchecked.
func TestThirdRefusalFilesUnchecked(t *testing.T) {
	bad := BriefText{Title: title2100}
	for try := 1; try <= 3; try++ {
		ok, refusals := FileBrief(bad, BriefWork, briefRefs{}, try)
		if ok != (try == 3) || len(refusals) == 0 {
			t.Fatalf("try %d: FileBrief = %v, %q; want ok only on the third, with the refusals", try, ok, refusals)
		}
	}
	if ok, refusals := FileBrief(acceptedBrief, BriefWork, briefRefs{}, 1); !ok || len(refusals) != 0 {
		t.Fatalf("the accepted brief: FileBrief = %v, %q", ok, refusals)
	}
}

func TestBriefJobOf(t *testing.T) {
	for _, tc := range []struct {
		t    Task
		want BriefJob
	}{
		{Task{Source: SourceWebhook}, BriefHumanIssue},
		{Task{Source: SourceAPI}, BriefHumanIssue},
		{Task{Source: SourceWebhook, Author: ChatUser}, BriefWork},
		{Task{Source: SourceWebhook, Author: ChatUser, Kind: "human"}, BriefAsk},
		{Task{Source: SourceWebhook, Kind: "human"}, BriefHumanIssue},
		{Task{Source: SourcePlan, Kind: "human"}, BriefAsk},
		{Task{Source: SourcePlan}, BriefWork},
		{Task{Source: SourceRoll}, BriefWork},
	} {
		if got := BriefJobOf(tc.t); got != tc.want {
			t.Errorf("BriefJobOf(%s, %s) = %d, want %d", tc.t.Source, tc.t.Kind, got, tc.want)
		}
	}
}
