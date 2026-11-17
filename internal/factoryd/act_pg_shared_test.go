package factoryd

import (
	"context"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The act tests' fixtures that stayed behind: the full rig moved to
// internal/factoryd/pg with the act tests, and the ladder, rung1 ladder
// and monitor pg files still in this package read the store's rows, the
// signals fake and the one-session extend through this minimal copy until
// their own cards move them.

// caseSignals records the signals that reached the workflows.
type caseSignals struct {
	Workflows
	mu  sync.Mutex
	got []reopenCall
}

func (w *caseSignals) Signal(_ context.Context, taskID, name string, sig TaskSignal) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, reopenCall{taskID, name, sig})
	return nil
}

func (w *caseSignals) Terminate(context.Context, string, string, string) error { return nil }

func (w *caseSignals) Alive(context.Context, string) (bool, error)       { return true, nil }
func (w *caseSignals) RunningRuns(context.Context) ([]RunningRun, error) { return nil, nil }

// actRig holds the store whose rows the ladder and monitor rigs read.
type actRig struct {
	s *PGStore
}

// rows are card t1's events of kind, oldest first.
func (g actRig) rows(t *testing.T, kind string) []Event {
	t.Helper()
	events, err := g.s.TaskEvents(context.Background(), "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == kind {
			out = append(out, events[i])
		}
	}
	return out
}

var oneSession = Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}}

// posting is a Post that answers comment id once before ran.
func posting(id int64, before func()) func(context.Context) (int64, error) {
	return func(context.Context) (int64, error) {
		if before != nil {
			before()
		}
		return id, nil
	}
}

// leaky stayed behind for the retry tests: a URL and a bearer token the
// refusals must scrub. Its file's own copy moved with it.
const leaky = "see https://user:pass@host/x with Authorization: Bearer abc123def456"

// atAutonomy stayed behind for the rung files not yet moved: it puts the
// rig's project at state, full auto autonomous with merge rule auto.
func (g rungRig) atAutonomy(state string) {
	v := g.r.cfg.Policy()
	p := v.Projects["demo-app"]
	p.Autonomy, p.MergeRule = state, policy.MergeHuman
	if state == policy.AutonomyFullAuto {
		p.Autonomy, p.MergeRule = policy.AutonomyAutonomous, policy.MergeAuto
	}
	v.Projects = map[string]policy.ProjectConfig{"demo-app": p}
	g.r.cfg.Policy = func() policy.Version { return v }
}
