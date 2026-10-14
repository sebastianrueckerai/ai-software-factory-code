package policy

import (
	"slices"
	"strings"
	"testing"
)

// Hand-derived from the plan (roadmap 239): manual holds every action
// tool, assisted takes the starts and filings off, autonomous keeps only
// the always-confirm set, full auto is autonomous with merges following
// merge_rule.

var wantManual = []string{"new_card", "move_card", "answer", "tell_card", "steer", "start_plan", "retry", "accept_plan", "request_changes", "reject_plan",
	"approve_merge", "deny_merge", "invite", "github_export", "github_sync", "close", "edit", "grant_session", "set_dependency", "split_card",
	"alert_verdict", "promote_resource", "release_resource", "write_doc", "set_autonomy"}

var wantAssisted = []string{"answer", "tell_card", "steer", "request_changes", "reject_plan",
	"approve_merge", "deny_merge", "invite", "github_export", "github_sync", "close", "edit", "set_dependency", "split_card",
	"alert_verdict", "promote_resource", "release_resource", "write_doc", "set_autonomy"}

var wantAutonomous = []string{"approve_merge", "deny_merge", "invite", "alert_verdict", "promote_resource", "release_resource",
	"github_export", "github_sync", "write_doc", "set_autonomy"}

var wantFullAuto = []string{"invite", "alert_verdict", "promote_resource", "release_resource",
	"github_export", "github_sync", "write_doc", "set_autonomy"}

func TestConfirmFor(t *testing.T) {
	base := DefaultChat.Confirm
	for _, tt := range []struct {
		level, merge string
		want         []string
	}{
		{AutonomyManual, MergeHuman, wantManual},
		{"", MergeHuman, wantManual},
		{AutonomyAssisted, MergeHuman, wantAssisted},
		{AutonomyAutonomous, MergeHuman, wantAutonomous},
		{AutonomyAutonomous, MergeAuto, wantFullAuto},
	} {
		if got := ConfirmFor(tt.level, tt.merge, base); !slices.Equal(got, tt.want) {
			t.Errorf("ConfirmFor(%q, %q) = %v, want %v", tt.level, tt.merge, got, tt.want)
		}
	}
}

func TestConfirmForCustomBase(t *testing.T) {
	// manual is the list as its files set it; assisted takes the starts
	// and filings off a custom list and keeps the always-confirm set
	if got := ConfirmFor(AutonomyManual, MergeHuman, []string{"close"}); !slices.Equal(got, []string{"close"}) {
		t.Errorf("ConfirmFor(manual, [close]) = %v, want [close]", got)
	}
	got := ConfirmFor(AutonomyAssisted, MergeHuman, []string{"deny_merge", "new_card"})
	want := []string{"deny_merge", "approve_merge", "invite", "alert_verdict", "promote_resource", "release_resource", "github_export", "github_sync", "write_doc", "set_autonomy"}
	if !slices.Equal(got, want) {
		t.Errorf("ConfirmFor(assisted, [deny_merge new_card]) = %v, want %v", got, want)
	}
}

func TestChatOfAutonomy(t *testing.T) {
	d := Document{}
	for _, tt := range []struct {
		p    ProjectConfig
		want []string
	}{
		{ProjectConfig{Autonomy: AutonomyAssisted}, wantAssisted},
		{ProjectConfig{Autonomy: AutonomyAutonomous, MergeRule: MergeAuto}, wantFullAuto},
		{ProjectConfig{}, wantManual},
	} {
		if got := d.ChatOf(tt.p).Confirm; !slices.Equal(got, tt.want) {
			t.Errorf("ChatOf(%+v) confirm = %v, want %v", tt.p, got, tt.want)
		}
	}
}

func TestChatOfManualKeepsConfirm(t *testing.T) {
	d := Document{}
	p := ProjectConfig{Chat: &ChatConfig{Confirm: []string{"close"}}}
	if got := d.ChatOf(p).Confirm; !slices.Equal(got, []string{"close"}) {
		t.Errorf("ChatOf(manual, confirm [close]) = %v, want [close]", got)
	}
}

func TestAutonomyState(t *testing.T) {
	for _, tt := range []struct {
		p    ProjectConfig
		want string
	}{
		{ProjectConfig{}, AutonomyManual},
		{ProjectConfig{Autonomy: AutonomyAssisted}, AutonomyAssisted},
		{ProjectConfig{Autonomy: AutonomyAutonomous, MergeRule: MergeHuman}, AutonomyAutonomous},
		{ProjectConfig{Autonomy: AutonomyAutonomous, MergeRule: MergeAuto}, AutonomyFullAuto},
	} {
		if got := tt.p.AutonomyState(); got != tt.want {
			t.Errorf("AutonomyState(%+v) = %q, want %q", tt.p, got, tt.want)
		}
	}
}

func TestProjectValidateAutonomy(t *testing.T) {
	ok := ProjectConfig{Repository: "u/u", MergeRule: MergeHuman, Objects: ObjectsSeed, Agents: AgentsNone, Tokens: &SeedTokens{Read: SecretConfig},
		Autonomy: AutonomyAssisted, BacklogCap: 8}
	if err := ok.Validate("u"); err != nil {
		t.Errorf("valid autonomy: %v", err)
	}
	bad := ok
	bad.Autonomy = "fly"
	if err := bad.Validate("u"); err == nil || !contains(err.Error(), "autonomy") {
		t.Errorf("Autonomy fly: want an autonomy refusal, got %v", err)
	}
	bad = ok
	bad.BacklogCap = -1
	if err := bad.Validate("u"); err == nil || !contains(err.Error(), "backlog_cap") {
		t.Errorf("backlog_cap -1: want a backlog_cap refusal, got %v", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestBacklogCapOf(t *testing.T) {
	tr := Tree{Projects: map[string]ProjectConfig{"a": {Repository: "o/a", BacklogCap: 3}, "b": {Repository: "o/b"}}}
	if got := tr.BacklogCapOf("o/a"); got != 3 {
		t.Errorf("BacklogCapOf(o/a) = %d, want 3", got)
	}
	if got := tr.BacklogCapOf("o/b"); got != DefaultBacklogCap {
		t.Errorf("BacklogCapOf(o/b) = %d, want %d", got, DefaultBacklogCap)
	}
	if got := tr.BacklogCapOf("o/x"); got != DefaultBacklogCap {
		t.Errorf("BacklogCapOf(o/x) = %d, want %d", got, DefaultBacklogCap)
	}
}

// A project's chat block reaches the chat whole: its document repository
// and its turn bound merge over the policy's like its other keys.
func TestChatOfMergesDocRepoAndParallelTurns(t *testing.T) {
	d := Document{Chat: &ChatConfig{DocRepo: "sf/ai-software-factory", ParallelTurns: 2}}
	if got := d.ChatOf(ProjectConfig{}); got.DocRepo != "sf/ai-software-factory" || got.ParallelTurns != 2 {
		t.Errorf("the policy's block = %+v, want its doc_repo and parallel_turns", got)
	}
	got := d.ChatOf(ProjectConfig{Chat: &ChatConfig{DocRepo: "uptime/docs", ParallelTurns: 5}})
	if got.DocRepo != "uptime/docs" || got.ParallelTurns != 5 {
		t.Errorf("the project's block = %+v, want its own doc_repo and parallel_turns", got)
	}
	if got := (Document{}).ChatOf(ProjectConfig{}); got.DocRepo != "" || got.ParallelTurns != 0 {
		t.Errorf("no block = %+v, want no doc_repo and the default bound left to the chat", got)
	}
	if err := (&ChatConfig{ParallelTurns: -1}).validate(); err == nil {
		t.Error("parallel_turns -1 validated")
	}
}
