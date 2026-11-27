package prompts

import (
	"fmt"
	"reflect"
	"strings"
)

// Prompt is every text a step puts into a context. Only Build makes one.
type Prompt struct {
	step, purpose   string
	standing, facts string
	parts           Parts
	answer          *answerType
}

// Parts are the words of each part, whitespace-separated.
type Parts struct{ Guide, Forms, Chunks, Paragraph, Facts int }

func (p Parts) Total() int { return p.Guide + p.Forms + p.Chunks + p.Paragraph + p.Facts }

// Build joins, in this order, each part separated by one blank line: the
// guide if set, the forms, the chunks, the paragraph, the rendered facts.
// A nil facts renders nothing. A facts value of the wrong type is an error.
func Build(step string, facts any) (Prompt, error) { return texts.build(step, facts) }

func (c *catalog) build(step string, facts any) (Prompt, error) {
	p, ok := c.profiles[step]
	if !ok {
		return Prompt{}, fmt.Errorf("step %s has no profile: write internal/prompts/profiles/%s.yaml", step, step)
	}
	var guide string
	if p.Guide {
		guide = strings.TrimSpace(c.Guide)
	}
	var forms, chunks []string
	for _, name := range p.Forms {
		text, _ := c.Form(name)
		forms = append(forms, text)
	}
	for _, name := range p.Chunks {
		chunks = append(chunks, c.chunks[name])
	}
	paragraph := c.paragraphs[p.Paragraph]
	rendered, err := render(p, facts)
	if err != nil {
		return Prompt{}, err
	}
	var answer *answerType
	if p.Answer != "" {
		a, ok := answerOf(p.Answer)
		if !ok {
			return Prompt{}, fmt.Errorf("step %s: answer %s is not a registered answer type: call prompts.RegisterAnswer beside the step's call", step, p.Answer)
		}
		answer = &a
	}
	return Prompt{
		answer:   answer,
		step:     step,
		purpose:  p.Purpose,
		standing: join(guide, join(forms...), join(chunks...), paragraph),
		facts:    rendered,
		parts: Parts{
			Guide:     words(guide),
			Forms:     words(join(forms...)),
			Chunks:    words(join(chunks...)),
			Paragraph: words(paragraph),
			Facts:     words(rendered),
		},
	}, nil
}

// join joins the non-empty parts with one blank line between two.
func join(parts ...string) string {
	var out []string
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n\n")
}

func words(s string) int { return len(strings.Fields(s)) }

func (p Prompt) Step() string    { return p.step }
func (p Prompt) Purpose() string { return p.purpose }

// Standing is the guide, the forms, the chunks and the paragraph: what a
// session carries as its system prompt.
func (p Prompt) Standing() string { return p.standing }

// Facts is the rendered facts: what a session carries as its first turn.
func (p Prompt) Facts() string { return p.facts }

// Text is Standing, then Facts: what a direct call sends as its system text.
func (p Prompt) Text() string { return join(p.standing, p.facts) }

func (p Prompt) Parts() Parts { return p.parts }

// Answer is the answer tool of the prompt's step, if its profile names one.
func (p Prompt) Answer() (Tool, bool) {
	if p.answer == nil {
		return Tool{}, false
	}
	return p.answer.tool, true
}

// AnswerType is the struct type the step's answer decodes into; nil
// without an answer.
func (p Prompt) AnswerType() reflect.Type {
	if p.answer == nil {
		return nil
	}
	return p.answer.want
}
