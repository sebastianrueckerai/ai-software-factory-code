package factoryd

import (
	"context"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func TestCardAcceptance(t *testing.T) {
	body := "Do the thing.\n\nAcceptance:\n- TestA passes\n- TestB passes\n\nSize: small\n"
	got := CardAcceptance(body)
	if len(got) != 2 || got[0] != "TestA passes" || got[1] != "TestB passes" {
		t.Errorf("CardAcceptance = %#v, want the two items", got)
	}
	if got := CardAcceptance("no section at all"); got != nil {
		t.Errorf("CardAcceptance without a section = %#v, want nil", got)
	}
	if got := CardAcceptance("Acceptance:\n- only one\n"); len(got) != 1 || got[0] != "only one" {
		t.Errorf("CardAcceptance = %#v, want [only one]", got)
	}
	// the section ends at the first line that is no item
	if got := CardAcceptance("Acceptance:\n- one\nSize: small\n- not in the section\n"); len(got) != 1 {
		t.Errorf("CardAcceptance = %#v, want the section's one item", got)
	}
}

func TestReadyFindings(t *testing.T) {
	key := "t-key"
	if got := ReadyFindings(key, "a body", "small", []string{"TestA passes"}, 0); len(got) != 0 {
		t.Errorf("a ready card: findings %v, want none", got)
	}
	got := ReadyFindings(key, "a body", "", nil, 0)
	want := `card t-key: size "": want small, medium or large` + "; card t-key: 0 acceptance tests; want 1 to 10"
	if len(got) != 2 || strings.Join(got, "; ") != want {
		t.Errorf("findings = %v, want %q", got, want)
	}
	got = ReadyFindings(key, "b", "large", []string{"x"}, 100)
	if len(got) != 1 || got[0] != "card t-key: size large plans up to 100 changed lines and the bound is 100: split the card" {
		t.Errorf("an over-bound large card: findings %v, want the split line", got)
	}
	got = ReadyFindings(key, "b\n\nAcceptance:\n- TestA\n", "small", nil, 0)
	if len(got) != 0 {
		t.Errorf("a card whose body carries the section: findings %v, want none", got)
	}
}

type fakeCounts struct {
	admitted, backlog int
	err               error
}

func (f fakeCounts) CountAdmitted(context.Context, string) (int, error) { return f.admitted, f.err }
func (f fakeCounts) CountBacklog(context.Context, string) (int, error)  { return f.backlog, f.err }

func readyTree() policy.Tree {
	return policy.Tree{Projects: map[string]policy.ProjectConfig{
		"p": {Repository: "o/r", WorkInProgress: 2, BacklogCap: 3}}}
}

func TestStartRefusal(t *testing.T) {
	ctx := context.Background()
	tree := readyTree()
	card := Task{ID: "t1", Repository: "o/r", PlanKey: "k", Size: "small", Objective: "T\n\nbody\n\nAcceptance:\n- TestA\n"}
	if got := StartRefusal(ctx, fakeCounts{admitted: 1}, tree, card); got != "" {
		t.Errorf("a ready card under the bound: refusal %q, want none", got)
	}
	got := StartRefusal(ctx, fakeCounts{admitted: 2}, tree, card)
	if got != "card t1: all 2 work slots of the project are busy; it starts when one frees" {
		t.Errorf("at the bound: refusal %q, want the slots line", got)
	}
	card.Size = ""
	got = StartRefusal(ctx, fakeCounts{admitted: 0}, tree, card)
	if got != `card k: size "": want small, medium or large` {
		t.Errorf("a card without a size: refusal %q, want the size line", got)
	}
	if got := StartRefusal(ctx, fakeCounts{err: context.DeadlineExceeded}, tree,
		Task{ID: "t1", Repository: "o/r", PlanKey: "k", Size: "small", Objective: "T\n\nbody\n\nAcceptance:\n- TestA\n"}); got != "" {
		t.Errorf("a failed count: refusal %q, want none: the workflow's bound holds", got)
	}
}

func TestFileRefusal(t *testing.T) {
	ctx := context.Background()
	tree := readyTree()
	got := FileRefusal(ctx, fakeCounts{backlog: 3}, tree, "o/r", "t-new", "Title", "body\n\nAcceptance:\n- TestA\n", "small")
	if got != "card t-new: the backlog holds 3 of its 3 cards; file it when one starts" {
		t.Errorf("at the cap: refusal %q, want the cap line", got)
	}
	if got := FileRefusal(ctx, fakeCounts{backlog: 2}, tree, "o/r", "t-new", "Title", "body\n\nAcceptance:\n- TestA\n", "small"); got != "" {
		t.Errorf("under the cap: refusal %q, want none", got)
	}
	got = FileRefusal(ctx, fakeCounts{backlog: 0}, tree, "o/r", "t-new", "Title", "a body without criteria", "small")
	if got != "card t-new: 0 acceptance tests; want 1 to 10" {
		t.Errorf("without criteria: refusal %q, want the acceptance line", got)
	}
}

func TestCountBacklog(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_ready"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`TRUNCATE tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, id := range []string{"t-backlog-1", "t-backlog-2"} {
		if err := s.insertTask(ctx, Task{ID: id, Repository: "o/r", Kind: "agent", Lane: policy.LaneBacklog, Status: "backlog", Objective: "b"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		id, status, lane string
		admitted         bool
	}{
		{"t-queued", "queued", policy.LaneReady, false},
		{"t-admitted", "queued", policy.LaneReady, true},
		{"t-running", "running", policy.LaneReady, true},
		{"t-waiting-human", "waiting-human", policy.LaneReady, true},
		{"t-done", "done", policy.LaneReady, true},
		{"t-human", "waiting-human", "", false},
	} {
		if err := s.insertTask(ctx, Task{ID: tt.id, Repository: "o/r", Kind: "agent", Lane: tt.lane, Status: tt.status, Objective: "b"}, ""); err != nil {
			t.Fatal(err)
		}
		if tt.admitted {
			if err := s.SetAdmitted(ctx, tt.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	n, err := s.CountBacklog(ctx, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("CountBacklog = %d, want 3: two backlog cards and the unadmitted queued one", n)
	}
	if n, err := s.CountBacklog(ctx, "o/other"); err != nil || n != 0 {
		t.Errorf("CountBacklog(other) = %d, %v, want 0, nil", n, err)
	}
}
