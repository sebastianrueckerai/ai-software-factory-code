package console

import (
	"bytes"
	"cmp"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// The chat's routine tools (roadmap 267, C16): routines reads the chat
// project's routines, routine_write and routine_delete change its file
// through commitProjectFile after a confirm, and routine_run and
// routine_skip are the board's Run now and Skip once.

// routinesText is the routines read: one line per routine of the chat's
// project.
func (t *turn) routinesText() (string, error) {
	views, err := t.c.cfg.Routines.Views(t.r.Context(), t.p.Name)
	if err != nil {
		log.Printf("console: the routines of %s: %v", t.p.Name, err)
		return "", refuse(http.StatusInternalServerError, "cannot read the routines")
	}
	if len(views) == 0 {
		return "Project " + t.p.Name + " has no routines.", nil
	}
	lines := make([]string, len(views))
	for i, v := range views {
		lines[i] = routineLine(v)
	}
	return strings.Join(lines, "\n"), nil
}

// routineLine is v on one line: its name, title, schedule in words, what
// it files, its next run, its last run with the card, and the open card.
func routineLine(v factoryd.RoutineView) string {
	rt := v.Routine
	parts := []string{rt.Name, rt.Title, rt.Words(), "files " + rt.Files}
	if v.NextWords != "" {
		parts = append(parts, "next "+v.NextWords)
	}
	switch last := "last " + cmp.Or(v.Last.Outcome, "running"); {
	case v.Last.Run == 0:
		parts = append(parts, "no run yet")
	case v.Last.Issue > 0:
		parts = append(parts, fmt.Sprintf("%s (#%d)", last, v.Last.Issue))
	default:
		parts = append(parts, last)
	}
	if v.Open != nil {
		parts = append(parts, fmt.Sprintf("open #%d", v.Open.IssueNumber))
	}
	return strings.Join(parts, " · ")
}

// scheduleOf reads routine_write's schedule: its parts split on ";" and
// trimmed, merges:<n> a count of merged cards, on:<event> an event, any
// other part a cron with its spaces made single, which Routine.Validate
// judges.
func scheduleOf(s string) ([]policy.Trigger, error) {
	var out []policy.Trigger
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
		case strings.HasPrefix(part, "merges:"):
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(part, "merges:")))
			if err != nil || n < 1 {
				return nil, fmt.Errorf("schedule part %q: merges takes a count of merged cards, such as merges:20", part)
			}
			out = append(out, policy.Trigger{Merges: n})
		case strings.HasPrefix(part, "on:"):
			out = append(out, policy.Trigger{On: strings.TrimSpace(strings.TrimPrefix(part, "on:"))})
		default:
			out = append(out, policy.Trigger{Cron: strings.Join(strings.Fields(part), " ")})
		}
	}
	return out, nil
}

// routineChange is what routine_write or routine_delete does to the chat
// project's file: rt is the routine as it will stand, zero for a deletion,
// old the routine the file holds, when exists, and extra the files beside
// the project file the commit writes, a path to its text, "" deleting it.
type routineChange struct {
	rt, old policy.Routine
	exists  bool
	extra   map[string]string
}

// routineChange reads in for tool against the configuration in force. A
// write's routine takes brief_text's file as its brief, else its own, and
// Routine.Validate judges it. A routine of policy.yaml is refused, and so
// is a deletion of a routine the project lacks. A brief the change leaves
// that no other routine names is deleted.
func (t *turn) routineChange(tool string, in toolInput) (routineChange, error) {
	ch := routineChange{extra: map[string]string{}}
	ch.old, ch.exists = t.tree.Routine(t.p.Name, in.Name)
	runs := slices.ContainsFunc(t.tree.ProjectRoutines(t.p.Name), func(r policy.Routine) bool { return r.Name == in.Name })
	switch {
	case runs && !ch.exists:
		return ch, refuse(http.StatusConflict, fmt.Sprintf("Routine %s is policy.yaml's, a pass over every project: the project's chat cannot change it.", in.Name))
	case tool == "routine_delete" && !ch.exists:
		return ch, refuse(http.StatusNotFound, fmt.Sprintf("Project %s has no routine %s. The routines read lists its routines.", t.p.Name, in.Name))
	case tool == "routine_write":
		schedule, err := scheduleOf(in.Schedule)
		if err != nil {
			return ch, refuse(http.StatusBadRequest, err.Error())
		}
		ch.rt = policy.Routine{Name: in.Name, Title: in.Title, Schedule: schedule, Files: in.Files, Brief: ch.old.Brief, Skill: in.Skill,
			WhileOpen: in.WhileOpen, Missed: in.Missed, Tell: in.Tell}
		if in.BriefText != "" {
			ch.rt.Brief = policy.DirRoutines + "/" + t.p.Name + "-" + in.Name + ".md"
			ch.extra[ch.rt.Brief] = strings.TrimRight(in.BriefText, "\n") + "\n"
		}
		if err := ch.rt.Validate(policy.ProjectFile(t.p.Name)); err != nil {
			return ch, refuse(http.StatusUnprocessableEntity, err.Error())
		}
	}
	if ch.old.Brief != "" && ch.old.Brief != ch.rt.Brief && !t.briefNamed(ch.old.Brief, in.Name) {
		ch.extra[ch.old.Brief] = ""
	}
	return ch, nil
}

// briefNamed says whether a routine of the configuration in force other
// than the chat project's routine name names the brief at path.
func (t *turn) briefNamed(path, name string) bool {
	if slices.ContainsFunc(t.tree.Document.Routines, func(r policy.Routine) bool { return r.Brief == path }) {
		return true
	}
	for project, pc := range t.tree.Projects {
		if slices.ContainsFunc(pc.Routines, func(r policy.Routine) bool { return r.Brief == path && (project != t.p.Name || r.Name != name) }) {
			return true
		}
	}
	return false
}

// describeRoutine is the proposal of a routine_write or a routine_delete:
// the routine as YAML, as the write puts it in the file or as the file
// holds what the deletion takes out, with the brief it writes or deletes.
func (t *turn) describeRoutine(tool chatTool, in toolInput) (string, error) {
	ch, err := t.routineChange(tool.name, in)
	if err != nil {
		return "", err
	}
	file := policy.ProjectFile(t.p.Name)
	shown, where := ch.rt, "new in "+file
	switch {
	case tool.name == "routine_delete":
		shown, where = ch.old, "out of "+file
	case ch.exists:
		where = "in place of " + in.Name + " in " + file
	}
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode([]policy.Routine{shown}); err != nil {
		return "", err
	}
	desc := tool.title + ", " + where + ":\n\n" + strings.TrimRight(b.String(), "\n")
	if text := ch.extra[ch.rt.Brief]; ch.rt.Brief != "" && text != "" {
		desc += "\n\nIts brief, " + ch.rt.Brief + ":\n\n" + strings.TrimRight(text, "\n")
	}
	switch text, ok := ch.extra[ch.old.Brief]; {
	case !ok || text != "":
	case tool.name == "routine_delete":
		desc += "\n\nIts brief, " + ch.old.Brief + ", goes too."
	default:
		desc += "\n\nIts old brief, " + ch.old.Brief + ", goes."
	}
	return sanitize.Inbound(desc), nil
}

// writeRoutine is routine_write: the routine into the chat project's file
// in place of the entry of its name, or last, and its brief beside it,
// one commit, and one routine_written event.
func (t *turn) writeRoutine(in toolInput) (string, error) {
	ch, err := t.routineChange("routine_write", in)
	if err != nil {
		return "", err
	}
	ctx := t.r.Context()
	created := false
	pr, err := t.c.commitProjectFile(ctx, t.a, t.p.Name, fmt.Sprintf("Project %s: routine %s", t.p.Name, in.Name),
		func(doc *yaml.Node) (err error) { created, err = putRoutine(doc, ch.rt); return err }, ch.extra)
	if err != nil {
		return "", err
	}
	log.Printf("console: %s wrote routine %s of %s via %s (%s#%d)", t.a.s.user, in.Name, t.p.Name, t.a.via, t.c.cfg.InfraRepo, pr)
	t.c.record(ctx, t.a, t.p.Repository, "", 0, factoryd.EventRoutineWritten, map[string]any{"project": t.p.Name, "routine": in.Name, "created": created})
	did := "changed routine " + in.Name + " in"
	if created {
		did = "added routine " + in.Name + " to"
	}
	return fmt.Sprintf("Pull request #%d %s %s. The board shows it once the factory loads the new configuration.", pr, did, policy.ProjectFile(t.p.Name)), nil
}

// deleteRoutine is routine_delete: the routine out of the chat project's
// file and its brief, unless another routine names it, deleted, one
// commit, and one routine_deleted event.
func (t *turn) deleteRoutine(in toolInput) (string, error) {
	ch, err := t.routineChange("routine_delete", in)
	if err != nil {
		return "", err
	}
	ctx := t.r.Context()
	pr, err := t.c.commitProjectFile(ctx, t.a, t.p.Name, fmt.Sprintf("Project %s: delete routine %s", t.p.Name, in.Name),
		func(doc *yaml.Node) error { return dropRoutine(doc, in.Name) }, ch.extra)
	if err != nil {
		return "", err
	}
	log.Printf("console: %s deleted routine %s of %s via %s (%s#%d)", t.a.s.user, in.Name, t.p.Name, t.a.via, t.c.cfg.InfraRepo, pr)
	t.c.record(ctx, t.a, t.p.Repository, "", 0, factoryd.EventRoutineDeleted, map[string]any{"project": t.p.Name, "routine": in.Name})
	note := fmt.Sprintf("Pull request #%d deleted routine %s from %s", pr, in.Name, policy.ProjectFile(t.p.Name))
	if _, ok := ch.extra[ch.old.Brief]; ok {
		note += ", and its brief " + ch.old.Brief
	}
	return note + ".", nil
}

// putRoutine sets rt in the project file's routines, in place of the
// entry of its name with the comment above it kept, or last, the list
// made when the file has none; created says it went last.
func putRoutine(doc *yaml.Node, rt policy.Routine) (created bool, err error) {
	var n yaml.Node
	if err := n.Encode(rt); err != nil {
		return false, err
	}
	m := doc.Content[0]
	i := keyIndex(m, "routines")
	if i < 0 {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "routines"}, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
		i = len(m.Content) - 2
	}
	list := m.Content[i+1]
	if list.Kind != yaml.SequenceNode {
		return false, fmt.Errorf("routines in the project file is no list")
	}
	if j := routineEntry(list, rt.Name); j >= 0 {
		n.HeadComment = list.Content[j].HeadComment
		list.Content[j] = &n
		return false, nil
	}
	list.Style = 0
	list.Content = append(list.Content, &n)
	return true, nil
}

// dropRoutine takes routine name out of the project file's routines, and
// the routines key with the last one.
func dropRoutine(doc *yaml.Node, name string) error {
	m := doc.Content[0]
	i := keyIndex(m, "routines")
	j := -1
	if i >= 0 && m.Content[i+1].Kind == yaml.SequenceNode {
		j = routineEntry(m.Content[i+1], name)
	}
	if j < 0 {
		return fmt.Errorf("the project file holds no routine %s", name)
	}
	list := m.Content[i+1]
	list.Content = slices.Delete(list.Content, j, j+1)
	if len(list.Content) == 0 {
		m.Content = slices.Delete(m.Content, i, i+2)
	}
	return nil
}

// routineEntry is the index of routine name's entry in list, -1 for none.
func routineEntry(list *yaml.Node, name string) int {
	return slices.IndexFunc(list.Content, func(e *yaml.Node) bool {
		k := keyIndex(e, "name")
		return e.Kind == yaml.MappingNode && k >= 0 && e.Content[k+1].Value == name
	})
}
