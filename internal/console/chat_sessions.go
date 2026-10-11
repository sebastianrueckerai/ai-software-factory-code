package console

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The session reads' bounds.
const (
	transcriptMax = 8 << 20
	stepLineMax   = 1 << 20
	stepsListed   = 100
	stepsInFull   = 20
	stepCut       = 8 << 10
	stepsCut      = 32 << 10
	searchHits    = 20
	filesListed   = 50
	excerptSide   = 100
	startEvents   = 500
)

// sessionRef is one of a card's sessions by its timeline, N its number
// among them from 1; a session that has not ended is running.
type sessionRef struct {
	N                        int
	ID, Model                string
	Attempt                  int
	Started, Ended           time.Time
	Outcome, Summary, Commit string
}

func textAt(m map[string]any, key string) string { s, _ := m[key].(string); return s }

func numAt(m map[string]any, key string) float64 { v, _ := m[key].(float64); return v }

func stepTime(m map[string]any) time.Time {
	at, _ := time.Parse(time.RFC3339, textAt(m, "time"))
	return at
}

// sessionsOf are a timeline's sessions in the order they started, each
// with its end and a pushed one with its commit.
func sessionsOf(steps []map[string]any) []sessionRef {
	var out []sessionRef
	for _, m := range steps {
		id := textAt(m, "session")
		i := len(out) - 1
		for i >= 0 && out[i].ID != id {
			i--
		}
		switch m["step"] {
		case "session_start":
			out = append(out, sessionRef{N: len(out) + 1, ID: id, Model: textAt(m, "model"), Attempt: int(numAt(m, "attempt")),
				Started: stepTime(m), Outcome: "running"})
		case "session_end":
			if i >= 0 && out[i].Ended.IsZero() {
				s := &out[i]
				s.Ended, s.Outcome, s.Summary, s.Commit = stepTime(m), textAt(m, "outcome"), textAt(m, "summary"), cmp.Or(textAt(m, "commit"), s.Commit)
			}
		case "pushed":
			if i >= 0 {
				out[i].Commit = cmp.Or(textAt(m, "commit"), out[i].Commit)
			}
		}
	}
	return out
}

// startedBecause is why session s of all started: the newest answer,
// grant, finding sent to the agent, change request, merge conflict,
// provider wait or retry between the previous session's end (the card's
// creation for the first) and s's start, in the card's timeline steps and
// its events; with none, how the previous session ended.
func startedBecause(all []sessionRef, s sessionRef, steps []map[string]any, events []factoryd.Event) string {
	var prev *sessionRef
	var since, newest time.Time
	if s.N > 1 {
		prev = &all[s.N-2]
		if since = prev.Ended; since.IsZero() {
			since = prev.Started
		}
	}
	why := ""
	take := func(at time.Time, reason string) {
		if reason != "" && at.After(since) && !at.After(s.Started) && !at.Before(newest) {
			why, newest = reason, at
		}
	}
	for _, m := range steps {
		reason := ""
		switch m["step"] {
		case "stage_findings":
			if stage := textAt(m, "stage"); textAt(m, "to") == "agent" && stage != "session" && !strings.HasPrefix(stage, "supervisor:") {
				reason = "the " + stage + "'s findings"
				if strings.HasSuffix(stage, "s") {
					reason = "the " + stage + "' findings"
				}
			}
		case "signal":
			if m["signal"] == factoryd.SignalChangesRequested && m["ignored"] == nil {
				reason = "a change request"
			}
		case "merge_conflict":
			reason = "a merge conflict with main"
		case factoryd.StepProviderRefused:
			reason = "the provider answered again"
		case "plan_refused", "plan_invalid":
			reason = "factoryd's findings on the plan"
		case factoryd.StepEvent:
			if m["event"] == factoryd.EventCardCreated && textAt(m, "replaces") != "" {
				reason = "a retry"
			}
		}
		take(stepTime(m), reason)
	}
	for _, e := range events {
		reason := ""
		switch e.Kind {
		case factoryd.EventQuestionAnswered, factoryd.EventPlanQuestionAnswered:
			reason = "the member's answer (" + e.Actor + ")"
		case factoryd.EventSessionGranted:
			reason = "a granted session (" + e.Actor + ")"
		case factoryd.EventMergeDenied, factoryd.EventPlanChangesRequested:
			reason = "a change request"
		}
		take(e.At, reason)
	}
	switch {
	case why != "":
		return why
	case prev == nil:
		return "the card's first session"
	case prev.Outcome == string(factoryd.OutcomePushed):
		return "the previous session's push went back to the agent"
	}
	return "the previous session ended without a push"
}

// sessions are card in's timeline and sessions, refused when in.N
// numbers none of them.
func (t *turn) sessions(in toolInput) (factoryd.Task, []map[string]any, []sessionRef, error) {
	k, err := t.card(in)
	if err != nil {
		return k, nil, nil, err
	}
	steps, err := t.c.readTimeline(t.r.Context(), k.ID)
	if err != nil {
		return k, nil, nil, err
	}
	all := sessionsOf(steps)
	if len(all) == 0 {
		return k, nil, nil, refuse(http.StatusConflict, fmt.Sprintf("Card #%d has had no session yet.", k.IssueNumber))
	}
	if in.N < 0 || in.N > len(all) {
		return k, nil, nil, refuse(http.StatusNotFound, fmt.Sprintf("Card #%d has no session %d; it has had %s.",
			k.IssueNumber, in.N, textutil.Plural(len(all), "session", "sessions")))
	}
	return k, steps, all, nil
}

// sessionAt is the session n numbers, the newest for 0.
func sessionAt(all []sessionRef, n int) sessionRef {
	if n == 0 {
		return all[len(all)-1]
	}
	return all[n-1]
}

// transcript is session s's transcript of card taskID; a refusal when it
// has none, and one with the body when it is too large to parse.
func (t *turn) transcript(taskID string, s sessionRef) ([]byte, error) {
	body, err := t.c.cfg.Timelines.Get(t.r.Context(), "sessions/"+taskID+"/"+s.ID+".jsonl")
	switch {
	case err != nil:
		return nil, err
	case body == nil:
		return nil, refuse(http.StatusNotFound, fmt.Sprintf("no transcript for session %d", s.N))
	case len(body) > transcriptMax:
		return body, refuse(http.StatusConflict, fmt.Sprintf("session %d's transcript is %.1f MiB; too large to read", s.N, float64(len(body))/(1<<20)))
	}
	return body, nil
}

// eachStep calls f with each line of a transcript, numbered from 1; a
// line too long to decode or no event is an unreadable step.
func eachStep(body []byte, f func(i int, ev harness.Event)) {
	i := 0
	for line := range bytes.Lines(body) {
		if line = bytes.TrimSpace(line); len(line) == 0 {
			continue
		}
		i++
		var ev harness.Event
		if len(line) > stepLineMax || json.Unmarshal(line, &ev) != nil {
			ev = harness.Event{Kind: "unreadable"}
		}
		f(i, ev)
	}
}

// sessionSteps are the steps of the session in numbers, parsed.
func (t *turn) sessionSteps(in toolInput) (sessionRef, []harness.Event, error) {
	k, _, all, err := t.sessions(in)
	if err != nil {
		return sessionRef{}, nil, err
	}
	s := sessionAt(all, in.N)
	body, err := t.transcript(k.ID, s)
	if err != nil {
		return s, nil, err
	}
	var evs []harness.Event
	eachStep(body, func(_ int, ev harness.Event) { evs = append(evs, ev) })
	return s, evs, nil
}

// offset is at's time since a session's start as m:ss.
func offset(at, start time.Time) string {
	if at.IsZero() || start.IsZero() {
		return "-:--"
	}
	d := max(at.Sub(start), 0)
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// compact is v as one line of JSON.
func compact(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// diffStat is a checkpoint's diff stat, "" for a checkpoint without one.
func diffStat(d map[string]any) string {
	diff, ok := d["diff"].(map[string]any)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s, +%d -%d", textutil.Plural(int(numAt(diff, "files")), "file", "files"), int(numAt(diff, "insertions")), int(numAt(diff, "deletions")))
}

// commas prints numbers with thousands separators.
var commas = message.NewPrinter(language.English)

// stepLine is step i on one line, without content. A model call's prompt
// tokens are opencode's total less its output and reasoning (its input
// leaves out the cached tokens); a step recorded before the cache count
// was kept shows no cached share. opencode's own cost is left out: the
// factory's provider publishes no price, so it is 0; the ledger's cost line
// is the call's EUR.
func stepLine(i int, ev harness.Event, start time.Time) string {
	d := ev.Detail
	head := fmt.Sprintf("%d %s ", i, offset(ev.Time, start))
	if ev.Kind == harness.EventModelCall {
		tokens, _ := d["tokens"].(map[string]any)
		prompt := numAt(tokens, "total") - numAt(tokens, "output") - numAt(tokens, "reasoning")
		line := head + commas.Sprintf("model call, %d prompt tokens", int(prompt))
		if cache, ok := tokens["cache"].(map[string]any); ok && prompt > 0 {
			line += fmt.Sprintf(" (%.0f%% cached)", 100*numAt(cache, "read")/prompt)
		}
		return line
	}
	name := string(ev.Kind)
	if ev.Kind == harness.EventToolCall {
		name = cmp.Or(textAt(d, "tool"), name)
	}
	if ms, ok := d["duration_ms"].(float64); ok && ms < 1000 {
		name += fmt.Sprintf(" %d ms", int(ms))
	} else if ok {
		name += fmt.Sprintf(" %.1f s", ms/1000)
	}
	var sum string
	switch ev.Kind {
	case harness.EventToolCall:
		if s := textAt(d, "status"); s != "" && s != "completed" {
			name += ", " + s
		}
		if args, ok := d["args"]; ok {
			sum = textutil.Truncate(compact(args), 80)
		}
	case harness.EventFileChange:
		sum = textutil.Truncate(oneLine(textAt(d, "path")), 200)
		if n, ok := d["bytes"].(float64); ok {
			sum += fmt.Sprintf(", %d bytes", int(n))
		}
	case harness.EventCommand:
		sum = textutil.Truncate(oneLine(textAt(d, "command")), 80)
		if exit, ok := d["exit"].(float64); ok {
			sum += fmt.Sprintf(", exit %d", int(exit))
		}
	case harness.EventCheckpoint:
		sum = diffStat(d)
	case harness.EventSupervisor:
		sum = textAt(d, "detector")
	}
	if sum == "" {
		return head + name
	}
	return head + name + ": " + sum
}

// stepFull is what step ev holds beyond its line; never a checkpoint's
// patch.
func stepFull(ev harness.Event) string {
	d := ev.Detail
	var parts []string
	add := func(prefix, v string) {
		if v != "" {
			parts = append(parts, prefix+v)
		}
	}
	switch ev.Kind {
	case harness.EventModelCall:
		add("", textAt(d, "text"))
	case harness.EventToolCall:
		if args, ok := d["args"]; ok {
			add("args: ", compact(args))
		}
		add("error: ", textAt(d, "error"))
	case harness.EventCommand:
		add("$ ", textAt(d, "command"))
		add("", textAt(d, "output"))
	case harness.EventCheckpoint:
	default:
		for _, k := range slices.Sorted(maps.Keys(d)) {
			if s, ok := d[k].(string); ok {
				add(k+": ", s)
			} else if d[k] != nil {
				add(k+": ", compact(d[k]))
			}
		}
	}
	return strings.Join(parts, "\n")
}

// stepRange is in's from and to over total steps: to defaults to span
// steps from from, clamped to the end and to most steps.
func stepRange(n int, in toolInput, total, span, most int) (int, int, error) {
	from, to := in.From, in.To
	switch {
	case from < 1 || to < 0:
		return 0, 0, refuse(http.StatusBadRequest, "from and to are step numbers, from 1.")
	case from > total:
		return 0, 0, refuse(http.StatusBadRequest, fmt.Sprintf("Session %d has %s.", n, textutil.Plural(total, "step", "steps")))
	case to != 0 && to < from:
		return 0, 0, refuse(http.StatusBadRequest, fmt.Sprintf("to %d is before from %d.", to, from))
	}
	if to == 0 {
		to = from + span - 1
	}
	return from, min(to, from+most-1, total), nil
}

// sessionText is a session's summary from the timeline and its
// transcript, read once.
func (t *turn) sessionText(in toolInput) (string, error) {
	k, steps, all, err := t.sessions(in)
	if err != nil {
		return "", err
	}
	events, err := t.c.cfg.Events.TaskEvents(t.r.Context(), k.ID, startEvents)
	if err != nil {
		return "", err
	}
	s := sessionAt(all, in.N)
	var b strings.Builder
	fmt.Fprintf(&b, "session %d of %d (%s)", s.N, len(all), s.ID)
	if s.Attempt > 0 {
		fmt.Fprintf(&b, ", attempt %d", s.Attempt)
	}
	fmt.Fprintf(&b, ", model %s: %s", s.Model, s.Outcome)
	if s.Commit != "" {
		b.WriteString(", commit " + s.Commit)
	}
	b.WriteString("\nstarted because: " + startedBecause(all, s, steps, events))
	const day = "2006-01-02 15:04 UTC"
	if s.Ended.IsZero() {
		fmt.Fprintf(&b, "\nrunning since %s", s.Started.UTC().Format(day))
	} else {
		fmt.Fprintf(&b, "\nstarted %s, took %s", s.Started.UTC().Format(day), minutes(s.Ended.Sub(s.Started)))
	}
	if s.Summary != "" {
		b.WriteString("\nsummary: " + textutil.Truncate(s.Summary, 2000))
	}
	var told []string
	for _, m := range steps {
		if textAt(m, "session") != s.ID {
			continue
		}
		var line string
		switch m["step"] {
		case "supervisor":
			line = textAt(m, "detector") + " tripped: " + textutil.Truncate(oneLine(cmp.Or(textAt(m, "guidance"), textAt(m, "reason"))), 300)
		case "nudge":
			line = fmt.Sprintf("nudged after %d identical %s calls", int(numAt(m, "count")), textAt(m, "tool"))
			if textAt(m, "fallback") != "" {
				line += ", by a redirect"
			}
		case "message":
			line = "the supervisor's message"
			if from := textAt(m, "from"); from != "" {
				line = "message from " + from
			}
		default:
			continue
		}
		if e := textAt(m, "error"); e != "" {
			line += ", not delivered: " + oneLine(e)
		}
		told = append(told, "\n  "+offset(stepTime(m), s.Started)+" "+line)
	}
	if len(told) > 0 {
		b.WriteString("\nsupervisor and messages:" + strings.Join(told, ""))
	}
	calls, err := t.c.cfg.Costs.ModelCalls(t.r.Context(), k.ID)
	if err != nil {
		return "", err
	}
	var during []policy.ModelCall
	for _, c := range calls {
		if !c.At.Before(s.Started) && (s.Ended.IsZero() || !c.At.After(s.Ended)) {
			during = append(during, c)
		}
	}
	b.WriteString("\n" + strings.Join(costLines(during), "\n"))
	body, err := t.transcript(k.ID, s)
	if no := (*refusal)(nil); errors.As(err, &no) {
		return b.String() + "\n" + no.msg, nil
	}
	if err != nil {
		return "", err
	}
	var files []string
	seen := map[string]bool{}
	stat, n := "none", 0
	eachStep(body, func(i int, ev harness.Event) {
		n = i
		switch ev.Kind {
		case harness.EventFileChange:
			if p := textAt(ev.Detail, "path"); p != "" && !seen[p] {
				seen[p] = true
				files = append(files, textutil.Truncate(oneLine(p), 200))
			}
		case harness.EventCheckpoint:
			stat = cmp.Or(diffStat(ev.Detail), stat)
		}
	})
	changed := "none"
	if len(files) > 0 {
		changed = fmt.Sprintf("%s (%d)", strings.Join(files[:min(len(files), filesListed)], ", "), len(files))
	}
	fmt.Fprintf(&b, "\nfiles changed: %s\ndiff: %s\n%s", changed, stat, textutil.Plural(n, "step", "steps"))
	return b.String(), nil
}

// costLines are calls per ref (an alias, or a factoryd purpose and its
// alias), one line each: the calls, the prompt tokens per call (the
// median, the lower middle one for an even count, and the max) and the
// share the provider served from its cache, over the calls that reported
// usage, and the EUR per call over all.
func costLines(calls []policy.ModelCall) []string {
	if len(calls) == 0 {
		return []string{"cost: no model calls"}
	}
	byRef := map[string][]policy.ModelCall{}
	for _, c := range calls {
		byRef[c.Ref] = append(byRef[c.Ref], c)
	}
	var lines []string
	for _, ref := range slices.Sorted(maps.Keys(byRef)) {
		var eur float64
		var prompts []int
		var prompt, cached int
		for _, c := range byRef[ref] {
			eur += c.EUR
			if c.PromptTokens > 0 {
				prompts = append(prompts, c.PromptTokens)
				prompt, cached = prompt+c.PromptTokens, cached+c.CachedTokens
			}
		}
		n, word := len(byRef[ref]), "calls"
		if n == 1 {
			word = "call"
		}
		line := commas.Sprintf("cost: %s, %d %s", ref, n, word)
		if without := n - len(prompts); without > 0 {
			line += commas.Sprintf(" (%d without usage)", without)
		}
		if len(prompts) > 0 {
			slices.Sort(prompts)
			line += commas.Sprintf(", prompt tokens per call median %d max %d, %.0f%% cached",
				prompts[(len(prompts)-1)/2], prompts[len(prompts)-1], 100*float64(cached)/float64(prompt))
		}
		lines = append(lines, line+fmt.Sprintf(", %.4f EUR per call", eur/float64(n)))
	}
	return lines
}

func (t *turn) stepsText(in toolInput) (string, error) {
	s, evs, err := t.sessionSteps(in)
	if err != nil {
		return "", err
	}
	in.From = cmp.Or(in.From, 1)
	from, to, err := stepRange(s.N, in, len(evs), stepsListed, stepsListed)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i := from; i <= to; i++ {
		b.WriteString(stepLine(i, evs[i-1], s.Started) + "\n")
	}
	fmt.Fprintf(&b, "steps %d to %d of %d", from, to, len(evs))
	return b.String(), nil
}

func (t *turn) stepText(in toolInput) (string, error) {
	s, evs, err := t.sessionSteps(in)
	if err != nil {
		return "", err
	}
	from, to, err := stepRange(s.N, in, len(evs), 1, stepsInFull)
	if err != nil {
		return "", err
	}
	var blocks []string
	for i := from; i <= to; i++ {
		block := stepLine(i, evs[i-1], s.Started)
		if full := stepFull(evs[i-1]); full != "" {
			block += "\n" + full
		}
		if n := len(block); n > stepCut {
			block = textutil.Truncate(block, stepCut) + fmt.Sprintf("\n(step cut at 8 KiB of %d bytes)", n)
		}
		blocks = append(blocks, block)
	}
	answer := strings.Join(blocks, "\n\n") + fmt.Sprintf("\n\nsteps %d to %d of %d", from, to, len(evs))
	if len(answer) > stepsCut {
		answer = textutil.Truncate(answer, stepsCut) + "\n(answer cut; ask for a narrower range)"
	}
	return answer, nil
}

// searched are the fields of ev a search reads, in order.
func searched(ev harness.Event) []string {
	var out []string
	for _, k := range []string{"args", "text", "output", "error", "path", "command", "guidance"} {
		switch v := ev.Detail[k].(type) {
		case nil:
		case string:
			if v != "" {
				out = append(out, v)
			}
		default:
			out = append(out, compact(v))
		}
	}
	return out
}

// excerpt is line around its match at [i, j): at most excerptSide bytes
// on either side.
func excerpt(line string, i, j int) string {
	if j+excerptSide < len(line) {
		line = textutil.Truncate(line, j+excerptSide)
	}
	if i > excerptSide {
		line = textutil.Tail(line, len(line)-(i-excerptSide))
	}
	return line
}

// search adds the hits of re in session s's transcript to hits, up to
// searchHits, and counts every hit in total.
func search(s sessionRef, body []byte, re *regexp.Regexp, hits *[]string, total *int) {
	var prev string
	var waiting []int
	eachStep(body, func(i int, ev harness.Event) {
		line := stepLine(i, ev, s.Started)
		for _, w := range waiting {
			(*hits)[w] += "\n  after: " + line
		}
		waiting = waiting[:0]
		for _, field := range searched(ev) {
			lines := strings.Split(strings.TrimRight(field, "\n"), "\n")
			for j, l := range lines {
				at := re.FindStringIndex(l)
				if at == nil {
					continue
				}
				if *total++; len(*hits) == searchHits {
					continue
				}
				hit := fmt.Sprintf("session %d step %d (%s): %s", s.N, i, ev.Kind, excerpt(l, at[0], at[1]))
				if len(lines) > 1 {
					if j > 0 {
						hit += "\n  before: " + textutil.Truncate(lines[j-1], 2*excerptSide)
					}
					if j < len(lines)-1 {
						hit += "\n  after: " + textutil.Truncate(lines[j+1], 2*excerptSide)
					}
				} else {
					if prev != "" {
						hit += "\n  before: " + prev
					}
					waiting = append(waiting, len(*hits))
				}
				*hits = append(*hits, hit)
			}
		}
		prev = line
	})
}

// endHits are the lines of the sessions' ends, a failed session's error
// among them, that re matches: each session's first, named by its number
// when the search reads more than one session.
func endHits(all []sessionRef, re *regexp.Regexp) []string {
	var out []string
	for _, s := range all {
		for _, l := range strings.Split(s.Summary, "\n") {
			if at := re.FindStringIndex(l); at != nil {
				whose := "the session's"
				if len(all) > 1 {
					whose = fmt.Sprintf("session %d's", s.N)
				}
				out = append(out, "no step matches; "+whose+" end says: "+excerpt(l, at[0], at[1]))
				break
			}
		}
	}
	return out
}

func (t *turn) searchText(in toolInput) (string, error) {
	if n := utf8.RuneCountInString(in.Query); n < 2 || n > 200 {
		return "", refuse(http.StatusBadRequest, "The query is 2 to 200 characters.")
	}
	k, _, all, err := t.sessions(in)
	if err != nil {
		return "", err
	}
	if in.N > 0 {
		all = all[in.N-1 : in.N]
	}
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(in.Query))
	var hits, notes []string
	total := 0
	for _, s := range all {
		body, err := t.transcript(k.ID, s)
		if body == nil {
			if no := (*refusal)(nil); errors.As(err, &no) {
				notes = append(notes, no.msg)
				continue
			}
			return "", err
		}
		search(s, body, re, &hits, &total)
	}
	answer := fmt.Sprintf("no hit for %q", in.Query)
	if len(hits) > 0 {
		answer = strings.Join(hits, "\n")
	} else if ends := endHits(all, re); len(ends) > 0 {
		answer = strings.Join(ends, "\n")
	}
	if total > len(hits) {
		answer += fmt.Sprintf("\n... and %d more", total-len(hits))
	}
	for _, n := range notes {
		answer += "\n" + n
	}
	return answer, nil
}
