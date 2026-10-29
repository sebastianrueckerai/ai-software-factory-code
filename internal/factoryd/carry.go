package factoryd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

const (
	carryTodoBytes   = 1 << 10
	carryReportBytes = 4 << 10
	carryMaxReports  = 8
	carryMaxFiles    = 30
	carryStopBytes   = 200
)

// carryState is what the next session is handed from a stopped one: the
// working state for its brief and the full subagent reports for the worker
// to write to files.
type carryState struct {
	Prior harness.PriorState
	Files []harness.ReportFile
}

type carryFrom struct {
	Session, Commit, Stop string
	Outcome               SessionOutcome
	Trip                  string
	DeadEnd               bool
}

type carryTask struct{ desc, result string }

// priorState is the working state s continues from, copied from its
// parent's record, which record returns. nil without a parent, for a
// report or plan session, after a parent that pushed (it finished its
// work), and when the record cannot be read or holds no state.
func priorState(s sessionSpec, record func() ([]byte, error)) *carryState {
	if s.parent == "" || s.report != "" || s.planner || s.parentOutcome == OutcomePushed {
		return nil
	}
	rec, err := record()
	if err != nil {
		return nil
	}
	return workingState(rec, carryFrom{Session: s.parent, Commit: s.startRef, Stop: s.parentStop, Outcome: s.parentOutcome, Trip: s.parentTrip,
		DeadEnd: s.parentDeadEnd})
}

// workingState builds the carried state from a session's record; nil when
// the record holds no todo list, no own task call and no file change. The
// heading holds only what factoryd owns; the stop reason, often the
// agent's own last message, is the body's first line.
func workingState(record []byte, from carryFrom) *carryState {
	var (
		last    time.Time
		todos   []string
		tasks   []carryTask
		files   []string
		seen    = map[string]bool{}
		scanner = bufio.NewScanner(bytes.NewReader(record))
	)
	scanner.Buffer(nil, len(record)+1)
	for scanner.Scan() {
		var ev harness.Event
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		if !ev.Time.IsZero() {
			last = ev.Time
		}
		switch ev.Kind {
		case harness.EventToolCall:
			if detailBool(ev, "subagent") {
				continue
			}
			args, _ := ev.Detail["args"].(map[string]any)
			switch detailString(ev, "tool") {
			case "todowrite":
				todos = nil
				list, _ := args["todos"].([]any)
				for _, it := range list {
					m, _ := it.(map[string]any)
					status, _ := m["status"].(string)
					content, _ := m["content"].(string)
					if status != "" {
						status = "[" + status + "] "
					}
					todos = append(todos, "- "+status+content)
				}
			case "task":
				desc, _ := args["description"].(string)
				tasks = append(tasks, carryTask{desc, detailString(ev, "result")})
			}
		case harness.EventFileChange:
			p := strings.TrimPrefix(detailString(ev, "path"), "/workspace/repo/")
			if p != "" && !seen[p] {
				seen[p] = true
				files = append(files, p)
			}
		}
	}
	if len(todos) == 0 && len(tasks) == 0 && len(files) == 0 {
		return nil
	}

	commit := from.Commit
	if len(commit) > 8 {
		commit = commit[:8]
	}
	var how string
	if !last.IsZero() {
		how = " at " + last.UTC().Format("15:04") + " UTC"
	}
	if from.Outcome != "" {
		how += " with outcome " + string(from.Outcome)
	}
	if from.Trip != "" {
		how += " after the " + from.Trip + " trip"
	}
	heading := fmt.Sprintf("From session %s, stopped%s, branch at commit %s. Written before your session started. Where they differ from the code on the branch, the code is right.",
		from.Session, how, commit)
	if from.DeadEnd {
		heading += " The check-in judged this approach wrong; the todo list below is the dead end's, not a plan to follow."
	}

	var sections []string
	if stop := textutil.Truncate(strings.Join(strings.Fields(sanitize.Inbound(from.Stop)), " "), carryStopBytes); stop != "" {
		sections = append(sections, "Stop reason: "+stop)
	}
	if len(todos) > 0 {
		title := "Todo list at the stop:"
		if from.DeadEnd {
			title = "Todo list at the stop (the dead end's):"
		}
		sections = append(sections, title+"\n"+textutil.Truncate(strings.Join(todos, "\n"), carryTodoBytes))
	}

	var reportFiles []harness.ReportFile
	if len(tasks) > 0 {
		skipped := max(len(tasks)-carryMaxReports, 0)
		tasks = tasks[skipped:]
		lines := []string{"Subagent reports (excerpts; the full text is in the files named):"}
		if skipped > 0 {
			lines = append(lines, fmt.Sprintf("%d earlier subagent calls are left out.", skipped))
		}
		for i, tk := range tasks {
			text := reportText(tk.result)
			if text == "" {
				lines = append(lines, fmt.Sprintf("- %q: no report was recorded.", tk.desc))
				continue
			}
			name := fmt.Sprintf("report-%d.md", i+1)
			reportFiles = append(reportFiles, harness.ReportFile{Name: name, Text: sanitize.Inbound("# " + tk.desc + "\n\n" + tk.result)})
			lines = append(lines, fmt.Sprintf("- %s, %q:", name, tk.desc), textutil.Truncate(text, carryReportBytes/len(tasks)))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}

	if len(files) > 0 {
		lines := []string{"Files the session changed:"}
		for i, f := range files {
			if i == carryMaxFiles {
				lines = append(lines, fmt.Sprintf("- and %d more.", len(files)-carryMaxFiles))
				break
			}
			lines = append(lines, "- "+f)
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}

	return &carryState{
		Prior: harness.PriorState{Session: from.Session, Heading: heading, Body: strings.Join(sections, "\n\n")},
		Files: reportFiles,
	}
}

// reportText strips the task tool's wrapper from a subagent's result: a
// leading task_id line and the task_result tags.
func reportText(s string) string {
	if first, rest, ok := strings.Cut(s, "\n"); ok && strings.HasPrefix(first, "task_id:") {
		s = rest
	}
	s = strings.ReplaceAll(s, "<task_result>", "")
	s = strings.ReplaceAll(s, "</task_result>", "")
	return strings.TrimSpace(s)
}
