package factoryd_test

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// everyRef resolves every card and every document heading: the fixtures'
// references are the cards and documents they name.
type everyRef struct{}

func (everyRef) Issue(int) (string, string, bool) { return "a card", "open", true }
func (everyRef) Files(path string) []string       { return []string{path} }
func (everyRef) Heading(string, string) bool      { return true }
func (everyRef) Err() error                       { return nil }

// writtenSources are the card sources whose brief a model or a human
// writes: the rest are Go code's.
var writtenSources = []string{factoryd.SourceWebhook, factoryd.SourceAPI, factoryd.SourcePlan, factoryd.SourceDelegate,
	factoryd.SourceChat, factoryd.SourceSuccessor}

// Every card source Go code files has its brief in SourceBriefs, and each
// fixture of the source stores that brief, written from the fixture's
// facts, which passes the filing door.
func TestEveryCodeFiledSourceHasABrief(t *testing.T) {
	fixtures := facefixtures.All()
	for _, src := range factoryd.CardSources() {
		if slices.Contains(writtenSources, src) {
			continue
		}
		brief, ok := factoryd.SourceBriefs[src]
		if !ok {
			t.Errorf("%s: no entry in SourceBriefs", src)
			continue
		}
		var seen int
		for _, fx := range fixtures {
			if fx.Group != facefixtures.GroupSource || !strings.HasPrefix(fx.Kind, src+"/") {
				continue
			}
			seen++
			want := brief(fx.Brief)
			k := fx.Task
			got := factoryd.BriefText{Title: factoryd.CardTitle(k), Why: k.Why, Summary: k.Summary}
			if got != (factoryd.BriefText{Title: want.Title, Why: want.Why, Summary: want.Summary}) {
				t.Errorf("%s: the fixture stores %+v, SourceBriefs writes %+v", fx.Kind, got, want)
			}
			_, got.Body, _ = strings.Cut(k.Objective, "\n\n")
			if refusals := factoryd.CheckBrief(got, factoryd.BriefJobOf(k), everyRef{}); len(refusals) > 0 {
				t.Errorf("%s: the door refuses the brief %+v: %q", fx.Kind, got, refusals)
			}
		}
		if seen == 0 {
			t.Errorf("%s: no fixture", src)
		}
	}
}

// The held production's ask fits an ask's length for the longest name a
// project may carry, 37 characters by the policy's rule of a project's file.
func TestTheHeldProductionsAskFitsTheLongestProjectName(t *testing.T) {
	name := strings.Repeat("a", 37)
	b := factoryd.SourceBriefs[factoryd.SourceProductionHeld](factoryd.BriefFacts{Name: name})
	if want := "Deploys of " + name + " to production wait for an open plan. How should they go on?"; b.Title != want {
		t.Errorf("the ask %q, want %q", b.Title, want)
	}
	if refusals := factoryd.CheckBrief(b, factoryd.BriefAsk, everyRef{}); len(refusals) > 0 {
		t.Errorf("the door refuses the ask of %d characters: %q", utf8.RuneCountInString(b.Title), refusals)
	}
}
