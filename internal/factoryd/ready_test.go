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

// A ready card names one of the card sizes and carries acceptance
// criteria, its own or its body's section.
func TestReadyFindings(t *testing.T) {
	key := "t-key"
	if got := ReadyFindings(key, "a body", "small", []string{"TestA passes"}); len(got) != 0 {
		t.Errorf("a ready card: findings %v, want none", got)
	}
	if got := ReadyFindings(key, "b", "large", []string{"x"}); len(got) != 0 {
		t.Errorf("a large card: findings %v, want none", got)
	}
	got := ReadyFindings(key, "a body", "", nil)
	want := `card t-key: size "": want small, medium or large` + "; card t-key: 0 acceptance tests; want 1 to 10"
	if len(got) != 2 || strings.Join(got, "; ") != want {
		t.Errorf("findings = %v, want %q", got, want)
	}
	got = ReadyFindings(key, "b", "huge", []string{"x"})
	if len(got) != 1 || got[0] != "card t-key: size huge: split it; a card is one package, one page or one set of endpoints" {
		t.Errorf("a size the ladder does not name: findings %v, want the taxonomy line", got)
	}
	got = ReadyFindings(key, "b\n\nAcceptance:\n- TestA\n", "small", nil)
	if len(got) != 0 {
		t.Errorf("a card whose body carries the section: findings %v, want none", got)
	}
}

type fakeCounts struct {
	admitted, queue int
	err             error
}

func (f fakeCounts) CountAdmitted(context.Context, string) (int, error) { return f.admitted, f.err }
func (f fakeCounts) CountQueue(context.Context, string) (int, error)    { return f.queue, f.err }

func readyTree() policy.Tree {
	return policy.Tree{Projects: map[string]policy.ProjectConfig{
		"p": {Repository: "o/r", WorkInProgress: 2, QueueCap: 3}}}
}

func TestFileRefusal(t *testing.T) {
	ctx := context.Background()
	tree := readyTree()
	got := FileRefusal(ctx, fakeCounts{queue: 3}, tree, "o/r", "t-new", "Title", "body\n\nAcceptance:\n- TestA\n", "small")
	if got != "card t-new: the queue holds 3 of its 3 cards; file it when one starts" {
		t.Errorf("at the cap: refusal %q, want the cap line", got)
	}
	if got := FileRefusal(ctx, fakeCounts{queue: 2}, tree, "o/r", "t-new", "Title", "body\n\nAcceptance:\n- TestA\n", "small"); got != "" {
		t.Errorf("under the cap: refusal %q, want none", got)
	}
	got = FileRefusal(ctx, fakeCounts{queue: 0}, tree, "o/r", "t-new", "Title", "a body without criteria", "small")
	if got != "card t-new: 0 acceptance tests; want 1 to 10" {
		t.Errorf("without criteria: refusal %q, want the acceptance line", got)
	}
}

// CountQueue counts the agent cards under the work bound that hold no
// admission stamp and have not ended: the queued cards not yet admitted;
// an admitted, a finished, a person's card and a roll, which the bound
// leaves out, count none.
func TestCountQueue(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_ready"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`TRUNCATE tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, tt := range []struct {
		id, status, kind, track, source string
		admitted                        bool
	}{
		{"t-queued-1", "queued", "agent", "direct", "webhook", false},
		{"t-queued-2", "queued", "agent", "plan", "webhook", false},
		{"t-admitted", "queued", "agent", "direct", "webhook", true},
		{"t-running", "running", "agent", "direct", "webhook", true},
		{"t-waiting-human", "waiting-human", "agent", "direct", "webhook", true},
		{"t-done", "done", "agent", "direct", "webhook", true},
		{"t-human", "waiting-human", "human", "", "webhook", false},
		{"t-roll", "queued", "agent", "direct", SourceRoll, false},
	} {
		if err := s.insertTask(ctx, Task{ID: tt.id, Repository: "o/r", Kind: tt.kind, Track: tt.track, Source: tt.source, Status: tt.status, Objective: "b"}, ""); err != nil {
			t.Fatal(err)
		}
		if tt.admitted {
			if err := s.SetAdmitted(ctx, tt.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	n, err := s.CountQueue(ctx, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("CountQueue = %d, want 2: the unadmitted queued cards", n)
	}
	if n, err := s.CountQueue(ctx, "o/other"); err != nil || n != 0 {
		t.Errorf("CountQueue(other) = %d, %v, want 0, nil", n, err)
	}
}
