package factoryd

import (
	"slices"
	"testing"
)

func TestStatusMeaning(t *testing.T) {
	tests := []struct {
		status, want string
	}{
		{"backlog", "waits in the backlog until a member moves it into a lane"},
		{"queued", "waits for its session to start"},
		{"waiting-deps", "waits for the cards it depends on"},
		{"running", "runs an agent session"},
		{"closing", "is being closed"},
		{"in-review", "passes the gates after its session"},
		{"waiting-human", "waits on a human: an answer, an approval or a decision"},
		{"waiting-poster", "waits on its poster's information"},
		{"done", "is done"},
		{"failed", "has failed"},
		{"closed", "was closed"},
		{"bogus", "is in an unknown state"},
	}
	for _, tt := range tests {
		if got := StatusMeaning(tt.status); got != tt.want {
			t.Errorf("StatusMeaning(%q) = %q, want %q", tt.status, got, tt.want)
		}
	}
	var listed []string
	for _, tt := range tests[:11] {
		listed = append(listed, tt.status)
	}
	if !slices.Equal(Statuses, listed) {
		t.Errorf("Statuses = %v, want %v", Statuses, listed)
	}
}

func TestGateMeaning(t *testing.T) {
	tests := []struct {
		gate, want string
	}{
		{GateIntake, "the issue becomes a card"},
		{GateSession, "an agent works in a sandbox"},
		{GateChecks, "the pull request's CI"},
		{GatePolicy, "risk class from the changed paths"},
		{GateMigration, "schema migrations flagged for review"},
		{GateReproduction, "the issue's reproduction command against the fix"},
		{GateReview, "a model reviews the diff"},
		{GateAudit, "a model audits the diff for security"},
		{GateApproval, "a human approves the merge"},
		{GateMerge, "factory-merge merges"},
		{GateHuman, "a human decides"},
		{GateCommissioning, "a server is commissioned"},
		{GateScreening, "a model screens an outsider's issue"},
		{GateDependencies, "waits for the cards it depends on"},
		{GateMergeQueue, "waits for its turn to merge"},
	}
	for _, tt := range tests {
		if got := GateMeaning(tt.gate); got != tt.want {
			t.Errorf("GateMeaning(%q) = %q, want %q", tt.gate, got, tt.want)
		}
	}
}
